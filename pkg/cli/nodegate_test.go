package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

// stubNodeGate points `stratum node` at a fake clientset holding one tainted
// node, at a fixed time, and counts the clientset requests.
func stubNodeGate(t *testing.T) (cs *fake.Clientset, dials *int) {
	t.Helper()
	cs = fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu1"},
		Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: k8sutil.UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}}},
	})
	dials = new(int)
	oldCS, oldNow := nodeGateClientset, nodeGateNow
	t.Cleanup(func() { nodeGateClientset, nodeGateNow = oldCS, oldNow })
	nodeGateClientset = func() (kubernetes.Interface, error) { *dials++; return cs, nil }
	nodeGateNow = func() time.Time { return time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC) }
	return cs, dials
}

func runNode(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newNodeCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestNodeHoldThenRelease(t *testing.T) {
	cs, _ := stubNodeGate(t)
	out, err := runNode(t, "release", "gpu1", "--by", "alice")
	if err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "gpu1", metav1.GetOptions{})
	if len(n.Spec.Taints) != 0 || n.Annotations[k8sutil.VerifiedAnnotation] != "alice@2026-10-31T16:00:00Z" {
		t.Errorf("after release the node is %+v", n)
	}

	if out, err = runNode(t, "release", "gpu1", "--by", "alice"); !errors.Is(err, k8sutil.ErrNotHeld) || !strings.Contains(err.Error(), "--force") {
		t.Errorf("a second release on an untainted node = %v, want ErrNotHeld with the --force hint:\n%s", err, out)
	}

	out, err = runNode(t, "hold", "gpu1", "--by", "bob", "--reason", "fan failure")
	if err != nil {
		t.Fatalf("hold: %v\n%s", err, out)
	}
	for _, want := range []string{kubectlHint("drain gpu1"), kubectlHint("uncordon gpu1"), "while it is still held", "--delete-emptydir-data only if", "without a toleration"} {
		if !strings.Contains(out, want) {
			t.Errorf("hold printed:\n%s\nwant %q", out, want)
		}
	}
	n, _ = cs.CoreV1().Nodes().Get(context.Background(), "gpu1", metav1.GetOptions{})
	if len(n.Spec.Taints) != 1 || n.Annotations[k8sutil.HeldAnnotation] != "bob@2026-10-31T16:00:00Z" || n.Annotations[k8sutil.HeldReasonAnnotation] != "fan failure" {
		t.Errorf("after hold the node is %+v", n)
	}
}

// A node drained during its hold stays cordoned after the release, which
// removes only the taint, so release says so instead of promising workloads.
func TestNodeReleaseOfACordonedNodeSaysToUncordonIt(t *testing.T) {
	cs, _ := stubNodeGate(t)
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "gpu1", metav1.GetOptions{})
	n.Spec.Unschedulable = true
	if _, err := cs.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := runNode(t, "release", "gpu1", "--by", "alice")
	if err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if !strings.Contains(out, "still cordoned") || !strings.Contains(out, "stratum kubectl --kubeconfig "+version.AdminConf+" uncordon gpu1") || strings.Contains(out, "can now schedule") {
		t.Errorf("release of a cordoned node printed:\n%s\nwant the uncordon hint", out)
	}
	if n, _ := cs.CoreV1().Nodes().Get(context.Background(), "gpu1", metav1.GetOptions{}); !n.Spec.Unschedulable {
		t.Error("release uncordoned the node; a cordon is the operator's to clear")
	}
}

// release reports what it did, removing the startup taint, and never that
// workloads can schedule: another taint or a cordon may still keep them off.
// A forced release of an untainted node reports that it removed nothing.
func TestNodeReleaseReportsOnlyTheTaintItRemoved(t *testing.T) {
	stubNodeGate(t)
	out, err := runNode(t, "release", "gpu1", "--by", "alice")
	if err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed the "+k8sutil.UnverifiedTaint+" taint") || strings.Contains(out, "can now schedule") || strings.Contains(out, "uncordon") {
		t.Errorf("release of an uncordoned node printed:\n%s", out)
	}

	out, err = runNode(t, "release", "gpu1", "--by", "alice", "--force")
	if err != nil {
		t.Fatalf("forced release: %v\n%s", err, out)
	}
	if !strings.Contains(out, "recorded gpu1 as verified by alice; it did not carry") || strings.Contains(out, "removed the") {
		t.Errorf("a forced release of an untainted node printed:\n%s\nwant no removal reported", out)
	}
}

// Usage errors refuse before the command reaches the API server.
func TestNodeGateRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	_, dials := stubNodeGate(t)
	for name, c := range map[string]struct {
		args []string
		flag string
	}{
		"release without a node": {args: []string{"release"}},
		"two nodes":              {args: []string{"release", "gpu1", "gpu2"}},
		"an empty node":          {args: []string{"release", "", "--by", "alice"}, flag: "node name"},
		"a blank node to hold":   {args: []string{"hold", " ", "--by", "bob", "--reason", "repair"}, flag: "node name"},
		"hold without a reason":  {args: []string{"hold", "gpu1", "--by", "bob"}, flag: "--reason"},
		"an empty actor":         {args: []string{"release", "gpu1", "--by", ""}, flag: "--by"},
		"a spaced actor":         {args: []string{"hold", "gpu1", "--by", "bob smith", "--reason", "x"}, flag: "--by"},
	} {
		_, err := runNode(t, c.args...)
		if err == nil {
			t.Errorf("%s: %v succeeded", name, c.args)
		} else if !strings.Contains(err.Error(), c.flag) {
			t.Errorf("%s: %v, want the refusal to name %s", name, err, c.flag)
		}
	}
	if *dials != 0 {
		t.Errorf("bad invocations connected %d times, want none", *dials)
	}
}

func TestDefaultActorPrefersSudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")
	if got := defaultActor(); got != "alice" {
		t.Errorf("defaultActor() = %q under sudo, want alice", got)
	}
	t.Setenv("SUDO_USER", "")
	if defaultActor() == "" {
		t.Error("defaultActor() is empty without sudo, want the current user")
	}
}
