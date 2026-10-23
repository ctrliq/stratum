package upgrade

import (
	"io"

	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

var errAPIDown = errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")

// singleNode is a one-node cluster's only node, Ready, mid-way through the
// upgrade, with the real nodejob.Run polling a Job whose reads fail for
// jobDown polls (the API server restarting under the orchestrator)
// and whose Node writes fail for patchDown calls once the Job is done.
func singleNode(t *testing.T, jobDown, patchDown int, jobStatus batchv1.JobStatus) (*fake.Clientset, *int) {
	t.Helper()
	cs := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	})
	reads := new(int)
	cs.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		*reads++
		if *reads <= jobDown {
			return true, nil, errAPIDown
		}
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: a.(clienttesting.GetAction).GetName(), Namespace: cuNamespace}, Status: jobStatus}, nil
	})
	rvGuard(cs)
	cs.PrependReactor("update", "nodes", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if *reads > jobDown && patchDown > 0 {
			patchDown--
			return true, nil, errAPIDown
		}
		return false, nil, nil
	})

	oldCheck, oldHealthz := runExitCode, waitAPIHealthz
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return ExitUpgradeNeeded, nil }
	waitAPIHealthz = func(context.Context, kubernetes.Interface) error { return nil }
	t.Cleanup(func() { runExitCode, waitAPIHealthz = oldCheck, oldHealthz })
	return cs, reads
}

var nodesGVR = corev1.SchemeGroupVersion.WithResource("nodes")

// rvGuard makes the fake clientset refuse an Update that carries a stale
// resourceVersion, as the API server does, and number each accepted one.
func rvGuard(cs *fake.Clientset) {
	cs.PrependReactor("update", "nodes", func(a clienttesting.Action) (bool, runtime.Object, error) {
		in := a.(clienttesting.UpdateAction).GetObject().(*corev1.Node)
		cur, err := cs.Tracker().Get(nodesGVR, "", in.Name)
		if err != nil {
			return false, nil, nil
		}
		rv := cur.(*corev1.Node).ResourceVersion
		if in.ResourceVersion != rv {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, in.Name, errors.New("stale resourceVersion"))
		}
		n, _ := strconv.Atoi(rv)
		in.ResourceVersion = strconv.Itoa(n + 1)
		return false, nil, nil
	})
}

// changeNode is another actor's write to the node: it goes straight to the
// tracker, and numbers itself as the API server would.
func changeNode(t *testing.T, cs *fake.Clientset, f func(*corev1.Node)) {
	t.Helper()
	cur, err := cs.Tracker().Get(nodesGVR, "", "cp1")
	if err != nil {
		t.Fatal(err)
	}
	n := cur.(*corev1.Node).DeepCopy()
	f(n)
	rv, _ := strconv.Atoi(n.ResourceVersion)
	n.ResourceVersion = strconv.Itoa(rv + 1)
	if err := cs.Tracker().Update(nodesGVR, n, ""); err != nil {
		t.Fatal(err)
	}
}

// fakeNode is a fake clientset holding n, with resourceVersion conflicts.
func fakeNode(n *corev1.Node) *fake.Clientset {
	cs := fake.NewSimpleClientset(n)
	rvGuard(cs)
	return cs
}

func getNode(t *testing.T, cs kubernetes.Interface) *corev1.Node {
	t.Helper()
	n, err := cs.CoreV1().Nodes().Get(context.Background(), "cp1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func runSingleNode(cs kubernetes.Interface) error {
	n := Node{Name: "cp1", Version: "v1.36.5", Ready: true, IsCP: true}
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", DrainGraceSec: 1}
	_, err := upgradeOneNode(io.Discard, context.Background(), cs, n, o)
	return err
}

// TestSingleNodeRidesOutItsOwnAPIServerRestart: the Job watch and the
// writes that follow the gate are retried through the restart, so the node is
// stamped and uncordoned instead of left cordoned with the workloads Pending.
func TestSingleNodeRidesOutItsOwnAPIServerRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, reads := singleNode(t, 20, 3, batchv1.JobStatus{Succeeded: 1}) // ~100s of refused reads
		if err := runSingleNode(cs); err != nil {
			t.Fatalf("upgradeOneNode: %v", err)
		}
		if *reads <= 20 {
			t.Errorf("only %d Job reads: the watch gave up before the API server came back", *reads)
		}
		got, err := cs.CoreV1().Nodes().Get(context.Background(), "cp1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Unschedulable {
			t.Error("the node was left cordoned")
		}
		if got.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" || got.Annotations[k8sutil.UpgradeInProgressAnnotation] != "" {
			t.Errorf("annotations = %v, want the version stamped and the in-progress record cleared", got.Annotations)
		}
	})
}

