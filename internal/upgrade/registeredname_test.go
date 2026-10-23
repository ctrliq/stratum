package upgrade

import (
	"io"

	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
)

// A kubelet with --hostname-override registers its Node under the flag's
// name, not the OS hostname: a successful upgrade's backstop must address
// that same Node, as its gates and stamp do.
func TestBackstopUsesRegisteredNodeName(t *testing.T) {
	const registered, target = "cp-1.registered", "v1.37.9"
	osName, err := host.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if osName == registered {
		t.Skip("probe requires differing OS and registered names")
	}
	events := controllerUpgradeFixture(t, target, 0)
	cs := fakeNode(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: registered, ResourceVersion: "1", Annotations: map[string]string{
			k8sutil.UpgradeCordonAnnotation:     target,
			k8sutil.UpgradeInProgressAnnotation: target,
		}},
		Spec: corev1.NodeSpec{Unschedulable: true},
	})
	requested := ""
	swap(t, &uncordonUpgraded, func(node, target string) (bool, error) {
		requested = node
		return releaseUpgradeCordon(context.Background(), cs, node, target)
	})
	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
	if err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	obj, err := cs.CoreV1().Nodes().Get(t.Context(), registered, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if requested != registered || obj.Spec.Unschedulable {
		t.Fatalf("backstop requested %q (OS=%q), registered=%q remains cordoned=%v; gates/stamp=%v; output:\n%s", requested, osName, registered, obj.Spec.Unschedulable, *events, out)
	}
}
