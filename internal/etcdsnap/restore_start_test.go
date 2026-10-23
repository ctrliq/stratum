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

// TestRestoreRefusesANodeStartInProgress: `stratum up` drops
// the data lock after preparing node state and before its supervisor is
// visible. A restore in that gap would replace the data dir under a start
// about to run with the old prepared state, so Restore refuses there, before
// it stops a single component (the snapshot path does not even exist, so
// getting past the refusal would fail differently).
//
// Through the exported entry point with the production effects value: the
// refusal probes are the real ones — no supervisor serves the control socket
// (it points nowhere), and this test holds the node-start lock the way a
// starting node does. The locks live in temp dirs, so nothing on the host is
// touched.
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
	// Hold the node-start lock the way `stratum up` does for its whole run.
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	err = Restore(io.Discard, RestoreOptions{Snapshot: "/nonexistent/snap.db", PeerURL: "https://10.0.0.11:2380", InitialCluster: "cp1=https://10.0.0.11:2380"})
	if err == nil || !strings.Contains(err.Error(), "a stratum node is starting") {
		t.Fatalf("Restore during a node start = %v", err)
	}
	// The refusal came after the data lock was taken, so it must have been
	// released on the way out: taking it again succeeds.
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatalf("the data lock was not released on the refusal: %v", err)
	}
	release()
}
