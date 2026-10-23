package upgrade

import (
	"io"

	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodejob"
)

func fakeClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	now := at
	old := leaseNow
	leaseNow = func() time.Time { return now }
	t.Cleanup(func() { leaseNow = old })
	return &now
}

// TestClusterUpgradeStillTakesTheReleasedLeaseName: the lease name is a
// cluster-wide contract, not a constant of this build. An orchestrator on the
// previous release takes kube-system/stratum-cluster-upgrade and reads it
// before upgrading; renaming the lease here, or dropping it early, lets an old
// and a new orchestrator upgrade the same cluster at once. The name is pinned
// as a literal, not through upgradeLeaseName.
func TestClusterUpgradeStillTakesTheReleasedLeaseName(t *testing.T) {
	fakeClock(t, time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC))
	ctx := context.Background()
	renew := metav1.NewMicroTime(time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC))
	dur := int32(60)
	held := "old-orchestrator/1/1"
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum-cluster-upgrade", Namespace: "kube-system"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &held, LeaseDurationSeconds: &dur, RenewTime: &renew},
	})

	_, _, err := acquireUpgradeLease(ctx, cs, "cp9/900/9")
	if err == nil || !strings.Contains(err.Error(), "old-orchestrator/1/1") {
		t.Fatalf("a live lease of the released name = %v, want a refusal naming its holder", err)
	}

	fresh := fake.NewSimpleClientset()
	_, release, err := acquireUpgradeLease(ctx, fresh, "cp9/900/9")
	if err != nil {
		t.Fatalf("a free cluster: %v", err)
	}
	defer release()
	l, err := fresh.CoordinationV1().Leases("kube-system").Get(ctx, "stratum-cluster-upgrade", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the acquired lease is not kube-system/stratum-cluster-upgrade: %v", err)
	}
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "cp9/900/9" {
		t.Errorf("the acquired lease is held by %v, want cp9/900/9", l.Spec.HolderIdentity)
	}
}

// TestUpgradeLeaseRefusalNamesAMaintenanceRun: a CA rotation takes the released
// upgrade lease as its compatibility fence, so the run holding it may be no
// upgrade at all. The refusal names a maintenance run and its holder, never
// "another `stratum upgrade --cluster`", which sent an operator looking for an
// upgrade that was not running.
func TestUpgradeLeaseRefusalNamesAMaintenanceRun(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	_, releaseRotation, err := maintlease.AcquireCompatible(ctx, cs, cuNamespace, maintenanceLeaseName, "cp1/931829/1")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRotation()
	_, _, err = acquireUpgradeLease(ctx, cs, "cp2/200/2")
	if err == nil || !strings.Contains(err.Error(), "another maintenance run is in progress (cp1/931829/1") {
		t.Fatalf("an upgrade beside a CA rotation = %v, want a refusal naming a maintenance run and its holder", err)
	}
	if strings.Contains(err.Error(), "upgrade --cluster") {
		t.Errorf("the refusal names an upgrade, which is not running: %v", err)
	}
}

