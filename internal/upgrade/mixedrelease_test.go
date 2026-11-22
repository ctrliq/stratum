package upgrade

// Old-release binaries meet this build on the lease name and the node lock path; both are pinned.

import (
	"io"

	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/version"
)

// Refusing direction: the new run refuses a live released lease and creates nothing of its own.
func TestUpgradeClusterRefusesAReleasedStyleHolderOfTheReleasedLease(t *testing.T) {
	fakeClock(t, time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC))
	oldHolder := "old-orchestrator/9/9"
	cs := fake.NewSimpleClientset(append(upgradeNodesFixture(),
		liveHeldLease(upgradeLeaseName, oldHolder))...)
	fastOrchestrator(t, nil)

	err := runUpgrade(t, cs)
	if err == nil {
		t.Fatal("the rollout ran while a released-style holder held " + cuNamespace + "/" + upgradeLeaseName)
	}
	for _, want := range []string{upgradeLeaseName, oldHolder} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if _, err := cs.CoordinationV1().Leases(cuNamespace).Get(context.Background(), fenceTestMaintenanceLease, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the refused run created the shared maintenance lease; the released lease must refuse it first")
	}
	l, err := cs.CoordinationV1().Leases(cuNamespace).Get(context.Background(), upgradeLeaseName, metav1.GetOptions{})
	if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != oldHolder {
		t.Fatalf("the released holder's lease did not survive the refused run: %v, %v", l, err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			t.Errorf("a per-node Job was created although the released lease refused the run: %v", a)
		}
	}
}

// Both leases stay renewed through the readiness window, so a released-style acquire would refuse.
func TestUpgradeClusterKeepsTheReleasedLeaseLiveThroughReadiness(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	now := fakeClock(t, t0)
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	var released, shared time.Time
	jobs := fastOrchestrator(t, func(n int) {
		if n == 1 {
			*now = now.Add(45 * time.Second)
		}
		if n == 2 {
			for _, read := range []struct {
				name string
				into *time.Time
			}{
				{upgradeLeaseName, &released},
				{fenceTestMaintenanceLease, &shared},
			} {
				l, err := cs.CoordinationV1().Leases(cuNamespace).Get(context.Background(), read.name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("read %s at the second node's dispatch: %v", read.name, err)
					continue
				}
				if l.Spec.RenewTime != nil {
					*read.into = l.Spec.RenewTime.Time
				}
				if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
					t.Errorf("%s has no holder at the second node's dispatch", read.name)
				}
			}
			// What a released-style acquire checks: another holder, window open.
			expires := released.Add(leaseDuration)
			if !now.Before(expires) {
				t.Errorf("the released lease expired at %v; a released-style acquire would take it beside this run", released)
			}
		}
	})

	if err := runUpgrade(t, cs); err != nil {
		t.Fatalf("a bounded lease-renewal gap stopped the run: %v", err)
	}
	if *jobs != 2 {
		t.Fatalf("the run dispatched %d per-node Jobs; want both nodes upgraded", *jobs)
	}
	if !released.Equal(*now) || !shared.Equal(*now) {
		t.Errorf("renew times at the second node: released %v, shared %v; want %v — both leases must be renewed through the readiness window", released, shared, *now)
	}
}

// The lock itself must stay the released flock, not a record a released binary cannot contend on.
func TestNewNodeLockStillArbitratesByFlock(t *testing.T) {
	redirect(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	release, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// What a released binary's lockNode does: open the path, flock it.
	f, err := os.OpenFile(version.NodeUpgradeLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		t.Fatal("a released-style raw flock took the node lock while the run holds it — the lock does not arbitrate by flock")
	}
}

// Reacquiring would refuse against the rollback's own hold, so the capability is passed.
func TestRollbackHoldsTheNodeLockThroughTheNestedRestore(t *testing.T) {
	redirect(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	redirect(t, &version.EtcdDataDir, filepath.Join(t.TempDir(), "etcd"))
	var opts etcdsnap.RestoreOptions
	etcdRollbackFixture(t, func(w io.Writer, o etcdsnap.RestoreOptions) error {
		opts = o
		if release, err := nodelock.Acquire("etcd restore"); err == nil {
			release()
			t.Error("the nested restore could take the node maintenance lock — the rollback is not holding it, so the capability papers over a missing hold")
		} else if !strings.Contains(err.Error(), "rollback") {
			t.Errorf("the node lock at the nested restore is held by %v; want the rollback's own hold", err)
		}
		// The capability lifts only the maintenance lock, not the data lock.
		releaseData, err := etcddata.Lock(version.EtcdDataDir)
		if err != nil {
			t.Errorf("the etcd data lock was not takeable under the rollback's node lock: %v", err)
		} else {
			releaseData()
		}
		return nil
	})

	if err := RollbackNode(io.Discard, RollbackOptions{EtcdSnapshot: "/snap.db"}); err != nil {
		t.Fatalf("a rollback whose nested restore holds rather than reacquires failed: %v", err)
	}
	if !opts.NodeLockHeld {
		t.Error("the nested restore was not told the caller holds the node lock; it would refuse against the rollback's own holder")
	}
}