// If a halt can still happen, the error says what to do about it.
func TestSingleNodeHaltNamesTheRecoverySteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, _ := singleNode(t, 1<<30, 0, batchv1.JobStatus{})
		err := runSingleNode(cs)
		if err == nil || !errors.Is(err, errAPIDown) {
			t.Fatalf("upgradeOneNode with the API never back = %v, want the read error", err)
		}
		for _, want := range []string{"get job/stratum-upgrade-cp1", "kubectl uncordon cp1", "/var/lib/stratum/upgrade/stratum-next upgrade --cluster --target v1.37.1 --from /var/lib/stratum/upgrade/stratum-next"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q:\n%v", want, err)
			}
		}
	})
}

func TestRetryAPIGivesUpAfterItsWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start, calls := time.Now(), 0
		err := retryAPI(context.Background(), func(context.Context) error { calls++; return errAPIDown })
		if !errors.Is(err, errAPIDown) || time.Since(start) > apiRetryWindow || time.Since(start) < apiRetryWindow-apiRetryInterval {
			t.Fatalf("err=%v after %s over %d calls, want the last error after about %s", err, time.Since(start), calls, apiRetryWindow)
		}
	})
}

// A request the API server accepts and never answers is bounded too: the
// window is enforced through the attempt's context, not only between attempts.
func TestRetryAPIBoundsAStalledRequest(t *testing.T) {
	oldWindow, oldCall, oldInterval := apiRetryWindow, apiCallTimeout, apiRetryInterval
	apiRetryWindow, apiCallTimeout, apiRetryInterval = 200*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { apiRetryWindow, apiCallTimeout, apiRetryInterval = oldWindow, oldCall, oldInterval })
	calls := make(chan struct{}, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		<-r.Context().Done() // never answers; ends only when the client gives up
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(context.Context) error{
		"finishNode": func(ctx context.Context) error {
			_, err := updateNode(ctx, cs, "cp1", func(*corev1.Node) (bool, error) { return true, nil })
			return err
		},
		"backstop": func(ctx context.Context) error { _, err := releaseUpgradeCordon(ctx, cs, "cp1", "v1.37.1"); return err },
	} {
		before, start := len(calls), time.Now()
		err := retryAPI(context.Background(), f)
		if err == nil || time.Since(start) > 10*apiRetryWindow {
			t.Errorf("%s: err=%v after %s, want a timeout error within about %s", name, err, time.Since(start), apiRetryWindow)
		}
		if n := len(calls) - before; n < 2 {
			t.Errorf("%s: the stalled request was not retried: %d call(s)", name, n)
		}
	}
}

// An answer retrying cannot change ends the retries at once; conflicts, throttling
// and server errors are retried.
func TestRetryAPIStopsOnATerminalError(t *testing.T) {
	oldInterval := apiRetryInterval
	apiRetryInterval = time.Millisecond
	t.Cleanup(func() { apiRetryInterval = oldInterval })
	res := schema.GroupResource{Resource: "nodes"}
	for _, c := range []struct {
		name      string
		err       error
		wantCalls int
	}{
		{"forbidden", apierrors.NewForbidden(res, "cp1", errors.New("no")), 1},
		{"not found", apierrors.NewNotFound(res, "cp1"), 1},
		{"invalid", apierrors.NewBadRequest("bad"), 1},
		{"conflict", apierrors.NewConflict(res, "cp1", errors.New("stale")), 3},
		{"throttled", apierrors.NewTooManyRequests("slow down", 1), 3},
		{"server error", apierrors.NewInternalError(errors.New("boom")), 3},
		{"network", errAPIDown, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			err := retryAPI(context.Background(), func(context.Context) error {
				calls++
				if calls == 3 {
					return nil
				}
				return c.err
			})
			if calls != c.wantCalls || (c.wantCalls == 1) != (err != nil) {
				t.Errorf("calls=%d err=%v, want %d call(s)", calls, err, c.wantCalls)
			}
		})
	}
}

