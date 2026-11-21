package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// The tier gate is not a security boundary; Kubernetes RBAC is. See AddTool.
type Tier int

const (
	TierRead Tier = iota
	TierWrite
	TierDestructive
)

func (t Tier) String() string {
	switch t {
	case TierRead:
		return "read"
	case TierWrite:
		return "write"
	case TierDestructive:
		return "destructive"
	}
	return "unknown"
}

// Proves the guard is wired: the definition test would pass even without the call site.
type ToolSet interface {
	Name() string
	Register(*Registrar)
}

type Registrar struct {
	srv              *mcp.Server
	allowWrite       bool
	allowDestructive bool

	registered []string

	// mismatches fail server startup, via registerAll.
	mismatches []error

	// byName makes a second claim on a name an error, not a shadowing.
	byName map[string]bool

	// records which Kubernetes credential each admitted tool reaches the API with
	credentials map[string]string

	nodeConn *kube.Connector
}

func newRegistrar(srv *mcp.Server, allowWrite, allowDestructive bool) *Registrar {
	return &Registrar{srv: srv, allowWrite: allowWrite, allowDestructive: allowDestructive}
}

func (r *Registrar) Allows(t Tier) bool {
	switch t {
	case TierRead:
		return true
	case TierWrite:
		return r.allowWrite
	case TierDestructive:
		return r.allowDestructive
	}
	return false
}

func (r *Registrar) Registered() []string {
	out := make([]string, len(r.registered))
	copy(out, r.registered)
	return out
}

// The map is a copy, so the record cannot be edited through it.
func (r *Registrar) credentialSources() map[string]string {
	out := make(map[string]string, len(r.credentials))
	maps.Copy(out, r.credentials)
	return out
}

// Registers at the tier, or does nothing when the operator has not enabled it.
func AddTool[In, Out any](r *Registrar, tier Tier, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	addTool(r, tier, t, h)
}

func addTool[In, Out any](r *Registrar, tier Tier, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) bool {
	if r == nil || t == nil {
		return false
	}
	if err := checkTier(tier, t); err != nil {
		r.mismatches = append(r.mismatches, err)
		return false
	}
	if !r.Allows(tier) {
		return false
	}
	// The SDK's registry replaces silently on a repeated name, so a duplicate must fail here.
	if r.byName[t.Name] {
		r.mismatches = append(r.mismatches,
			fmt.Errorf("tool %q is registered twice: the second registration would silently replace the first", t.Name))
		return false
	}
	if r.byName == nil {
		r.byName = make(map[string]bool)
	}
	r.byName[t.Name] = true

	mcp.AddTool(r.srv, t, h)
	r.registered = append(r.registered, t.Name)
	return true
}

// What a NodeOperation runs with: Out is progress output; Clientset uses the node connector.
type NodeEnv struct {
	Out       io.Writer
	Clientset kubernetes.Interface
}

// What a NodeOperation reports; Next and Warnings reach the operator even on failure.
type NodeResult struct {
	Next     string
	Warnings []string
}

// NodeOperation is a destructive operation on this node, run on the node connector's client.
type NodeOperation[In any] func(ctx context.Context, env NodeEnv, in In) (NodeResult, error)

// Registers op at the destructive tier, run like the engine's destructive tools.
func AddDestructiveNodeTool[In any](r *Registrar, t *mcp.Tool, op NodeOperation[In]) {
	if r == nil || t == nil {
		return
	}
	if op == nil {
		r.mismatches = append(r.mismatches,
			fmt.Errorf("tool %q carries no operation: the tool would register and never answer", t.Name))
		return
	}
	conn := r.nodeConn
	if conn == nil {
		r.mismatches = append(r.mismatches,
			fmt.Errorf("tool %q has no node connector to run on: destructive node tools register only from a ToolSet", t.Name))
		return
	}
	action := t.Name
	h := func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, mcpsrv.DestructiveOut, error) {
		var next string
		out, err := mcpsrv.RunExclusive(action, func(w io.Writer) ([]string, error) {
			cl, err := conn.ReloadCluster("")
			if err != nil {
				return nil, fmt.Errorf("connect to cluster with the node credential: %w", err)
			}
			res, err := op(ctx, NodeEnv{Out: w, Clientset: cl.Typed}, in)
			next = res.Next
			if err != nil {
				return nil, withNodeResult(err, res)
			}
			return res.Warnings, nil
		})
		if err != nil {
			return nil, out, err
		}
		out.Next = next
		return nil, out, nil
	}
	if !addTool(r, TierDestructive, t, h) {
		return
	}
	if r.credentials == nil {
		r.credentials = make(map[string]string)
	}
	r.credentials[t.Name] = string(mcpsrv.CredentialNodeConnector)
}

func withNodeResult(err error, res NodeResult) error {
	var b strings.Builder
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "\nwarning: %s", w)
	}
	if res.Next != "" {
		fmt.Fprintf(&b, "\nnext: %s", res.Next)
	}
	if b.Len() == 0 {
		return err
	}
	return fmt.Errorf("%w\n%s", err, b.String())
}

func checkTier(tier Tier, t *mcp.Tool) error {
	a := t.Annotations
	if a == nil {
		return fmt.Errorf("tool %q was registered at tier %s with no annotations: build it with ReadTool, WriteTool or DestructiveTool", t.Name, tier)
	}
	destructive := a.DestructiveHint != nil && *a.DestructiveHint
	switch {
	case a.ReadOnlyHint && destructive:
		return fmt.Errorf("tool %q is annotated both read-only and destructive", t.Name)
	case a.ReadOnlyHint && tier != TierRead:
		return fmt.Errorf("tool %q is annotated read-only but registered at tier %s: a client may auto-approve it", t.Name, tier)
	case !a.ReadOnlyHint && tier == TierRead:
		return fmt.Errorf("tool %q is registered at tier read but is not annotated read-only", t.Name)
	case tier == TierDestructive && !destructive:
		return fmt.Errorf("tool %q is registered at tier destructive but its DestructiveHint does not say so: a client would treat the call as safe", t.Name)
	}
	return nil
}

func (r *Registrar) err() error {
	if r == nil || len(r.mismatches) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to serve MCP: %w", errors.Join(r.mismatches...))
}

func ReadTool(name, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}
}

func WriteTool(name, description string) *mcp.Tool {
	no := false
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
	}
}

func DestructiveTool(name, description string) *mcp.Tool {
	yes := true
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes},
	}
}

// WithToolSets adds tool sets to `stratum mcp` after the engine's own tools.
func WithToolSets(sets ...ToolSet) Option {
	return func(c *rootConfig) {
		c.toolSets = append(c.toolSets, sets...)
	}
}
