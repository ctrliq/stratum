// Package interrupt owns the one signal set every stratum command stops under.
package interrupt

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Err stays context.Canceled; the cause carries the signal provenance.
type stopError struct{}

func (stopError) Error() string { return "stop requested by signal" }

func (stopError) Unwrap() error { return context.Canceled }

func CleanStop(err error) bool {
	var stop stopError
	return errors.As(err, &stop)
}

// Child contexts report plain context.Canceled; the owning context supplies provenance.
func ExpectedStop(ctx context.Context, err error) bool {
	return CleanStop(context.Cause(ctx)) && CancellationOnly(err)
}

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

// Signals are restored before cancellation so a second signal kills; SIGHUP guards nohup runs.
func Context() (context.Context, context.CancelFunc) {
	return signalContextFrom(context.Background())
}

func signalContextFrom(parent context.Context) (context.Context, context.CancelFunc) {
	inner, cancel := context.WithCancelCause(parent)
	// An inherited SIG_IGN must still force termination on a second signal.
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
		signal.Stop(sigs)
		cancel(stopError{})
		if len(ignoredSet) == 0 {
			return
		}
		select {
		case <-ignored:
			// Re-sending an ignored signal does nothing; SIGKILL guarantees force-stop.
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		case <-stopped:
		}
	}()
	// Stand the handler down here so a caller installing its own cannot race ours.
	return inner, func() {
		stopOnce.Do(func() { close(stopped) })
		cancel(context.Canceled)
		signal.Stop(sigs)
		signal.Stop(ignored)
	}
}

// Derives the shared signal context from parent, so host cancellation stops the command.
func DerivedContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signalContextFrom(parent)
}
