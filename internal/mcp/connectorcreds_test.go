package mcp

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func setThisNodeName(t *testing.T, name string) {
	t.Helper()
	old := thisNodeName
	t.Cleanup(func() { thisNodeName = old })
	thisNodeName = func() (string, error) { return name, nil }
}

func kubeSystem(uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)}}
}

// A cluster-wide operation runs on the server's credential only when that
// credential addresses this node's own cluster, proven by the kube-system
// namespace's UID and not only by a Node of this node's name.
func TestRequireThisNodeInProvesTheCluster(t *testing.T) {
	setThisNodeName(t, "cp1")
	local := fake.NewSimpleClientset(kubeSystem("uid-prod"))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1"}}

	same := fake.NewSimpleClientset(node, kubeSystem("uid-prod"))
	if err := requireThisNodeIn(context.Background(), same, "https://10.0.0.1:6443", local, "https://10.0.0.1:6443", repointByFlags); err != nil {
		t.Fatalf("this node's own cluster: %v", err)
	}

	twin := fake.NewSimpleClientset(node.DeepCopy(), kubeSystem("uid-staging"))
	err := requireThisNodeIn(context.Background(), twin, "https://10.9.9.9:6443", local, "https://10.0.0.1:6443", repointByFlags)
	if err == nil || !strings.Contains(err.Error(), "is not this node's cluster") ||
		!strings.Contains(err.Error(), "https://10.9.9.9:6443") || !strings.Contains(err.Error(), "https://10.0.0.1:6443") {
		t.Fatalf("another cluster with a node of the same name: %v, want a refusal naming both clusters", err)
	}

	elsewhere := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other"}}, kubeSystem("uid-staging"))
	err = requireThisNodeIn(context.Background(), elsewhere, "https://10.9.9.9:6443", local, "https://10.0.0.1:6443", repointNodeCredential)
	if err == nil || !strings.Contains(err.Error(), "is not in the cluster the MCP credential addresses") {
		t.Fatalf("a cluster without this node: %v, want a refusal", err)
	}
}

// The server URL in a refusal never carries the credentials a kubeconfig can
// embed in it.
func TestRedactHostDropsUserInfo(t *testing.T) {
	if got := redactHost("https://admin:s3cret@10.0.0.1:6443"); strings.Contains(got, "s3cret") || got != "https://10.0.0.1:6443" {
		t.Errorf("redactHost = %q", got)
	}
	if got := redactHost("https://10.0.0.1:6443"); got != "https://10.0.0.1:6443" {
		t.Errorf("redactHost changed a plain URL: %q", got)
	}
}
