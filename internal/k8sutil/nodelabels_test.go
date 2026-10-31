package k8sutil

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcileNodeLabels(t *testing.T) {
	cs := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: map[string]string{
		"kubernetes.io/hostname":             "gpu-1",
		"topology.kubernetes.io/zone":        "rack-1",
		RuntimeHandlerLabelPrefix + "nvidia": "true",
		RuntimeHandlerLabelPrefix + "old":    "true",
		"team":                               "ml",
		"stratum.ciq.com/control-plane":      "true",
	}}})
	declared := map[string]string{"topology.kubernetes.io/zone": "rack-2", "pool": "h200"}
	if err := reconcileNodeLabels(context.Background(), cs, "gpu-1", declared, []string{"nvidia", "gvisor"}); err != nil {
		t.Fatal(err)
	}
	n, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"kubernetes.io/hostname":             "gpu-1",
		"topology.kubernetes.io/zone":        "rack-2",
		"pool":                               "h200",
		RuntimeHandlerLabelPrefix + "nvidia": "true",
		RuntimeHandlerLabelPrefix + "gvisor": "true",
		"team":                               "ml",
		"stratum.ciq.com/control-plane":      "true",
	}
	for k, v := range want {
		if n.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, n.Labels[k], v)
		}
	}
	if _, ok := n.Labels[RuntimeHandlerLabelPrefix+"old"]; ok {
		t.Error("the label of a handler the node no longer declares survived")
	}
	if len(n.Labels) != len(want) {
		t.Errorf("labels = %v, want exactly %v", n.Labels, want)
	}

	cs.ClearActions()
	if err := reconcileNodeLabels(context.Background(), cs, "gpu-1", declared, []string{"gvisor", "nvidia"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("a start that changes nothing sent %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestNodeLabelChangesPatchesLabelsOnly(t *testing.T) {
	if c := nodeLabelChanges(map[string]string{"a": "1"}, map[string]string{"a": "1"}, nil); c != nil {
		t.Errorf("an unchanged node produced changes %v", c)
	}
	c := nodeLabelChanges(nil, nil, []string{"nvidia"})
	if v := c[RuntimeHandlerLabelPrefix+"nvidia"]; v == nil || *v != "true" {
		t.Errorf("a node with no labels was not given its handler label: %v", c)
	}
}
