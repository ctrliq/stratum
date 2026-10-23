package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
)

const nodeToolServer = "https://node-credential.invalid:6443"

type nodeToolIn struct {
	Note string `json:"note,omitempty"`
}

type nodeToolSet struct {
	tool *sdk.Tool
	op   NodeOperation[nodeToolIn]
}

func (nodeToolSet) Name() string { return "node-probe" }

func (s nodeToolSet) Register(r *Registrar) { AddDestructiveNodeTool(r, s.tool, s.op) }

func nodeTool(op NodeOperation[nodeToolIn]) nodeToolSet {
	return nodeToolSet{tool: DestructiveTool("node_probe_destroy", "destroy on this node"), op: op}
}

func nodeConnector(t *testing.T) *kube.Connector {
	t.Helper()
	path := filepath.Join(t.TempDir(), "super-admin.conf")
	conf := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: node
  cluster:
    server: %s
users:
- name: node
  user:
    token: node-token
contexts:
- name: node
  context:
    cluster: node
    user: node
current-context: node
`, nodeToolServer)
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	return kube.NewConnector(kube.Options{KubeconfigPath: func() string { return path }})
}

func onStratumHostForTest(t *testing.T, on bool) {
	t.Helper()
	dir := t.TempDir()
	if !on {
		dir = filepath.Join(dir, "absent")
	}
	t.Cleanup(mcpsrv.SetStratumHostDirsForTest([]string{dir}))
}

func nodeToolSession(t *testing.T, allowDestructive bool, sets ...ToolSet) (*sdk.ClientSession, *Registrar) {
	t.Helper()
	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
	r := newRegistrar(srv, true, allowDestructive)
	if err := registerAll(r, rootConfig{displayVersion: "v0-test", toolSets: sets}, testConnector(), nodeConnector(t)); err != nil {
		t.Fatal(err)
	}
	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, r
}

func callNodeTool(t *testing.T, cs *sdk.ClientSession) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "node_probe_destroy", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call node_probe_destroy: %v", err)
	}
	return res
}

func callNodeToolAsync(t *testing.T, cs *sdk.ClientSession) <-chan string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "node_probe_destroy", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			done <- "failed: " + resultText(t, res, err)
			return
		}
		done <- ""
	}()
	return done
}

func structured(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured content %s: %v", raw, err)
	}
	return out
}

func TestDestructiveNodeToolIsAbsentWithoutAllowDestructive(t *testing.T) {
	called := false
	cs, r := nodeToolSession(t, false, nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		called = true
		return NodeResult{}, nil
	}))
	if _, ok := advertised(t, cs)["node_probe_destroy"]; ok {
		t.Error("node_probe_destroy is advertised without --allow-destructive")
	}
	if _, ok := r.credentialSources()["node_probe_destroy"]; ok {
		t.Error("an unadmitted tool recorded a credential source")
	}
	if res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "node_probe_destroy"}); err == nil && !res.IsError {
		t.Error("calling the unregistered node_probe_destroy succeeded")
	}
	if called {
		t.Error("the operation ran without --allow-destructive")
	}
}

func TestDestructiveNodeToolRunsOnTheNodeConnector(t *testing.T) {
	onStratumHostForTest(t, true)
	var host string
	cs, r := nodeToolSession(t, true, nodeTool(func(_ context.Context, env NodeEnv, _ nodeToolIn) (NodeResult, error) {
		host = env.Clientset.CoreV1().RESTClient().Get().URL().Host
		fmt.Fprintln(env.Out, "remediated")
		return NodeResult{Next: "check the node"}, nil
	}))
	res := callNodeTool(t, cs)
	if res.IsError {
		t.Fatalf("node_probe_destroy failed: %s", resultText(t, res, nil))
	}
	if want := strings.TrimPrefix(nodeToolServer, "https://"); host != want {
		t.Errorf("the operation's client addresses %q, want the node connector's %q", host, want)
	}
	got := structured(t, res)
	for k, want := range map[string]any{"action": "node_probe_destroy", "done": true, "progress": "remediated", "next": "check the node"} {
		if got[k] != want {
			t.Errorf("result %s = %v, want %v (result %v)", k, got[k], want, got)
		}
	}
	if src := r.credentialSources()["node_probe_destroy"]; src != string(mcpsrv.CredentialNodeConnector) {
		t.Errorf("credential source = %q, want %q", src, mcpsrv.CredentialNodeConnector)
	}
}

func TestDestructiveNodeToolWarningsArePartial(t *testing.T) {
	onStratumHostForTest(t, true)
	cs, _ := nodeToolSession(t, true, nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		return NodeResult{Warnings: []string{"one node was skipped"}}, nil
	}))
	got := structured(t, callNodeTool(t, cs))
	if got["done"] != false {
		t.Errorf("done = %v with a warning, want false", got["done"])
	}
	if w, _ := got["warnings"].([]any); len(w) != 1 || w[0] != "one node was skipped" {
		t.Errorf("warnings = %v, want the operation's warning", got["warnings"])
	}
}

func TestDestructiveNodeToolFailureCarriesTheProgress(t *testing.T) {
	onStratumHostForTest(t, true)
	cs, _ := nodeToolSession(t, true, nodeTool(func(_ context.Context, env NodeEnv, _ nodeToolIn) (NodeResult, error) {
		fmt.Fprintln(env.Out, "half way")
		return NodeResult{Next: "roll back with stratum undo", Warnings: []string{"cp2 left half-migrated"}}, errors.New("boom")
	}))
	res := callNodeTool(t, cs)
	text := resultText(t, res, nil)
	for _, want := range []string{"half way", "boom", "warning: cp2 left half-migrated", "next: roll back with stratum undo"} {
		if !res.IsError || !strings.Contains(text, want) {
			t.Errorf("a failed operation answered %q, want an error carrying %q", text, want)
		}
	}
}

func TestDestructiveNodeToolRefusesOffHost(t *testing.T) {
	onStratumHostForTest(t, false)
	called := false
	cs, _ := nodeToolSession(t, true, nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		called = true
		return NodeResult{}, nil
	}))
	res := callNodeTool(t, cs)
	if !res.IsError || !strings.Contains(resultText(t, res, nil), "not a Stratum node") {
		t.Errorf("off a Stratum node the tool answered %q, want the engine's host refusal", resultText(t, res, nil))
	}
	if called {
		t.Error("the operation ran off a Stratum node")
	}
}

func TestDestructiveNodeToolResultMatchesTheEngines(t *testing.T) {
	cs, _ := nodeToolSession(t, true, nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		return NodeResult{}, nil
	}))
	tools := advertised(t, cs)
	ours, engine := tools["node_probe_destroy"], tools["uninstall"]
	if ours == nil || engine == nil {
		t.Fatalf("node_probe_destroy or uninstall not advertised")
	}
	a, err := json.Marshal(ours.OutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(engine.OutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("the contributed tool's output schema differs from the engine's destructive result:\n%s\n%s", a, b)
	}
	if ours.Annotations == nil || ours.Annotations.DestructiveHint == nil || !*ours.Annotations.DestructiveHint {
		t.Errorf("node_probe_destroy is not annotated destructive: %+v", ours.Annotations)
	}
}

func TestDestructiveNodeToolRegistrationErrors(t *testing.T) {
	op := func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		return NodeResult{}, nil
	}
	for _, tc := range []struct {
		name string
		set  nodeToolSet
		want string
	}{
		{"an engine tool's name", nodeToolSet{DestructiveTool("uninstall", "shadow"), op}, "registered twice"},
		{"write annotations", nodeToolSet{WriteTool("node_probe_destroy", "mislabelled"), op}, "DestructiveHint"},
		{"no operation", nodeToolSet{DestructiveTool("node_probe_destroy", "empty"), nil}, "carries no operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, true)
			err := registerAll(r, rootConfig{displayVersion: "v0", toolSets: []ToolSet{tc.set}}, testConnector(), nodeConnector(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("registerAll = %v, want a startup error containing %q", err, tc.want)
			}
		})
	}
}

const lockWait = 200 * time.Millisecond

func TestDestructiveNodeToolWaitsForAnEngineOperation(t *testing.T) {
	onStratumHostForTest(t, true)
	ran := make(chan struct{})
	cs, _ := nodeToolSession(t, true, nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		close(ran)
		return NodeResult{}, nil
	}))
	held, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	engineDone := make(chan error, 1)
	go func() {
		_, err := mcpsrv.RunExclusive("engine operation", func(io.Writer) ([]string, error) {
			close(held)
			<-release
			return nil, nil
		})
		engineDone <- err
	}()
	<-held
	called := callNodeToolAsync(t, cs)
	select {
	case <-ran:
		t.Fatal("the contributed operation ran while an engine destructive operation held the lock")
	case <-time.After(lockWait):
	}
	free()
	if err := <-engineDone; err != nil {
		t.Fatal(err)
	}
	if msg := <-called; msg != "" {
		t.Fatalf("node_probe_destroy once the lock was free: %s", msg)
	}
	<-ran
}

func TestEngineOperationWaitsForADestructiveNodeTool(t *testing.T) {
	onStratumHostForTest(t, true)
	started, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	cs, _ := nodeToolSession(t, true, nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) {
		close(started)
		<-release
		return NodeResult{}, nil
	}))
	called := callNodeToolAsync(t, cs)
	<-started
	engineRan := make(chan struct{})
	engineDone := make(chan error, 1)
	go func() {
		_, err := mcpsrv.RunExclusive("engine operation", func(io.Writer) ([]string, error) {
			close(engineRan)
			return nil, nil
		})
		engineDone <- err
	}()
	select {
	case <-engineRan:
		t.Fatal("an engine destructive operation ran while the contributed operation held the lock")
	case <-time.After(lockWait):
	}
	free()
	if msg := <-called; msg != "" {
		t.Fatalf("node_probe_destroy: %s", msg)
	}
	if err := <-engineDone; err != nil {
		t.Fatal(err)
	}
	<-engineRan
}

func TestDestructiveNodeToolWithoutANodeConnectorIsAStartupError(t *testing.T) {
	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, true)
	nodeTool(func(context.Context, NodeEnv, nodeToolIn) (NodeResult, error) { return NodeResult{}, nil }).Register(r)
	if len(r.mismatches) != 1 || !strings.Contains(r.mismatches[0].Error(), "no node connector") {
		t.Errorf("mismatches = %v, want one naming the missing node connector", r.mismatches)
	}
	if slices.Contains(r.Registered(), "node_probe_destroy") {
		t.Error("node_probe_destroy registered with no node connector to run on")
	}
}
