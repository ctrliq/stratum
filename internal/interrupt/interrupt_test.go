package interrupt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestContextSecondSignalKills(t *testing.T) {
	if os.Getenv("STRATUM_INTERRUPT_CHILD") == "1" {
		if sig, _ := strconv.Atoi(os.Getenv("STRATUM_INTERRUPT_IGNORE")); sig != 0 {
			signal.Ignore(syscall.Signal(sig))
		}
		ctx, stop := Context()
		defer stop()
		os.Stdout.WriteString("ready\n")
		<-ctx.Done()
		os.Stdout.WriteString("cancelled\n")
		fmt.Fprintf(os.Stdout, "verdict cleanstop=%v canceled=%v\n",
			CleanStop(context.Cause(ctx)), ctx.Err() == context.Canceled)
		time.Sleep(time.Minute) // a phase that never looks at ctx
		return
	}
	for _, tc := range []struct {
		name   string
		sig    syscall.Signal
		ignore bool
	}{
		{"SIGINT", syscall.SIGINT, false},
		{"SIGTERM", syscall.SIGTERM, false},
		{"SIGHUP", syscall.SIGHUP, false},
		{"ignored SIGINT", syscall.SIGINT, true},
		{"ignored SIGHUP", syscall.SIGHUP, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestContextSecondSignalKills$", "-test.v")
			cmd.Env = append(os.Environ(), "STRATUM_INTERRUPT_CHILD=1")
			if tc.ignore {
				cmd.Env = append(cmd.Env, "STRATUM_INTERRUPT_IGNORE="+strconv.Itoa(int(tc.sig)))
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Drain before Wait: the pipe close can drop the child's last lines.
			drained := make(chan struct{})
			waited := make(chan error, 1)
			go func() {
				<-drained
				waited <- cmd.Wait()
			}()
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			lines := make(chan string, 16)
			go func() {
				defer close(drained)
				sc := bufio.NewScanner(out)
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
			waitFor("ready")
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			waitFor("cancelled")
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before its verdict line")
				}
				if want := "verdict cleanstop=true canceled=true"; l != want {
					t.Fatalf("verdict line = %q, want %q: a signal's cause must identify a clean stop and Err must be exactly context.Canceled", l, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("child never printed its verdict line")
			}
			select {
			case err := <-waited:
				t.Fatalf("child exited after one %s (%v); the first signal should only cancel the context", tc.name, err)
			default:
			}
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("child err = %v, want it killed by the signal", err)
				}
				wantSignal := tc.sig
				if tc.ignore {
					wantSignal = syscall.SIGKILL
				}
				if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || ws.Signal() != wantSignal {
					t.Fatalf("child ended with %v, want force-stop by %s", ee.ProcessState, wantSignal)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("a second %s did not kill the child: the default handler was not restored", tc.name)
			}
		})
	}
}

func TestCleanStopDistinguishesThePackagesOwnCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a clean nil", nil},
		{"an unrelated failure", errors.New("etcd not serving")},
		{"an unrelated cancellation", context.Canceled},
		{"a wrapped unrelated cancellation", fmt.Errorf("run: %w", context.Canceled)},
		{"a deadline", context.DeadlineExceeded},
	} {
		if CleanStop(tc.err) {
			t.Errorf("%s: CleanStop(%v) = true, want false: only the package's own cancellation is a clean stop", tc.name, tc.err)
		}
	}
}

func TestDerivedContextWithoutAParentBehavesAsContext(t *testing.T) {
	ctx, stop := DerivedContext(context.Background())
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the context was not cancelled by SIGTERM")
	}
	if !CleanStop(context.Cause(ctx)) {
		t.Errorf("CleanStop(Cause) = false, want a signal cause: %v", context.Cause(ctx))
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("Err() = %v, want it to read as context.Canceled", ctx.Err())
	}
}

func TestDerivedContextFollowsItsParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, stop := DerivedContext(parent)
	defer stop()

	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the derived context did not close within 5s of its parent's cancellation")
	}
	if CleanStop(context.Cause(ctx)) {
		t.Errorf("CleanStop(Cause) = true for parent cancellation: %v", context.Cause(ctx))
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("Err() = %v, want context.Canceled", ctx.Err())
	}
}

func TestDerivedContextKeepsThePackageSignalStop(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, stop := DerivedContext(parent)
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the derived context was not cancelled by SIGTERM")
	}
	if !CleanStop(context.Cause(ctx)) {
		t.Errorf("CleanStop(Cause) = false, want a signal cause: %v", context.Cause(ctx))
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("Err() = %v, want it to read as context.Canceled", ctx.Err())
	}
}

