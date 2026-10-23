package upgrade

import (
	"io"

	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodestate"
)

// The supervisor that survives `stratum upgrade` is the old process,
// whose callbacks predate the kubelet-start restamp, so nothing it does can
// refresh the annotations. The upgrade itself must, on a controller.
func TestStampUpgradedStampsControllerWithoutSupervisorHelp(t *testing.T) {
	var stamped []string
	old := stampNode
	stampNode = func(_ context.Context, node string) error { stamped = append(stamped, node); return nil }
	t.Cleanup(func() { stampNode = old })

	// The persisted name, which is what the supervisor stamps and the node
	// registered under, not whatever the OS hostname says.
	stampUpgraded(io.Discard, &nodestate.NodeState{Role: "controller", Hostname: "cp-1.registered"})
	if len(stamped) != 1 || stamped[0] != "cp-1.registered" {
		t.Errorf("controller stamped %v, want [cp-1.registered]", stamped)
	}

	stamped = nil
	stampUpgraded(io.Discard, &nodestate.NodeState{Role: "worker", Hostname: "w-1"}) // a worker has no admin.conf; --cluster stamps it
	if len(stamped) != 0 {
		t.Errorf("worker stamped %v, want none", stamped)
	}
}

func TestStampUpgradedFailureIsNonFatalButVisible(t *testing.T) {
	old := stampNode
	stampNode = func(context.Context, string) error { return errors.New("apiserver down") }
	t.Cleanup(func() { stampNode = old })

	out, err := runWriter(t, func(w io.Writer) error {
		stampUpgraded(w, &nodestate.NodeState{Role: "controller", Hostname: "cp-1"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "apiserver down") {
		t.Errorf("failure not reported: %q", out)
	}
}

// An orchestrator that cannot name the staged release must not
// overwrite a node's stratum version with its own.
func TestStampVersionLeavesStratumVersionWhenUnknown(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "w1", Annotations: map[string]string{k8sutil.StratumVersionAnnotation: "v1.37.1-stratum.0"}}})
	if err := finishNode(ctx, cs, "w1", "v1.37.1", "", ""); err != nil {
		t.Fatal(err)
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Annotations[k8sutil.StratumVersionAnnotation]; got != "v1.37.1-stratum.0" {
		t.Errorf("stratum version = %q, want it left as it was", got)
	}
	if got := n.Annotations[k8sutil.VersionAnnotation]; got != "v1.37.1" {
		t.Errorf("k8s version = %q, want v1.37.1", got)
	}
}
