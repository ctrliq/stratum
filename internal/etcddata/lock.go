// Package etcddata coordinates etcd data-directory create, replace and restore operations.
package etcddata

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ctrliq/stratum/internal/version"
)

// The production lock lives outside version.DataDir, which uninstall removes in its entirety.
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

// The probe takes the lock for an instant, so AcquireStartLock retries briefly around it.
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

// Call with the data lock held: it closes the gap between a start's preparation and its supervisor.
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