// TestUpgradeLeaseMakesClusterUpgradesExclusive: two runs each serialize only
// their own nodes, so the lease is what makes the run itself exclusive.
func TestUpgradeLeaseMakesClusterUpgradesExclusive(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	ctx := context.Background()

	_, release, err := acquireUpgradeLease(ctx, cs, "cp1/100/1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := acquireUpgradeLease(ctx, cs, "cp2/200/2"); err == nil ||
		!strings.Contains(err.Error(), "another maintenance run is in progress (cp1/100/1") {
		t.Fatalf("a second run while the first holds the lease = %v, want a refusal naming the holder", err)
	}

	// Renewal keeps it live past the original duration.
	*now = now.Add(leaseDuration - time.Second)
	if err := renewUpgradeLease(ctx, cs, "cp1/100/1"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(leaseDuration - time.Second)
	if _, _, err := acquireUpgradeLease(ctx, cs, "cp2/200/2"); err == nil {
		t.Fatal("a renewed lease was taken over")
	}

	release()
	if _, err := cs.CoordinationV1().Leases(cuNamespace).Get(ctx, upgradeLeaseName, metav1.GetOptions{}); err == nil {
		t.Error("release left the lease behind")
	}
	_, release2, err := acquireUpgradeLease(ctx, cs, "cp2/200/2")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}

// TestUpgradeLeaseOfACrashedRunExpires: a process that dies holding the lease
// blocks the next run for one lease duration, not forever.
func TestUpgradeLeaseOfACrashedRunExpires(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if _, _, err := acquireUpgradeLease(ctx, cs, "cp1/100/1"); err != nil { // never released: the process died
		t.Fatal(err)
	}
	*now = now.Add(leaseDuration + time.Second)
	_, release, err := acquireUpgradeLease(ctx, cs, "cp2/200/2")
	if err != nil {
		t.Fatalf("an expired lease was not taken over: %v", err)
	}
	release()
}

// TestReleaseNeverDeletesASuccessorsLease: a run whose lease expired and was
// taken over must not delete the new holder's lease on its way out.
func TestReleaseNeverDeletesASuccessorsLease(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if _, _, err := acquireUpgradeLease(ctx, cs, "cp1/100/1"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(leaseDuration + time.Second)
	_, release2, err := acquireUpgradeLease(ctx, cs, "cp2/200/2")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	releaseUpgradeLease(ctx, cs, "cp1/100/1") // the stale run finally exits
	l, err := cs.CoordinationV1().Leases(cuNamespace).Get(ctx, upgradeLeaseName, metav1.GetOptions{})
	if err != nil || *l.Spec.HolderIdentity != "cp2/200/2" {
		t.Fatalf("the successor's lease was removed or changed: %v, %v", l, err)
	}
}

// TestLeaseGuardStopsTheRunBeforeTheLeaseCanBeTaken: the guard trips one
// renewal interval before expiry, or at once if the lease has another holder.
func TestLeaseGuardStopsTheRunBeforeTheLeaseCanBeTaken(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	g := &leaseGuard{lastRenew: t0}
	down := errors.New("dial tcp 127.0.0.1:6443: connection refused")

	g.observe(nil, t0.Add(leaseRenew))
	g.observe(down, t0.Add(2*leaseRenew))
	if g.Err() != nil {
		t.Fatalf("one missed renewal tripped the guard: %v", g.Err())
	}
	g.observe(down, t0.Add(leaseRenew+leaseDuration-leaseRenew))
	if err := g.Err(); err == nil || !strings.Contains(err.Error(), "may be taken by another run") {
		t.Fatalf("no renewal for leaseDuration-leaseRenew: %v, want the guard tripped", err)
	}
	// The API server came back and the lease still named this run: that
	// renewal is a compare-and-swap, so nobody held it meanwhile.
	g.observe(nil, t0.Add(10*leaseDuration))
	if err := g.Err(); err != nil {
		t.Fatalf("a renewal that succeeded after the outage left the guard tripped: %v", err)
	}

	taken := &leaseGuard{lastRenew: t0}
	taken.observe(fmt.Errorf("%w (cp2/200/2)", errLeaseTaken), t0.Add(leaseRenew))
	if taken.Err() == nil {
		t.Fatal("a lease held by another run did not trip the guard at once")
	}
	taken.observe(nil, t0.Add(2*leaseRenew))
	if taken.Err() == nil {
		t.Fatal("a lease lost to another run came back")
	}
	var unset *leaseGuard
	if unset.Err() != nil {
		t.Fatal("a nil guard (dry-run, tests) must read as held")
	}
}

// TestUpgradeOneNodeStopsOnALostLease: the first destructive step is refused,
// and nothing is cordoned, when the lease is gone.
func TestUpgradeOneNodeStopsOnALostLease(t *testing.T) {
	g := &leaseGuard{lost: errors.New("lease lost")}
	_, err := upgradeOneNode(io.Discard, context.Background(), nil, Node{Name: "w1"}, ClusterOptions{Target: "v1.37.1", lease: g})
	if err == nil || !strings.Contains(err.Error(), "lease lost") {
		t.Fatalf("upgradeOneNode with a lost lease = %v", err)
	}
}

// TestRefuseActiveUpgradeJobs: the lease fences this process,
// not the per-node Jobs it started. A takeover must not replace an earlier
// run's Job that is still upgrading its node; a finished one is fine.
func TestSweepUpgradeJobsRefusesActiveOnes(t *testing.T) {
	present := []string{"w1", "w2", "w3", "w4"}
	job := func(name, node string, conds ...batchv1.JobConditionType) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cuNamespace,
			Labels: map[string]string{upgradeJobLabel: node}}}
		for _, c := range conds {
			j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: c, Status: corev1.ConditionTrue})
		}
		return j
	}
	ctx := context.Background()
	if err := sweepUpgradeJobs(io.Discard, ctx, fake.NewSimpleClientset(
		job("stratum-upgrade-w1", "w1", batchv1.JobComplete), job("stratum-upgrade-w2", "w2", batchv1.JobFailed),
	), present); err != nil {
		t.Errorf("finished Jobs from an earlier run blocked the takeover: %v", err)
	}
	err := sweepUpgradeJobs(io.Discard, ctx, fake.NewSimpleClientset(
		job("stratum-upgrade-w1", "w1", batchv1.JobComplete), job("stratum-upgrade-w3", "w3"),
	), present)
	if err == nil || !strings.Contains(err.Error(), "stratum-upgrade-w3 (node w3)") {
		t.Fatalf("a still-running upgrade Job was not refused by name: %v", err)
	}
	// A Job being deleted can still have its upgrade Pod running.
	terminating := job("stratum-upgrade-w4", "w4")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"foregroundDeletion"}
	if err := sweepUpgradeJobs(io.Discard, ctx, fake.NewSimpleClientset(terminating), present); err == nil {
		t.Error("a terminating upgrade Job was treated as finished")
	}
	unrelated := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: cuNamespace}}
	if err := sweepUpgradeJobs(io.Discard, ctx, fake.NewSimpleClientset(unrelated), present); err != nil {
		t.Errorf("a Job without the upgrade label blocked the run: %v", err)
	}
}

