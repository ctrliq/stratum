package upgrade

// The cluster maintenance fence for `stratum upgrade --cluster`. The upgrade
// already takes kube-system/stratum-cluster-upgrade, the lease identity a
// released binary reads; it also takes a second, shared maintenance Lease
// (kube-system/stratum-cluster-maintenance) that the upgrade, rotate-ca
// --cluster and migrate-etcd-ca --cluster all hold, so those three cluster
// orchestrators exclude one another whatever node each runs on. These tests
// pin the upgrade side of that fence, all against the fake clientset the
// lease and finish-fence tests use:
//
//   - the run takes BOTH leases, the released name first, and releases both
//     on every exit, a refused or failing run included;
//   - another run holding the shared lease refuses the run before any change
//     (the finished-Job sweep, the first change, never runs), naming the
//     holder, and leaves the other run's lease untouched;
//   - the shared lease's guard is confirmed before each node's phase, so a
//     successor that took the lease mid-run stops the run before the next
//     node is touched — cordon included;
//   - renewal rides through the run: the lease is renewed as the phases go,
//     so a bounded gap between the API server and this process never expires
//     it.
//
// The full run drives UpgradeCluster with the package's seams: runNodeJob,
// runExitCode and waitAPIHealthz answer for the nodes, so no Job ever runs;
// the staged binary is a real file for its hash and answered for by
// readStagedBuild and readStagedVersion.

import (
	"io"

	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/maintlease"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

// The shared maintenance lease's released identity: pinned as literals, the
// way lease_test.go pins stratum-cluster-upgrade. Production code owns the
// real constant; these names are the test's own.
const (
	fenceTestMaintenanceNS    = "kube-system"
	fenceTestMaintenanceLease = "stratum-cluster-maintenance"
)

// upgradeNodeFixture is one Ready node at v1.36.5, behind the v1.37.1 target,
// so it takes the full per-node path under the seams.
func upgradeNodeFixture(name string, cp bool, ip string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.36.5", Architecture: "amd64"},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
		},
	}
	if cp {
		n.Labels[k8sutil.ControlPlaneLabel] = "true"
	}
	return n
}

func upgradeNodesFixture() []runtime.Object {
	return []runtime.Object{
		upgradeNodeFixture("cp1", true, "10.0.0.9"),
		upgradeNodeFixture("w1", false, "10.0.0.8"),
	}
}

// liveHeldLease is kube-system/name, held live by holder: renewed now, so it
// is an hour from expiring.
func liveHeldLease(name, holder string) *coordinationv1.Lease {
	h := holder
	now := metav1.NewMicroTime(leaseNow())
	dur := int32(60)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cuNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &h, LeaseDurationSeconds: &dur, RenewTime: &now},
	}
}

// upgradeJob returns a per-node upgrade Job labeled for node.
func upgradeJob(name, node string, finished bool) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cuNamespace,
		Labels: map[string]string{upgradeJobLabel: node}}}
	if finished {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

// leaseEvents records which leases a run created and deleted, in order.
type leaseEvents struct {
	creates, deletes []string
}

func recordLeaseEvents(t *testing.T, cs *fake.Clientset) *leaseEvents {
	t.Helper()
	ev := &leaseEvents{}
	cs.PrependReactor("create", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if ca, ok := a.(clienttesting.CreateAction); ok {
			if l, ok := ca.GetObject().(*coordinationv1.Lease); ok {
				ev.creates = append(ev.creates, l.Name)
			}
		}
		return false, nil, nil
	})
	cs.PrependReactor("delete", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if da, ok := a.(clienttesting.DeleteAction); ok {
			ev.deletes = append(ev.deletes, da.GetName())
		}
		return false, nil, nil
	})
	return ev
}

// fastOrchestrator answers for the staged binary and for every node: the
// image import succeeds, the build check says an upgrade is needed, the node
// Job succeeds at once, the health gate passes. It counts the per-node upgrade
// Jobs and hands each count to onJob as it dispatches, so a test can act (a
// lease takeover, a clock jump) while the run is between nodes.
func fastOrchestrator(t *testing.T, onJob func(n int)) *int {
	t.Helper()
	oldExit, oldJob, oldHealth := runExitCode, runNodeJob, waitAPIHealthz
	oldBuild, oldVersion := readStagedBuild, readStagedVersion
	swap(t, &runImageImport, func(context.Context, kubernetes.Interface, nodejob.Spec) error { return nil })
	swap(t, &stagedImportsImages, func(context.Context, string) (bool, error) { return true, nil })
	jobs := 0
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) {
		return ExitUpgradeNeeded, nil
	}
	answerLogsWithExitCode(t)
	runNodeJob = func(context.Context, kubernetes.Interface, nodejob.Spec) error {
		jobs++
		if onJob != nil {
			onJob(jobs)
		}
		return nil
	}
	waitAPIHealthz = func(context.Context, kubernetes.Interface) error { return nil }
	readStagedBuild = func(string) string { return "build-20261012" }
	readStagedVersion = func(string) (string, error) { return "v9.9.9-stratum", nil }
	t.Cleanup(func() {
		runExitCode, runNodeJob, waitAPIHealthz = oldExit, oldJob, oldHealth
		readStagedBuild, readStagedVersion = oldBuild, oldVersion
	})
	return &jobs
}

