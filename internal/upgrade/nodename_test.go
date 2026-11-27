package upgrade

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// A worker an earlier release joined on a mixed-case hostname saved the name
// with its case, while its kubelet registered the Node lowercased. The
// upgrade's gates read the Node the kubelet registered, so they must ask for
// the lowercased name: with the saved case the version check finds no Node,
// and the upgrade rolls back a node that upgraded fine.
func TestUpgradeGatesReadTheLowercasedSavedName(t *testing.T) {
	st := &nodestate.NodeState{Role: "worker", Hostname: "HK-wrk"}
	if got := nodeName(st); got != "hk-wrk" {
		t.Fatalf("nodeName(%q) = %q, want hk-wrk", st.Hostname, got)
	}
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	runtimeFixture(t, "v1.37.1-stratum", "v1.37.1-stratum", "v1.37.1-stratum")
	var asked []string
	kubeletReports = func(_ context.Context, node string) (string, error) {
		asked = append(asked, node)
		if node != "hk-wrk" {
			return "", errors.New("nodes " + strconv.Quote(node) + " not found")
		}
		return "v1.37.1-stratum", nil
	}
	if err := nodeGate(false, nodeName(st)); err != nil {
		t.Errorf("nodeGate: %v (asked %v)", err, asked)
	}
	asked = nil
	if err := waitNodeGate(false, nodeName(st), time.Second); err != nil {
		t.Errorf("waitNodeGate: %v (asked %v)", err, asked)
	}
}

// The post-upgrade stamp addresses the same registered Node.
func TestStampUpgradedStampsTheLowercasedSavedName(t *testing.T) {
	var stamped []string
	old := stampNode
	stampNode = func(_ context.Context, node string) error { stamped = append(stamped, node); return nil }
	t.Cleanup(func() { stampNode = old })
	stampUpgraded(io.Discard, &nodestate.NodeState{Role: "controller", Hostname: "HK-k8s-ctrl-01"})
	if len(stamped) != 1 || stamped[0] != "hk-k8s-ctrl-01" {
		t.Errorf("stamped %v, want [hk-k8s-ctrl-01]", stamped)
	}
}

// A control plane's certificates are reissued for the name it registered
// under, from its node state, not for the OS hostname: a host renamed since,
// or renamed to something that is no node name at all, would otherwise get a
// kubelet certificate for a Node that does not exist, or for "".
func TestUpgradeRefreshesCertsForTheRegisteredName(t *testing.T) {
	controllerUpgradeFixture(t, "v1.37.9", 0)
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
		"role: controller\nhostname: CP-1.Registered\nnodeIP: 10.0.0.1\n", 0o600)
	var refreshed []string
	swap(t, &refreshCerts, func(_ io.Writer, _ *nodestate.NodeState, h string) (bool, error) {
		refreshed = append(refreshed, h)
		return false, nil
	})
	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if len(refreshed) == 0 || refreshed[0] != "cp-1.registered" {
		t.Errorf("certificates refreshed for %v, want cp-1.registered", refreshed)
	}
}