// TestLeaseCallsAreBounded: a blackholed apiserver must not hang renewal (so a
// lost lease is observed) or release (so the run exits).
func TestLeaseCallsAreBounded(t *testing.T) {
	if leaseCallTimeout <= 0 || leaseCallTimeout >= leaseRenew {
		t.Fatalf("leaseCallTimeout = %s; it must be positive and shorter than the renewal interval %s", leaseCallTimeout, leaseRenew)
	}
}

// TestSweepUpgradeJobsRemovesFinishedOrphans: a finished upgrade Job
// whose node has left the cluster is removed, including one that holds the
// name a current node's Job needs, where nodejob.Run refuses to replace
// another node's Job; one still running blocks, naming the departed node,
// since its Pod may still run there; and a present node's Job is never
// touched by the sweep.
func TestSweepUpgradeJobsRemovesFinishedOrphans(t *testing.T) {
	ctx := context.Background()
	job := func(name, node string, finished bool) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cuNamespace, UID: types.UID("uid-" + name),
			Labels:      map[string]string{upgradeJobLabel: nodejob.NodeLabelValue(node)},
			Annotations: map[string]string{nodejob.NodeAnnotation: node}}}
		if finished {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}
		return j
	}
	present := []string{"cp1", "w1"}

	collided := job(jobName("cp1"), "departed-node", true)
	kept := job(jobName("w1"), "w1", true)
	cs := fake.NewSimpleClientset(collided, kept, job("stratum-upgrade-old", "gone-too", true))
	if err := sweepUpgradeJobs(io.Discard, ctx, cs, present); err != nil {
		t.Fatal(err)
	}
	left, err := cs.BatchV1().Jobs(cuNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range left.Items {
		names = append(names, j.Name)
	}
	if !slices.Equal(names, []string{jobName("w1")}) {
		t.Errorf("Jobs left = %q, want only the present node's", names)
	}

	running := job("stratum-upgrade-gone", "departed-node", false)
	err = sweepUpgradeJobs(io.Discard, ctx, fake.NewSimpleClientset(running), present)
	if err == nil || !strings.Contains(err.Error(), "node departed-node, which is no longer in the cluster") ||
		!strings.Contains(err.Error(), "kubectl -n "+cuNamespace+" delete job stratum-upgrade-gone") {
		t.Fatalf("a running Job of a departed node did not block with its node named: %v", err)
	}
}

// TestSweepUpgradeJobsKeepsAJobWithNoNode: a Job whose node
// cannot be resolved is not known to be an orphan, so it is never removed,
// and while it runs it still blocks the run, since it may still be upgrading
// a host.
func TestSweepUpgradeJobsKeepsAJobWithNoNode(t *testing.T) {
	ctx := context.Background()
	noNode := func(name string, finished bool) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cuNamespace,
			Labels: map[string]string{upgradeJobLabel: ""}}}
		if finished {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}
		return j
	}
	cs := fake.NewSimpleClientset(noNode("stratum-upgrade-unknown", true))
	if err := sweepUpgradeJobs(io.Discard, ctx, cs, []string{"cp1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BatchV1().Jobs(cuNamespace).Get(ctx, "stratum-upgrade-unknown", metav1.GetOptions{}); err != nil {
		t.Errorf("a finished Job with no node to resolve was removed: %v", err)
	}
	if err := sweepUpgradeJobs(io.Discard, ctx, fake.NewSimpleClientset(noNode("stratum-upgrade-unknown", false)), []string{"cp1"}); err == nil ||
		!strings.Contains(err.Error(), "stratum-upgrade-unknown") {
		t.Errorf("a running Job with no node to resolve did not block the run: %v", err)
	}
}