// ownership is who may undo a node's cordon: what the orchestrator and the
// per-node Job's backstop both ask. Only the upgrade's own cordon is undone.
func TestOnlyTheUpgradesOwnCordonIsUndone(t *testing.T) {
	const target = "v1.37.1"
	for _, c := range []struct {
		name          string
		unschedulable bool
		annotations   map[string]string
		wantUndone    bool
	}{
		{"cordoned by this upgrade", true, map[string]string{k8sutil.UpgradeCordonAnnotation: target, k8sutil.UpgradeInProgressAnnotation: target}, true},
		{"cordoned by an older orchestrator that recorded no owner", true, map[string]string{k8sutil.UpgradeInProgressAnnotation: target}, true},
		{"cordoned by an admin before the upgrade", true, map[string]string{k8sutil.UpgradeCordonAnnotation: cordonPreexisting, k8sutil.UpgradeInProgressAnnotation: target}, false},
		{"cordoned by an admin after the Job released its own", true, map[string]string{k8sutil.UpgradeCordonAnnotation: cordonReleased, k8sutil.UpgradeInProgressAnnotation: target}, false},
		{"cordoned by an admin, no upgrade", true, nil, false},
		// An earlier target's claim: the Job (no lease, its own target) leaves it,
		// the run that holds the lease takes it over and undoes it.
		{"cordon left by an earlier target", true, map[string]string{k8sutil.UpgradeCordonAnnotation: "v1.36.4", k8sutil.UpgradeInProgressAnnotation: target}, false},
		{"already schedulable", false, map[string]string{k8sutil.UpgradeCordonAnnotation: target, k8sutil.UpgradeInProgressAnnotation: target}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			node := func() *corev1.Node {
				return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: c.annotations}, Spec: corev1.NodeSpec{Unschedulable: c.unschedulable}}
			}
			// The Job's backstop.
			cs := fake.NewSimpleClientset(node())
			rvGuard(cs)
			done, err := releaseUpgradeCordon(context.Background(), cs, "cp1", target)
			got := getNode(t, cs)
			if err != nil || done != (c.wantUndone && c.unschedulable) || got.Spec.Unschedulable != (c.unschedulable && !c.wantUndone) {
				t.Errorf("backstop: done=%v err=%v unschedulable=%v", done, err, got.Spec.Unschedulable)
			}
			if got.Annotations[k8sutil.UpgradeInProgressAnnotation] != c.annotations[k8sutil.UpgradeInProgressAnnotation] {
				t.Error("the backstop changed the in-progress record; only the orchestrator's finish may")
			}
			// The orchestrator's finish.
			cs = fake.NewSimpleClientset(node())
			rvGuard(cs)
			if err := finishNode(context.Background(), cs, "cp1", target, "", ""); err != nil {
				t.Fatal(err)
			}
			got = getNode(t, cs)
			finishUndoes := c.wantUndone || c.name == "cordon left by an earlier target"
			if want := c.unschedulable && !finishUndoes; got.Spec.Unschedulable != want {
				t.Errorf("finish: unschedulable=%v, want %v", got.Spec.Unschedulable, want)
			}
			a := got.Annotations
			if a[k8sutil.VersionAnnotation] != target || a[k8sutil.UpgradeInProgressAnnotation] != "" || a[k8sutil.UpgradeCordonAnnotation] != "" {
				t.Errorf("finish left annotations %v, want the version stamped and both records cleared", a)
			}
		})
	}
}

// The backstop writes only to the node as it read it: an actor who removes the
// upgrade's claim between the read and the write wins, and the node stays
// cordoned.
func TestBackstopYieldsToAConcurrentChange(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: map[string]string{
			k8sutil.UpgradeCordonAnnotation: "v1.37.1", k8sutil.UpgradeInProgressAnnotation: "v1.37.1"}},
		Spec: corev1.NodeSpec{Unschedulable: true},
	})
	rvGuard(cs)
	first := true
	cs.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if first {
			first = false
			changeNode(t, cs, func(n *corev1.Node) {
				delete(n.Annotations, k8sutil.UpgradeCordonAnnotation)
				delete(n.Annotations, k8sutil.UpgradeInProgressAnnotation)
			})
		}
		return false, nil, nil
	})
	if done, err := releaseUpgradeCordon(context.Background(), cs, "cp1", "v1.37.1"); !apierrors.IsConflict(err) || done {
		t.Fatalf("backstop on a changed node: done=%v err=%v, want a conflict", done, err)
	}
	// The retry reads the changed node, which the upgrade no longer claims.
	if err := retryAPI(context.Background(), func(ctx context.Context) error {
		_, err := releaseUpgradeCordon(ctx, cs, "cp1", "v1.37.1")
		return err
	}); err != nil || !getNode(t, cs).Spec.Unschedulable {
		t.Fatalf("err=%v unschedulable=%v, want the admin's cordon kept", err, getNode(t, cs).Spec.Unschedulable)
	}
}

