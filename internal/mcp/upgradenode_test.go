package mcp

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
)

// An SKE kubelet reports "v1.37.9-stratum", compared against the bare in-progress record.
func TestUpgradePlanComparesBareVersions(t *testing.T) {
	fakeStratumHost(t)
	srv := servePlanNodes(t, []corev1.Node{
		nodeForPlan("cp1", map[string]string{k8sutil.ControlPlaneLabel: "true"}, "v1.37.9-stratum", map[string]string{k8sutil.UpgradeInProgressAnnotation: "v1.37.9"}),
	})
	conn := kube.NewConnector(kube.Options{Kubeconfig: kubetest.Kubeconfig(t, &rest.Config{Host: srv.URL})})

	_, out, err := UpgradePlanHandler(conn)(context.Background(), nil, UpgradePlanIn{Target: "v1.37.10"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Allowed || !strings.Contains(out.Blocker, "unfinished upgrade to v1.37.9") {
		t.Fatalf("upgrade_plan = allowed %v, blocker %q; want the unfinished-upgrade refusal the CLI gives", out.Allowed, out.Blocker)
	}
}
