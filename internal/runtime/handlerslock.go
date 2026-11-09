package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// handlersLockPoll is how often LockHandlers retries a held lock.
var handlersLockPoll = 100 * time.Millisecond

// handlersLockNotice is how long LockHandlers waits before saying it waits.
const handlersLockNotice = 2 * time.Second

// LockHandlers takes the runtime-handlers lock, waiting for its holder until
// ctx ends; a flock, so it is dropped with a holder that dies.
func LockHandlers(ctx context.Context, w io.Writer) (release func(), err error) {
	return LockHandlersAt(ctx, w, version.RuntimeHandlersLockPath)
}

// LockHandlersAt is LockHandlers with the lock file at path.
func LockHandlersAt(ctx context.Context, w io.Writer, path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the runtime-handlers lock: %w", err)
	}
	start := time.Now()
	noticed := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("take the runtime-handlers lock: %w", err)
		}
		if !noticed && time.Since(start) >= handlersLockNotice {
			fmt.Fprintf(w, "stratum: waiting for another change to this node's runtime handlers to finish (%s)\n", path)
			noticed = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("take the runtime-handlers lock: %w", ctx.Err())
		case <-time.After(handlersLockPoll):
		}
	}
}
