package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/rest"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
	"github.com/ctrliq/stratum/internal/version"
)

// engineReadTools is every read-tier tool the engine registers: version_get
// plus the generic Kubernetes toolset.
var engineReadTools = []string{
	"certs_check",
	"cluster_status",
	"context_list",
	"event_list",
	"namespace_list",
	"node_list",
	"pod_get",
	"pod_list",
	"pod_logs",
	"registry_check",
	"resource_get",
	"resource_list",
	"snapshot_list",
	"snapshot_restore_copies",
	"token_list",
	"uninstall_preview",
	"upgrade_plan",
	"version_get",
}

// engineWriteTools is every write-tier tool the engine registers.
// Absent entirely without --allow-write.
var engineWriteTools = []string{
	"registry_apply",
	"resource_apply",
	"resource_delete",
	"resource_scale",
	"snapshot_prune",
	"snapshot_save",
	"snapshot_schedule",
	"token_create",
}

// engineDestructiveTools is every destructive-tier tool the engine
// registers. Absent entirely without --allow-destructive.
var engineDestructiveTools = []string{
	"certs_migrate_etcd_ca",
	"certs_migrate_kubelet_client_ca",
	"certs_rotate",
	"certs_rotate_ca",
	"ha_repoint",
	"rollback",
	"snapshot_prune_restores",
	"snapshot_rejoin",
	"snapshot_restore",
	"uninstall",
	"upgrade_apply",
}

// withProbes returns the engine's read tools plus the named extras, sorted,
// which is the order tools/list assertions compare against.
func withProbes(extra ...string) []string {
	out := append(append([]string{}, engineReadTools...), extra...)
	slices.Sort(out)
	return out
}

// testConnector is an unconnected Connector.
func testConnector() *kube.Connector { return kube.NewConnector(kube.Options{}) }

// probeSet is a contributed tool set carrying one tool at each tier, which
// is how these tests exercise the gate: the engine itself ships only a read
// tool today, so without this there would be nothing to gate.
type probeSet struct{}

func (probeSet) Name() string { return "probe" }

func (probeSet) Register(r *Registrar) {
	type in struct{}
	type out struct {
		OK bool `json:"ok"`
	}
	ok := func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, out, error) {
		return nil, out{OK: true}, nil
	}
	AddTool(r, TierRead, ReadTool("probe_read", "read"), ok)
	AddTool(r, TierWrite, WriteTool("probe_write", "write"), ok)
	AddTool(r, TierDestructive, DestructiveTool("probe_destroy", "destroy"), ok)
}

// connect builds a server at the given tiers and returns a live client
// session over in-memory transports — the real protocol, not a handler call.
func connect(t *testing.T, allowWrite, allowDestructive bool) *sdk.ClientSession {
	t.Helper()
	ctx := t.Context()

	cfg := rootConfig{displayVersion: "v1.2.3-test", toolSets: []ToolSet{probeSet{}}}
	opts := mcpsrv.Options{Version: cfg.displayVersion, AllowWrite: allowWrite, AllowDestructive: allowDestructive}

	srv := mcpsrv.NewServer(opts)
	registerAll(newRegistrar(srv, allowWrite, allowDestructive), cfg, testConnector(), testConnector())

	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// TestAdvertisedSurfacePerTier pins the exact tool list at each tier.
func TestAdvertisedSurfacePerTier(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		allowWrite, allowDestructive bool
		want                         []string
	}{
		{
			name: "read only, the default",
			want: withProbes("probe_read"),
		},
		{
			name:       "with --allow-write",
			allowWrite: true,
			want:       withProbes(append([]string{"probe_read", "probe_write"}, engineWriteTools...)...),
		},
		{
			name:             "with --allow-destructive",
			allowDestructive: true,
			want:             withProbes(append([]string{"probe_destroy", "probe_read"}, engineDestructiveTools...)...),
		},
		{
			name:             "with both",
			allowWrite:       true,
			allowDestructive: true,
			want: withProbes(slices.Concat(
				[]string{"probe_destroy", "probe_read", "probe_write"},
				engineWriteTools, engineDestructiveTools)...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolNames(t, connect(t, tc.allowWrite, tc.allowDestructive))
			if !slices.Equal(got, tc.want) {
				t.Errorf("tools/list =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestMutatingToolsAreAbsentNotRefused is the security assertion.
func TestMutatingToolsAreAbsentNotRefused(t *testing.T) {
	cs := connect(t, false, false)

	for _, name := range slices.Concat([]string{"probe_write", "probe_destroy"}, engineWriteTools, engineDestructiveTools) {
		if slices.Contains(toolNames(t, cs), name) {
			t.Fatalf("%s is advertised at the read tier; it must not be registered at all", name)
		}
		if _, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name}); err == nil {
			t.Errorf("calling unregistered %s succeeded; want an error", name)
		}
	}
}

// TestVersionGetRoundTrips runs one tool end to end over the protocol, so
// the transport, the registry and the SDK's schema inference are all
// exercised rather than assumed.
func TestVersionGetRoundTrips(t *testing.T) {
	cs := connect(t, false, false)

	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "version_get"})
	if err != nil {
		t.Fatalf("call version_get: %v", err)
	}
	if res.IsError {
		t.Fatalf("version_get reported an error result: %+v", res.Content)
	}

	var got mcpsrv.VersionOut
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if got.Version != "v1.2.3-test" {
		t.Errorf("version = %q, want v1.2.3-test", got.Version)
	}
	if got.OS == "" || got.Arch == "" {
		t.Errorf("os/arch not populated: %+v", got)
	}
}

// TestReadToolsAreAnnotated pins the annotation over the wire, at the read
// tier the operator gets by default.
func TestReadToolsAreAnnotated(t *testing.T) {
	res, err := connect(t, false, false).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is advertised at the read tier but ReadOnlyHint is false", tool.Name)
		}
	}
}

