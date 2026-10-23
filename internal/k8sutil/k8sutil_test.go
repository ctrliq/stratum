package k8sutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

func TestStampArgsAssertsBothAnnotations(t *testing.T) {
	got := stampArgs("n1", "v1.37.1", "v1.37.1-stratum.0")
	want := []string{"annotate", "node", "n1",
		"stratum.ciq.com/k8s-version=v1.37.1", "stratum.ciq.com/version=v1.37.1-stratum.0", "--overwrite"}
	if !slices.Equal(got, want) {
		t.Errorf("stampArgs = %v, want %v", got, want)
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stratum")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstalledStratumVersion(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	running := script(t, `echo "v2.0.0-embed.1 linux/amd64"`) // a version override, as an embedding binary reports
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		// The installed binary wins over this process's own (older) build.
		{"installed", script(t, `echo "v1.37.1-stratum.0 linux/amd64"`), "v1.37.1-stratum.0", false},
		{"installed with components", script(t, `echo "v9.9.9 linux/arm64"; echo "  kubernetes 1.37.1"`), "v9.9.9", false},
		// First `up`, before stratum is installed: the running executable
		// answers, so a version override is honoured.
		{"not installed", absent, "v2.0.0-embed.1", false},
		// Installed but unable to report: never the running (stale) build.
		{"fails", script(t, `exit 1`), "", true},
		{"silent", script(t, `true`), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldPath, oldSelf := stratumPath, selfExe
			stratumPath, selfExe = tt.path, func() (string, error) { return running, nil }
			t.Cleanup(func() { stratumPath, selfExe = oldPath, oldSelf })
			got, err := installedStratumVersion()
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("installedStratumVersion() = %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// A probe that exits cleanly answers, even when a child it left behind still
// holds its stdout: a WaitDelay meant to cut short a killed probe also failed
// a clean one whose output had not drained in time, which a loaded node hits.
func TestBinaryVersionWaitsOutAnInheritedPipe(t *testing.T) {
	p := script(t, `echo 'v1.37.1 linux/amd64'; (sleep 0.2) &`)
	got, err := BinaryVersion(p)
	if err != nil || got != "v1.37.1" {
		t.Fatalf("BinaryVersion = %q, %v; want v1.37.1, nil", got, err)
	}
}

func TestStampArgsOmitsAnUnknownStratumVersion(t *testing.T) {
	got := stampArgs("n1", "v1.37.1", "")
	want := []string{"annotate", "node", "n1", "stratum.ciq.com/k8s-version=v1.37.1", "--overwrite"}
	if !slices.Equal(got, want) {
		t.Errorf("stampArgs = %v, want %v", got, want)
	}
}

// A worker patches its own Node; the patch carries what stampArgs does.
func TestStampPatch(t *testing.T) {
	for _, tc := range []struct{ stratum, want string }{
		{"v1.37.1-stratum.0", `{"metadata":{"annotations":{"stratum.ciq.com/k8s-version":"v1.37.1","stratum.ciq.com/version":"v1.37.1-stratum.0"}}}`},
		{"", `{"metadata":{"annotations":{"stratum.ciq.com/k8s-version":"v1.37.1"}}}`},
	} {
		if got := string(stampPatch("v1.37.1", tc.stratum)); got != tc.want {
			t.Errorf("stampPatch(%q) = %s, want %s", tc.stratum, got, tc.want)
		}
	}
}

// One deadline bounds the whole wait, not each attempt. The fake clock
// makes the elapsed time exact, so a deadline reset per attempt (or a request
// that outlives it) cannot pass. The calls > 100 guard makes such a regression
// fail instead of poll forever.
func TestPollUntilDeadline(t *testing.T) {
	const timeout, interval = 200 * time.Millisecond, 30 * time.Millisecond

	run := func(t *testing.T, check func(ctx context.Context, call int) error) (calls int, elapsed time.Duration, err error) {
		t.Helper()
		start := time.Now()
		var deadline time.Time
		err = pollUntil(timeout, interval, func(ctx context.Context) error {
			calls++
			d, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("call %d: context has no deadline", calls)
			}
			if deadline.IsZero() {
				deadline = d
			} else if !d.Equal(deadline) {
				t.Fatalf("call %d: deadline %v, want the first call's %v: it was reset", calls, d, deadline)
			}
			if calls > 100 {
				return nil
			}
			return check(ctx, calls)
		})
		return calls, time.Since(start), err
	}

	t.Run("never succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls, elapsed, err := run(t, func(context.Context, int) error { return errors.New("not yet") })
			if err == nil || err.Error() != "not yet" {
				t.Errorf("err = %v, want the last check error", err)
			}
			// attempts at 0, 30, ... 180ms; the next sleep outlasts the deadline
			if want := 7; calls != want || elapsed != timeout {
				t.Errorf("%d calls over %s, want %d over %s", calls, elapsed, want, timeout)
			}
		})
	})

	t.Run("hung request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls, elapsed, err := run(t, func(ctx context.Context, call int) error {
				if call == 4 { // blocks from 90ms; only the overall deadline frees it
					<-ctx.Done()
				}
				return errors.New("not yet")
			})
			if err == nil {
				t.Error("err = nil, want the last check error")
			}
			if calls != 4 || elapsed != timeout {
				t.Errorf("%d calls over %s, want 4 over %s", calls, elapsed, timeout)
			}
		})
	})

	t.Run("succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls, elapsed, err := run(t, func(_ context.Context, call int) error {
				if call < 3 {
					return errors.New("not yet")
				}
				return nil
			})
			if err != nil || calls != 3 || elapsed != 2*interval {
				t.Errorf("err = %v after %d calls over %s, want nil after 3 over %s", err, calls, elapsed, 2*interval)
			}
		})
	})
}

