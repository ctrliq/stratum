package upgrade

import (
	"io"

	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/version"
)

// A rollout given a client runs on it; one without falls back to this node's
// admin.conf, as the CLI does.
func TestUpgradeClusterRunsOnTheGivenClient(t *testing.T) {
	oldConf := version.AdminConf
	t.Cleanup(func() { version.AdminConf = oldConf })
	version.AdminConf = filepath.Join(t.TempDir(), "missing-admin.conf")

	cs := fake.NewSimpleClientset()
	err := UpgradeCluster(io.Discard, ClusterOptions{Target: "not-a-version", From: "/staged/stratum", Client: cs})
	if err == nil || strings.Contains(err.Error(), "missing-admin.conf") {
		t.Fatalf("with a client: %v, want it to stop on that client's nodes, not to load admin.conf", err)
	}
	listed := false
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "nodes" {
			listed = true
		}
	}
	if !listed {
		t.Errorf("the given client was not asked for the nodes: %v", cs.Actions())
	}

	err = UpgradeCluster(io.Discard, ClusterOptions{Target: "not-a-version", From: "/staged/stratum"})
	if err == nil || !strings.Contains(err.Error(), "missing-admin.conf") {
		t.Fatalf("without a client: %v, want it to load this node's admin.conf", err)
	}
}
