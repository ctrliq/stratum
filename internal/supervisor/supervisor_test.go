package supervisor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopPreventsPendingStart(t *testing.T) {
	for _, ignoresCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellable gate", true: "gate returns after stop"}[ignoresCancel], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			marker := filepath.Join(t.TempDir(), "started")
			st := &state{wanted: true, proc: Process{Name: "test", Path: "/bin/sh", Args: []string{"-c", "touch \"$1\"", "sh", marker}, WaitFor: func(ctx context.Context) error {
				close(entered)
				if ignoresCancel {
					<-release
					return nil
				}
				<-ctx.Done()
				return ctx.Err()
			}}}
			s := &Supervisor{procs: map[string]*state{"test": st}}
			done := make(chan error, 1)
			go func() { done <- runOnce(ctx, st, Options{}) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("dependency gate not entered")
			}
			if err := s.apply("stop", "test"); err != nil {
				t.Fatal(err)
			}
			close(release)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stopped attempt succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stop did not cancel pending start")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("stopped child executed: %v", err)
			}
		})
	}
}

// TestRestartCancelsPendingStart pins the reload-vs-runOnce logic-race fix:
// when a restart lands while a start is still in its
// dependency gate — before it has published st.cur — restart() must cancel that
// in-flight attempt, not merely buffer a kick. Otherwise the gated start would
// go on to exec the OLD argv (a reload swaps st.proc but the gated attempt
// already snapshotted the old value) and run until it exits — the exact stale-
// args window reload closes. The started child must never appear.
func TestRestartCancelsPendingStart(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marker := filepath.Join(t.TempDir(), "started")
	st := &state{wanted: true, kick: make(chan struct{}, 1), proc: Process{
		Name: "test", Path: "/bin/sh",
		Args: []string{"-c", "touch \"$1\"", "sh", marker},
		WaitFor: func(ctx context.Context) error {
			close(entered)
			<-ctx.Done() // block in the gate until restart() cancels this attempt
			return ctx.Err()
		},
	}}
	done := make(chan error, 1)
	go func() { done <- runOnce(ctx, st, Options{}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dependency gate not entered")
	}

	st.restart() // in-flight start (cur==nil) must be cancelled, not just kicked

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-flight start was not cancelled by restart()")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart() did not cancel the pending start")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled child executed with stale args: %v", err)
	}
}

// TestReloadUpdatesArgsAndRestarts pins the reload op: it replaces a
// component's exec definition with the caller's new one (so an in-place upgrade's
// changed argv/config path takes effect) and kicks a restart, and it clears any
// ArgsFn re-render hook since explicit args were supplied.
func TestReloadUpdatesArgsAndRestarts(t *testing.T) {
	st := &state{
		wanted: false, // deliberately stopped: restart must wake it via kick
		proc: Process{
			Name:   "containerd",
			Path:   "/old/bin/containerd",
			Args:   []string{"--config", "/etc/containerd/config.toml"},
			ArgsFn: func() []string { return []string{"stale"} },
		},
		kick: make(chan struct{}, 1),
	}
	s := &Supervisor{procs: map[string]*state{"containerd": st}}

	spec := ProcSpec{
		Path: "/var/lib/stratum/bin/containerd",
		Args: []string{"--config", "/var/lib/stratum/config/containerd.toml"},
		Env:  []string{"PATH=/var/lib/stratum/bin"},
	}
	if err := s.reload("containerd", spec); err != nil {
		t.Fatalf("reload: %v", err)
	}

	got := st.get().proc
	if got.Path != spec.Path {
		t.Errorf("path = %q, want %q", got.Path, spec.Path)
	}
	if strings.Join(got.Args, " ") != strings.Join(spec.Args, " ") {
		t.Errorf("args = %v, want %v", got.Args, spec.Args)
	}
	if strings.Join(got.Env, " ") != strings.Join(spec.Env, " ") {
		t.Errorf("env = %v, want %v", got.Env, spec.Env)
	}
	if got.ArgsFn != nil {
		t.Error("ArgsFn must be cleared when explicit args are supplied, else it overrides them")
	}
	if !st.get().wanted {
		t.Error("reload must mark the component wanted so the supervise loop restarts it")
	}
	select {
	case <-st.kick:
	default:
		t.Error("reload did not kick the (stopped) supervise loop to restart the component")
	}

	if err := s.reload("nope", spec); err == nil {
		t.Error("reload of an unknown component should error")
	}
}