// TestRegistrarRecordsWhatItAdmitted keeps Registered() honest, since the
// tier tests above and any future audit rely on it.
func TestRegistrarRecordsWhatItAdmitted(t *testing.T) {
	cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{probeSet{}}}

	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), false, false)
	registerAll(r, cfg, testConnector(), testConnector())
	if got, want := len(r.Registered()), len(engineReadTools)+1; got != want {
		t.Errorf("read tier admitted %d tools (%q), want %d", got, r.Registered(), want)
	}

	r = newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, true)
	registerAll(r, cfg, testConnector(), testConnector())
	wantAll := len(engineReadTools) + len(engineWriteTools) + len(engineDestructiveTools) + 3
	if got, want := len(r.Registered()), wantAll; got != want {
		t.Errorf("all tiers admitted %d tools (%q), want %d", got, r.Registered(), want)
	}
}

// TestNilToolSetIsIgnored: WithToolSets is public API, so a nil entry is a
// caller mistake we should survive rather than panic on at startup.
func TestNilToolSetIsIgnored(t *testing.T) {
	cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{nil}}
	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), false, false)
	registerAll(r, cfg, testConnector(), testConnector()) // must not panic
	if got, want := len(r.Registered()), len(engineReadTools); got != want {
		t.Errorf("registered %d tools, want the engine's %d with no contributed set", got, want)
	}
}

// mislabelSet files a destructive tool at the read tier — the mistake this
// seam has to catch, because a read-tier registration is also what tells an
// MCP client the call is safe to auto-approve.
type mislabelSet struct {
	tier Tier
	tool *sdk.Tool
}

func (mislabelSet) Name() string { return "mislabel" }

func (m mislabelSet) Register(r *Registrar) {
	type in struct{}
	type out struct{}
	AddTool(r, m.tier, m.tool,
		func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, out, error) {
			return nil, out{}, nil
		})
}

// TestTierMustMatchAnnotations: the tier and the annotations are two claims
// made by the same code, and until they were checked against each other a
// contributed set could file anything at all under TierRead — which both
// skipped the operator's flags and put the client's auto-approve hint on it.
func TestTierMustMatchAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tier    Tier
		tool    *sdk.Tool
		wantErr string
	}{
		{"destructive at read", TierRead, DestructiveTool("wipe", "wipe everything"), "not annotated read-only"},
		{"write at read", TierRead, WriteTool("touch", "touch something"), "not annotated read-only"},
		{"no annotations at all", TierRead, &sdk.Tool{Name: "bare", Description: "bare"}, "no annotations"},
		{"read-only and destructive", TierRead, &sdk.Tool{
			Name:        "both",
			Description: "both",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: ptr(true)},
		}, "both read-only and destructive"},
		// The inverse mislabel: the operator's heaviest gate, and a hint
		// telling the client the call is safe.
		{"write hint at destructive", TierDestructive, WriteTool("looks_recoverable", "looks recoverable"),
			"DestructiveHint does not say so"},
		{"no hint at destructive", TierDestructive, &sdk.Tool{
			Name:        "unhinted",
			Description: "unhinted",
			Annotations: &sdk.ToolAnnotations{},
		}, "DestructiveHint does not say so"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{mislabelSet{tc.tier, tc.tool}}}
			// Open every tier: the check has to reject a mislabelled tool
			// because it is mislabelled, not because a flag happened to
			// gate it out first.
			r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, true)

			err := registerAll(r, cfg, testConnector(), testConnector())
			if err == nil {
				t.Fatalf("registration succeeded; %q was admitted at tier %s", tc.tool.Name, tc.tier)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if slices.Contains(r.Registered(), tc.tool.Name) {
				t.Errorf("%s was registered anyway: %q", tc.tool.Name, r.Registered())
			}
		})
	}
}

