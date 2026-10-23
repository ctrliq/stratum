package upgrade

import (
	"io"

	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

// A node already on the target build is left alone, not drained, but the
// upgrade's own cordon is not left behind. An admin's stays.
func TestCurrentBuildReleasesOwnCordon(t *testing.T) {
	const target = "v1.37.1"
	for _, c := range []struct {
		name       string
		claim      string // "" = no claim annotation
		inProgress string
		cordoned   bool
		wantCordon bool
		wantClaim  string // "" = no claim annotation after
	}{
		{"claim of another version, dead holder", cordonClaim("v1.37.2", "dead"), "", true, false, ""},
		{"claim of this target, dead holder", cordonClaim(target, "dead"), "", true, false, ""},
		{"claim with no holder", "v1.36.9", "", true, false, ""},
		{"no claim, in-progress record of another target", "", "v1.37.2", true, false, ""},
		{"admin's preexisting cordon", cordonClaim(cordonPreexisting, "dead"), "", true, true, cordonClaim(cordonPreexisting, "dead")},
		{"admin's cordon after release", cordonClaim(cordonReleased, "dead"), "", true, true, cordonClaim(cordonReleased, "dead")},
		{"unknown claim kind", cordonClaim("v1.37.2 ", "dead"), "", true, true, cordonClaim("v1.37.2 ", "dead")},
		{"no claim, no record: admin's", "", "", true, true, ""},
		{"not cordoned: claim marked released, holder kept", cordonClaim(target, "dead"), "", false, false, cordonClaim(cordonReleased, "dead")},
		{"not cordoned: claim with no holder marked released", "v1.36.9", "", false, false, cordonReleased},
		{"not cordoned, no claim", "", "v1.37.2", false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
			changeNode(t, cs, func(n *corev1.Node) {
				n.Spec.Unschedulable = c.cordoned
				n.Annotations = map[string]string{}
				if c.claim != "" {
					n.Annotations[k8sutil.UpgradeCordonAnnotation] = c.claim
				}
				if c.inProgress != "" {
					n.Annotations[k8sutil.UpgradeInProgressAnnotation] = c.inProgress
				}
			})
			g, release, err := acquireUpgradeLease(t.Context(), cs, "me")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			old := runExitCode
			t.Cleanup(func() { runExitCode = old })
			runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return 0, nil }
			n := Node{Name: "cp1", Version: target, Ready: true, IsCP: true, InProgress: c.inProgress}
			if why := Preflight(target, []Node{n}); why != "" {
				t.Fatal(why)
			}
			out, err := upgradeOneNode(io.Discard, t.Context(), cs, n, ClusterOptions{Target: target, From: "/var/lib/stratum/upgrade/next", lease: g})
			got := getNode(t, cs)
			if err != nil || out != nodeCurrent || got.Spec.Unschedulable != c.wantCordon {
				t.Fatalf("out=%v err=%v unschedulable=%v, want %v (annotations %v)", out, err, got.Spec.Unschedulable, c.wantCordon, got.Annotations)
			}
			if gotClaim := got.Annotations[k8sutil.UpgradeCordonAnnotation]; gotClaim != c.wantClaim {
				t.Errorf("claim = %q, want %q", gotClaim, c.wantClaim)
			}
		})
	}
}

