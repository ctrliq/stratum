package ha

import (
	"io"

	"context"
	"errors"
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
	old := version.NodeUpgradeLockPath
	version.NodeUpgradeLockPath = filepath.Join(t.TempDir(), "node.lock")
	t.Cleanup(func() { version.NodeUpgradeLockPath = old })
}

// holdNodeLock holds the node maintenance lock for op until the test ends, the
// way another maintenance operation — an upgrade, a rollback, a CA rotation —
// holds it while a rejoin runs beside it.
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

// TestRejoinRefusesWhileAMaintenanceOperationHoldsTheNodeLock is the fence:
// a rejoin stops this node's control plane and wipes its etcd data dir, so
// while any other maintenance operation — an upgrade, a rollback, a CA
// rotation — holds the node maintenance lock, RejoinEtcd refuses, names the
// holder, and touches nothing.
func TestRejoinRefusesWhileAMaintenanceOperationHoldsTheNodeLock(t *testing.T) {
	touched := []string{}
	restoreSeams(t,
		func() error { touched = append(touched, "pki"); return nil },
		func(string) error { touched = append(touched, "seed"); return nil },
		func(w io.Writer) error { touched = append(touched, "wipe"); return errors.New("stop after the wipe") })
	redirectNodeLock(t)
	holdNodeLock(t, "upgrade")
	attempts := 0
	rejoinAcquireDataLock = func() (func(), error) {
		attempts++
		return func() {}, nil
	}

	err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false)
	if err == nil || !strings.Contains(err.Error(), "already running on this node") ||
		!strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("rejoin while `upgrade` holds the node maintenance lock = %v; want the operation-naming refusal", err)
	}
	if attempts != 0 {
		t.Error("the etcd data lock was attempted before the maintenance refusal")
	}
	if len(touched) != 0 {
		t.Errorf("work ran before the maintenance refusal: %v", touched)
	}
}

// TestRejoinTakesTheNodeLockBeforeTheDataLock pins the order: the node
// maintenance lock first, then the etcd data lock. The data lock alone fences
// only other data-dir writers, and an upgrade or CA rotation takes no data
// lock, so a rejoin that took it first would run beside them. The fixture
// fails at the wipe, so this is also the error exit: the lock is released
// again when the rejoin ends.
func TestRejoinTakesTheNodeLockBeforeTheDataLock(t *testing.T) {
	restoreSeams(t, func() error { return nil }, func(string) error { return nil },
		func(w io.Writer) error { return errors.New("stop after the wipe") })
	redirectNodeLock(t)
	nodeHeldAtDataLock := false
	rejoinAcquireDataLock = func() (func(), error) {
		nodeHeldAtDataLock = nodeLockHeld()
		return func() {}, nil
	}

	err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false)
	if err == nil {
		t.Fatal("rejoin succeeded with every later step stubbed to fail")
	}
	if !nodeHeldAtDataLock {
		t.Error("the node maintenance lock was not held when the data lock was taken: rejoin takes the locks in the wrong order, or not at all")
	}
	if nodeLockHeld() {
		t.Error("the node maintenance lock was still held after the failed rejoin returned")
	}
}

// TestRejoinReleasesTheNodeLockWhenTheRejoinSucceeds: a rejoin that finishes —
// learner added, member promoted, apiserver started — must hand the node on
// unlocked, or the next maintenance operation refuses for nothing.
func TestRejoinReleasesTheNodeLockWhenTheRejoinSucceeds(t *testing.T) {
	f := newFakeRejoin(t, seedWith(), controller())
	f.local = servingAfter(0, addedID, true)
	redirectNodeLock(t)
	nodeHeldAtWipe := false
	wipe := rejoinWipeLocalState
	rejoinWipeLocalState = func(w io.Writer) error {
		nodeHeldAtWipe = nodeLockHeld()
		return wipe(w)
	}

	if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
		t.Fatal(err)
	}
	if !nodeHeldAtWipe {
		t.Error("the node maintenance lock was not held at the wipe")
	}
	if nodeLockHeld() {
		t.Error("the node maintenance lock was still held after the rejoin finished")
	}
}