// TestWriteTierMayCarryADestructiveHint: the cautious direction is allowed
// on purpose.
func TestWriteTierMayCarryADestructiveHint(t *testing.T) {
	cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{
		// A name no engine tool uses.
		mislabelSet{TierWrite, DestructiveTool("probe_recoverable_delete", "delete a resource")},
	}}
	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, false)
	if err := registerAll(r, cfg, testConnector(), testConnector()); err != nil {
		t.Fatalf("a destructive hint at the write tier was rejected: %v", err)
	}
	if !slices.Contains(r.Registered(), "probe_recoverable_delete") {
		t.Errorf("registered %q, want probe_recoverable_delete among them", r.Registered())
	}
}

// TestReadOnlyHintCannotBeGatedAboveRead is the other half of the rule: a
// tool advertised read-only must be one the operator's flags did not gate,
// or the hint is describing a capability they withheld.
func TestReadOnlyHintCannotBeGatedAboveRead(t *testing.T) {
	type in struct{}
	type out struct{}
	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, true)
	AddTool(r, TierWrite, ReadTool("looks_safe", "looks safe"),
		func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, out, error) {
			return nil, out{}, nil
		})
	if err := r.err(); err == nil {
		t.Fatal("a read-only annotation at TierWrite was accepted")
	}
	if len(r.Registered()) != 0 {
		t.Errorf("registered %q, want nothing", r.Registered())
	}
}

// TestEngineToolsAreConsistentlyLabelled runs the real registration, tier
// sets included, so a future tool that files itself wrong fails here rather
// than at a customer's startup.
func TestEngineToolsAreConsistentlyLabelled(t *testing.T) {
	for _, tiers := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{probeSet{}}}
		r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), tiers[0], tiers[1])
		if err := registerAll(r, cfg, testConnector(), testConnector()); err != nil {
			t.Errorf("write=%v destructive=%v: %v", tiers[0], tiers[1], err)
		}
	}
}

// shadowSet claims a name the engine already used.
type shadowSet struct{}

func (shadowSet) Name() string { return "shadow" }

func (shadowSet) Register(r *Registrar) {
	type in struct{}
	type out struct{}
	AddTool(r, TierRead, ReadTool("version_get", "not the engine's"),
		func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, out, error) {
			return nil, out{}, nil
		})
}

// TestAToolSetCannotShadowAnEngineTool.
func TestAToolSetCannotShadowAnEngineTool(t *testing.T) {
	cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{shadowSet{}}}
	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), true, true)

	err := registerAll(r, cfg, testConnector(), testConnector())
	if err == nil {
		t.Fatal("a contributed set replaced version_get without complaint")
	}
	if !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("error %q does not say the name was taken", err)
	}
	// The assertion is that the name appears once, not that the surface is
	// any particular size — the engine's tool list grows, and a count here
	// would go stale as it grows for no reason.
	var seen int
	for _, name := range r.Registered() {
		if name == "version_get" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("version_get appears %d times in Registered(): %q", seen, r.Registered())
	}
}

func ptr[T any](v T) *T { return &v }

