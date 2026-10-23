// Package interrupt owns the one signal set every stratum command stops under,
// so no two handlers can drift apart: SIGINT, SIGTERM and SIGHUP.
//
// Context installs the handler and returns a context the first of those
// signals cancels, arranging force-stop handling before the cancellation
// becomes visible so a second signal kills the process. The cancellation cause
// carries this package's own error, which CleanStop recognizes,
// so a caller can map a signal-asked stop to a clean stop (exit 0) while every
// other failure keeps its own meaning and exit code.
package interrupt

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// stopError is the cancellation cause Context's signal handler records.
// Err remains context.Canceled; the distinct cause lets ExpectedStop recognize
// signal shutdown without hiding parent cancellation or unrelated failures.
type stopError struct{}

func (stopError) Error() string { return "stop requested by signal" }

func (stopError) Unwrap() error { return context.Canceled }

// CleanStop reports whether err is, or wraps, this package's signal cause.
// Callers inspect context.Cause(ctx); context.Err() stays a standard sentinel.
// Nothing else qualifies: not a clean nil return, not another caller's
// context.Canceled (plain or wrapped), not a deadline, not a real failure. A
// run that stopped for any other reason keeps its own meaning and exit code.
func CleanStop(err error) bool {
	var stop stopError
	return errors.As(err, &stop)
}

// ExpectedStop recognizes only cancellation caused by this operation's signal
// context. Child contexts report plain context.Canceled even when their cause
// is our stop error, so the owning context supplies that provenance. Every
// error leaf must be cancellation: joining a real failure with cancellation
// must keep the failure visible and its exit status nonzero.
func ExpectedStop(ctx context.Context, err error) bool {
	return CleanStop(context.Cause(ctx)) && CancellationOnly(err)
}

// CancellationOnly reports whether every leaf of err is context.Canceled.
// It also accepts wrapped and joined cancellations, but never a nil error,
// a deadline or a failure joined with cancellation. Background task owners
// use it when their run context ends, independently of the signal exit policy.
func CancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		seen := false
		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			seen = true
			if !CancellationOnly(child) {
				return false
			}
		}
		return seen
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return CancellationOnly(wrapped.Unwrap())
	}
	return err == context.Canceled
}

// Context returns a context cancelled by the first SIGINT, SIGTERM or SIGHUP,
// and a stop function that cancels it and stands the handler down.
//
// Normal signal actions are restored BEFORE the context is cancelled;
// inherited ignored signals retain a watcher that uses SIGKILL on the next
// signal. A second signal therefore forces termination once cancellation is
// visible. A bare signal.NotifyContext intercepts every signal until stop is
// called, and the phases a cluster run drives do not
// observe the context, so an operator could not otherwise interrupt a stalled
// one with a second signal; restoring the handler only after ctx.Done() fires
// would leave a window in which that second signal is still swallowed.
//
// The shared set includes SIGHUP so a nohup-detach cannot kill a `certs
// rotate-ca --cluster` or `certs migrate-etcd-ca --cluster` run mid-step,
// past the deferred cleanup of the transient Secret holding the key to
// every cluster private key. On this set the first SIGHUP cancels the run
// instead, so it halts at its next step boundary and the cleanup runs; the
// second SIGHUP kills, exactly like a second Ctrl-C.
func Context() (context.Context, context.CancelFunc) {
	return signalContextFrom(context.Background())
}

func signalContextFrom(parent context.Context) (context.Context, context.CancelFunc) {
	inner, cancel := context.WithCancelCause(parent)
	// Stop restores the inherited disposition, which may be SIG_IGN under
	// nohup or a background shell. Keep those signals watched separately so
	// they still force termination on a second signal during graceful stop.
	sigs := make(chan os.Signal, 1)
	ignored := make(chan os.Signal, 1)
	var normalSet, ignoredSet []os.Signal
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if signal.Ignored(sig) {
			ignoredSet = append(ignoredSet, sig)
		} else {
			normalSet = append(normalSet, sig)
		}
	}
	if len(normalSet) > 0 {
		signal.Notify(sigs, normalSet...)
	}
	if len(ignoredSet) > 0 {
		signal.Notify(ignored, ignoredSet...)
	}
	stopped := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer signal.Stop(ignored)
		select {
		case <-sigs:
		case <-ignored:
		case <-inner.Done():
			signal.Stop(sigs)
			return
		}
		// Restore normal signal actions before exposing cancellation. The
		// first cause wins, preserving an earlier parent cancellation.
		signal.Stop(sigs)
		cancel(stopError{})
		if len(ignoredSet) == 0 {
			return
		}
		select {
		case <-ignored:
			// Re-sending an inherited ignored signal would do nothing, and
			// signal.Reset restores that same ignore. SIGKILL guarantees the
			// requested force-stop even while a phase ignores cancellation.
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		case <-stopped:
		}
	}()
	// The stop function stands the handler down itself rather than waiting for
	// the goroutine above, so the moment stop returns no signal of ours is
	// still intercepted — a caller that immediately installs its own handler
	// must not race ours.
	return inner, func() {
		stopOnce.Do(func() { close(stopped) })
		cancel(context.Canceled)
		signal.Stop(sigs)
		signal.Stop(ignored)
	}
}

// DerivedContext returns the shared interrupt context — SIGINT, SIGTERM and
// SIGHUP, where the first signal cancels the run and a second forces
// termination (Context above) — derived from parent:
// a host that cancels the context it passed to ExecuteContext stops the
// command instead of leaving it running. Err follows the standard context
// contract for every cancellation; Cause identifies signal shutdown and
// preserves the parent's cause, so each keeps its own meaning and exit code.
//
// The commands disagree, deliberately, on what a host's plain cancellation
// means for their exit code, and ExpectedStop is what tells the two classes
// apart: the node commands (`stratum up`, `stratum operator`) surface it — a
// host's cancellation is not the signal-asked stop that exits 0 — while the
// long-running servers (`stratum mcp`, `stratum dash`) treat it as their
// expected shutdown, because the host cancelling the context is the normal
// way those sessions end. Every command still exits 0 on the signal's own
// stop and 1 on every unrelated failure.
//
// The context is a child of parent, preserving its values, deadline and
// cancellation cause. An already cancelled parent is reflected before this
// function returns, so callers cannot start work in a bridge-goroutine race.
func DerivedContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signalContextFrom(parent)
}
