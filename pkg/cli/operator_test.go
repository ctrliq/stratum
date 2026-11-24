package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// a port nothing serves: only the signal decides when the command returns.
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

// the startup log is the barrier proving the signal context is installed.
func TestOperatorCancelledContextSurfacesAStopError(t *testing.T) {
	if os.Getenv("STRATUM_OPERATOR_STOP_CHILD") == "1" {
		kc := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(kc, []byte(deadAPIServerKubeconfig), 0o600); err != nil {
			t.Fatal(err)
		}
		root := NewRootCommand()
		root.SetArgs([]string{"operator", "--kubeconfig=" + kc, "--leader-elect=false"})
		err := root.Execute()
		if errors.Is(err, context.Canceled) {
			fmt.Println("stop error surfaced")
		} else {
			fmt.Printf("unexpected result: %v\n", err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOperatorCancelledContextSurfacesAStopError$", "-test.v")
	// KUBEBUILDER_ASSETS is dropped so the child's TestMain never stands up envtest.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUBEBUILDER_ASSETS=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "STRATUM_OPERATOR_STOP_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// drain stdout before Wait: Wait closes the pipes and a fast exit would lose the verdict.
	drained := make(chan struct{})
	waited := make(chan error, 1)
	go func() {
		<-drained
		waited <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	errLines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			errLines <- sc.Text()
		}
		close(errLines)
	}()
	waitForLog := func(substr string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-errLines:
				if !ok {
					t.Fatalf("child exited before logging %q", substr)
				}
				if strings.Contains(l, substr) {
					return
				}
			case <-deadline:
				t.Fatalf("child never logged %q", substr)
			}
		}
	}
	// the startup log prints inside operator.Run, after the signal context is installed.
	waitForLog("stratum-operator: reconciling")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	lines := make(chan string, 16)
	go func() {
		defer close(drained)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before printing %q", want)
				}
				if l == want {
					return
				}
			case <-deadline:
				t.Fatalf("child never printed %q", want)
			}
		}
	}
	waitFor("stop error surfaced")
	// the exit code is not asserted: a signal-caused stop maps to exit 0.
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the CLI printed its verdict but the process did not terminate")
	}
}

// maps a root-command error the way cmd/stratum/main.go does, so children exit as main would.
func exitAsMainMaps(err error) int {
	if err == nil {
		return 0
	}
	var usageErr *ErrUsage
	var exitErr *ErrExitCode
	switch {
	case errors.Is(err, ErrNoCommand):
		return 2
	case errors.As(err, &usageErr):
		return 2
	case errors.Is(err, ErrCheckFailed):
		return 1
	case errors.As(err, &exitErr):
		return exitErr.Code
	default:
		fmt.Fprintln(os.Stderr, "stratum:", err)
		return 1
	}
}

func writeDeadKubeconfig(t *testing.T) string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(deadAPIServerKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc
}

// runs the real `stratum operator` in-process and exits with the code main would map.
func operatorChild(t *testing.T, kubeconfig string, args ...string) {
	t.Helper()
	root := NewRootCommand()
	root.SetArgs(append([]string{"operator", "--kubeconfig=" + kubeconfig}, args...))
	err := root.Execute()
	code := exitAsMainMaps(err)
	fmt.Printf("exit code: %d\n", code)
	os.Exit(code)
}

// stdin is a held-open pipe, so a stdio child blocks until a signal arrives.
type cliProc struct {
	cmd      *exec.Cmd
	lines    chan string
	errLines chan string
	waited   chan error
	mu       sync.Mutex
	stderr   []string
}

// KUBEBUILDER_ASSETS is dropped so the child's TestMain never stands up envtest.
func startCLIChild(t *testing.T, env, run string) *cliProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+run+"$", "-test.v")
	envs := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUBEBUILDER_ASSETS=") || strings.HasPrefix(kv, env+"=") {
			continue
		}
		envs = append(envs, kv)
	}
	cmd.Env = append(envs, env+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &cliProc{
		cmd:      cmd,
		lines:    make(chan string, 64),
		errLines: make(chan string, 64),
		waited:   make(chan error, 1),
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// drain both pipes before Wait, or a fast clean exit loses its final verdict.
	var streams sync.WaitGroup
	streams.Add(2)
	go func() {
		defer streams.Done()
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	go func() {
		defer streams.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			l := sc.Text()
			p.mu.Lock()
			p.stderr = append(p.stderr, l)
			p.mu.Unlock()
			p.errLines <- l
		}
		close(p.errLines)
	}()
	go func() {
		streams.Wait()
		p.waited <- cmd.Wait()
	}()
	return p
}

func (p *cliProc) waitForErrLog(t *testing.T, substr string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.errLines:
			if !ok {
				t.Fatalf("child exited before logging %q", substr)
			}
			if strings.Contains(l, substr) {
				return
			}
		case <-deadline:
			t.Fatalf("child never logged %q", substr)
		}
	}
}

