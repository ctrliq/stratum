package etcddata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var (
	startLockRetryWindow   = 500 * time.Millisecond
	startLockRetryInterval = 10 * time.Millisecond
)

// Preflight is read-only, so two concurrent starts can both pass it; this lock refuses all but one.
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

// Re-runs the read-only preflight while holding the lock; releases it if the check fails.
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

// flockRetrying takes an exclusive flock, retrying EWOULDBLOCK until the retry window passes.
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