// Lifecycle through upgradeOneNode, with the Job running the backstop as it
// does on the node.
func TestCordonOwnershipThroughAnUpgrade(t *testing.T) {
	for _, c := range []struct {
		name             string
		adminCordoned    bool
		wantCordonedMid  bool // after the Job's backstop
		wantCordonedDone bool
	}{
		{"schedulable node: the Job's backstop uncordons it", false, false, false},
		{"admin-cordoned node: stays cordoned throughout", true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
			if c.adminCordoned {
				changeNode(t, cs, func(n *corev1.Node) { n.Spec.Unschedulable = true })
			}
			cordonedMid := !c.wantCordonedMid
			oldRun := runNodeJob
			t.Cleanup(func() { runNodeJob = oldRun })
			runNodeJob = func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) error {
				if !getNode(t, cs).Spec.Unschedulable {
					t.Error("the node was not cordoned for the Job")
				}
				if _, err := releaseUpgradeCordon(ctx, cs, "cp1", "v1.37.1"); err != nil {
					t.Errorf("backstop: %v", err)
				}
				cordonedMid = getNode(t, cs).Spec.Unschedulable
				return nil
			}
			if err := runSingleNode(cs); err != nil {
				t.Fatal(err)
			}
			if cordonedMid != c.wantCordonedMid {
				t.Errorf("cordoned after the backstop = %v, want %v", cordonedMid, c.wantCordonedMid)
			}
			got := getNode(t, cs)
			if got.Spec.Unschedulable != c.wantCordonedDone {
				t.Errorf("cordoned at the end = %v, want %v", got.Spec.Unschedulable, c.wantCordonedDone)
			}
			if got.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" || got.Annotations[k8sutil.UpgradeCordonAnnotation] != "" || got.Annotations[k8sutil.UpgradeInProgressAnnotation] != "" {
				t.Errorf("annotations = %v, want only the version stamp", got.Annotations)
			}
		})
	}
}

// TestFailedNodeJobLeavesTheNodeCordonedAndUnstamped: a node whose upgrade
// failed exits, so its Job fails. The orchestrator must fail with the recovery
// steps, leave the cordon it owns in place for inspection, and leave the
// version stamp unchanged — not count the node upgraded or uncordon one that
// is not at the target.
func TestFailedNodeJobLeavesTheNodeCordonedAndUnstamped(t *testing.T) {
	const target = "v1.37.1"
	cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
	const priorVersion = "v1.36.5"
	changeNode(t, cs, func(n *corev1.Node) {
		n.Annotations = map[string]string{k8sutil.VersionAnnotation: priorVersion}
	})
	swap(t, &runNodeJob, func(context.Context, kubernetes.Interface, nodejob.Spec) error {
		return errors.New("per-node upgrade failed: the Job exited non-zero")
	})
	err := runSingleNode(cs)
	if err == nil {
		t.Fatal("a failed node Job must fail the cluster upgrade")
	}
	for _, want := range []string{"get job/stratum-upgrade-cp1", "kubectl uncordon cp1", "upgrade --cluster --target " + target} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing the recovery step %q:\n%v", want, err)
		}
	}
	got := getNode(t, cs)
	if !got.Spec.Unschedulable {
		t.Error("the node was uncordoned though its upgrade Job failed")
	}
	a := got.Annotations
	if a[k8sutil.UpgradeCordonAnnotation] != target || a[k8sutil.UpgradeInProgressAnnotation] != target || a[k8sutil.VersionAnnotation] != priorVersion {
		t.Errorf("annotations = %v, want this run's cordon and in-progress claims and preserved version stamp %q", a, priorVersion)
	}
}

// A failed per-node Job is reported as failed, with its log, and nothing more:
// the orchestrator sees only the Job's status, and the node's run can end
// without a rollback, as when its post-reload gate fails with the new version
// installed.
func TestFailedNodeJobClaimsNoRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{Failed: 1})
		err := runSingleNode(cs)
		const want = "per-node upgrade Job stratum-upgrade-cp1 failed (logs: kubectl -n kube-system logs job/stratum-upgrade-cp1)"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), "rolled") {
			t.Errorf("the error claims a rollback the orchestrator cannot know of:\n%v", err)
		}
	})
}

