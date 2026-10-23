package etcddata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// startLockRetryWindow bounds how long AcquireStartLock waits out a holder
// before it refuses; startLockRetryInterval is the pause between attempts.
var (
	startLockRetryWindow   = 500 * time.Millisecond
	startLockRetryInterval = 10 * time.Millisecond
)

// AcquireStartLock takes an exclusive flock held for the life of the
// process (released when the returned closer runs, or by the kernel when the
// process exits). It closes the concurrent-start race: preflight is
// read-only, so two `stratum up` invocations can both pass it, but only one can
// hold this lock — the other is refused rather than starting a second supervisor
// over the same host processes. flock is the right primitive here: only `stratum
// up` holds it for the full run; uninstall, etcd restore and the restore a
// rollback runs only probe it, through StartLockHeld and RefuseStartInProgress,
// before they delete or replace node data while no supervisor is visible. A
// crashed holder's lock is released by the kernel, so a genuine leftover from a
// dead start still falls through to the orphan preflight rather than being
// masked by a stale lock file.
//
// StartLockHeld probes by taking the lock for an instant, so a start that lands
// in that instant retries for startLockRetryWindow before it refuses. A probe
// releases within microseconds; a real start holds the lock for its whole run.
func AcquireStartLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s for the node-start lock: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open node-start lock %s: %w", path, err)
	}
	if err := flockRetrying(f); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another `stratum up` is already starting or running on this node (it holds %s). Only one node start may run at a time — stop the other invocation or wait for it to finish, then retry", path)
		}
		return nil, fmt.Errorf("acquire node-start lock %s: %w", path, err)
	}
	// Closing the fd releases the advisory lock; the process exiting does too.
	return func() { _ = f.Close() }, nil
}

// AcquireStartLockChecked makes the post-lock preflight authoritative: it acquires
// the whole-run start lock first, then re-runs the read-only node preflight while
// still holding that lock. If the second check fails, the lock is released before
// the error is returned so a later clean start is not blocked by a failed attempt.
func AcquireStartLockChecked(path string, check func() error) (func(), error) {
	release, err := AcquireStartLock(path)
	if err != nil {
		return nil, err
	}
	if err := check(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// flockRetrying takes an exclusive flock on f, retrying EWOULDBLOCK until
// startLockRetryWindow has passed. Any other error returns at once.
func flockRetrying(f *os.File) error {
	deadline := time.Now().Add(startLockRetryWindow)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(startLockRetryInterval)
	}
}
