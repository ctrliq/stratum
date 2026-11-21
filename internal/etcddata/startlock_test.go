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

func TestAcquireStartLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.lock")

	rel, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := AcquireStartLock(path); err == nil {
		t.Fatal("second acquire succeeded while the lock was held — concurrent starts are not serialized")
	} else if !strings.Contains(err.Error(), "already starting or running") {
		t.Errorf("second acquire error = %v, want the concurrent-start message", err)
	}

	rel()
	rel2, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	rel2()
}

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

	release, err := AcquireStartLock(path)
	if err != nil {
		t.Fatalf("lock leaked after failed post-lock preflight: %v", err)
	}
	release()
}

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
