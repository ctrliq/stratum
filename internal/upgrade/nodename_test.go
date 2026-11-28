package upgrade

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
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

// A control plane renamed since it registered still has a valid saved name,
// but the upgrade ends by restarting it with `stratum up`, which re-derives the
// name from the hostname: it refuses a hostname no Node can take, and
// registers the node under any other one while the upgrade's checks wait for
// the saved name. The upgrade refuses both first, --check included, before it
// backs anything up or reissues any certificate. A worker restarts under its
// saved name, so it is not refused.
func TestUpgradeRefusesAControlPlaneWhoseHostnameChanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current func() (string, error)
		want    string
	}{
		{"no node name", func() (string, error) {
			return "", errors.New(`this host's name "cp_1" cannot name a Kubernetes node`)
		}, `"cp_1" cannot name`},
		{"another node name", func() (string, error) { return "cp-new", nil }, `registered as Node "cp-1", but its hostname now names "cp-new"`},
	} {
		for _, check := range []bool{false, true} {
			t.Run(tc.name+"/check="+strconv.FormatBool(check), func(t *testing.T) {
				controllerUpgradeFixture(t, "v1.37.9", 0)
				writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
					"role: controller\nhostname: CP-1\nnodeIP: 10.0.0.1\n", 0o600)
				swap(t, &upgradeHostNodeName, tc.current)
				refreshed := 0
				swap(t, &refreshCerts, func(io.Writer, *nodestate.NodeState, string) (bool, error) {
					refreshed++
					return false, nil
				})
				_, err := runWriter(t, func(w io.Writer) error {
					return UpgradeNode(w, NodeOptions{SkipSnapshot: true, Check: check})
				})
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was changed") {
					t.Fatalf("UpgradeNode = %v; want the refusal %q, saying nothing was changed", err, tc.want)
				}
				if refreshed != 0 {
					t.Errorf("certificates were refreshed %d times before the refusal", refreshed)
				}
				if m, err := readUpgradeMarker(); err != nil || m != nil {
					t.Errorf("the refused upgrade left a marker %+v, %v", m, err)
				}
			})
		}
	}
}

// The same control plane whose hostname differs from its saved name only in
// case is the same Node: the kubelet registered the name lowercased, which is
// what the current hostname names too. It upgrades.
func TestUpgradeTakesAControlPlaneSavedWithItsHostnamesCase(t *testing.T) {
	controllerUpgradeFixture(t, "v1.37.9", 0)
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
		"role: controller\nhostname: CP-1\nnodeIP: 10.0.0.1\n", 0o600)
	swap(t, &upgradeHostNodeName, func() (string, error) { return "cp-1", nil })
	if _, err := runWriter(t, func(w io.Writer) error {
		return UpgradeNode(w, NodeOptions{SkipSnapshot: true, Check: true})
	}); !errors.Is(err, ErrUpgradeNeeded) {
		t.Fatalf("UpgradeNode --check = %v on a control plane saved as CP-1 on the host cp-1; want %v", err, ErrUpgradeNeeded)
	}
}