// stagedBinary is the --from binary: a real file, since the orchestrator
// hashes it before the rollout.
func stagedBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stratum-next")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// runUpgrade is a two-node rollout (cp1 serial, then w1) on cs.
func runUpgrade(t *testing.T, cs kubernetes.Interface) error {
	t.Helper()
	return UpgradeCluster(io.Discard, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs})
}

// takeOverTheMaintenanceLease makes another run the holder of the shared
// lease, freshly renewed, as a successor that took it over would.
func takeOverTheMaintenanceLease(t *testing.T, cs *fake.Clientset, holder string) {
	t.Helper()
	gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
	obj, err := cs.Tracker().Get(gvr, cuNamespace, fenceTestMaintenanceLease)
	if err != nil {
		t.Fatalf("find the maintenance lease to take over: %v", err)
	}
	l := obj.(*coordinationv1.Lease)
	h, renewed := holder, metav1.NewMicroTime(leaseNow())
	l.Spec.HolderIdentity, l.Spec.RenewTime = &h, &renewed
	if err := cs.Tracker().Update(gvr, l, cuNamespace); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeClusterTakesTheReleasedLeaseThenTheMaintenanceLease: the run
// holds BOTH leases before it touches anything, the released name first — an
// orchestrator on the previous release reads kube-system/stratum-cluster-upgrade
// before it upgrades, so it must still be taken, and taken before the shared
// lease — and both are released when the run stops, here on the sweep's
// refusal of a still-running earlier Job. The sweep itself is the first
// change a run makes (it deletes finished Jobs), so both leases must already
// be held when it runs, and no per-node Job may exist when it refuses.
func TestUpgradeClusterTakesTheReleasedLeaseThenTheMaintenanceLease(t *testing.T) {
	cs := fake.NewSimpleClientset(append(upgradeNodesFixture(),
		upgradeJob("stratum-upgrade-old", "gone", false))...)
	ev := recordLeaseEvents(t, cs)
	fastOrchestrator(t, nil)

	err := runUpgrade(t, cs)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("a run blocked by an earlier live upgrade Job = %v; the run must reach the sweep past both leases", err)
	}
	if len(ev.creates) != 2 || ev.creates[0] != upgradeLeaseName || ev.creates[1] != fenceTestMaintenanceLease {
		t.Fatalf("the run created leases %v; want %s taken first, then %s",
			ev.creates, upgradeLeaseName, fenceTestMaintenanceLease)
	}
	for _, name := range []string{upgradeLeaseName, fenceTestMaintenanceLease} {
		if !containsOnce(ev.deletes, name) {
			t.Errorf("the refused run left lease %s behind (deletes: %v); both leases must be released on every exit", name, ev.deletes)
		}
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			t.Errorf("a per-node Job was created although the sweep refused the run: %v", a)
		}
	}
}

func containsOnce(xs []string, want string) bool {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n == 1
}

// TestUpgradeClusterRefusesWhileTheMaintenanceLeaseIsHeld: rotate-ca,
// migrate-etcd-ca or a second upgrade holding the shared lease means this run
// refuses — naming the holder — before any change: the sweep's deletion of an
// earlier run's finished Job is the first change a run makes, and it must
// still be there afterwards. The other run's lease must survive the refusal
// untouched: our release may never delete a successor's lease.
func TestUpgradeClusterRefusesWhileTheMaintenanceLeaseIsHeld(t *testing.T) {
	cs := fake.NewSimpleClientset(append(upgradeNodesFixture(),
		upgradeJob("stratum-upgrade-gone", "gone", true),
		liveHeldLease(fenceTestMaintenanceLease, "another-run/9/9"))...)
	fastOrchestrator(t, nil)

	err := runUpgrade(t, cs)
	if err == nil {
		t.Fatal("the rollout ran while another run held " + fenceTestMaintenanceNS + "/" + fenceTestMaintenanceLease)
	}
	for _, want := range []string{fenceTestMaintenanceLease, "another-run/9/9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if _, err := cs.BatchV1().Jobs(cuNamespace).Get(context.Background(), "stratum-upgrade-gone", metav1.GetOptions{}); err != nil {
		t.Error("the refused run removed the earlier run's finished upgrade Job — the lease must be taken before any change")
	}
	l, err := cs.CoordinationV1().Leases(cuNamespace).Get(context.Background(), fenceTestMaintenanceLease, metav1.GetOptions{})
	if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "another-run/9/9" {
		t.Fatalf("the other run's lease did not survive the refusal: %v, %v", l, err)
	}
}

