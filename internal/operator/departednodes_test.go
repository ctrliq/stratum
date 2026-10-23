package operator

import (
	"context"
	"slices"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// markedNode is a worker marked by uninstall at mark, Ready or not.
func markedNode(name string, ready bool, mark time.Time) corev1.Node {
	n := node(name, ready)
	n.UID = types.UID("uid-" + name)
	n.Annotations = map[string]string{k8sutil.UninstalledAnnotation: k8sutil.UninstallMark(mark)}
	return n
}

func lease(name string, renewed time.Time) coordinationv1.Lease {
	return coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: corev1.NamespaceNodeLease},
		Spec: coordinationv1.LeaseSpec{RenewTime: &metav1.MicroTime{Time: renewed}}}
}

// TestDepartedWorkers is the rule table. Uninstall marks only once
// kubelet has stopped, so a worker whose last renewal came before the mark and
// is past the grace period is removed; one whose kubelet renewed since the mark
// ran again (reinstalled under the same name, onto the same Node), so its mark
// is stale; anything still within the grace period, Ready, never marked, or a
// control plane is left.
func TestDepartedWorkers(t *testing.T) {
	now := epoch
	mark := now.Add(-10 * time.Minute)
	stopped := mark.Add(-5 * time.Second)

	cp := markedNode("cp2", false, mark)
	cp.Labels = map[string]string{k8sutil.ControlPlaneLabel: "true"}
	stdCP := markedNode("cp3", false, mark)
	stdCP.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	legacyCP := markedNode("cp4", false, mark)
	legacyCP.Labels = map[string]string{"node-role.kubernetes.io/master": ""}
	markSub := mark.Add(700 * time.Millisecond)
	unmarked := node("w-unmarked", false)
	badMark := markedNode("w-badmark", false, mark)
	badMark.Annotations[k8sutil.UninstalledAnnotation] = "yesterday"
	deleting := markedNode("w-deleting", false, mark)
	deleting.DeletionTimestamp = &metav1.Time{Time: now}
	noRenew := lease("w1", time.Time{})
	noRenew.Spec.RenewTime = nil

	for _, tc := range []struct {
		name  string
		node  corev1.Node
		lease *coordinationv1.Lease
		want  string
	}{
		{"gone after its uninstall", markedNode("w1", false, mark), leaseRef(lease("w1", stopped)), "gone"},
		{"still Ready", markedNode("w1", true, mark), leaseRef(lease("w1", stopped)), "left"},
		{"gone for just under the grace period", markedNode("w1", false, now.Add(-time.Minute)), leaseRef(lease("w1", now.Add(-departedGrace+time.Second))), "left"},
		{"gone for exactly the grace period", markedNode("w1", false, now.Add(-time.Minute)), leaseRef(lease("w1", now.Add(-departedGrace))), "gone"},
		{"last renewed at the mark", markedNode("w1", false, mark), leaseRef(lease("w1", mark)), "gone"},
		{"reinstalled a second after the mark and down again", markedNode("w1", false, mark), leaseRef(lease("w1", mark.Add(time.Second))), "stale"},
		{"a control plane by the standard label", stdCP, leaseRef(lease("cp3", stopped)), "left"},
		{"a control plane by the legacy label", legacyCP, leaseRef(lease("cp4", stopped)), "left"},
		{"last renewed earlier in the second of the mark", markedNode("w1", false, markSub), leaseRef(lease("w1", markSub.Add(-200*time.Millisecond))), "gone"},
		{"reinstalled and Ready", markedNode("w1", true, mark), leaseRef(lease("w1", now)), "stale"},
		{"reinstalled two minutes after the mark", markedNode("w1", false, mark), leaseRef(lease("w1", mark.Add(2*time.Minute))), "stale"},
		{"a lease renewed at the zero time", markedNode("w1", false, mark), leaseRef(lease("w1", time.Time{})), "left"},
		{"not marked", unmarked, leaseRef(lease("w-unmarked", stopped)), "left"},
		{"a control plane", cp, leaseRef(lease("cp2", stopped)), "left"},
		{"a mark that does not parse", badMark, leaseRef(lease("w-badmark", time.Time{})), "left"},
		{"no lease", markedNode("w1", false, mark), nil, "left"},
		{"a lease with no renewal", markedNode("w1", false, mark), &noRenew, "left"},
		{"already being deleted", deleting, leaseRef(lease("w-deleting", stopped)), "left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := map[string]coordinationv1.Lease{}
			if tc.lease != nil {
				leases[tc.lease.Name] = *tc.lease
			}
			gone, stale := departedWorkers([]corev1.Node{tc.node}, leases, now)
			got := "left"
			switch {
			case len(gone) == 1 && len(stale) == 0:
				got = "gone"
			case len(stale) == 1 && len(gone) == 0:
				got = "stale"
			case len(gone)+len(stale) > 0:
				got = "both"
			}
			if got != tc.want {
				t.Errorf("departedWorkers: %s, want %s", got, tc.want)
			}
		})
	}
}

