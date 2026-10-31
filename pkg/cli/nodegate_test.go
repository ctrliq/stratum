package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
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

	if out, err = runNode(t, "release", "gpu1", "--by", "alice"); err == nil {
		t.Errorf("a second release succeeded on an untainted node:\n%s", out)
	}

	out, err = runNode(t, "hold", "gpu1", "--by", "bob", "--reason", "fan failure")
	if err != nil {
		t.Fatalf("hold: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stratum kubectl drain gpu1") {
		t.Errorf("hold printed:\n%s\nwant the drain hint", out)
	}
	n, _ = cs.CoreV1().Nodes().Get(context.Background(), "gpu1", metav1.GetOptions{})
	if len(n.Spec.Taints) != 1 || n.Annotations[k8sutil.HeldAnnotation] != "bob@2026-10-31T16:00:00Z: fan failure" {
		t.Errorf("after hold the node is %+v", n)
	}
}

// Usage errors refuse before the command reaches the API server.
func TestNodeGateRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	_, dials := stubNodeGate(t)
	for name, args := range map[string][]string{
		"release without a node": {"release"},
		"two nodes":              {"release", "gpu1", "gpu2"},
		"hold without a reason":  {"hold", "gpu1", "--by", "bob"},
		"an empty actor":         {"release", "gpu1", "--by", ""},
		"a spaced actor":         {"hold", "gpu1", "--by", "bob smith", "--reason", "x"},
	} {
		if _, err := runNode(t, args...); err == nil {
			t.Errorf("%s: %v succeeded", name, args)
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
