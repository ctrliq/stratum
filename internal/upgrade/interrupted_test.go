package upgrade

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// The kubelet version alone cannot tell finished from interrupted-after-swap.
func TestNeedsUpgradeFinishesAnInterruptedNode(t *testing.T) {
	const target = "v1.37.9"
	for _, c := range []struct {
		name        string
		n           Node
		interrupted string
		needs       bool
	}{
		{"behind", Node{Version: "v1.37.1"}, "", true},
		{"finished", Node{Version: target}, "", false},
		{"interrupted after its kubelet swap", Node{Version: target, InProgress: target}, target, true},
		{"interrupted before it: an ordinary upgrade", Node{Version: "v1.37.1", InProgress: target}, "", true},
		{"rolled back since: the record is stale", Node{Version: "v1.37.1", InProgress: "v1.37.5"}, "", true},
		{"at target, stale record of another", Node{Version: target, InProgress: "v1.37.5"}, "", false},
	} {
		if got := c.n.Interrupted(); got != c.interrupted {
			t.Errorf("%s: Interrupted() = %q, want %q", c.name, got, c.interrupted)
		}
		if got := c.n.NeedsUpgrade(target); got != c.needs {
			t.Errorf("%s: NeedsUpgrade(%s) = %v, want %v", c.name, target, got, c.needs)
		}
	}
}

// Refusing at preflight does it before anything is drained.
func TestPreflightRefusesAnotherInterruptedUpgrade(t *testing.T) {
	nodes := cpNodes("v1.37.1", 3)
	nodes = append(nodes, Node{Name: "w1", Ready: true, Version: "v1.37.5", InProgress: "v1.37.5"})

	reason := Preflight("v1.37.9", nodes)
	for _, want := range []string{"w1", "unfinished upgrade to v1.37.5", "--target v1.37.5", "stratum rollback"} {
		if !strings.Contains(reason, want) {
			t.Errorf("preflight reason missing %q: %q", want, reason)
		}
	}
	// Finishing that same upgrade is exactly what is allowed.
	if reason := Preflight("v1.37.5", nodes); reason != "" {
		t.Errorf("finishing the interrupted upgrade was refused: %s", reason)
	}
}

// The stamp and the record's removal are one write, so no reader sees a stamped node in progress.
func TestUpgradeRecordLifecycle(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}})
	annotations := func() map[string]string {
		n, err := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return n.Annotations
	}

	if err := markUpgradeInProgress(ctx, cs, "w1", "v1.37.9", ""); err != nil {
		t.Fatal(err)
	}
	if got := annotations()[k8sutil.UpgradeInProgressAnnotation]; got != "v1.37.9" {
		t.Errorf("in-progress record = %q, want v1.37.9", got)
	}

	if err := finishNode(ctx, cs, "w1", "v1.37.9", "v1.37.9-stratum.0", ""); err != nil {
		t.Fatal(err)
	}
	a := annotations()
	if got := a[k8sutil.VersionAnnotation]; got != "v1.37.9" {
		t.Errorf("version stamp = %q, want v1.37.9", got)
	}
	if got := a[k8sutil.StratumVersionAnnotation]; got != "v1.37.9-stratum.0" {
		t.Errorf("stratum version stamp = %q, want the staged binary's v1.37.9-stratum.0", got)
	}
	if got, ok := a[k8sutil.UpgradeInProgressAnnotation]; ok {
		t.Errorf("a finished node is still recorded in progress (%q)", got)
	}
}

// A node at target may still run another stratum build of the release; NeedsBuildCheck decides.
func TestNodesToUpgradeVisitsEveryNode(t *testing.T) {
	const target = "v1.37.9"
	ordered := []Node{
		{Name: "cp1", IsCP: true, Version: target},
		{Name: "cp2", IsCP: true, Version: target, InProgress: target},
		{Name: "cp3", IsCP: true, Version: "v1.37.1"},
		{Name: "w1", Version: target},
		{Name: "w2", Version: target, InProgress: target},
		{Name: "w3", Version: target, InProgress: "v1.37.5"},
	}
	cps, workers := nodesToUpgrade(ordered)
	names := func(ns []Node) (out []string) {
		for _, n := range ns {
			out = append(out, n.Name)
		}
		return out
	}
	if got := strings.Join(names(cps), ","); got != "cp1,cp2,cp3" {
		t.Errorf("control planes visited = %s, want cp1,cp2,cp3", got)
	}
	if got := strings.Join(names(workers), ","); got != "w1,w2,w3" {
		t.Errorf("workers visited = %s, want w1,w2,w3", got)
	}
	var checked []string
	for _, n := range ordered {
		if n.NeedsBuildCheck(target) {
			checked = append(checked, n.Name)
		}
	}
	if got := strings.Join(checked, ","); got != "cp1,w1,w3" {
		t.Errorf("build-checked before any drain = %s, want cp1,w1,w3 (at target by a finished upgrade)", got)
	}
}

// Counting a mislabeled node would put it in the batch that must keep etcd quorum.
func TestNodeFromAPI_ControlPlaneOnlyWhenTheLabelIsTrue(t *testing.T) {
	const kubeadmCP = "node-role.kubernetes.io/control-plane"
	for _, c := range []struct {
		name   string
		labels map[string]string
		wantCP bool
	}{
		{"label true", map[string]string{k8sutil.ControlPlaneLabel: "true"}, true},
		{"label false", map[string]string{k8sutil.ControlPlaneLabel: "false"}, false},
		{"label empty", map[string]string{k8sutil.ControlPlaneLabel: ""}, false},
		{"label absent", nil, false},
		{"only the kubeadm label", map[string]string{kubeadmCP: "true"}, false},
	} {
		n := nodeFromAPI(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: c.labels}})
		if n.IsCP != c.wantCP {
			t.Errorf("%s: nodeFromAPI(...).IsCP = %v, want %v", c.name, n.IsCP, c.wantCP)
		}
	}
}

func TestNodeFromAPIReadsTheRecord(t *testing.T) {
	n := nodeFromAPI(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1", Annotations: map[string]string{k8sutil.UpgradeInProgressAnnotation: "v1.37.9"}},
		// What an SKE kubelet really reports; compared raw it never matched a bare target.
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.37.9-stratum"}},
	})
	if n.Interrupted() != "v1.37.9" {
		t.Errorf("nodeFromAPI = %+v; want it seen as interrupted at v1.37.9", n)
	}
}