// TestUpgradeClusterStopsWhenTheMaintenanceLeaseIsLost: a successor that took
// the shared lease mid-run stops the run before the next node's phase —
// before its cordon, which is a change — and the run's exit leaves the
// successor's lease alone.
func TestUpgradeClusterStopsWhenTheMaintenanceLeaseIsLost(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	jobs := fastOrchestrator(t, func(n int) {
		if n == 1 { // cp1's Job is running when the successor takes over
			takeOverTheMaintenanceLease(t, cs, "successor")
		}
	})

	err := runUpgrade(t, cs)
	if err == nil || !strings.Contains(err.Error(), "successor") {
		t.Fatalf("a run whose maintenance lease was taken over = %v; it must stop naming the holder", err)
	}
	if *jobs != 1 {
		t.Errorf("the run dispatched %d per-node Jobs after losing the maintenance lease; it must stop before the next node", *jobs)
	}
	w1, err := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w1.Spec.Unschedulable || w1.Annotations[k8sutil.UpgradeCordonAnnotation] != "" ||
		w1.Annotations[k8sutil.UpgradeInProgressAnnotation] != "" {
		t.Errorf("worker w1 was touched after the lease was lost: cordoned=%v annotations=%v", w1.Spec.Unschedulable, w1.Annotations)
	}
	l, err := cs.CoordinationV1().Leases(cuNamespace).Get(context.Background(), fenceTestMaintenanceLease, metav1.GetOptions{})
	if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "successor" {
		t.Fatalf("the successor's lease did not survive this run's exit: %v, %v", l, err)
	}
}

// TestUpgradeClusterRenewsTheMaintenanceLeaseThroughTheRun: the shared lease
// is renewed as the phases go, on the run's clock. The clock jumps 25 s
// between the nodes — inside the lease's 60 s window, so nothing can have
// taken it — and the guard is confirmed before w1's phase: that confirmation
// is a renewal, so by the time w1's Job dispatches the lease's renew time
// reads the run's clock. A run that never renewed the shared lease (its
// renew time still at the acquisition moment) is what this refuses.
func TestUpgradeClusterRenewsTheMaintenanceLeaseThroughTheRun(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	now := fakeClock(t, t0)
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	var renewedAt time.Time
	jobs := fastOrchestrator(t, func(n int) {
		if n == 1 {
			*now = now.Add(25 * time.Second)
		}
		if n == 2 {
			l, err := cs.CoordinationV1().Leases(cuNamespace).Get(context.Background(), fenceTestMaintenanceLease, metav1.GetOptions{})
			if err != nil {
				t.Errorf("read the maintenance lease at the second node's dispatch: %v", err)
				return
			}
			if l.Spec.RenewTime != nil {
				renewedAt = l.Spec.RenewTime.Time
			}
		}
	})

	if err := runUpgrade(t, cs); err != nil {
		t.Fatalf("a bounded lease-renewal gap stopped the run: %v", err)
	}
	if *jobs != 2 {
		t.Fatalf("the run dispatched %d per-node Jobs; want both nodes upgraded", *jobs)
	}
	if !renewedAt.Equal(*now) {
		t.Errorf("the maintenance lease was last renewed at %v; want %v — the guard confirmed before the second node's phase renews it", renewedAt, *now)
	}
	for _, name := range []string{"cp1", "w1"} {
		n, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if n.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" {
			t.Errorf("%s was not stamped with the target: annotations=%v", name, n.Annotations)
		}
	}
}

func TestConcurrentUpgradeConfirmations(t *testing.T) {
	cs := fake.NewSimpleClientset()
	guard, release, err := acquireUpgradeLease(context.Background(), cs, "concurrent-confirm/1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 20 {
				if err := guard.confirm(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

func TestCordonRefusesLostCommonLease(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1"}})
	legacy, releaseLegacy, err := acquireUpgradeLease(ctx, cs, "run/1")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLegacy()
	common, releaseCommon, err := acquireMaintenanceLease(ctx, cs, "run/1")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCommon()
	legacy.maintenance = common
	common.Observe(maintlease.ErrTaken, time.Now())
	if err := cordonForUpgrade(ctx, cs, "cp1", "v1", legacy); !errors.Is(err, maintlease.ErrTaken) {
		t.Fatalf("cordon ignored common lease loss: %v", err)
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, "cp1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n.Spec.Unschedulable || len(n.Annotations) != 0 {
		t.Fatalf("lost run modified node: %+v", n)
	}
}
