package upgrade

import (
	"context"
	"log"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Drain PDB-aware-evicts pods on node (skipping DaemonSet/mirror/completed pods,
// the upgrade controller, and the orchestrator's own per-node Job pod). Returns
// the count of pods STILL needing to leave; drain is complete only at 0. Evicts
// through the typed policy/v1 Eviction API.
func Drain(ctx context.Context, cs kubernetes.Interface, node, jobPrefix string, graceSec int64) (int, error) {
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return 1, err // list failed; the orchestrator halts the rollout on this error
	}
	remaining := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if shouldSkipPod(p, jobPrefix) {
			continue
		}
		if p.DeletionTimestamp != nil {
			remaining++ // already terminating -> wait
			continue
		}
		grace := graceSec
		ev := &policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
			DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: &grace},
		}
		err := cs.PolicyV1().Evictions(p.Namespace).Evict(ctx, ev)
		if apierrors.IsNotFound(err) {
			continue // already gone
		}
		remaining++ // accepted(->terminating), PDB-blocked(429), or transient -> wait/retry
		if err != nil && !apierrors.IsTooManyRequests(err) {
			log.Printf("drain: evict %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	return remaining, nil
}

// shouldSkipPod reports whether a pod is exempt from a drain. Pure (unit-tested).
func shouldSkipPod(p *corev1.Pod, jobPrefix string) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	if _, mirror := p.Annotations["kubernetes.io/config.mirror"]; mirror {
		return true
	}
	if p.Labels["app"] == "stratum-upgrade-controller" {
		return true
	}
	if p.Namespace == cuNamespace && strings.HasPrefix(p.Name, jobPrefix) {
		return true
	}
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return true
	}
	return false
}