// A controller's upgrade Job uncordons the node once its upgrade passes,
// before the orchestrator's own gates run. When one of those then fails, the
// recovery steps must not say the node is left cordoned.
func TestRecoveryStepsDoNotClaimTheCordonOfAControllerItsJobReleased(t *testing.T) {
	cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
	swap(t, &runNodeJob, func(_ context.Context, cs kubernetes.Interface, _ nodejob.Spec) error {
		_, err := runBackstop(cs, "cp1", "v1.37.1")
		return err
	})
	swap(t, &waitAPIHealthz, func(context.Context, kubernetes.Interface) error { return errAPIDown })
	err := runSingleNode(cs)
	if err == nil {
		t.Fatal("a failed API health gate must fail the cluster upgrade")
	}
	if getNode(t, cs).Spec.Unschedulable {
		t.Fatal("the Job's backstop did not uncordon the node; the test needs it to")
	}
	if strings.Contains(err.Error(), "cp1 is left cordoned") || !strings.Contains(err.Error(), "cp1 may be left cordoned") {
		t.Errorf("recovery steps for a node the Job uncordoned:\n%v", err)
	}
}

// The lease is confirmed by a renewal before the node is stamped and
// uncordoned. After the Job, the Ready wait and the gate (which can span an API
// outage) a successor may hold the lease, and its cordon and records for this
// target must not be taken for this run's own.
func TestFinishNeedsAConfirmedLease(t *testing.T) {
	holdLease := func(t *testing.T, cs kubernetes.Interface, holder string) {
		t.Helper()
		secs, now := int32(60), metav1.NewMicroTime(time.Now())
		l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: upgradeLeaseName, Namespace: cuNamespace},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, RenewTime: &now}}
		leases := cs.CoordinationV1().Leases(cuNamespace)
		if _, err := leases.Update(context.Background(), l, metav1.UpdateOptions{}); apierrors.IsNotFound(err) {
			_, err = leases.Create(context.Background(), l, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	run := func(cs *fake.Clientset, g *leaseGuard, during func(context.Context, kubernetes.Interface)) error {
		oldRun := runNodeJob
		defer func() { runNodeJob = oldRun }()
		runNodeJob = func(ctx context.Context, cs kubernetes.Interface, _ nodejob.Spec) error {
			during(ctx, cs)
			return nil
		}
		n := Node{Name: "cp1", Version: "v1.36.5", Ready: true, IsCP: true}
		_, err := upgradeOneNode(io.Discard, context.Background(), cs, n, ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", DrainGraceSec: 1, lease: g})
		return err
	}
	guard := func(cs kubernetes.Interface) *leaseGuard {
		return &leaseGuard{lastRenew: time.Now(), renew: func(ctx context.Context) error { return renewUpgradeLease(ctx, cs, "me") }}
	}

	t.Run("taken over during the Job: finishNode is not called", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
			holdLease(t, cs, "me")
			g := guard(cs)
			err := run(cs, g, func(ctx context.Context, cs kubernetes.Interface) {
				// The background loop has not seen it: the guard still reads held.
				holdLease(t, cs, "successor")
				if g.Err() != nil {
					t.Fatal("the guard already knew; the test needs it to still read held")
				}
			})
			if !errors.Is(err, errLeaseTaken) || !strings.Contains(err.Error(), "stamp version and uncordon") {
				t.Fatalf("err = %v, want the takeover", err)
			}
			got := getNode(t, cs)
			if !got.Spec.Unschedulable || got.Annotations[k8sutil.UpgradeCordonAnnotation] != "v1.37.1" ||
				got.Annotations[k8sutil.UpgradeInProgressAnnotation] != "v1.37.1" || got.Annotations[k8sutil.VersionAnnotation] != "" {
				t.Errorf("finishNode ran: unschedulable=%v annotations=%v", got.Spec.Unschedulable, got.Annotations)
			}
			if g.Err() == nil {
				t.Error("the takeover was not recorded as lost")
			}
		})
	})
	t.Run("still ours: the renewal is made and the node is finished", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
			holdLease(t, cs, "me")
			if err := run(cs, guard(cs), func(context.Context, kubernetes.Interface) {}); err != nil {
				t.Fatal(err)
			}
			if got := getNode(t, cs); got.Spec.Unschedulable || got.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" {
				t.Errorf("node not finished: unschedulable=%v annotations=%v", got.Spec.Unschedulable, got.Annotations)
			}
		})
	})
	t.Run("API still down at the check: retried until the renewal succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
			holdLease(t, cs, "me")
			down := 3
			cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
				if down > 0 {
					down--
					return true, nil, errAPIDown
				}
				return false, nil, nil
			})
			if err := run(cs, guard(cs), func(context.Context, kubernetes.Interface) {}); err != nil {
				t.Fatal(err)
			}
			if down != 0 || getNode(t, cs).Spec.Unschedulable {
				t.Errorf("down=%d unschedulable=%v, want the check retried through the outage and the node finished", down, getNode(t, cs).Spec.Unschedulable)
			}
		})
	})
}
