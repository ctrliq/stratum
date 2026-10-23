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

// The MCP extension seam. The MCP server is written and owned by CIQ rather
// than vendored, and gating happens at registration: a tool above the enabled
// tier is never registered, so it never appears in tools/list. The tier gate,
// not the CLI's --yes, is the control. It is not a security boundary;
// Kubernetes RBAC is.

// Tier is how much damage a tool can do. It decides whether the tool is
// registered, not whether a call is allowed: see AddTool.
type Tier int

const (
	// TierRead cannot change anything. Always registered.
	TierRead Tier = iota
	// TierWrite mutates recoverably — a snapshot, a token, a config. Needs
	// --allow-write.
	TierWrite
	// TierDestructive can take a cluster down or lose data. Needs
	// --allow-destructive.
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

// A ToolSet is a named group of MCP tools. Register is called once, at
// startup, and should call AddTool for each tool it contributes.
type ToolSet interface {
	Name() string
	Register(*Registrar)
}

// Registrar admits tools by tier.
type Registrar struct {
	srv              *mcp.Server
	allowWrite       bool
	allowDestructive bool

	// registered is every tool name admitted, in registration order.
	registered []string

	// mismatches are rejections registerAll turns into a startup failure.
	mismatches []error

	// byName makes a second claim on a name an error, not a shadowing.
	byName map[string]bool

	// credentials records, for every admitted catalog tool, which Kubernetes
	// credential that tool reaches the API with, as its row in internal/mcp's
	// catalog declares it. Of the contributed tools only those admitted
	// through AddDestructiveNodeTool record one, the node connector they run
	// on; AddTool's handlers bring their own credential and record none.
	credentials map[string]string

	nodeConn *kube.Connector
}

// newRegistrar returns a Registrar admitting the tiers the operator enabled.
func newRegistrar(srv *mcp.Server, allowWrite, allowDestructive bool) *Registrar {
	return &Registrar{srv: srv, allowWrite: allowWrite, allowDestructive: allowDestructive}
}

// Allows reports whether tools at this tier will be registered.
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

// Registered returns the names of every admitted tool, in registration order.
func (r *Registrar) Registered() []string {
	out := make([]string, len(r.registered))
	copy(out, r.registered)
	return out
}

// credentialSources returns which Kubernetes credential each admitted tool
// reaches the API with — "configurable connector", "node connector" or
// "none" — as its catalog row declared when it was admitted. A tool admitted
// from a row that declares no source records an empty string, so the
// credential-source pin fails on it instead of letting a tool ride an
// undeclared credential. The map is a copy, so the record cannot be edited
// through it.
func (r *Registrar) credentialSources() map[string]string {
	out := make(map[string]string, len(r.credentials))
	maps.Copy(out, r.credentials)
	return out
}

// AddTool registers a typed tool at the given tier, or does nothing at all
// when the operator has not enabled that tier. On a server built by the
// internal MCP package's NewServer, the text of every result and error the
// tool returns reaches the client fenced as untrusted data.
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
	// The SDK's registry is keyed by name and replaces silently, so without this
	// a contributed set could register "version_get" and take over the engine's
	// handler with nothing said — at whatever tier and annotations it liked,
	// since its own registration is checked against its own labels and not
	// against the tool it displaced.
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

// NodeEnv is what a NodeOperation runs with: Out collects the progress the operator reads, and Clientset is on the node connector.
type NodeEnv struct {
	Out       io.Writer
	Clientset kubernetes.Interface
}

// NodeResult is what a NodeOperation reports; Next and Warnings reach the operator whether or not the operation fails.
type NodeResult struct {
	Next     string
	Warnings []string
}

// NodeOperation is a destructive operation on this node, run on the node connector's client.
type NodeOperation[In any] func(ctx context.Context, env NodeEnv, in In) (NodeResult, error)

// AddDestructiveNodeTool registers op at the destructive tier, run like the engine's destructive tools: on a Stratum node only, one at a time with them.
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

// checkTier rejects a tool whose tier contradicts its annotations.
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

// err reports every rejection seen during registration — a tier that
// contradicts its annotations, or a name claimed twice.
func (r *Registrar) err() error {
	if r == nil || len(r.mismatches) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to serve MCP: %w", errors.Join(r.mismatches...))
}

// ReadTool builds the annotations for a tool that changes nothing.
func ReadTool(name, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}
}

// WriteTool builds the annotations for a tool that mutates recoverably.
func WriteTool(name, description string) *mcp.Tool {
	no := false
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
	}
}

// DestructiveTool builds the annotations for a tool that can take a cluster
// down or lose data.
func DestructiveTool(name, description string) *mcp.Tool {
	yes := true
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes},
	}
}

// WithToolSets contributes tool sets to `stratum mcp`. The engine's own
// tools are always present; these are added after them.
func WithToolSets(sets ...ToolSet) Option {
	return func(c *rootConfig) {
		c.toolSets = append(c.toolSets, sets...)
	}
}
