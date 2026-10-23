package etcddata

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAcquireStartLock pins start serialization: while one node start holds
// the lock, a second cannot take it and fails fast with a clear message; once the
// first releases, a fresh start can take it again.
func TestAcquireStartLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.lock")

	rel, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire while the first is held must fail fast — this is the race
	// the lock exists to close (two `stratum up` both passing the read-only
	// preflight and both reaching supervisor.Run).
	if _, err := AcquireStartLock(path); err == nil {
		t.Fatal("second acquire succeeded while the lock was held — concurrent starts are not serialized")
	} else if !strings.Contains(err.Error(), "already starting or running") {
		t.Errorf("second acquire error = %v, want the concurrent-start message", err)
	}

	// After release, a fresh acquire succeeds (the lock is not leaked).
	rel()
	rel2, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	rel2()
}

// TestAcquireStartLockCreatesParentDir: the /run/stratum dir may not exist yet on
// a fresh node, so the lock acquire must create it rather than error.
func TestAcquireStartLockCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "run", "start.lock")
	rel, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("acquire with a missing parent dir: %v", err)
	}
	rel()
}

func TestAcquireStartLockCheckedReleasesOnPostLockFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.lock")
	wantErr := errors.New("post-lock preflight failed")

	if _, err := AcquireStartLockChecked(path, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("AcquireStartLockChecked error = %v, want %v", err, wantErr)
	}

	// The failed post-lock preflight must not leak the lock; a later clean start
	// needs to acquire it immediately.
	release, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("lock leaked after failed post-lock preflight: %v", err)
	}
	release()
}

// TestAcquireStartLockWaitsOutABriefHolder pins the start lock's brief retry:
// StartLockHeld probes by taking the lock for an instant, so a start that lands
// in that instant must wait it out instead of reporting a concurrent start that
// does not exist.
func TestAcquireStartLockWaitsOutABriefHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	unlocked := make(chan struct{})
	go func() {
		defer close(unlocked)
		time.Sleep(30 * time.Millisecond)
		_ = syscall.Flock(fd, syscall.LOCK_UN)
	}()

	rel, err := AcquireStartLock(path)
	<-unlocked
	if err != nil {
		t.Fatalf("acquire behind a brief holder: %v", err)
	}
	rel()
}

// TestStartLockProbeNeverRefusesAStart runs the real probe in a tight loop
// against starts that acquire and release the lock; no start may be refused.
func TestStartLockProbeNeverRefusesAStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.lock")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := startLockHeldAt(path); err != nil {
				t.Errorf("probe: %v", err)
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()

	for i := 0; i < 2000; i++ {
		rel, err := AcquireStartLock(path)
		if err != nil {
			t.Fatalf("start %d refused while only the probe touched the lock: %v", i, err)
		}
		rel()
	}
}

// TestAcquireStartLockRefusesAWholeRunHolder: the retry only covers a probe's
// instant; a start that keeps the lock is still refused once the window ends.
func TestAcquireStartLockRefusesAWholeRunHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.lock")
	rel, err := AcquireStartLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	begin := time.Now()
	if _, err := AcquireStartLock(path); err == nil {
		t.Fatal("second acquire succeeded while the lock was held for the whole run")
	} else if !strings.Contains(err.Error(), "already starting or running") {
		t.Errorf("error = %v, want the concurrent-start message", err)
	}
	if waited := time.Since(begin); waited < startLockRetryWindow {
		t.Errorf("refused after %v, before the %v retry window ended", waited, startLockRetryWindow)
	}
}
