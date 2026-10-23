package upgrade

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShouldSkipPod(t *testing.T) {
	ds := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "a", Namespace: "x",
		OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet"}}}}
	mirror := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "b", Namespace: "x",
		Annotations: map[string]string{"kubernetes.io/config.mirror": "y"}}}
	done := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "x"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded}}
	ownJob := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stratum-upgrade-n1-abc", Namespace: "kube-system"}}
	normal := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet"}}}}

	if !shouldSkipPod(ds, "stratum-upgrade-n1") {
		t.Error("DaemonSet pod should be skipped")
	}
	if !shouldSkipPod(mirror, "stratum-upgrade-n1") {
		t.Error("mirror pod should be skipped")
	}
	if !shouldSkipPod(done, "stratum-upgrade-n1") {
		t.Error("completed pod should be skipped")
	}
	if !shouldSkipPod(ownJob, "stratum-upgrade-n1") {
		t.Error("the orchestrator's own job pod should be skipped")
	}
	if shouldSkipPod(normal, "stratum-upgrade-n1") {
		t.Error("a normal workload pod should NOT be skipped")
	}
}