// A run that no longer holds the lease releases nothing, and does not report
// the node current, whoever's claim the node carries or whether it needs a
// write: a successor may have taken the lease and claimed the node.
func TestCurrentBuildLostLeaseLeavesCordon(t *testing.T) {
	const target = "v1.37.1"
	for _, c := range []struct{ name, claim string }{
		{"another run's claim", cordonClaim("v1.37.2", "other")},
		{"admin's cordon", cordonClaim(cordonPreexisting, "other")},
		{"no claim", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
			changeNode(t, cs, func(n *corev1.Node) {
				n.Spec.Unschedulable = true
				n.Annotations = map[string]string{}
				if c.claim != "" {
					n.Annotations[k8sutil.UpgradeCordonAnnotation] = c.claim
				}
			})
			g, release, err := acquireUpgradeLease(t.Context(), cs, "me")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			// The successor takes the lease over, as after this run's expiry.
			other := "other"
			l, err := cs.CoordinationV1().Leases(cuNamespace).Get(t.Context(), upgradeLeaseName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			l.Spec.HolderIdentity = &other
			if _, err := cs.CoordinationV1().Leases(cuNamespace).Update(t.Context(), l, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			old := runExitCode
			t.Cleanup(func() { runExitCode = old })
			runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return 0, nil }
			n := Node{Name: "cp1", Version: target, Ready: true, IsCP: true}
			out, err := upgradeOneNode(io.Discard, t.Context(), cs, n, ClusterOptions{Target: target, From: "/var/lib/stratum/upgrade/next", lease: g})
			got := getNode(t, cs)
			if err == nil || out == nodeCurrent || !got.Spec.Unschedulable || got.Annotations[k8sutil.UpgradeCordonAnnotation] != c.claim {
				t.Fatalf("out=%v err=%v unschedulable=%v annotations=%v, want an error and the node untouched", out, err, got.Spec.Unschedulable, got.Annotations)
			}
		})
	}
}

// The confirm after the node is read, before the write, is the only fence on a
// successor that takes the lease and claims the node during the read, after the
// first confirm: the read already returns its claim, so the write's
// resourceVersion check cannot refuse it.
func TestCurrentBuildLeaseTakenDuringReadLeavesCordon(t *testing.T) {
	const target = "v1.37.1"
	cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
	changeNode(t, cs, func(n *corev1.Node) {
		n.Spec.Unschedulable = true
		n.Annotations = map[string]string{k8sutil.UpgradeCordonAnnotation: cordonClaim("v1.37.2", "dead")}
	})
	g, release, err := acquireUpgradeLease(t.Context(), cs, "me")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	// Inside the first node GET after a confirm, before it returns, the successor
	// takes the lease and claims the node. Once only.
	renew, confirmed, taken := g.renew, false, false
	g.renew = func(ctx context.Context) error {
		err := renew(ctx)
		confirmed = err == nil
		return err
	}
	successorClaim := cordonClaim("v1.37.2", "other")
	cs.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !confirmed || taken {
			return false, nil, nil
		}
		taken = true
		// Through the tracker: the clientset holds its lock while a reactor runs.
		leaseGVR := coordinationv1.SchemeGroupVersion.WithResource("leases")
		obj, err := cs.Tracker().Get(leaseGVR, cuNamespace, upgradeLeaseName)
		if err != nil {
			t.Fatal(err)
		}
		l, other := obj.(*coordinationv1.Lease).DeepCopy(), "other"
		l.Spec.HolderIdentity = &other
		if err := cs.Tracker().Update(leaseGVR, l, cuNamespace); err != nil {
			t.Fatal(err)
		}
		changeNode(t, cs, func(n *corev1.Node) { n.Annotations[k8sutil.UpgradeCordonAnnotation] = successorClaim })
		return false, nil, nil
	})
	old := runExitCode
	t.Cleanup(func() { runExitCode = old })
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return 0, nil }
	n := Node{Name: "cp1", Version: target, Ready: true, IsCP: true}
	out, err := upgradeOneNode(io.Discard, t.Context(), cs, n, ClusterOptions{Target: target, From: "/var/lib/stratum/upgrade/next", lease: g})
	// Before the verification read below, which the reactor would also answer.
	if !taken {
		t.Fatal("the successor never took the lease: the test did not reach the read")
	}
	got := getNode(t, cs)
	if err == nil || out == nodeCurrent || !got.Spec.Unschedulable || got.Annotations[k8sutil.UpgradeCordonAnnotation] != successorClaim {
		t.Fatalf("out=%v err=%v unschedulable=%v annotations=%v, want an error and the successor's claim untouched", out, err, got.Spec.Unschedulable, got.Annotations)
	}
}
