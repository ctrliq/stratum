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

// deadAPIServerKubeconfig addresses a port nothing serves, so every API call
// the operator makes fails the moment it is issued and only the signal decides
// when the command returns — the shape of `stratum operator` on a node whose
// apiserver is down, which is exactly the case where a signal stop must not
// be mistaken for a failure.
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

// TestOperatorCancelledContextSurfacesAStopError pins that a cancelled
// operator context surfaces through the CLI as the stop error: `stratum
// operator`'s handler builds its signal context (pkg/cli/operator.go:28) and
// returns the run's error, so a SIGTERM that cancels the command's context
// must come back from root.Execute() wrapping context.Canceled — the identity
// the `stratum up` and `stratum mcp` handlers already filter their clean
// stops on (pkg/cli/up.go:227-229, internal/mcp/server.go:59). Deliberately
// NOT pinned, because the shared handler owns them: the process exit code
// (ExitCode maps a signal-caused stop to 0), the operator's second-signal
// rule (the second signal kills), and the operator's signal set (SIGINT,
// SIGTERM and SIGHUP).
// The pin that must hold: the stop error keeps surfacing
// through the CLI so the exit mapping has something to map.
//
// The parent re-executes the test binary as a child that runs the real root
// command in-process — the house re-exec child pattern
// (internal/certs/interrupt_test.go:87-158; no unit test builds a Go binary)
// — and signals it once. The operator's startup log is the barrier that the
// signal context is installed: it prints inside operator.Run, after the
// handler's signal.NotifyContext, so a signal sent earlier would kill the
// child by the default action and the test would report a false failure.
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
	// KUBEBUILDER_ASSETS is dropped so the child's TestMain does not stand up
	// the envtest control plane (pkg/cli/mcp_k8s_test.go:29-33): this child
	// never touches it, and starting it would spend seconds the barrier's
	// own deadline has to absorb.
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
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
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
	// Barrier: this line prints inside operator.Run, after the handler's
	// signal.NotifyContext is installed, so the SIGTERM that follows cannot
	// race ahead of the signal context and kill the child by default.
	waitForLog("stratum-operator: reconciling")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	lines := make(chan string, 16)
	go func() {
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
	// The verdict is printed only after root.Execute() returned, so the CLI
	// has already unwound the stop; require the child to be gone. Its exit
	// code is not asserted: a signal-caused stop maps to exit 0 (pinned in
	// TestOperatorSignalStopExitsZero).
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the CLI printed its verdict but the process did not terminate")
	}
}

// exitAsMainMaps reports the process exit status cmd/stratum/main.go gives a
// root-command error, printing the default case's line exactly the same way
// (cmd/stratum/main.go:46-70): nil exits 0, the special error types keep
// their codes, everything else prints "stratum: <err>" and exits 1. The
// re-exec children below exit with the mapped code, so a test observes the
// exit status the real binary would produce — and, on stderr, whether the
// failure was logged as one.
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

// writeDeadKubeconfig writes the unreachable-apiserver kubeconfig the
// operator and mcp children run against (deadAPIServerKubeconfig above).
func writeDeadKubeconfig(t *testing.T) string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(deadAPIServerKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc
}

// operatorChild runs the real `stratum operator` command in-process, maps
// the result the way cmd/stratum/main.go would, and exits with that code.
func operatorChild(t *testing.T, kubeconfig string, args ...string) {
	t.Helper()
	root := NewRootCommand()
	root.SetArgs(append([]string{"operator", "--kubeconfig=" + kubeconfig}, args...))
	err := root.Execute()
	code := exitAsMainMaps(err)
	fmt.Printf("exit code: %d\n", code)
	os.Exit(code)
}

// cliProc is a re-executed test binary running one of the command children:
// the real root command in-process, with its stdout, stderr and exit wired
// back to the parent test. Its stdin is a pipe this parent holds open, so a
// stdio child blocks reading it and only a signal decides when it returns.
type cliProc struct {
	cmd      *exec.Cmd
	lines    chan string
	errLines chan string
	waited   chan error
	mu       sync.Mutex
	stderr   []string
}