// fakeKubectl installs a kubectl that runs script, and points BinDir at it.
func fakeKubectl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := version.BinDir
	version.BinDir = dir
	t.Cleanup(func() { version.BinDir = old })
}

// TestWaitHealthzContextEndsWithItsContext: a kubectl that
// fails at once must not be followed by the full retry sleep once ctx is done,
// and a kubectl that hangs is killed with ctx — neither outlasts the window.
func TestWaitHealthzContextEndsWithItsContext(t *testing.T) {
	for name, script := range map[string]string{
		"fails at once": "exit 1",
		"hangs":         "exec sleep 30",
	} {
		t.Run(name, func(t *testing.T) {
			fakeKubectl(t, script)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			begin := time.Now()
			err := WaitHealthzContext(ctx)
			if err == nil {
				t.Fatal("WaitHealthzContext succeeded against a failing kubectl")
			}
			if took := time.Since(begin); took > time.Second {
				t.Errorf("took %v inside a 100ms window", took)
			}
		})
	}
}

func TestWaitHealthzContextOK(t *testing.T) {
	fakeKubectl(t, "echo ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := WaitHealthzContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHealthzContextNoTimeLeft(t *testing.T) {
	fakeKubectl(t, "echo ok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitHealthzContext(ctx); err == nil {
		t.Error("WaitHealthzContext made a request with no time left")
	}
}

// countKubectl installs a kubectl that records one attempt per run and fails,
// so a wait's poll count is readable after the fact.
func countKubectl(t *testing.T, count string) {
	t.Helper()
	fakeKubectl(t, "printf x >> "+count+"\nexit 1")
}

// kubectlAttempts reads the attempt count countKubectl's kubectl recorded.
func kubectlAttempts(count string) int {
	b, err := os.ReadFile(count)
	if err != nil {
		return 0 // no run yet: the file only appears on the first attempt
	}
	return bytes.Count(b, []byte("x"))
}

// TestWaitNodeRegisteredStopsAtAPollBoundary checks cancellation for the
// registration wait: a run group that owns it joins it at shutdown, so the
// wait must end when its context does — at a poll boundary, the same shape
// WaitHealthzContext already has. The in-flight kubectl may finish; the sleep
// between polls is cut short; no further poll starts. The fake clock makes
// the elapsed time exact, the way TestPollUntilDeadline's does: a wait that
// sleeps the retry out to the next poll (or polls again after the cancel)
// cannot pass.
func TestWaitNodeRegisteredStopsAtAPollBoundary(t *testing.T) {
	t.Run("cancelled while waiting out the retry sleep", func(t *testing.T) {
		count := filepath.Join(t.TempDir(), "attempts")
		countKubectl(t, count)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- WaitNodeRegistered(ctx, "cp1", 30) }()
			// Polls at 0, 3 and 6s; the cancel lands inside the 6-to-9s sleep.
			time.Sleep(7 * time.Second)
			cancel()
			err := <-done
			if err == nil {
				t.Fatal("WaitNodeRegistered succeeded against a failing kubectl")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled, so a shutdown is tellable from a give-up", err)
			}
			if elapsed := time.Since(start); elapsed != 7*time.Second {
				t.Errorf("returned after %s, want the cancel's 7s: the 3s retry sleep must be cut short at the poll boundary, not slept out to 9s", elapsed)
			}
			if got := kubectlAttempts(count); got != 3 {
				t.Errorf("polled %d times, want the 3 before the cancel and none after it", got)
			}
		})
	})

	t.Run("cancelled before the first poll", func(t *testing.T) {
		count := filepath.Join(t.TempDir(), "attempts")
		countKubectl(t, count)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		begin := time.Now()
		err := WaitNodeRegistered(ctx, "cp1", 10)
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if got := kubectlAttempts(count); got != 0 {
			t.Errorf("polled %d times with no time left, want 0", got)
		}
		if took := time.Since(begin); took > time.Second {
			t.Errorf("took %s before the first poll, want an immediate boundary stop", took)
		}
	})

	// Same shape as TestWaitHealthzContextEndsWithItsContext's "hangs" case: a
	// kubectl the apiserver never answers must be killed with the context, not
	// waited out for the whole attempt budget.
	t.Run("a hung kubectl is killed with the context", func(t *testing.T) {
		fakeKubectl(t, "exec sleep 30")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		begin := time.Now()
		err := WaitNodeRegistered(ctx, "cp1", 10)
		if err == nil {
			t.Fatal("WaitNodeRegistered succeeded against a hung kubectl")
		}
		if took := time.Since(begin); took > time.Second {
			t.Errorf("took %v inside a 100ms window", took)
		}
	})
}

