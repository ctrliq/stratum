// Package etcddata coordinates Stratum operations that create, replace, or
// restore the local etcd data directory.
package etcddata

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ctrliq/stratum/internal/version"
)

// lockPath lives outside the data directory. The production lock also lives
// outside version.DataDir, which uninstall removes in its entirety.
func lockPath(dir string) string {
	if dir == version.EtcdDataDir {
		return version.DataDir + "-etcd.lock"
	}
	return dir + ".lock"
}

// Lock excludes other Stratum data-dir writers until release is called.
func Lock(dir string) (release func(), err error) {
	path := lockPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("etcd data lock: create parent: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("etcd data lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("etcd data lock: another operation is changing %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// StartLockHeld reports whether a `stratum up` holds the node-start lock.
// `stratum up` holds it for its whole run, so on its own this means "a node is
// starting or running"; RefuseStartInProgress pairs it with the supervisor
// probe to tell the two apart.
//
// The probe takes the lock itself for an instant; AcquireStartLock retries
// briefly so a start that lands in that instant is not refused.
func StartLockHeld() (bool, error) { return startLockHeldAt(version.StartLockPath) }

func startLockHeldAt(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("inspect node-start lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			return true, nil
		}
		return false, fmt.Errorf("inspect node-start lock: %w", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// RefuseStartInProgress refuses a data-replacing operation while a node start is
// between preparation and a visible supervisor.
//
// `stratum up` holds this package's data lock only while it prepares node state
// (PrepareController), then releases it before RunController renders manifests
// and brings the supervisor up. In that gap the data lock is free, so an
// operation that took only the data lock could replace the etcd data dir under a
// start that is about to run with state prepared against the old one. The start
// lock closes the gap: it is held from before preparation until the process
// exits, so "start lock held, no supervisor visible" is exactly that window.
// Once the supervisor is visible the start is complete and the operation is
// acting on a running node, which is what it was written for.
//
// Call it with the data lock already held. A start that begins after this check
// then fails on the data lock, so there is no window after the check either.
func RefuseStartInProgress(op, doing string, supervisorRunning func() bool, startLockHeld func() (bool, error)) error {
	if supervisorRunning() {
		return nil
	}
	starting, err := startLockHeld()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if starting {
		return fmt.Errorf("%s: a stratum node is starting; wait for it to finish starting or to stop before %s", op, doing)
	}
	return nil
}