func (p *cliProc) waitForExitCode(t *testing.T) int {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatal("child exited before reporting an exit code (a signal outside its set kills it by default action)")
			}
			var code int
			if strings.HasPrefix(l, "exit code: ") {
				if _, err := fmt.Sscanf(l, "exit code: %d", &code); err == nil {
					return code
				}
			}
		case <-deadline:
			t.Fatal("child never reported an exit code")
		}
	}
}

func (p *cliProc) waitExitedCode(t *testing.T, want int) {
	t.Helper()
	select {
	case err := <-p.waited:
		if err == nil {
			if want != 0 {
				t.Fatalf("child exited 0, want exit %d", want)
			}
			return
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		if got := ee.ExitCode(); got == -1 {
			t.Fatal("child was killed by a signal; the command must stop and exit by itself")
		} else if got != want {
			t.Fatalf("child exited %d, want exit %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child printed its verdict but did not terminate")
	}
}

func (p *cliProc) stderrSnapshot(t *testing.T) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-p.errLines:
			if !ok {
				p.mu.Lock()
				defer p.mu.Unlock()
				return p.stderr
			}
		case <-deadline:
			t.Fatal("the child's stderr did not close within 5s of its exit")
		}
	}
}

// a signal-caused stop must be a clean exit 0 on SIGINT, SIGTERM and SIGHUP.
func TestOperatorSignalStopExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("STRATUM_OPERATOR_EXIT_CHILD") == "1" {
				operatorChild(t, writeDeadKubeconfig(t), "--leader-elect=false")
				return
			}
			p := startCLIChild(t, "STRATUM_OPERATOR_EXIT_CHILD", "TestOperatorSignalStopExitsZero")
			// the startup log prints inside operator.Run, after the signal context is installed.
			p.waitForErrLog(t, "stratum-operator: reconciling")
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if code := p.waitForExitCode(t); code != 0 {
				t.Fatalf("operator stopped by one %s maps to exit %d, want 0 (a signal-caused clean stop)", tc.name, code)
			}
			p.waitExitedCode(t, 0)
			// a clean stop must produce no "stratum: ..." failure line of its own.
			for _, l := range p.stderrSnapshot(t) {
				if strings.HasPrefix(l, "stratum: ") {
					t.Errorf("the signal stop was logged as a failed exit: %q", l)
				}
			}
		})
	}
}

// only a signal-caused stop is clean; any other operator failure keeps exit 1.
func TestOperatorUnrelatedErrorStillExitsNonzero(t *testing.T) {
	if os.Getenv("STRATUM_OPERATOR_ERR_CHILD") == "1" {
		kc := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(kc, []byte("::: not a kubeconfig"), 0o600); err != nil {
			t.Fatal(err)
		}
		operatorChild(t, kc, "--leader-elect=false")
		return
	}
	p := startCLIChild(t, "STRATUM_OPERATOR_ERR_CHILD", "TestOperatorUnrelatedErrorStillExitsNonzero")
	if code := p.waitForExitCode(t); code != 1 {
		t.Fatalf("an unrelated operator failure maps to exit %d, want 1", code)
	}
	p.waitExitedCode(t, 1)
}

// the operator's signal context descends from the command's parent context.
func TestOperatorStopsWhenTheParentContextIsCancelled(t *testing.T) {
	if os.Getenv("STRATUM_OPERATOR_PARENT_CHILD") == "1" {
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
		root.SetArgs([]string{"operator", "--kubeconfig=" + kc, "--leader-elect=false"})
		err := root.ExecuteContext(ctx)
		code := exitAsMainMaps(err)
		fmt.Printf("exit code: %d\n", code)
		os.Exit(code)
	}
	p := startCLIChild(t, "STRATUM_OPERATOR_PARENT_CHILD", "TestOperatorStopsWhenTheParentContextIsCancelled")
	p.waitForErrLog(t, "stratum-operator: reconciling")
	if err := p.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	// a host's cancellation is not a signal's clean stop, so it exits 1.
	if code := p.waitForExitCode(t); code != 1 {
		t.Fatalf("a parent-context cancellation maps to exit %d, want 1: a host's cancellation is not a signal's clean stop", code)
	}
	p.waitExitedCode(t, 1)
}
