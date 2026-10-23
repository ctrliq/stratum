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

// redirectNodeLock points the node maintenance lock at a fresh file, so these
// tests never touch the production lock at /run/stratum-upgrade.lock.
func redirectNodeLock(t *testing.T) {
	t.Helper()
	swap(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "node.lock"))
}

// holdNodeLock holds the node maintenance lock for op until the test ends, the
// way another maintenance operation — an upgrade, a rollback, a CA rotation —
// holds it while a restore runs beside it.
func holdNodeLock(t *testing.T, op string) {
	t.Helper()
	release, err := nodelock.Acquire(op)
	if err != nil {
		t.Fatalf("hold the node maintenance lock as %q: %v", op, err)
	}
	t.Cleanup(release)
}

// nodeLockHeld reports whether the node maintenance lock is held right now: a
// probe Acquire refuses exactly while another holder has it.
func nodeLockHeld() bool {
	release, err := nodelock.Acquire("fence probe")
	if err != nil {
		return true
	}
	release()
	return false
}

// TestRestoreRefusesWhileAMaintenanceOperationHoldsTheNodeLock is the fence:
// a restore stops the control plane and rewrites the etcd data dir, so while
// any other maintenance operation — an upgrade, a rollback, a CA rotation —
// holds the node maintenance lock, Restore refuses, names the holder, and
// touches nothing.
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

// TestRestoreTakesTheNodeLockBeforeTheDataLock pins the order: the node
// maintenance lock first, then the etcd data lock. The data lock alone fences
// only other data-dir writers, and an upgrade or CA rotation takes no data
// lock, so a restore that took it first would run beside them. The lock is
// released again when the restore ends; the restore succeeds here.
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

// TestRestoreReleasesTheNodeLockWhenTheRestoreFails: a restore that fails and
// rolls back must still hand the node on unlocked, or the next maintenance
// operation refuses for nothing.
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

// TestRestoreSkipsTheNodeLockForARollbackThatHoldsIt: upgrade.RollbackNode
// takes the node maintenance lock for the whole rollback and then restores etcd
// through Restore. That nested call must run because the caller already holds
// the lock, not refuse against it. The held-lock capability lifts only the
// maintenance-lock acquire, never the data lock.
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

// TestRestoreLocalSeedRefusesWhileAMaintenanceOperationHoldsTheNodeLock:
// `stratum snapshot restore` is the operator-facing entry point, and it reaches
// the fence through RestoreLocalSeed, not only through Restore.
func TestRestoreLocalSeedRefusesWhileAMaintenanceOperationHoldsTheNodeLock(t *testing.T) {
	n := newFakeNode(t)
	// RestoreLocalSeed asks nodestate for this node's address; with no state
	// file it falls back to $NODE_IP.
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
