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

// Addresses a port nothing serves, so the dashboard starts and only a signal ends it.
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

func exitCodeAsMainMaps(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

func writeDeadKubeconfig(t *testing.T) string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(deadAPIServerKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc
}

const dashChildEnv = "STRATUM_DASH_CHILD"

func dashChild(t *testing.T) {
	t.Helper()
	kc := writeDeadKubeconfig(t)
	cmd := Command()
	cmd.SetArgs([]string{"--listen=127.0.0.1:0", "--kubeconfig=" + kc})
	code := exitCodeAsMainMaps(cmd.Execute())
	fmt.Printf("exit code: %d\n", code)
	os.Exit(code)
}

// A single SIGINT, SIGTERM or SIGHUP stops `stratum dash` cleanly, never by default action.
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
			// A served response proves the signal handler is installed.
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

// A second signal during a stalled shutdown must kill the process, not be swallowed.
func TestDashSecondSignalKillsDuringShutdown(t *testing.T) {
	if os.Getenv(dashChildEnv) == "1" {
		dashChild(t)
		return
	}
	child := startDashChild(t, "TestDashSecondSignalKillsDuringShutdown")
	addr := waitForServing(t, child)
	servingBarrier(t, addr)
	// A connection stuck mid-headers is active, not idle, so Shutdown waits for it.
	stalled, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial the dash child at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = stalled.Close() })
	if _, err := stalled.Write([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n")); err != nil {
		t.Fatalf("write a partial request head: %v", err)
	}
	// Proves the accept loop already accepted the stalled connection.
	servingBarrier(t, addr)

	if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
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

// run()'s context must descend from the command's parent context.
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
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of the parent context being cancelled: the dashboard is not derived from its command's parent context")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type dashProc struct {
	cmd    *exec.Cmd
	lines  chan string
	waited chan error
}

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
	// Wait closes StdoutPipe once the child exits; drain first so a fast exit keeps its exit-code line.
	drained := make(chan struct{})
	go func() {
		<-drained
		p.waited <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() {
		defer close(drained)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	return p
}

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

// A served response proves the signal handler is in place and a signal cannot race it.
func servingBarrier(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET http://%s/ before signaling: %v", addr, err)
	}
	resp.Body.Close()
}

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

func waitForListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return
			}
			// Keep polling until refusal proves the listener has closed.
			var ne net.Error
			if !errors.Is(err, syscall.ECONNRESET) && !(errors.As(err, &ne) && ne.Timeout()) {
				t.Fatalf("observe listener shutdown: %v", err)
			}
		} else {
			conn.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener still accepts connections after the first signal")
}
