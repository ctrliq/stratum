package etcdsnap

import (
	"io"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/version"
)

// redirectNodeLock points the node maintenance lock at a fresh file.
func redirectNodeLock(t *testing.T) {
	t.Helper()
	swap(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "node.lock"))
}

// holdNodeLock holds the node maintenance lock for op until the test ends.
func holdNodeLock(t *testing.T, op string) {
	t.Helper()
	release, err := nodelock.Acquire(op)
	if err != nil {
		t.Fatalf("hold the node maintenance lock as %q: %v", op, err)
	}
	t.Cleanup(release)
}

// nodeLockHeld reports whether the node maintenance lock is held right now.
func nodeLockHeld() bool {
	release, err := nodelock.Acquire("fence probe")
	if err != nil {
		return true
	}
	release()
	return false
}

func TestRestoreRefusesWhileAMaintenanceOperationHoldsTheNodeLock(t *testing.T) {
	n := newFakeNode(t)
	redirectNodeLock(t)
	n.fx.restoreAcquireNodeLock = func() (func(), error) { return nodelock.Acquire("etcd restore") }
	holdNodeLock(t, "upgrade")
	attempts := 0
	n.fx.restoreAcquireDataLock = func() (func(), error) {
		attempts++
		return func() {}, nil
	}

	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "already running on this node") ||
		!strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("restore while `upgrade` holds the node maintenance lock = %v; want the operation-naming refusal", err)
	}
	if attempts != 0 {
		t.Error("the etcd data lock was attempted before the maintenance refusal")
	}
	if len(n.events) != 0 {
		t.Errorf("events = %q; something ran before the maintenance refusal", n.events)
	}
}

func TestRestoreTakesTheNodeLockBeforeTheDataLock(t *testing.T) {
	n := newFakeNode(t)
	redirectNodeLock(t)
	n.fx.restoreAcquireNodeLock = func() (func(), error) { return nodelock.Acquire("etcd restore") }
	nodeHeldAtDataLock := false
	n.fx.restoreAcquireDataLock = func() (func(), error) {
		nodeHeldAtDataLock = nodeLockHeld()
		return func() {}, nil
	}

	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	if !nodeHeldAtDataLock {
		t.Error("the node maintenance lock was not held when the data lock was taken: restore takes the locks in the wrong order, or not at all")
	}
	if nodeLockHeld() {
		t.Error("the node maintenance lock was still held after Restore returned")
	}
}

func TestRestoreReleasesTheNodeLockWhenTheRestoreFails(t *testing.T) {
	n := newFakeNode(t)
	redirectNodeLock(t)
	n.fx.restoreAcquireNodeLock = func() (func(), error) { return nodelock.Acquire("etcd restore") }
	nodeHeldAtRun := false
	fake := n.fx.restoreRun
	n.fx.restoreRun = func(cmd *exec.Cmd) error {
		nodeHeldAtRun = nodeLockHeld()
		return fake(cmd)
	}
	n.fail("run", 0)

	if err := n.restoreErr(); err == nil {
		t.Fatal("a failed restore reported success")
	}
	if !nodeHeldAtRun {
		t.Error("the node maintenance lock was not held while etcdutl ran")
	}
	if nodeLockHeld() {
		t.Error("the node maintenance lock was still held after the failed restore returned")
	}
}

func TestRestoreSkipsTheNodeLockForARollbackThatHoldsIt(t *testing.T) {
	n := newFakeNode(t)
	redirectNodeLock(t)
	n.fx.restoreAcquireNodeLock = func() (func(), error) { return nodelock.Acquire("etcd restore") }
	holdNodeLock(t, "rollback")
	attempts := 0
	n.fx.restoreAcquireDataLock = func() (func(), error) {
		attempts++
		return func() {}, nil
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runRestore(io.Discard, RestoreOptions{
		Snapshot:       snap,
		PeerURL:        testPeer,
		InitialCluster: testLedger,
		Stamp:          testStamp,
		NodeLockHeld:   true,
	}, n.fx)
	if err != nil {
		t.Fatalf("a restore nested in a rollback that holds the node maintenance lock refused: %v", err)
	}
	if !n.called("run") {
		t.Error("the nested restore did not run etcdutl")
	}
	if attempts != 1 {
		t.Errorf("the etcd data lock was attempted %d times, want 1: the capability must not lift the data lock", attempts)
	}
}

func TestRestoreLocalSeedRefusesWhileAMaintenanceOperationHoldsTheNodeLock(t *testing.T) {
	n := newFakeNode(t)
	// RestoreLocalSeed falls back to $NODE_IP with no state file.
	swap(t, &version.K8sConfDir, t.TempDir())
	t.Setenv("NODE_IP", "10.0.0.11")
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	redirectNodeLock(t)
	n.fx.restoreAcquireNodeLock = func() (func(), error) { return nodelock.Acquire("etcd restore") }
	holdNodeLock(t, "upgrade")

	err := RestoreLocalSeed(io.Discard, snap)
	if err == nil || !strings.Contains(err.Error(), "already running on this node") ||
		!strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("snapshot restore while `upgrade` holds the node maintenance lock = %v; want the operation-naming refusal", err)
	}
	if len(n.events) != 0 {
		t.Errorf("events = %q; something was stopped before the maintenance refusal", n.events)
	}
}
