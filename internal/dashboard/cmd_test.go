package dashboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Signal-handling tests for `stratum dash`. They live here in
// internal/dashboard, not in pkg/cli, because the dashboard command is
// registered by cmd/stratum/main.go and must stay out of pkg/cli — the one
// package a downstream build imports (internal/dashboard/cmd.go:26-32).
//
// Every long-running command uses the shared set: SIGINT, SIGTERM and SIGHUP, where the
// first signal cancels and restores the default action so the second kills.
// The re-exec child pattern is the house one (internal/certs/interrupt_test.go:125):
// no unit test builds a Go binary; the parent signals a child that runs the
// real command in-process.

// deadAPIServerKubeconfig addresses a port nothing serves, so the dashboard
// starts (it builds its clients without contacting the apiserver,
// internal/dashboard/cmd.go:155-162) and only a signal decides when it
// returns.
const deadAPIServerKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: dead
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: dead
    context:
      cluster: dead
      user: dead
current-context: dead
users:
  - name: dead
    user: {}
`

// exitCodeAsMainMaps reports the exit status cmd/stratum/main.go gives a dash
// run's error: run never returns pkg/cli's special error types, so the
// mapping is nil to 0 and everything else to main's default case, exit 1
// (cmd/stratum/main.go:46-70). The child exits with the mapped code, so the
// parent observes the exit status the real binary would produce.
func exitCodeAsMainMaps(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// writeDeadKubeconfig writes the unreachable-apiserver kubeconfig the dash
// children run against.
func writeDeadKubeconfig(t *testing.T) string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(deadAPIServerKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc
}

// dashChildEnv is the env var that selects the child inside each test; one
// value serves every dash child because the child body is the same.
const dashChildEnv = "STRATUM_DASH_CHILD"

// dashChild runs the real `stratum dash` command in-process against an
// unreachable apiserver on an ephemeral loopback port, maps the result the
// way cmd/stratum/main.go would, and exits with that code.
func dashChild(t *testing.T) {
	t.Helper()
	kc := writeDeadKubeconfig(t)
	cmd := Command()
	cmd.SetArgs([]string{"--listen=127.0.0.1:0", "--kubeconfig=" + kc})
	code := exitCodeAsMainMaps(cmd.Execute())
	fmt.Printf("exit code: %d\n", code)
	os.Exit(code)
}

// TestDashStopsCleanlyOnEverySignalInTheSharedSet pins the shared interrupt
// set at the command level: a single SIGINT, SIGTERM or SIGHUP must stop
// `stratum dash` cleanly, exiting 0, never killing the process. A SIGHUP
// must not take its default action and kill the process outright.
// SIGINT and SIGTERM are the guards: they must stay clean stops through
// the HTTP shutdown, never regressing to the default action.
func TestDashStopsCleanlyOnEverySignalInTheSharedSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv(dashChildEnv) == "1" {
				dashChild(t)
				return
			}
			child := startDashChild(t, "TestDashStopsCleanlyOnEverySignalInTheSharedSet")
			// Barrier: a served response proves the Serve goroutine is
			// running, and it only starts after run() has installed its
			// signal context (internal/dashboard/cmd.go:229-238) — the
			// serving line alone prints BEFORE that install (cmd.go:223),
			// so a signal sent on it alone can race ahead of the handler and
			// kill the child by the default action.
			servingBarrier(t, waitForServing(t, child))
			if err := child.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if code := waitForExitCode(t, child); code != 0 {
				t.Fatalf("dash stopped by one %s maps to exit %d, want 0 (a signal-caused clean stop)", tc.name, code)
			}
			if err := waitExited(t, child); err != nil {
				t.Fatalf("dash stopped by one %s did not exit cleanly: %v", tc.name, err)
			}
		})
	}
}

// TestDashSecondSignalKillsDuringShutdown pins the second-signal rule of the
// shared set: once the first signal has asked the dashboard to stop, the
// next one must kill the process instead of being swallowed. The parent
// holds the stop open by leaving a connection mid-request-headers, which
// http.Server.Shutdown waits for (internal/dashboard/cmd.go:246) — the
// window a stalled stop exists for, and exactly when an operator needs the
// second signal to work: the child must die to the signal, not exit on
// its own once the grace runs out.
func TestDashSecondSignalKillsDuringShutdown(t *testing.T) {
	if os.Getenv(dashChildEnv) == "1" {
		dashChild(t)
		return
	}
	child := startDashChild(t, "TestDashSecondSignalKillsDuringShutdown")
	addr := waitForServing(t, child)
	servingBarrier(t, addr)
	// A connection stuck mid-headers is active, not idle, so the shutdown
	// waits for it: net/http treats a StateNew connection as idle only
	// after a 5s grace (Go issue 22682), which holds the stop open.
	stalled, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial the dash child at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = stalled.Close() })
	if _, err := stalled.Write([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n")); err != nil {
		t.Fatalf("write a partial request head: %v", err)
	}
	// A response over a later connection proves the serial accept loop
	// already accepted the stalled connection.
	servingBarrier(t, addr)

	if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Shutdown closes the listener after signal handling has exposed
	// cancellation. Wait for that observable condition before signal two.
	waitForListenerClosed(t, addr)
	select {
	case err := <-child.waited:
		t.Fatalf("child exited before the force-stop signal: %v", err)
	default:
	}

	if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	select {
	case err := <-child.waited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the second SIGTERM let dash exit cleanly (%v); it must kill the process", err)
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGTERM {
			t.Fatalf("dash ended with %v rather than being killed by the second SIGTERM", ee.ProcessState)
		}
	case <-deadline:
		t.Fatal("a second SIGTERM did not end the dash child within 15s: the signal is still being swallowed")
	}
}

// TestDashStopsWhenItsParentContextIsCancelled pins the derivation the
// shared handler must keep: run()'s context descends from the command's parent
// context, so a host that cancels the context it passed to ExecuteContext
// stops the dashboard rather than leaving it serving forever. It is green
// today (signal.NotifyContext wraps the parent,
// internal/dashboard/cmd.go:229) and guards the move onto internal/interrupt,
// which must not silently detach dash from its parent. It runs in-process
// because no signal is involved.
func TestDashStopsWhenItsParentContextIsCancelled(t *testing.T) {
	kc := writeDeadKubeconfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serving := make(chan struct{})
	var once sync.Once
	out := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "stratum dash: serving") {
			once.Do(func() { close(serving) })
		}
		return len(p), nil
	})

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, options{
			Listen:     "127.0.0.1:0",
			Kubeconfig: kc,
			SessionTTL: defaultTTL,
			Out:        out,
		})
	}()
	select {
	case <-serving:
	case <-time.After(10 * time.Second):
		t.Fatal("the dashboard never reported it was serving")
	}
	cancel()
	select {
	case <-done:
		// Stopped: the parent's cancellation reached run(). The error value
		// is not pinned — a host's cancellation is not one of the signals
		// whose stop this task maps to a clean exit — only that it stops.
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of the parent context being cancelled: the dashboard is not derived from its command's parent context")
	}
}

// writerFunc adapts a function to io.Writer for the barrier above.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// dashProc is a re-executed test binary running the real dash command
// in-process, with its stdout wired back to the parent test.
type dashProc struct {
	cmd    *exec.Cmd
	lines  chan string
	waited chan error
}

// startDashChild re-executes this test binary as the dash child of the named
// test (the house re-exec pattern, internal/certs/interrupt_test.go:125).
func startDashChild(t *testing.T, run string) *dashProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+run+"$", "-test.v")
	cmd.Env = append(os.Environ(), dashChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &dashProc{cmd: cmd, lines: make(chan string, 64), waited: make(chan error, 1)}
	go func() { p.waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	return p
}

// waitForServing blocks until the child prints its serving line and returns
// the address it is listening on.
func waitForServing(t *testing.T, p *dashProc) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatal("the dash child exited before reporting it was serving")
			}
			const prefix = "stratum dash: serving http://"
			if i := strings.Index(l, prefix); i >= 0 {
				addr := l[i+len(prefix):]
				if j := strings.Index(addr, " "); j >= 0 {
					addr = addr[:j]
				}
				return addr
			}
		case <-deadline:
			t.Fatal("the dash child never reported it was serving")
		}
	}
}

// servingBarrier proves the child's HTTP loop is serving: the Serve
// goroutine only starts after run() installs its signal context
// (internal/dashboard/cmd.go:229-238), so a served response means the
// handler is in place and a signal cannot race ahead of it.
func servingBarrier(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET http://%s/ before signaling: %v", addr, err)
	}
	resp.Body.Close()
}

// waitForExitCode blocks until the child prints the exit status it mapped,
// and returns it.
func waitForExitCode(t *testing.T, p *dashProc) int {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatal("the dash child exited before reporting an exit code (a signal outside its set kills it by default action)")
			}
			var code int
			if strings.HasPrefix(l, "exit code: ") {
				if _, err := fmt.Sscanf(l, "exit code: %d", &code); err == nil {
					return code
				}
			}
		case <-deadline:
			t.Fatal("the dash child never reported an exit code")
		}
	}
}

// waitExited waits for the child to end after its verdict and reports a
// non-nil error for any termination other than a clean exit 0.
func waitExited(t *testing.T, p *dashProc) error {
	t.Helper()
	select {
	case err := <-p.waited:
		if err == nil {
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("the child ended abnormally (%v), want a clean exit 0", ee.ProcessState)
		}
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("the child printed its verdict but did not terminate")
	}
}

// waitForListenerClosed observes shutdown rather than guessing how long the
// child needs to receive its first signal. The deadline only bounds failure.
func waitForListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return
			}
			// A dial racing listener shutdown can reset before the next dial
			// is refused. Keep polling until refusal proves it has closed.
			if !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("observe listener shutdown: %v", err)
			}
		} else {
			conn.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener still accepts connections after the first signal")
}