// mcpChild runs the real `stratum mcp` command in-process with the given
// flags added beside a dead-apiserver kubeconfig, maps the result the way
// cmd/stratum/main.go would, and exits with that code.
// writeMCPToken writes a private bearer-token file for --token-file.
func writeMCPToken(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp-token")
	if err := os.WriteFile(p, []byte(strings.Repeat("a", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mcpChild(t *testing.T, args ...string) {
	t.Helper()
	root := NewRootCommand()
	root.SetArgs(append([]string{"mcp", "--kubeconfig=" + writeDeadKubeconfig(t)}, args...))
	err := root.Execute()
	code := exitAsMainMaps(err)
	fmt.Printf("exit code: %d\n", code)
	os.Exit(code)
}

// TestMCPSignalStopExitsZero pins the shared interrupt set at the mcp
// command: a single SIGINT, SIGTERM or SIGHUP must stop `stratum mcp` with
// exit 0, never killing the process. A SIGHUP outside the shared set takes
// its default action and kills the process. SIGINT and SIGTERM are the
// guards: the stdio transport's context.Canceled filter
// (internal/mcp/server.go:59) and the HTTP transport's shutdown
// (internal/mcp/server.go:129-135) are clean stops, and they must not
// regress.
func TestMCPSignalStopExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("STRATUM_MCP_HTTP_CHILD") == "1" {
				mcpChild(t, "--transport=http", "--addr=127.0.0.1:0", "--token-file="+writeMCPToken(t))
				return
			}
			p := startCLIChild(t, "STRATUM_MCP_HTTP_CHILD", "TestMCPSignalStopExitsZero")
			// Barrier: this line prints inside Serve
			// (internal/mcp/server.go:92), after the command's signal context
			// is installed (pkg/cli/mcp.go:100), so the signal cannot race
			// ahead of the handler and kill the child by the default action.
			p.waitForErrLog(t, "serving MCP over streamable HTTP")
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if code := p.waitForExitCode(t); code != 0 {
				t.Fatalf("mcp stopped by one %s maps to exit %d, want 0 (a signal-caused clean stop)", tc.name, code)
			}
			p.waitExitedCode(t, 0)
		})
	}
}

// TestMCPSecondSignalKillsDuringShutdown pins the shared set's second-signal
// rule for `stratum mcp`: once the first signal has asked the server to
// stop, the next one must kill the process instead of being swallowed. The
// parent holds the stop open by leaving a connection mid-request-headers,
// which http.Server.Shutdown waits for through its 5s grace
// (internal/mcp/server.go:121-135) — the window a stalled stop exists for.
// Fails if the second signal is swallowed: the child must die to the
// signal, not shut down on its own.
func TestMCPSecondSignalKillsDuringShutdown(t *testing.T) {
	if os.Getenv("STRATUM_MCP_HTTP_CHILD") == "1" {
		mcpChild(t, "--transport=http", "--addr=127.0.0.1:0", "--token-file="+writeMCPToken(t))
		return
	}
	p := startCLIChild(t, "STRATUM_MCP_HTTP_CHILD", "TestMCPSecondSignalKillsDuringShutdown")
	addr := waitForMCPServingAddr(t, p)
	// A connection stuck mid-headers is active, not idle, so the shutdown
	// waits for it: net/http treats a StateNew connection as idle only after
	// a 5s grace (Go issue 22682), which holds the stop open through the
	// server's own 5s shutdown grace (internal/mcp/server.go:121-135).
	stalled, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial the mcp child at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = stalled.Close() })
	if _, err := stalled.Write([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n")); err != nil {
		t.Fatalf("write a partial request head: %v", err)
	}
	// A response over a later connection proves the serial accept loop
	// already accepted the stalled connection.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTP acceptance barrier: %v", err)
	}
	resp.Body.Close()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Shutdown closes the listener after signal handling has exposed
	// cancellation. Wait for that observable condition before signal two.
	waitForListenerClosed(t, addr)
	select {
	case err := <-p.waited:
		t.Fatalf("child exited before the force-stop signal: %v", err)
	default:
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	select {
	case err := <-p.waited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the second SIGTERM let mcp exit cleanly (%v); it must kill the process", err)
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGTERM {
			t.Fatalf("mcp ended with %v rather than being killed by the second SIGTERM", ee.ProcessState)
		}
	case <-deadline:
		t.Fatal("a second SIGTERM did not end the mcp child within 15s: the signal is still being swallowed")
	}
}

// waitForMCPServingAddr blocks until the child logs that it is serving over
// HTTP and returns the address it bound, parsed out of the log line
// (internal/mcp/server.go:92).
func waitForMCPServingAddr(t *testing.T, p *cliProc) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.errLines:
			if !ok {
				t.Fatal("the mcp child exited before logging it was serving")
			}
			const marker = " addr="
			if strings.Contains(l, "serving MCP over streamable HTTP") {
				if i := strings.Index(l, marker); i >= 0 {
					addr := l[i+len(marker):]
					if j := strings.Index(addr, " "); j >= 0 {
						addr = addr[:j]
					}
					return addr
				}
			}
		case <-deadline:
			t.Fatal("the mcp child never logged it was serving")
		}
	}
}

