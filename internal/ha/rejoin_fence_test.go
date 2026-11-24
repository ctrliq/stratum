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

// these tests must never touch the production lock at /run/stratum-upgrade.lock
func redirectNodeLock(t *testing.T) {
	t.Helper()
	old := version.NodeUpgradeLockPath
	version.NodeUpgradeLockPath = filepath.Join(t.TempDir(), "node.lock")
	t.Cleanup(func() { version.NodeUpgradeLockPath = old })
}

func holdNodeLock(t *testing.T, op string) {
	t.Helper()
	release, err := nodelock.Acquire(op)
	if err != nil {
		t.Fatalf("hold the node maintenance lock as %q: %v", op, err)
	}
	t.Cleanup(release)
}

// Acquire refuses exactly while another holder has the lock
func nodeLockHeld() bool {
	release, err := nodelock.Acquire("fence probe")
	if err != nil {
		return true
	}
	release()
	return false
}

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
