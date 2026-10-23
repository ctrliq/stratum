package upgrade

// The acceptance suite for released-identity exclusivity and the
// rollback nesting, on the upgrade side. Two identities are released
// contracts — version.NodeUpgradeLockPath (/run/stratum-upgrade.lock) and the
// cluster Lease kube-system/stratum-cluster-upgrade — because an orchestrator
// and a lockNode on the PREVIOUS release meet this build on exactly those. A
// rename, a second lock kind, or a lease that is not live would let an old
// and a new run work the same cluster or host at once, so each pin here holds
// the identity from one side and refuses from the other:
//
//   - a released-style holder of the released lease (a live lease object, no
//     shared maintenance lease — the released binary creates none) refuses the
//     new orchestrator before it creates anything;
//   - the new run holds the released lease LIVE — holder set, renewed through
//     a readiness window — so a released-style acquire would refuse it;
//   - the new node lock still arbitrates by flock: a released-style raw flock
//     taker cannot take the node while the new code holds it (the reverse
//     direction, a released holder refusing the new entries, is
//     nodelock_test.go's and the certs suite's);
//   - a rollback holds the node lock through its nested etcd restore and
//     passes the held-lock capability, so the nested Restore holds rather
//     than reacquires.
//
// Patterns are the package's own: the fake clientset, the fake clock over
// leaseNow, and the orchestrator seams of duallease_test.go
// (fastOrchestrator, runUpgrade, liveHeldLease) and resume_test.go
// (etcdRollbackFixture, captureStdout).

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

// TestUpgradeClusterRefusesAReleasedStyleHolderOfTheReleasedLease is the
// mixed-release cluster pin, refusing direction: an orchestrator on the
// previous release holds kube-system/stratum-cluster-upgrade and knows no
// shared lease. The new UpgradeCluster refuses it, names the old holder, and
// gets no further than the released lease: the shared maintenance lease is
// never created, no per-node Job is dispatched, and the old holder's lease
// survives the refused run.
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

// TestUpgradeClusterKeepsTheReleasedLeaseLiveThroughReadiness is the
// mixed-release pin, holding direction, with the renewal case: the clock
// jumps 45 s while node 1 waits for readiness — an API server restarting and
// rejoining, long enough to cross two renewal ticks — and the run completes
// both nodes with BOTH leases renewed to the jumped clock, the released lease
// among them. The released check is spelled the released binary's acquire
// reads it: a holder other than ours, with a renew time whose 60 s window is
// still open, is refused — so a released orchestrator meeting this run
// mid-rollout refuses instead of upgrading beside it.
func TestUpgradeClusterKeepsTheReleasedLeaseLiveThroughReadiness(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	now := fakeClock(t, t0)
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	var released, shared time.Time
	jobs := fastOrchestrator(t, func(n int) {
		if n == 1 {
			*now = now.Add(45 * time.Second) // the readiness wait spans an API restart
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

// TestNewNodeLockStillArbitratesByFlock pins the mixed-release host pin,
// holding direction: the new lockNode records "op pid" in the lock file, but
// the lock itself must still be the released flock. A released binary takes a
// raw syscall.Flock on a fresh descriptor; while the new code holds the lock,
// that raw taker must be refused. A rewrite to a pid file, a different lock
// kind, or a moved path would pass every record-format test and still let an
// old and a new binary run one host at once.
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

// TestRollbackHoldsTheNodeLockThroughTheNestedRestore is the rollback ->
// Restore nesting acceptance case: RollbackNode holds the node maintenance
// lock for the whole rollback and its nested etcd restore runs under it, so
// the restore must be told the caller holds the lock (RestoreOptions.
// NodeLockHeld) instead of reacquiring — which would refuse against the
// rollback's own holder and fail every rollback with a snapshot. The probe
// that stands in for the restore checks both halves: the capability is set,
// and at that moment the lock really is held by the rollback operation. The
// etcd data lock stays the restore's own: it is a different lock, and it is
// takeable under the held node lock.
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
		// The data lock is the nested restore's own acquire, under the held
		// node lock: the capability lifts only the maintenance lock.
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