// TestMCPStdioStopExitsZero pins the stdio transport's clean-stop filter
// (internal/mcp/server.go:59) that must agree with the shared
// clean-stop helper or keeps consistent with it: a server stopped by a
// signal must return nil from Serve so `stratum mcp` exits 0, sessions in
// flight or not. It is green today — Serve filters context.Canceled — and
// must stay green when the shared context records a signal cause while
// keeping Err exactly context.Canceled. The parent holds the child's
// stdin open, so the stdio server blocks reading it and only the signal
// decides when Serve returns.
func TestMCPStdioStopExitsZero(t *testing.T) {
	if os.Getenv("STRATUM_MCP_STDIO_CHILD") == "1" {
		mcpChild(t) // stdio is the default transport
		return
	}
	p := startCLIChild(t, "STRATUM_MCP_STDIO_CHILD", "TestMCPStdioStopExitsZero")
	// Barrier: this line prints inside Serve (internal/mcp/server.go:57),
	// after the command's signal context is installed (pkg/cli/mcp.go:98).
	p.waitForErrLog(t, "serving MCP over stdio")
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.waitForExitCode(t); code != 0 {
		t.Fatalf("mcp stopped over stdio by one SIGTERM maps to exit %d, want 0", code)
	}
	p.waitExitedCode(t, 0)
}

// TestMCPStopsWhenTheParentContextIsCancelled pins the derivation the
// shared handler must keep: mcp's signal context descends from the command's
// parent context (pkg/cli/mcp.go:98 uses cmd.Context()), so a host that
// cancels the context it passed to ExecuteContext stops the server instead
// of leaving it serving forever. It is green today and guards the move onto
// internal/interrupt, which must not detach mcp from its parent. The parent
// asks for the cancellation over SIGUSR1, a signal outside the shared set.
func TestMCPStopsWhenTheParentContextIsCancelled(t *testing.T) {
	if os.Getenv("STRATUM_MCP_PARENT_CHILD") == "1" {
		ctx, cancel := context.WithCancel(context.Background())
		usr1 := make(chan os.Signal, 1)
		signal.Notify(usr1, syscall.SIGUSR1)
		defer signal.Stop(usr1)
		go func() {
			<-usr1
			cancel()
		}()
		kc := writeDeadKubeconfig(t)
		root := NewRootCommand()
		root.SetArgs([]string{"mcp", "--transport=http", "--addr=127.0.0.1:0", "--token-file=" + writeMCPToken(t), "--kubeconfig=" + kc})
		err := root.ExecuteContext(ctx)
		code := exitAsMainMaps(err)
		fmt.Printf("exit code: %d\n", code)
		os.Exit(code)
	}
	p := startCLIChild(t, "STRATUM_MCP_PARENT_CHILD", "TestMCPStopsWhenTheParentContextIsCancelled")
	p.waitForErrLog(t, "serving MCP over streamable HTTP")
	if err := p.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	// Serve's HTTP path returns nil after its shutdown for any ended
	// context (internal/mcp/server.go:129-135), so a host's cancellation
	// is also exit 0.
	if code := p.waitForExitCode(t); code != 0 {
		t.Fatalf("a parent-context cancellation maps to exit %d, want 0: Serve returns nil after its shutdown", code)
	}
	p.waitExitedCode(t, 0)
}

// waitForListenerClosed observes shutdown rather than guessing how long the
// child needs to receive its first signal. The deadline only bounds failure.
func waitForListenerClosed(t *testing.T, addr string) {
	t.Helper()
	if err := observeListenerClosed(addr, net.DialTimeout); err != nil {
		t.Fatal(err)
	}
}

func observeListenerClosed(addr string, dial func(string, string, time.Duration) (net.Conn, error)) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := dial("tcp", addr, 100*time.Millisecond)
		if err != nil {
			var op *net.OpError
			if errors.As(err, &op) && errors.Is(op.Err, syscall.ECONNREFUSED) {
				return nil
			}
			// A connect racing listener closure can be reset, or time out on a
			// loaded runner, before subsequent probes are refused. Only refusal
			// confirms the listener is closed.
			var ne net.Error
			if !errors.Is(err, syscall.ECONNRESET) && !(errors.As(err, &ne) && ne.Timeout()) {
				return fmt.Errorf("observe listener shutdown: %w", err)
			}
		} else {
			conn.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("listener still accepts connections after the first signal")
}