// startCLIChild re-executes the test binary as the child of the test named
// run, selected by the env var env — the house re-exec pattern
// (internal/certs/interrupt_test.go:125). KUBEBUILDER_ASSETS is dropped so
// the child's TestMain does not stand up the envtest control plane
// (pkg/cli/mcp_k8s_test.go:29-33): the child never touches it.
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
	// Wait closes StdoutPipe and StderrPipe. Drain both streams first so a
	// fast clean exit cannot lose its final verdict to that close.
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

// waitForErrLog blocks until the child logs a stderr line containing substr.
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

// waitForExitCode blocks until the child prints the exit status it mapped,
// and returns it.
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

// waitExitedCode waits for the child to end after its verdict and fails the
// test unless it exited with exactly the wanted code, never killed by a
// signal.
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

// stderrSnapshot returns every stderr line the child produced. It waits for
// the child's stderr to close, so the snapshot is complete.
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

// TestOperatorSignalStopExitsZero checks signal handling for `stratum operator`:
// a stop a signal asked for must be a clean stop, exiting 0, on the whole
// shared set — SIGINT, SIGTERM and SIGHUP. Fails in every case if the
// operator's signals are not the shared interrupt set: a cancellation
// returned as a plain error reaches main's default case and exits 1 with a
// "stratum: context canceled" line, and a SIGHUP outside the set kills the
// process outright. The operator rides internal/interrupt — SIGHUP
// included, the second signal killing — and the signal-caused stop maps
// to exit 0 through the shared clean-stop helper, so main does not log
// it as a failed exit.
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
			// Barrier: the operator's startup log prints inside operator.Run
			// (internal/operator/operator.go:124), after the handler at
			// pkg/cli/operator.go:28 is installed, so the signal cannot race
			// ahead of it and kill the child by the default action.
			p.waitForErrLog(t, "stratum-operator: reconciling")
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if code := p.waitForExitCode(t); code != 0 {
				t.Fatalf("operator stopped by one %s maps to exit %d, want 0 (a signal-caused clean stop)", tc.name, code)
			}
			p.waitExitedCode(t, 0)
			// The stop is not a failure: main's default case is the only
			// place a failing command's error is printed, so a clean stop
			// must produce no "stratum: ..." line of its own.
			for _, l := range p.stderrSnapshot(t) {
				if strings.HasPrefix(l, "stratum: ") {
					t.Errorf("the signal stop was logged as a failed exit: %q", l)
				}
			}
		})
	}
}

// TestOperatorUnrelatedErrorStillExitsNonzero pins the other half of the
// exit mapping: only a signal-caused stop is clean. An operator failure with
// nothing to do with signals — here a kubeconfig no client can load — must
// keep exiting 1 with the shared handler, as it did before.
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

// TestOperatorStopsWhenTheParentContextIsCancelled pins the derivation the
// shared handler must keep: the operator's signal context descends from the
// command's parent context (pkg/cli/operator.go:28 uses cmd.Context()), so a
// host that cancels the context it passed to ExecuteContext stops the
// operator instead of leaving it running. It is green today and guards the
// move onto internal/interrupt, which must not detach the operator from its
// parent. The parent asks for the cancellation over SIGUSR1, a signal
// outside the shared set, so the request cannot be mistaken for one of the
// signals under test.
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
	// A host's cancellation is not a signal's clean stop — interrupt's
	// clean-stop helper must not claim it — so the operator reports the
	// cancellation and main's default case exits 1, exactly as today.
	if code := p.waitForExitCode(t); code != 1 {
		t.Fatalf("a parent-context cancellation maps to exit %d, want 1: a host's cancellation is not a signal's clean stop", code)
	}
	p.waitExitedCode(t, 1)
}
