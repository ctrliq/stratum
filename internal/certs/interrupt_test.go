package certs

import (
	"io"

	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/interrupt"
)

// A --cluster run interrupted between steps must not start the next one: the local
// phases do not observe ctx, so each is guarded.
func TestRunInitiatorPhaseDoesNotStartAfterAnInterrupt(t *testing.T) {
	cause := errors.New("etcd not serving")
	for _, tc := range []struct {
		name        string
		phase, done string
		wantGuide   []string
		noGuide     []string
	}{
		{name: "sign-new after trust-new", phase: PhaseSignNew, done: PhaseTrustNew,
			wantGuide: []string{"interrupted before sign-new", "stratum certs check", "--rollback --yes"}},
		{name: "finalize after sign-new", phase: PhaseFinalize, done: PhaseSignNew,
			wantGuide: []string{"interrupted before finalize", "stratum certs check", "--rollback --yes"},
			noGuide:   []string{"Do NOT roll back"}}, // nothing has finalized, so rollback is still the way out
		{name: "trust-new with nothing staged", phase: PhaseTrustNew, done: "",
			wantGuide: []string{"interrupted before trust-new"}, noGuide: []string{"--rollback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			ran := false
			err := runInitiatorPhase(ctx, tc.phase, tc.done, func() error { ran = true; return nil })
			if ran {
				t.Fatal("the phase ran after the run was interrupted")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want it to wrap context.Canceled", err)
			}
			for _, w := range tc.wantGuide {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("error = %v, want it to contain %q", err, w)
				}
			}
			for _, w := range tc.noGuide {
				if err != nil && strings.Contains(err.Error(), w) {
					t.Errorf("error = %v, must not contain %q", err, w)
				}
			}
		})
	}

	t.Run("a live context runs the phase and keeps its failure guidance", func(t *testing.T) {
		ran := false
		if err := runInitiatorPhase(context.Background(), PhaseSignNew, PhaseTrustNew, func() error { ran = true; return nil }); err != nil || !ran {
			t.Fatalf("ran=%v err=%v, want the phase to run cleanly", ran, err)
		}
		err := runInitiatorPhase(context.Background(), PhaseSignNew, PhaseTrustNew, func() error { return cause })
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "initiator sign-new") || !strings.Contains(err.Error(), "stratum certs check") {
			t.Errorf("error = %v, want the cause under 'initiator sign-new' plus halt guidance", err)
		}
	})
}