func TestObserveListenerClosedRetriesConnectionReset(t *testing.T) {
	calls := 0
	err := observeListenerClosed("127.0.0.1:1", func(string, string, time.Duration) (net.Conn, error) {
		calls++
		cause := syscall.ECONNRESET
		if calls > 1 {
			cause = syscall.ECONNREFUSED
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", cause)}
	})
	if err != nil {
		t.Fatalf("a reset during listener closure must be retried: %v", err)
	}
	if calls != 2 {
		t.Fatalf("made %d probes, want a reset followed by confirmed refusal", calls)
	}
}

func TestObserveListenerClosedRetriesDialTimeout(t *testing.T) {
	calls := 0
	err := observeListenerClosed("127.0.0.1:1", func(string, string, time.Duration) (net.Conn, error) {
		calls++
		if calls == 1 {
			// What net.DialTimeout returns when its deadline passes.
			_, err := (&net.Dialer{Deadline: time.Now().Add(-time.Second)}).Dial("tcp", "127.0.0.1:1")
			return nil, err
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	})
	if err != nil {
		t.Fatalf("a dial timeout during listener closure must be retried: %v", err)
	}
	if calls != 2 {
		t.Fatalf("made %d probes, want a timeout followed by confirmed refusal", calls)
	}
}

func TestObserveListenerClosedRejectsOtherDialErrors(t *testing.T) {
	calls := 0
	err := observeListenerClosed("127.0.0.1:1", func(string, string, time.Duration) (net.Conn, error) {
		calls++
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}
	})
	if !errors.Is(err, syscall.EACCES) || calls != 1 {
		t.Fatalf("unrelated dial errors must fail immediately: calls=%d, error=%v", calls, err)
	}
}

// TestNodeConnector_CarriesTheServersRequestTimeout pins the node connector's
// request timeout. Beside the configurable connector the operator scopes with
// --kubeconfig, --context and --request-timeout, the server builds a second,
// explicit node connector on version.AdminConf carrying the same
// --request-timeout, and token_list is registered with it: a private
// admin.conf connector of its own would default to 30 seconds, so the
// server's request timeout would never reach it
// (internal/mcp/stratumread.go:470-472).
func TestNodeConnector_CarriesTheServersRequestTimeout(t *testing.T) {
	// Two distinguishable kubeconfigs: A stands for the node's admin.conf, B
	// for the operator's --kubeconfig. Building a connector contacts nothing,
	// so the API servers are names rather than listeners.
	adminConf := kubetest.Kubeconfig(t, &rest.Config{Host: "https://admin.example:6443"})
	operatorConf := kubetest.Kubeconfig(t, &rest.Config{Host: "https://operator.example:6443"})
	t.Setenv("KUBECONFIG", operatorConf)
	oldConf := version.AdminConf
	version.AdminConf = adminConf
	t.Cleanup(func() { version.AdminConf = oldConf })

	conn, nodeConn := newConnectors("", "", 5*time.Second, nil)

	t.Run("the node connector stands explicitly on admin.conf", func(t *testing.T) {
		cl, err := nodeConn.Cluster("")
		if err != nil {
			t.Fatal(err)
		}
		// Explicit, not the fallback precedence: $KUBECONFIG points at the
		// operator's config here, and the node credential must not move with
		// the operator's environment.
		if cl.Source != kube.SourceExplicit || cl.ConfigPath != version.AdminConf {
			t.Errorf("node connector resolved %s from %q, want admin.conf taken explicitly", cl.Source, cl.ConfigPath)
		}
		if got, want := cl.Config.Host, "https://admin.example:6443"; got != want {
			t.Errorf("node connector host = %q, want the admin.conf server %q", got, want)
		}
		if got, want := cl.Config.Timeout, 5*time.Second; got != want {
			t.Errorf("node connector request timeout = %v, want the server's --request-timeout %v", got, want)
		}
	})

	t.Run("the configurable connector keeps the operator's scope", func(t *testing.T) {
		cl, err := conn.Cluster("")
		if err != nil {
			t.Fatal(err)
		}
		if cl.ConfigPath != operatorConf {
			t.Errorf("configurable connector resolved %q, want the operator's $KUBECONFIG", cl.ConfigPath)
		}
		if got, want := cl.Config.Timeout, 5*time.Second; got != want {
			t.Errorf("configurable connector request timeout = %v, want %v", got, want)
		}
	})

	t.Run("token_list is registered with the node connector", func(t *testing.T) {
		// registerAll threads both connectors into the catalog. Whether the
		// token_list row really builds its handler on the node connector is
		// pinned behaviourally in internal/mcp
		// (TestTokenList_RidesTheNodeConnectorsTimeout): pkg/cli cannot make
		// that call, because token_list's Stratum host check fails off a node.
		r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{Version: "v0"}), false, false)
		if err := registerAll(r, rootConfig{displayVersion: "v0"}, conn, nodeConn); err != nil {
			t.Fatalf("registerAll refused the server's two connectors: %v", err)
		}
		if !slices.Contains(r.Registered(), "token_list") {
			t.Errorf("token_list is not among the registered tools: %q", r.Registered())
		}
	})
}