func TestWaitNodeRegisteredPreservesCancellationDuringFinalPoll(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "poll-started")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRATUM_TEST_POLL_STARTED", fifo)
	fakeKubectl(t, "printf x > \"$STRATUM_TEST_POLL_STARTED\"\nexec sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- WaitNodeRegistered(ctx, "cp1", 1) }()
	// The child writes only after it starts; cancel its one and final poll
	// while it is in flight, rather than guessing a scheduler delay.
	if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
		t.Fatalf("wait for the final poll to start: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("final poll returned %v, want cancellation preserved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("final poll did not return after cancellation")
	}
}

// captureRealStdout runs fn with the process's stdout captured, so a test
// can prove a write stayed off it. os.Stdout is restored even when fn panics,
// so a failed assertion cannot leave the test binary with a pipe as its
// stdout.
func captureRealStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	t.Cleanup(func() { os.Stdout = saved })
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()

	fn()

	os.Stdout = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

// TestStepWritesTheProgressHeaderThroughTheWriter pins that Step reports
// progress through an io.Writer the caller passes — os.Stdout for a CLI
// caller, a buffer for an MCP handler — instead of always writing to the
// process's stdout, which on the MCP stdio transport is the JSON-RPC stream.
//
// The header bytes are pinned exactly, because the CLI's green progress
// header must not change shape whichever writer it goes to.
func TestStepWritesTheProgressHeaderThroughTheWriter(t *testing.T) {
	t.Run("the header bytes reach the writer unchanged", func(t *testing.T) {
		var buf bytes.Buffer
		Step(&buf, "Install stratum v1.36.2")
		if want := "\n\033[1;32m==>\033[0m Install stratum v1.36.2\n"; buf.String() != want {
			t.Errorf("Step wrote %q, want the green header %q", buf.String(), want)
		}
	})

	t.Run("nothing reaches os.Stdout", func(t *testing.T) {
		onStdout := captureRealStdout(t, func() {
			var buf bytes.Buffer
			Step(&buf, "Swap host binaries")
			if buf.Len() == 0 {
				t.Error("Step wrote nothing through the writer")
			}
		})
		if strings.Contains(onStdout, "Swap host binaries") {
			t.Errorf("Step printed the progress header to os.Stdout: %q", onStdout)
		}
	})
}

// TestWaitNodeRegisteredStillRegistersAndStillGivesUp pins the wait's own
// behavior through the context change: a node that appears ends the wait on
// the poll that sees it, and a node that never does ends it at the attempt
// budget with an error that names it.
func TestWaitNodeRegisteredStillRegistersAndStillGivesUp(t *testing.T) {
	t.Run("registers", func(t *testing.T) {
		fakeKubectl(t, "exit 0")
		if err := WaitNodeRegistered(context.Background(), "cp1", 5); err != nil {
			t.Errorf("err = %v, want nil on the first successful poll", err)
		}
	})

	t.Run("gives up at the attempt budget", func(t *testing.T) {
		count := filepath.Join(t.TempDir(), "attempts")
		countKubectl(t, count)
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			err := WaitNodeRegistered(context.Background(), "cp1", 2)
			if err == nil || !strings.Contains(err.Error(), "cp1") {
				t.Fatalf("err = %v, want the give-up error naming the node", err)
			}
			if elapsed := time.Since(start); elapsed != 3*time.Second {
				t.Errorf("gave up after %s, want one inter-poll sleep of 3s: no sleep follows the last attempt, so with attempts=2 the budget is poll + sleep + poll", elapsed)
			}
			if got := kubectlAttempts(count); got != 2 {
				t.Errorf("polled %d times, want the 2 the budget allows", got)
			}
		})
	})
}