func TestInterruptedBeforePublishing(t *testing.T) {
	if err := interruptedBefore(context.Background(), "publishing the staged material", PhaseTrustNew); err != nil {
		t.Errorf("live context: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := interruptedBefore(ctx, "publishing the staged material", PhaseTrustNew)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "publishing the staged material") {
		t.Errorf("error = %v, want it to wrap the cancellation and name the step", err)
	}
}

func TestClusterCertContextsDoNotStartAfterParentCancellation(t *testing.T) {
	cs := fakeAdminClientset(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"rotate-ca", func() error { return RotateCAClusterContext(ctx, io.Discard, CARotateOptions{}) }},
		{"migrate-etcd-ca", func() error { return MigrateEtcdCAClusterContext(ctx, io.Discard, EtcdCAOptions{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled run = %v, want context.Canceled", err)
			}
			if actions := cs.Actions(); len(actions) != 0 {
				t.Fatalf("cancelled run contacted the cluster: %v", actions)
			}
		})
	}
}

// TestInterruptContextSecondSignalKills moved with interruptContext into the
// shared package as TestContextSecondSignalKills
// (internal/interrupt/interrupt_test.go), now covering SIGHUP as well: the
// signal set certs runs under is owned there.

// TestClusterCertsRunNeverStartsItsNextStepAfterSIGHUP pins the invariant a
// `certs rotate-ca --cluster` (or `migrate-etcd-ca --cluster`) run owes the
// operator under SIGHUP: once the signal arrives, the run never starts its
// next step. Before the shared interrupt package it held because SIGHUP was
// not in the local set interruptContext installed, so the default action
// killed the run mid-step; on the shared set (internal/interrupt) it holds by
// cancelling the run so runInitiatorPhase refuses instead. The pin asserts
// the invariant, not the mechanism: whichever way the run ends, the sign-new
// marker must never appear and the run must end.
func TestClusterCertsRunNeverStartsItsNextStepAfterSIGHUP(t *testing.T) {
	if os.Getenv("STRATUM_CERTS_SIGHUP_CHILD") == "1" {
		// A --cluster run in miniature, shaped like RotateCACluster
		// (internal/certs/cacluster.go:82): the shared interrupt context at
		// entry, every phase started through runInitiatorPhase
		// (internal/certs/cacluster.go:134). Wait at the boundary until the
		// parent sends a signal, so scheduler delays cannot start sign-new early.
		ctx, stop := interrupt.Context()
		defer stop()
		if err := runInitiatorPhase(ctx, PhaseTrustNew, "", func() error {
			os.Stdout.WriteString("trust-new done\n")
			return nil
		}); err != nil {
			os.Exit(3)
		}
		<-ctx.Done() // a signal, rather than elapsed wall time, releases the boundary
		if err := runInitiatorPhase(ctx, PhaseSignNew, PhaseTrustNew, func() error {
			os.Stdout.WriteString("sign-new started\n") // the step that must never begin
			time.Sleep(time.Minute)
			return nil
		}); err != nil {
			fmt.Printf("halted before sign-new: %v\n", err)
			os.Exit(3)
		}
		time.Sleep(time.Minute)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestClusterCertsRunNeverStartsItsNextStepAfterSIGHUP$", "-test.v")
	cmd.Env = append(os.Environ(), "STRATUM_CERTS_SIGHUP_CHILD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := make(chan string, 16)
	go func() {
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
	waitFor("trust-new done")
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// Read to the child's end: every line it ever printed is checked, so a
	// sign-new that started after the SIGHUP cannot hide behind the exit.
	deadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				break drain // the child is gone and its whole output is read
			}
			if l == "sign-new started" {
				t.Fatal("the run started sign-new after a SIGHUP: a cluster certs run must never start its next step once SIGHUP arrives")
			}
		case <-deadline:
			t.Fatal("the run neither died nor halted within 10s of the SIGHUP")
		}
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the child's output closed but the process did not terminate")
	}
}

// TestClusterCertsRunCancelsAtTheNextStepBoundaryAndASecondSignalKills pins
// the interrupt contract the shared interrupt package (internal/interrupt)
// gives the cluster certs commands. On the shared set — SIGINT, SIGTERM and
// SIGHUP — the first signal must cancel the run so it halts at its next
// step boundary through runInitiatorPhase, never starting the step, and the
// second signal must kill the process. A context that trapped only SIGINT and
// SIGTERM would leave SIGHUP killing the run mid-step, before any boundary halt.
func TestClusterCertsRunCancelsAtTheNextStepBoundaryAndASecondSignalKills(t *testing.T) {
	if os.Getenv("STRATUM_CERTS_BOUNDARY_CHILD") == "1" {
		// A --cluster run in miniature, shaped like RotateCACluster
		// (internal/certs/cacluster.go:82): the shared interrupt context at
		// entry, every phase started through runInitiatorPhase
		// (internal/certs/cacluster.go:134). Wait at the boundary until the
		// parent sends a signal, so scheduler delays cannot start sign-new early.
		ctx, stop := interrupt.Context()
		defer stop()
		if err := runInitiatorPhase(ctx, PhaseTrustNew, "", func() error {
			os.Stdout.WriteString("trust-new done\n")
			return nil
		}); err != nil {
			os.Exit(3)
		}
		<-ctx.Done() // a signal, rather than elapsed wall time, releases the boundary
		if err := runInitiatorPhase(ctx, PhaseSignNew, PhaseTrustNew, func() error {
			os.Stdout.WriteString("sign-new started\n") // the step that must never begin
			time.Sleep(time.Minute)
			return nil
		}); err != nil {
			fmt.Printf("halted before sign-new: %v\n", err)
			// The run has not exited yet: the deferred cleanup, which deletes
			// the staged key Secret on a detached context, still runs — so a
			// second signal must still kill it.
			time.Sleep(time.Minute)
			os.Exit(3)
		}
		time.Sleep(time.Minute)
		return
	}
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestClusterCertsRunCancelsAtTheNextStepBoundaryAndASecondSignalKills$", "-test.v")
			cmd.Env = append(os.Environ(), "STRATUM_CERTS_BOUNDARY_CHILD=1")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			lines := make(chan string, 16)
			go func() {
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
			waitFor("trust-new done")
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			// The first signal must cancel the run, not kill it: the run
			// proves it reached its boundary and refused sign-new by printing
			// the interrupted-before halt.
			deadline := time.After(10 * time.Second)
		drain:
			for {
				select {
				case l, ok := <-lines:
					if !ok {
						t.Fatalf("the child's output closed after one %s without the boundary halt: the first signal must cancel the run, not kill it", tc.name)
					}
					if strings.HasPrefix(l, "halted before sign-new:") {
						break drain
					}
					if l == "sign-new started" {
						t.Fatalf("the run started sign-new after a %s instead of halting at the boundary", tc.name)
					}
				case <-deadline:
					t.Fatalf("the run neither halted nor started sign-new within 10s of the %s", tc.name)
				}
			}
			select {
			case err := <-waited:
				t.Fatalf("child exited after one %s (%v); the first signal must cancel the run at the boundary, not kill it", tc.name, err)
			default:
			}
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("child err = %v, want it killed by the second %s", err, tc.name)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("a second %s did not kill the child: the default handler was not restored", tc.name)
			}
			// Nothing the child printed after the halt may start the next
			// step either: read to the child's end.
			for l := range lines {
				if l == "sign-new started" {
					t.Fatal("the run started sign-new after the boundary halt")
				}
			}
		})
	}
}