// TestTokenListDescriptionStopsClaimingItIgnoresKubeconfig pins the
// registration's contract: token_list rides the server's explicit node
// connector, so the description the client sees in tools/list must not say it
// "uses the node's own admin credential rather than --kubeconfig" — read as a
// promise that the operator's configuration does not govern the tool at
// all — the claim the least-privilege guide repeats.
func TestTokenListDescriptionStopsClaimingItIgnoresKubeconfig(t *testing.T) {
	res, err := connect(t, false, false).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "token_list" {
			continue
		}
		if strings.Contains(tool.Description, "rather than --kubeconfig") {
			t.Errorf("token_list's registered description still claims it ignores --kubeconfig:\n%s", tool.Description)
		}
		return
	}
	t.Fatal("token_list is not registered")
}

// TestUpgradePlanAndApply_DescribeTheSameCluster is the client-visible half
// of the same-cluster guard. Both tools ride the node connector on admin.conf
// — the plan previews the cluster the apply rolls — and the surface a client
// sees in tools/list must say so: upgrade_plan advertises no per-call context
// argument, both descriptions name the credential they read the way
// token_list's does, and the plan's description names the tool whose cluster
// it previews. pkg/cli cannot drive upgrade_plan against an API server (its
// Stratum host check fails off a node), so the behavioural half of the guard
// lives in internal/mcp (TestUpgradePlanAndApply_AddressTheSameCluster).
// Fails when upgrade_plan's schema advertises "context", its description
// names neither credential nor tool, or upgrade_apply's description
// claims scope=cluster runs on the server's configurable credential and
// default context.
func TestUpgradePlanAndApply_DescribeTheSameCluster(t *testing.T) {
	tools := advertised(t, tierSession(t, true, true))

	t.Run("upgrade_plan takes no per-call context", func(t *testing.T) {
		plan, ok := tools["upgrade_plan"]
		if !ok {
			t.Fatal("upgrade_plan is not registered")
		}
		schema, err := json.Marshal(plan.InputSchema)
		if err != nil {
			t.Fatalf("marshal input schema: %v", err)
		}
		if strings.Contains(string(schema), `"context"`) {
			t.Errorf("upgrade_plan still advertises a per-call context; it must read the cluster upgrade_apply rolls: %s", schema)
		}
		if !strings.Contains(string(schema), `"target"`) {
			t.Errorf("upgrade_plan no longer advertises target: %s", schema)
		}
	})

	t.Run("both descriptions name the node credential and the tool they agree with", func(t *testing.T) {
		for _, name := range []string{"upgrade_plan", "upgrade_apply"} {
			tool, ok := tools[name]
			if !ok {
				t.Fatalf("%s is not registered", name)
			}
			if !strings.Contains(tool.Description, "admin.conf") {
				t.Errorf("%s's description does not say it reads this node's own admin.conf, the credential both tools share:\n%s", name, tool.Description)
			}
		}
		if plan := tools["upgrade_plan"]; !strings.Contains(plan.Description, "upgrade_apply") {
			t.Errorf("upgrade_plan's description does not say it previews the cluster upgrade_apply rolls:\n%s", plan.Description)
		}
	})
}

// The credential-source pin, grouped the way the least-privilege guide
// presents it. "configurable connector" is the operator-scoped connector
// (--kubeconfig, --context, --request-timeout); "node connector" is this
// node's admin.conf, captured by the catalog's handler dependencies; "none"
// is a handler the registration hands no Kubernetes client at all — the host-local tools,
// whose deeper operations may still touch admin.conf on their own, as a
// node upgrade's cordon does. The groups follow the guide's least-privilege
// order, and a row moves only when the credential its tools use changes.
var (
	toolsOnTheConfigurableConnector = []string{
		"context_list",
		"event_list",
		"ha_repoint",
		"namespace_list",
		"node_list",
		"pod_get",
		"pod_list",
		"pod_logs",
		"resource_apply",
		"resource_delete",
		"resource_get",
		"resource_list",
		"resource_scale",
	}

	toolsOnTheNodeConnector = []string{
		"certs_migrate_etcd_ca",
		"certs_migrate_kubelet_client_ca",
		"certs_rotate_ca",
		"token_create",
		"token_list",
		"upgrade_apply",
		"upgrade_plan",
	}

	toolsOnNoKubernetesCredential = []string{
		"certs_check",
		"certs_rotate",
		"cluster_status",
		"registry_apply",
		"registry_check",
		"rollback",
		"snapshot_list",
		"snapshot_prune",
		"snapshot_prune_restores",
		"snapshot_rejoin",
		"snapshot_restore",
		"snapshot_restore_copies",
		"snapshot_save",
		"snapshot_schedule",
		"uninstall",
		"uninstall_preview",
		"version_get",
	}
)

