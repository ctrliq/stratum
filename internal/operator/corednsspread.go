// corednsspread.go — move a CoreDNS replica off a node that holds all of them.
// The anti-affinity is preferred, so it is only consulted at scheduling time
// and never rebalances. Eviction, not delete, so a PodDisruptionBudget is
// honoured.
package operator

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	corednsNamespace  = "kube-system"
	corednsDeployment = "coredns"
	// spreadCooldown bounds how often a replica is moved.
	spreadCooldown = 5 * time.Minute
)

// corednsSpreader moves a CoreDNS replica off a node that holds all of them,
// at most one move per spreadCooldown. One value per operator leader, built by
// Run and driven by the leader loop's spread step: the last-move time is the
// value's own clock state, not package state, so a fresh leader starts with a
// cooldown that has never fired. now, the time of the pass a move is counted
// at, is read through reconcileCoreDNSSpread.
type corednsSpreader struct {
	lastMove time.Time
}

// corednsToMove decides which replica, if any, to evict, and why. It moves
// nothing while anything is in flux. Pure.
func corednsToMove(pods []corev1.Pod, nodes []corev1.Node) (pod, reason string) {
	var live []corev1.Pod
	for _, p := range pods {
		if p.DeletionTimestamp == nil {
			live = append(live, p)
		}
	}
	if len(live) < 2 {
		return "", fmt.Sprintf("%d live replica(s); nothing to spread", len(live))
	}
	for _, p := range live {
		if p.Spec.NodeName == "" || p.Status.Phase != corev1.PodRunning || !podReady(p) {
			return "", fmt.Sprintf("replica %s is not Running and Ready; waiting", p.Name)
		}
	}
	host := live[0].Spec.NodeName
	for _, p := range live[1:] {
		if p.Spec.NodeName != host {
			return "", "replicas already span more than one node"
		}
	}
	target := ""
	for _, n := range nodes {
		if n.Name != host && canTake(n, live[0].Spec.Tolerations) {
			target = n.Name
			break
		}
	}
	if target == "" {
		return "", fmt.Sprintf("every replica is on %s and no other node can take one", host)
	}
	// The newest replica, name breaking a tie for determinism.
	sort.Slice(live, func(i, j int) bool {
		ti, tj := live[i].CreationTimestamp.Time, live[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return live[i].Name > live[j].Name
	})
	return live[0].Name, fmt.Sprintf("every replica is on %s; %s can take one", host, target)
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// canTake reports whether a replica with these tolerations could run on n.
func canTake(n corev1.Node, tolerations []corev1.Toleration) bool {
	if n.Spec.Unschedulable {
		return false
	}
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return false
	}
	logger := klog.Background()
	for i := range n.Spec.Taints {
		taint := &n.Spec.Taints[i]
		if taint.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for j := range tolerations {
			if tolerations[j].ToleratesTaint(logger, taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// managedCoreDNSSelector returns the pod selector of the CoreDNS Deployment
// this operator's deployer applied, or "" when there is none to act on. Only
// Stratum's own CoreDNS is ever moved.
func managedCoreDNSSelector(ctx context.Context, cs kubernetes.Interface) (string, error) {
	d, err := cs.AppsV1().Deployments(corednsNamespace).Get(ctx, corednsDeployment, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if d.Labels[group+"/stack"] != CoreDNSStack || d.Spec.Selector == nil {
		return "", nil
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return "", err
	}
	return sel.String(), nil
}

// reconcileCoreDNSSpread runs on the operator leader. Errors are logged,
// never fatal. now is the pass's clock reading, the time a move is counted at.
func (s *corednsSpreader) reconcileCoreDNSSpread(ctx context.Context, cs kubernetes.Interface, now time.Time) {
	if !s.lastMove.IsZero() && now.Sub(s.lastMove) < spreadCooldown {
		return
	}
	selector, err := managedCoreDNSSelector(ctx, cs)
	if err != nil {
		log.Printf("coredns spread: read the CoreDNS Deployment: %v", err)
		return
	}
	if selector == "" {
		return
	}
	pods, err := cs.CoreV1().Pods(corednsNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		log.Printf("coredns spread: list replicas: %v", err)
		return
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("coredns spread: list nodes: %v", err)
		return
	}
	name, why := corednsToMove(pods.Items, nodes.Items)
	if name == "" {
		return
	}
	err = cs.CoreV1().Pods(corednsNamespace).EvictV1(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: corednsNamespace},
	})
	// Counted even when it fails, so a refusal waits out the cooldown.
	s.lastMove = now
	if err != nil {
		log.Printf("coredns spread: %s; evicting %s failed: %v", why, name, err)
		return
	}
	log.Printf("coredns spread: %s; evicted %s so it reschedules there", why, name)
}