func TestDerivedContextCancelFuncEndsTheWatcher(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	before := runtime.NumGoroutine()
	ctx, stop := DerivedContext(parent)
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the returned CancelFunc did not cancel the derived context")
	}
	deadline := time.After(5 * time.Second)
	for runtime.NumGoroutine() > before {
		select {
		case <-deadline:
			t.Fatalf("%d goroutines still running after the returned CancelFunc, want <= %d: the derivation goroutine does not stop", runtime.NumGoroutine(), before)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestDerivedContextWithAnAlreadyCancelledParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx, stop := DerivedContext(parent)
	defer stop()
	if ctx.Err() != context.Canceled {
		t.Fatalf("Err() immediately after deriving = %v, want context.Canceled", ctx.Err())
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a derived context whose parent was cancelled before the install never closed")
	}
	if CleanStop(context.Cause(ctx)) {
		t.Errorf("CleanStop(Cause) = true for parent cancellation: %v", context.Cause(ctx))
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("Err() = %v, want context.Canceled", ctx.Err())
	}
}

func TestDerivedContextPreservesParentValuesDeadlineAndCause(t *testing.T) {
	type key struct{}
	deadline := time.Now().Add(time.Hour)
	parent, cancelDeadline := context.WithDeadline(context.WithValue(context.Background(), key{}, "trace"), deadline)
	defer cancelDeadline()
	parent, cancelCause := context.WithCancelCause(parent)
	defer cancelCause(nil)
	ctx, stop := DerivedContext(parent)
	defer stop()
	if got := ctx.Value(key{}); got != "trace" {
		t.Errorf("Value = %v, want trace", got)
	}
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Errorf("Deadline = %v, %v; want %v, true", got, ok, deadline)
	}
	cause := errors.New("host stopped")
	cancelCause(cause)
	if ctx.Err() != context.Canceled || context.Cause(ctx) != cause || CleanStop(context.Cause(ctx)) {
		t.Errorf("parent cancellation: Err = %v, Cause = %v, CleanStop = %v", ctx.Err(), context.Cause(ctx), CleanStop(context.Cause(ctx)))
	}
}

func TestDerivedContextPreservesExpiredParentDeadline(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	ctx, stop := DerivedContext(parent)
	defer stop()
	if ctx.Err() != context.DeadlineExceeded || context.Cause(ctx) != context.DeadlineExceeded || CleanStop(context.Cause(ctx)) {
		t.Errorf("expired parent: Err = %v, Cause = %v, CleanStop = %v", ctx.Err(), context.Cause(ctx), CleanStop(context.Cause(ctx)))
	}
}

func TestExpectedStopPreservesChildContextProvenanceAndFailures(t *testing.T) {
	if sig := os.Getenv("STRATUM_EXPECTED_STOP_SIGNAL"); sig != "" {
		n, err := strconv.Atoi(sig)
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := DerivedContext(context.Background())
		defer stop()
		child, cancel := context.WithTimeout(ctx, time.Hour)
		defer cancel()
		cancelChild, cancelDerived := context.WithCancel(ctx)
		defer cancelDerived()
		if err := syscall.Kill(syscall.Getpid(), syscall.Signal(n)); err != nil {
			t.Fatal(err)
		}
		<-child.Done()
		<-cancelChild.Done()
		for name, c := range map[string]context.Context{"owner": ctx, "timeout child": child, "cancel child": cancelChild} {
			if c.Err() != context.Canceled || !CleanStop(context.Cause(c)) {
				t.Errorf("%s: Err = %v, Cause = %v; want context.Canceled and a signal cause", name, c.Err(), context.Cause(c))
			}
			if !ExpectedStop(c, c.Err()) {
				t.Errorf("%s: expected signal cancellation was not recognized", name)
			}
		}
		failure := errors.New("API unavailable")
		for _, tc := range []struct {
			name string
			err  error
			want bool
		}{
			{"own cancellation", ctx.Err(), true},
			{"child cancellation", fmt.Errorf("lease: %w", child.Err()), true},
			{"joined cancellations", errors.Join(ctx.Err(), child.Err()), true},
			{"nil", nil, false},
			{"deadline", context.DeadlineExceeded, false},
			{"failure", failure, false},
			{"joined failure", errors.Join(child.Err(), failure), false},
			{"wrapped joined failure", fmt.Errorf("confirm lease: %w", errors.Join(ctx.Err(), failure)), false},
		} {
			if got := ExpectedStop(ctx, tc.err); got != tc.want {
				t.Errorf("%s: ExpectedStop = %v, want %v", tc.name, got, tc.want)
			}
		}
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExpectedStopPreservesChildContextProvenanceAndFailures$")
			cmd.Env = append(os.Environ(), "STRATUM_EXPECTED_STOP_SIGNAL="+strconv.Itoa(int(sig)))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("signal child failed: %v\n%s", err, out)
			}
		})
	}
}

func TestExpectedStopRejectsParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, stop := DerivedContext(parent)
	defer stop()
	if ExpectedStop(ctx, ctx.Err()) {
		t.Fatal("host cancellation was classified as a signal stop")
	}
}