// pinnedCredentialSources composes the pinned per-tool table from the three
// groups above.
func pinnedCredentialSources() map[string]string {
	pin := make(map[string]string,
		len(toolsOnTheConfigurableConnector)+len(toolsOnTheNodeConnector)+len(toolsOnNoKubernetesCredential))
	for _, group := range []struct {
		source string
		names  []string
	}{
		{"configurable connector", toolsOnTheConfigurableConnector},
		{"node connector", toolsOnTheNodeConnector},
		{"none", toolsOnNoKubernetesCredential},
	} {
		for _, name := range group.names {
			pin[name] = group.source
		}
	}
	return pin
}

// credentialTable renders a name-to-source map as one sorted "name: source"
// line per tool, so a pin failure names the rows that moved.
func credentialTable(m map[string]string) string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "\n    %s: %s", name, m[name])
	}
	return b.String()
}

// TestEveryToolDeclaresItsCredentialSource pins each tool's credential.
// Registration records, beside every tool it admits, which Kubernetes
// credential that tool reaches the API with, and the full record equals the
// pinned table above: the table the MCP page and its least-privilege section
// are corrected against, so the guide grants rights to the credential each
// tool really uses and never claims a credential a registration does not.
//
// The pinned rows follow the wiring. token_list, upgrade_plan and
// upgrade_apply ride the node connector — the plan rides it so it previews
// the same cluster the apply rolls, pinned behaviourally by
// TestUpgradePlanAndApply_AddressTheSameCluster — as do token_create,
// certs_rotate_ca and certs_migrate_etcd_ca, whose handlers capture the node
// connector through the catalog. The generic Kubernetes tools and ha_repoint
// keep the configurable connector.
//
// Fails when a new tool reaches the API without declaring which credential
// it uses, or when an existing tool's source changes without this table
// changing.
func TestEveryToolDeclaresItsCredentialSource(t *testing.T) {
	// The highest tier, so every row the engine ships is admitted: a source
	// recorded for a tool that never registered would be about nothing. Two
	// distinct connectors, as the real server builds them, so the declaration
	// is read off a registration shaped like the real one.
	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0"})
	r := newRegistrar(srv, true, true)
	if err := registerAll(r, rootConfig{displayVersion: "v0"}, testConnector(), testConnector()); err != nil {
		t.Fatalf("registerAll refused the engine's tools at the highest tier: %v", err)
	}

	got := r.credentialSources()

	for _, name := range r.Registered() {
		if _, ok := got[name]; !ok {
			t.Errorf("%s is registered but records no credential source: every tool must declare which Kubernetes credential it uses", name)
		}
	}
	for name, source := range got {
		switch source {
		case "configurable connector", "node connector", "none":
		default:
			t.Errorf("%s records credential source %q: want \"configurable connector\", \"node connector\" or \"none\"", name, source)
		}
	}

	want := pinnedCredentialSources()
	if groups := len(toolsOnTheConfigurableConnector) + len(toolsOnTheNodeConnector) + len(toolsOnNoKubernetesCredential); len(want) != groups {
		t.Fatalf("the pin's three groups name %d tools but compose a table of %d: a tool is listed in two groups", groups, len(want))
	}
	if !maps.Equal(got, want) {
		t.Errorf("recorded credential sources do not equal the pinned table.\n  recorded:%s\n  pinned:  %s",
			credentialTable(got), credentialTable(want))
	}
}

// Each root can build its server independently. In particular, registration
// must not replace a client builder used by a different server.
func TestRegisterAllConcurrentServers(t *testing.T) {
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			srv := mcpsrv.NewServer(mcpsrv.Options{Version: "test"})
			r := newRegistrar(srv, true, true)
			if err := registerAll(r, rootConfig{displayVersion: "test"}, testConnector(), testConnector()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