func leaseRef(l coordinationv1.Lease) *coordinationv1.Lease { return &l }

// markPolicy is the node-mark admission policy and its denying binding, as
// the manifest deployer applies them.
func markPolicy() []runtime.Object {
	return []runtime.Object{
		&admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: k8sutil.NodeMarkPolicy}},
		&admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: k8sutil.NodeMarkPolicy},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: k8sutil.NodeMarkPolicy,
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}}},
	}
}

// TestReconcileDepartedWorkersActsOnlyOnTheNodeItRead: the departed worker is
// deleted with preconditions on the UID and resourceVersion it was read with,
// the reinstalled one loses its stale mark and stays, and a live one is left.
func TestReconcileDepartedWorkersActsOnlyOnTheNodeItRead(t *testing.T) {
	now := epoch
	mark := now.Add(-10 * time.Minute)
	gone, back, alive := markedNode("w1", false, mark), markedNode("w2", true, mark), node("w3", true)
	gone.ResourceVersion, back.ResourceVersion = "7", "8"
	l1, l2 := lease("w1", mark.Add(-5*time.Second)), lease("w2", now)
	cs := fake.NewClientset(append(markPolicy(), &gone, &back, &alive, &l1, &l2)...)

	reconcileDepartedWorkers(context.Background(), cs, now)

	nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, n := range nodes.Items {
		left = append(left, n.Name)
		if _, marked := n.Annotations[k8sutil.UninstalledAnnotation]; marked {
			t.Errorf("Node %s still carries the uninstall mark", n.Name)
		}
	}
	if !slices.Equal(left, []string{"w2", "w3"}) {
		t.Errorf("nodes left = %q, want the reinstalled and the live worker", left)
	}
	var sawPreconditions bool
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && d.GetResource().Resource == "nodes" {
			p := d.GetDeleteOptions().Preconditions
			sawPreconditions = p != nil && p.UID != nil && *p.UID == "uid-w1" && p.ResourceVersion != nil && *p.ResourceVersion == "7"
		}
	}
	if !sawPreconditions {
		t.Error("the Node was deleted without preconditions on the UID and resourceVersion it was read with")
	}
}

// TestReconcileDepartedWorkersRereadsTheLeaseBeforeDeleting:
// no precondition covers the Lease, so it is read again just before the
// delete, and a kubelet that renewed since the listing is not deleted.
func TestReconcileDepartedWorkersRereadsTheLeaseBeforeDeleting(t *testing.T) {
	now := epoch
	mark := now.Add(-10 * time.Minute)
	w := markedNode("w1", false, mark)
	l := lease("w1", mark.Add(-5*time.Second))
	cs := fake.NewClientset(append(markPolicy(), &w, &l)...)
	gets := 0
	cs.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return false, nil, nil
		}
		renewed := lease("w1", now)
		return true, &renewed, nil
	})

	reconcileDepartedWorkers(context.Background(), cs, now)

	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{}); err != nil {
		t.Errorf("a worker whose kubelet renewed between the listing and the delete was deleted: %v", err)
	}
	if gets != 2 {
		t.Errorf("the lease was read %d times, want it read again before the delete", gets)
	}
}

// TestReconcileDepartedWorkersDeletesNothingWithoutTheMarkPolicy: only the
// admission policy makes a mark the node's own, so without it, or with a
// binding that only warns, nothing is deleted; a stale mark is still taken off,
// which removes nothing.
func TestReconcileDepartedWorkersDeletesNothingWithoutTheMarkPolicy(t *testing.T) {
	now := epoch
	mark := now.Add(-10 * time.Minute)
	warnOnly := markPolicy()
	warnOnly[1].(*admissionregistrationv1.ValidatingAdmissionPolicyBinding).Spec.ValidationActions =
		[]admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn}
	for name, policy := range map[string][]runtime.Object{
		"no policy":                 nil,
		"no binding":                markPolicy()[:1],
		"a binding that only warns": warnOnly,
	} {
		t.Run(name, func(t *testing.T) {
			gone, back := markedNode("w1", false, mark), markedNode("w2", true, mark)
			l1, l2 := lease("w1", mark.Add(-5*time.Second)), lease("w2", now)
			cs := fake.NewClientset(append(policy, &gone, &back, &l1, &l2)...)

			reconcileDepartedWorkers(context.Background(), cs, now)

			if _, err := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{}); err != nil {
				t.Errorf("a marked Node was deleted with the mark policy not enforced: %v", err)
			}
			if n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w2", metav1.GetOptions{}); n != nil {
				if _, marked := n.Annotations[k8sutil.UninstalledAnnotation]; marked {
					t.Error("the stale mark was left on the reinstalled worker")
				}
			}
		})
	}
}
