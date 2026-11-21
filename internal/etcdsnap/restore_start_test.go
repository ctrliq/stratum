package etcdsnap

import (
	"io"

	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/version"
)

// stratum up drops the data lock before its supervisor is visible; restore refuses in that gap.
func TestRestoreRefusesANodeStartInProgress(t *testing.T) {
	swap(t, &version.ControlSocket, filepath.Join(t.TempDir(), "absent.sock"))
	swap(t, &version.DataDir, filepath.Join(t.TempDir(), "stratum"))
	swap(t, &version.EtcdDataDir, filepath.Join(version.DataDir, "etcd"))
	startLock := filepath.Join(version.DataDir, "start.lock")
	swap(t, &version.StartLockPath, startLock)
	if err := os.MkdirAll(version.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(startLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	err = Restore(io.Discard, RestoreOptions{Snapshot: "/nonexistent/snap.db", PeerURL: "https://10.0.0.11:2380", InitialCluster: "cp1=https://10.0.0.11:2380"})
	if err == nil || !strings.Contains(err.Error(), "a stratum node is starting") {
		t.Fatalf("Restore during a node start = %v", err)
	}
	// taking the data lock again proves the refusal released it.
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatalf("the data lock was not released on the refusal: %v", err)
	}
	release()
}
