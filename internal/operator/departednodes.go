// Only the node itself can write the uninstall mark; the admission policy refuses everyone else.
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
	departedGrace = 2 * time.Minute
)

// Mark and renewal carry sub-second precision, so a renewal in the mark's second still compares.
type departure struct {
	node    corev1.Node
	marked  time.Time
	renewed time.Time
}

// A renewal after the mark means a kubelet ran again: a reinstalled worker re-registers.
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

// Marks written before the policy shipped cannot be told from the node's own.
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

// Deletes carry preconditions on the UID and resourceVersion the Node was read with.
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
		// Only the admission policy makes a mark the node's own.
		if err := markPolicyEnforced(ctx, cs); err != nil {
			log.Printf("stratum-operator: departed workers: not deleting %d marked Node(s): %v", len(gone), err)
			gone = nil
		}
	}
	for _, d := range gone {
		// No precondition covers the Lease, so it is re-read just before the delete.
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
