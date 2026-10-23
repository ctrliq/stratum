package maintlease

// Tests for the lease guard: how a run that holds the maintenance lease
// notices that it may no longer hold it. Two distinct states: lost — someone
// else holds the lease, permanent; stale — renewals have been failing for one
// renewal interval before expiry, cleared by one renewal that succeeds, which
// is a compare-and-swap on a lease still naming this run and so proves nobody
// held it in between. That is what lets a run ride out its own API server
// restarting without aborting an in-flight node.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// TestGuardStaleRenewalsTripBeforeExpiryAndClear: one missed renewal holds;
// no renewal for one renewal interval before expiry trips the guard; one
// successful same-holder renewal clears it.
func TestGuardStaleRenewalsTripBeforeExpiryAndClear(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	down := errors.New("dial tcp 127.0.0.1:6443: connection refused")

	g := &Guard{lastRenew: t0}
	g.Observe(nil, t0.Add(RenewEvery))
	g.Observe(down, t0.Add(2*RenewEvery))
	if g.Err() != nil {
		t.Fatalf("one missed renewal tripped the guard: %v", g.Err())
	}
	g.Observe(down, t0.Add(Duration)) // no renewal for one interval before expiry
	err := g.Err()
	if err == nil || !strings.Contains(err.Error(), "may be taken by another run") {
		t.Fatalf("no renewal for %s: %v, want the guard stale", Duration-RenewEvery, err)
	}
	if !errors.Is(err, down) {
		t.Errorf("the stale error %v does not carry the renewal failure %v", err, down)
	}
	// The API server came back and the lease still named this run: that
	// renewal is a compare-and-swap, so nobody held it meanwhile.
	g.Observe(nil, t0.Add(10*Duration))
	if err := g.Err(); err != nil {
		t.Fatalf("a renewal that succeeded after the outage left the guard tripped: %v", err)
	}
}

// TestGuardTakeoverIsPermanent: a lease another run holds loses the guard at
// once, and no later successful renewal brings it back.
func TestGuardTakeoverIsPermanent(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	g := &Guard{lastRenew: t0}
	g.Observe(fmt.Errorf("%w (m2/200/2)", ErrTaken), t0.Add(RenewEvery))
	if err := g.Err(); !errors.Is(err, ErrTaken) {
		t.Fatalf("a lease held by another run = %v, want the guard lost at once", err)
	}
	g.Observe(nil, t0.Add(2*RenewEvery))
	if err := g.Err(); !errors.Is(err, ErrTaken) {
		t.Fatalf("a lost lease came back: %v", err)
	}
}

// TestGuardConfirmProvesNobodyHeldTheLease: Err alone cannot say nobody held
// the lease since the last renewal — after a long step, such as an API
// outage, a successor may already hold it before the next tick sees it.
// Confirm renews now: a same-holder renewal clears stale, a takeover is
// surfaced at once, and an ordinary failure comes back for the caller to
// retry.
func TestGuardConfirmProvesNobodyHeldTheLease(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	stale := &Guard{lastRenew: t0.Add(-Duration)}
	stale.Observe(errors.New("dial tcp 127.0.0.1:6443: connection refused"), t0)
	stale.renew = func(context.Context) error { return nil }
	if err := stale.Confirm(ctx); err != nil {
		t.Fatalf("confirm after the API server came back = %v, want held", err)
	}

	taken := &Guard{lastRenew: t0, renew: func(context.Context) error {
		return fmt.Errorf("%w (m2/200/2)", ErrTaken)
	}}
	if err := taken.Confirm(ctx); !errors.Is(err, ErrTaken) {
		t.Fatalf("confirm on a taken lease = %v, want the takeover at once", err)
	}

	down := &Guard{lastRenew: t0, renew: func(context.Context) error {
		return errors.New("dial tcp 127.0.0.1:6443: connection refused")
	}}
	err := down.Confirm(ctx)
	if !strings.Contains(err.Error(), "connection refused") || errors.Is(err, ErrTaken) {
		t.Fatalf("confirm during an outage = %v, want the renewal error left to the caller to retry", err)
	}
}

// TestGuardConfirmReturnsTerminalAPIErrorsAsTheyCame: a renewal failing for
// good — Forbidden or NotFound on the lease — is returned as it came,
// whatever state the guard is in, so the caller's retry loop stops at once
// instead of running out its window.
func TestGuardConfirmReturnsTerminalAPIErrorsAsTheyCame(t *testing.T) {
	ctx := context.Background()
	res := coordinationv1.SchemeGroupVersion.WithResource("leases").GroupResource()
	for name, renewErr := range map[string]error{
		"forbidden": apierrors.NewForbidden(res, testName, errors.New("denied")),
		"not found": apierrors.NewNotFound(res, testName),
	} {
		t.Run(name, func(t *testing.T) {
			g := &Guard{lastRenew: time.Now().Add(-Duration), renew: func(context.Context) error { return renewErr }}
			if err := g.Confirm(ctx); err != renewErr {
				t.Errorf("err = %v, want the renewal error as it came so the retry loop stops", err)
			}
		})
	}
}

// TestNilGuardReadsAsHeld: a nil guard (dry-run, tests) is held, and answers
// the same through confirm.
func TestNilGuardReadsAsHeld(t *testing.T) {
	var g *Guard
	if g.Err() != nil || g.ID() != "" {
		t.Errorf("a nil guard = %v %q, want held with no identity", g.Err(), g.ID())
	}
	if err := g.Confirm(context.Background()); err != nil {
		t.Errorf("a nil guard's confirm = %v, want held", err)
	}
}

func TestLostGuardDoesNotAttemptAnotherRenewal(t *testing.T) {
	calls := 0
	g := &Guard{lastRenew: time.Now(), renew: func(context.Context) error { calls++; return nil }}
	g.Observe(ErrTaken, time.Now())
	if err := g.Confirm(context.Background()); !errors.Is(err, ErrTaken) {
		t.Fatalf("lost guard: %v", err)
	}
	if calls != 0 {
		t.Fatalf("lost guard attempted %d renewals", calls)
	}
}

func TestConfirmWithinRequiresRecoveryAndBoundsOutages(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				down := errors.New("API unavailable")
				g := &Guard{lastRenew: time.Now(), renew: func(context.Context) error {
					calls++
					if recover && calls == 2 {
						return nil
					}
					return down
				}}
				started := time.Now()
				err := g.ConfirmWithin(context.Background(), 3*time.Second)
				if recover {
					if err != nil || calls != 2 {
						t.Fatalf("recovery: calls=%d err=%v", calls, err)
					}
				} else {
					if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, down) {
						t.Fatalf("outage must stop: %v", err)
					}
					if time.Since(started) != 3*time.Second {
						t.Fatalf("unbounded outage: %s", time.Since(started))
					}
				}
			})
		})
	}
}

func TestConfirmBoundsAnUnresponsiveAPI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := &Guard{lastRenew: time.Now(), renew: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
		started := time.Now()
		if err := g.Confirm(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled call: %v", err)
		}
		if time.Since(started) != CallTimeout {
			t.Fatalf("call duration=%s", time.Since(started))
		}
	})
}