// TestForeignSocketOwnerIsFlagged pins that a component that exits while
// its socket is still served by another process (an orphan from an earlier
// run) is reported Foreign so the node is not declared healthy; a plain crash
// with a dead socket is not; and the flag clears once a child stays up.
func TestForeignSocketOwnerIsFlagged(t *testing.T) {
	dir, err := os.MkdirTemp("", "foreign-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	st := &state{wanted: true, proc: Process{Name: "containerd", Socket: sock}}

	noteForeignSocket(st) // socket absent: an ordinary crash
	if st.get().foreign {
		t.Fatal("flagged foreign with nothing listening")
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	noteForeignSocket(st)
	if !st.get().foreign {
		t.Fatal("live socket after the child exited was not flagged foreign")
	}
	s := &Supervisor{procs: map[string]*state{"containerd": st}}
	if got := s.statuses(); len(got) != 1 || !got[0].Foreign {
		t.Fatalf("statuses = %+v; want Foreign", got)
	}

	// The owner goes away and a child now stays up: the flag clears.
	ln.Close()
	old := foreignSettle
	foreignSettle = 50 * time.Millisecond
	t.Cleanup(func() { foreignSettle = old })
	st.proc = Process{Name: "containerd", Path: "/bin/sleep", Args: []string{"30"}, Socket: sock}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runOnce(ctx, st, Options{}) }()
	deadline := time.Now().Add(5 * time.Second)
	for st.get().foreign && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st.get().foreign {
		t.Error("foreign flag did not clear after the child stayed up")
	}
	cancel()
	<-done
}

// TestForeignSocketMessageDoesNotSuggestUninstall: `stratum uninstall` deletes
// node state (etcd data, PKI, binaries); the recovery for an orphan is to stop
// the process that owns the socket, and only that.
func TestForeignSocketMessageDoesNotSuggestUninstall(t *testing.T) {
	m := foreignSocketMessage("containerd", "/run/stratum/containerd/containerd.sock")
	if strings.Contains(m, "uninstall") {
		t.Errorf("message recommends uninstall: %q", m)
	}
	for _, want := range []string{"containerd", "/run/stratum/containerd/containerd.sock", "NOT healthy", "stop only that process"} {
		if !strings.Contains(m, want) {
			t.Errorf("message %q lacks %q", m, want)
		}
	}
}

func TestStartLabelledSetsTheExecContextAndFallsBack(t *testing.T) {
	old := threadExecAttr
	t.Cleanup(func() { threadExecAttr = old })

	threadExecAttr = filepath.Join(t.TempDir(), "exec")
	if err := os.WriteFile(threadExecAttr, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := startLabelled(cmd, "system_u:system_r:container_runtime_t:s0"); err != nil {
		t.Fatalf("startLabelled: %v", err)
	}
	_ = cmd.Wait()
	if b, err := os.ReadFile(threadExecAttr); err != nil || string(b) != "system_u:system_r:container_runtime_t:s0" {
		t.Errorf("exec context written = %q, %v", b, err)
	}

	threadExecAttr = filepath.Join(t.TempDir(), "missing", "exec")
	cmd = exec.Command("true")
	if err := startLabelled(cmd, "system_u:system_r:container_runtime_t:s0"); err != nil {
		t.Fatalf("a refused exec context stopped the start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("the unlabelled fallback did not run: %v", err)
	}
}
