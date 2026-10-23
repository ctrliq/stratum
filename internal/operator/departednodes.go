// departednodes.go — remove the Node of a worker that `stratum uninstall` took
// off the cluster. The worker's own credential cannot delete its Node (the Node
// authorizer does not let system:node:<name> delete nodes), so it marks the
// Node as it leaves and this deletes it once it is gone. Only the
// node itself can write the mark: the stratum-node-uninstall-mark admission
// policy (internal/addons) refuses it from anyone else.
package operator

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

const (
	// departedGrace is how long a marked worker's kubelet must have stopped
	// renewing its Lease: the kubelet renews every 10s, so a live one never
	// gets near it.
	departedGrace = 2 * time.Minute
)

// departure is what departedWorkers decided about one marked worker. The mark
// is RFC 3339 with sub-second precision, the Lease's own, so a renewal in the
// same second as the mark still compares correctly; time.RFC3339 parses both.
type departure struct {
	node    corev1.Node
	marked  time.Time
	renewed time.Time
}

// departedWorkers sorts the marked workers into those to delete, not Ready,
// whose kubelet last renewed its Lease before the mark and not for
// departedGrace, and those whose mark is stale, whose kubelet renewed after
// it. Uninstall marks only once kubelet has stopped, and the mark and the
// renewal are both the worker's clock, so any renewal after the mark means a
// kubelet ran again since: a worker reinstalled under the same name, which
// re-registers onto the same Node, mark and all. A control plane, by either
// label, is never among either: removing one also takes its etcd member,
// which is the cluster's decision. A Node with no Lease, a mark that does not
// parse, or a kubelet gone for less than departedGrace is left for a later
// pass. Pure.
func departedWorkers(nodes []corev1.Node, leases map[string]coordinationv1.Lease, now time.Time) (gone, stale []departure) {
	for _, n := range nodes {
		mark, ok := n.Annotations[k8sutil.UninstalledAnnotation]
		if !ok || controlPlane(n) || n.DeletionTimestamp != nil {
			continue
		}
		marked, err := time.Parse(time.RFC3339, mark)
		if err != nil {
			continue
		}
		l, ok := leases[n.Name]
		if !ok || l.Spec.RenewTime == nil || l.Spec.RenewTime.IsZero() {
			continue
		}
		d := departure{node: n, marked: marked, renewed: l.Spec.RenewTime.Time}
		switch {
		case d.renewed.After(marked):
			stale = append(stale, d)
		case !nodeReady(n) && now.Sub(d.renewed) >= departedGrace:
			gone = append(gone, d)
		}
	}
	return gone, stale
}

// controlPlane is any control-plane label: Stratum's own, which the upgrade
// machinery reads, Kubernetes' standard role label, or its legacy one.
func controlPlane(n corev1.Node) bool {
	if n.Labels[k8sutil.ControlPlaneLabel] == "true" {
		return true
	}
	for _, l := range []string{"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"} {
		if _, ok := n.Labels[l]; ok {
			return true
		}
	}
	return false
}

// markPolicyEnforced is nil while the node-mark admission policy and a binding
// that denies on it are both in place. Marks written before they were cannot
// be told from the node's own: the policy ships in the same manifest pass that
// first starts this reconciler, so that is only a cluster upgraded into it.
func markPolicyEnforced(ctx context.Context, cs kubernetes.Interface) error {
	adm := cs.AdmissionregistrationV1()
	if _, err := adm.ValidatingAdmissionPolicies().Get(ctx, k8sutil.NodeMarkPolicy, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("the %s admission policy is not in place: %w", k8sutil.NodeMarkPolicy, err)
	}
	b, err := adm.ValidatingAdmissionPolicyBindings().Get(ctx, k8sutil.NodeMarkPolicy, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("the %s admission policy binding is not in place: %w", k8sutil.NodeMarkPolicy, err)
	}
	if b.Spec.PolicyName != k8sutil.NodeMarkPolicy || !slices.Contains(b.Spec.ValidationActions, admissionregistrationv1.Deny) {
		return fmt.Errorf("the %s binding does not deny on its policy", k8sutil.NodeMarkPolicy)
	}
	return nil
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// reconcileDepartedWorkers deletes the Nodes departedWorkers picks and takes
// the stale marks off, each logged. Both carry preconditions on the Node as it
// was read: its UID, so a Node re-registered under the same name since is not
// the one changed, and its resourceVersion, so a Node that changed since the
// read is not acted on from what it looked like before.
func reconcileDepartedWorkers(ctx context.Context, cs kubernetes.Interface, now time.Time) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("stratum-operator: departed workers: list nodes: %v", err)
		return
	}
	leases := map[string]coordinationv1.Lease{}
	for _, n := range nodes.Items {
		if _, ok := n.Annotations[k8sutil.UninstalledAnnotation]; !ok {
			continue
		}
		l, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, n.Name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.Printf("stratum-operator: departed workers: read the lease of %s: %v", n.Name, err)
			}
			continue
		}
		leases[n.Name] = *l
	}
	gone, stale := departedWorkers(nodes.Items, leases, now)
	if len(gone) > 0 {
		// Only the admission policy makes a mark the node's own. Without it,
		// anyone allowed to patch nodes could have written one, so nothing is
		// deleted until it is in place again. Taking a stale
		// mark off below needs no such proof: it removes nothing.
		if err := markPolicyEnforced(ctx, cs); err != nil {
			log.Printf("stratum-operator: departed workers: not deleting %d marked Node(s): %v", len(gone), err)
			gone = nil
		}
	}
	for _, d := range gone {
		// The Lease is read again just before the delete, which no
		// precondition can cover: a kubelet that renewed since the listing is
		// not deleted on the age it had then.
		l, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, d.node.Name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.Printf("stratum-operator: departed workers: read the lease of %s again: %v", d.node.Name, err)
			}
			continue
		}
		again, _ := departedWorkers([]corev1.Node{d.node}, map[string]coordinationv1.Lease{d.node.Name: *l}, now)
		if len(again) == 0 {
			continue
		}
		d = again[0]
		uid, rv := d.node.UID, d.node.ResourceVersion
		err = cs.CoreV1().Nodes().Delete(ctx, d.node.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		switch {
		case err == nil:
			log.Printf("stratum-operator: departed workers: deleted Node %s: `stratum uninstall` marked it at %s, and its kubelet last renewed its lease %s ago",
				d.node.Name, d.marked.Format(time.RFC3339), now.Sub(d.renewed).Round(time.Second))
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			log.Printf("stratum-operator: departed workers: delete Node %s: %v", d.node.Name, err)
		}
	}
	for _, d := range stale {
		patch := fmt.Appendf(nil, `{"metadata":{"uid":%q,"resourceVersion":%q,"annotations":{%q:null}}}`,
			d.node.UID, d.node.ResourceVersion, k8sutil.UninstalledAnnotation)
		_, err := cs.CoreV1().Nodes().Patch(ctx, d.node.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		switch {
		case err == nil:
			log.Printf("stratum-operator: departed workers: took the stale uninstall mark off Node %s: marked at %s, but its kubelet has renewed its lease since, at %s",
				d.node.Name, d.marked.Format(time.RFC3339), d.renewed.Format(time.RFC3339))
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			log.Printf("stratum-operator: departed workers: take the stale mark off Node %s: %v", d.node.Name, err)
		}
	}
}
