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

// Every read-tier tool the engine registers: version_get plus the generic set.
var engineReadTools = []string{
	"backup_evidence",
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
	"runtime_check",
	"resource_get",
	"resource_list",
	"snapshot_list",
	"snapshot_restore_copies",
	"token_list",
	"uninstall_preview",
	"upgrade_plan",
	"version_get",
}

// Every write-tier tool; absent entirely without --allow-write.
var engineWriteTools = []string{
	"registry_apply",
	"runtime_apply",
	"resource_apply",
	"resource_delete",
	"resource_scale",
	"snapshot_prune",
	"snapshot_save",
	"snapshot_schedule",
	"token_create",
}

// Every destructive-tier tool; absent entirely without --allow-destructive.
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

// The engine's read tools plus the named extras, sorted — the order tools/list assertions compare.
func withProbes(extra ...string) []string {
	out := append(append([]string{}, engineReadTools...), extra...)
	slices.Sort(out)
	return out
}

func testConnector() *kube.Connector { return kube.NewConnector(kube.Options{}) }

// One tool at each tier, the only thing there is to gate.
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

// Builds a server at the given tiers and a live client session over in-memory transports.
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

// One end-to-end round trip over the protocol, transport included.
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

// Pins the annotation over the wire, at the default read tier.
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

// Keeps Registered() honest.
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

// WithToolSets is public API; a nil entry is a caller mistake to survive.
func TestNilToolSetIsIgnored(t *testing.T) {
	cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{nil}}
	r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), false, false)
	registerAll(r, cfg, testConnector(), testConnector()) // must not panic
	if got, want := len(r.Registered()), len(engineReadTools); got != want {
		t.Errorf("registered %d tools, want the engine's %d with no contributed set", got, want)
	}
}

// Files a destructive tool at the read tier, the mistake this seam catches.
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

// The tier and the annotations are two claims; a set could file anything under TierRead.
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
		// The inverse mislabel: the heaviest gate, and a hint the call is safe.
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
			// Reject it because it is mislabelled, not because a flag gates it out first.
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

// The cautious direction is allowed on purpose.
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

// A tool advertised read-only must be one the operator's flags did not gate.
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

// Real registration, tier sets included, so a mislabelled tool fails here.
func TestEngineToolsAreConsistentlyLabelled(t *testing.T) {
	for _, tiers := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		cfg := rootConfig{displayVersion: "v0", toolSets: []ToolSet{probeSet{}}}
		r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{}), tiers[0], tiers[1])
		if err := registerAll(r, cfg, testConnector(), testConnector()); err != nil {
			t.Errorf("write=%v destructive=%v: %v", tiers[0], tiers[1], err)
		}
	}
}

// Claims a name the engine already used.
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
	// The name appears once; a count would go stale as the tool list grows.
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

// A single SIGINT, SIGTERM or SIGHUP stops `stratum mcp` with exit 0.
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
			// Prints inside Serve, after the signal context is installed, so the signal cannot race.
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

// Once the first signal has asked the server to stop, the next must kill it.
func TestMCPSecondSignalKillsDuringShutdown(t *testing.T) {
	if os.Getenv("STRATUM_MCP_HTTP_CHILD") == "1" {
		mcpChild(t, "--transport=http", "--addr=127.0.0.1:0", "--token-file="+writeMCPToken(t))
		return
	}
	p := startCLIChild(t, "STRATUM_MCP_HTTP_CHILD", "TestMCPSecondSignalKillsDuringShutdown")
	addr := waitForMCPServingAddr(t, p)
	// A StateNew connection counts as idle only after a 5s grace, which holds the stop open.
	stalled, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial the mcp child at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = stalled.Close() })
	if _, err := stalled.Write([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n")); err != nil {
		t.Fatalf("write a partial request head: %v", err)
	}
	// Proves the accept loop already took the stalled connection.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTP acceptance barrier: %v", err)
	}
	resp.Body.Close()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Wait for the observable listener closure before signal two.
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

// Waits for the child's serving log and returns the address it bound.
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

// A signal-stopped stdio server returns nil from Serve, so `stratum mcp` exits 0.
func TestMCPStdioStopExitsZero(t *testing.T) {
	if os.Getenv("STRATUM_MCP_STDIO_CHILD") == "1" {
		mcpChild(t) // stdio is the default transport
		return
	}
	p := startCLIChild(t, "STRATUM_MCP_STDIO_CHILD", "TestMCPStdioStopExitsZero")
	// Prints inside Serve, after the command's signal context is installed.
	p.waitForErrLog(t, "serving MCP over stdio")
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.waitForExitCode(t); code != 0 {
		t.Fatalf("mcp stopped over stdio by one SIGTERM maps to exit %d, want 0", code)
	}
	p.waitExitedCode(t, 0)
}

// The signal context descends from the command's parent, so a host cancel stops the server.
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
	// Serve's HTTP path returns nil for any ended context, so this is also exit 0.
	if code := p.waitForExitCode(t); code != 0 {
		t.Fatalf("a parent-context cancellation maps to exit %d, want 0: Serve returns nil after its shutdown", code)
	}
	p.waitExitedCode(t, 0)
}

// Observes shutdown rather than guessing how long the first signal takes.
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
			// Only refusal confirms the listener is closed.
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

// Pins the node connector's request timeout; without it the default would be 30s.
func TestNodeConnector_CarriesTheServersRequestTimeout(t *testing.T) {
	// A is the node's admin.conf, B the operator's --kubeconfig; building a connector contacts nothing.
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
		// Explicit, not the fallback: the node credential must not move with the operator's env.
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
		// Whether token_list really rides the node connector is pinned in internal/mcp.
		r := newRegistrar(mcpsrv.NewServer(mcpsrv.Options{Version: "v0"}), false, false)
		if err := registerAll(r, rootConfig{displayVersion: "v0"}, conn, nodeConn); err != nil {
			t.Fatalf("registerAll refused the server's two connectors: %v", err)
		}
		if !slices.Contains(r.Registered(), "token_list") {
			t.Errorf("token_list is not among the registered tools: %q", r.Registered())
		}
	})
}

// token_list rides the node connector, so its description must not disclaim --kubeconfig.
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

// upgrade_plan previews the cluster upgrade_apply rolls; the tools/list surface must say so.
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

// Credential groups, guide order; 'none' means no client, but deeper ops may touch admin.conf.
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
		"backup_evidence",
		"certs_check",
		"certs_rotate",
		"cluster_status",
		"registry_apply",
		"registry_check",
		"rollback",
		"runtime_apply",
		"runtime_check",
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

// Composes the pinned per-tool table from the three groups above.
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

// Renders a name-to-source map as one sorted line per tool, so failures name the rows.
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

// Pins each tool's declared credential source against the recorded one.
func TestEveryToolDeclaresItsCredentialSource(t *testing.T) {
	// Every row admitted, on two connectors shaped like the real server's.
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

// Registration must not replace a client builder used by a different server.
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
