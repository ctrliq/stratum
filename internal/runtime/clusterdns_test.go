package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// TestClusterDNSIPFollowsTheRecordedServiceCIDR: the kubelet hands pods the DNS
// IP of the node's recorded service range, and the historical
// 10.96.0.10 when none is recorded (every existing worker, every default node).
func TestClusterDNSIPFollowsTheRecordedServiceCIDR(t *testing.T) {
	for _, tc := range []struct{ cidr, want string }{
		{"", "10.96.0.10"},
		{"10.96.0.0/12", "10.96.0.10"},
		{"10.43.0.0/16", "10.43.0.10"},
		{"10.43.0.0/16,fd00:43::/108", "10.43.0.10"},
	} {
		got, err := ClusterDNSIP(&nodestate.NodeState{ServiceCIDR: tc.cidr})
		if err != nil || got != tc.want {
			t.Errorf("ClusterDNSIP(%q) = %q, %v; want %q", tc.cidr, got, err, tc.want)
		}
	}
	if _, err := ClusterDNSIP(&nodestate.NodeState{ServiceCIDR: "10.43.0.0/30"}); err == nil {
		t.Error("a service CIDR too small for a DNS IP was accepted")
	}
}

// TestClusterDNSIPPrefersTheServiceCIDR: the one derivation both sides of a
// node's resolver run — CoreDNS's staged stack (writeBundles, internal/cluster)
// and the kubelet's clusterDNS (writeKubeletConfig) — takes the service range's
// DNS address even when the state also records one, and falls back to the
// recorded address only when no range is recorded (a worker that learned it
// from the kube-dns Service). internal/cluster/clusterdns_test.go pins the
// CoreDNS side to this same table, so a change that reaches only one side
// fails there.
func TestClusterDNSIPPrefersTheServiceCIDR(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *nodestate.NodeState
		want string
	}{
		{"a recorded address does not move a recorded range",
			&nodestate.NodeState{ServiceCIDR: "10.43.0.0/16", ClusterDNSIP: "10.99.0.10"}, "10.43.0.10"},
		{"a learned address stands in for a missing range",
			&nodestate.NodeState{ClusterDNSIP: "10.43.0.10"}, "10.43.0.10"},
		{"neither is the default",
			&nodestate.NodeState{}, netdefault.DNSClusterIP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClusterDNSIP(tc.st)
			if err != nil || got != tc.want {
				t.Errorf("ClusterDNSIP(%+v) = %q, %v; want %q", tc.st, got, err, tc.want)
			}
		})
	}
	// A range the DNS address cannot be derived from is an error even with a
	// recorded address to fall back on: the range is the authority where it exists.
	if _, err := ClusterDNSIP(&nodestate.NodeState{ServiceCIDR: "10.43.0.0/30", ClusterDNSIP: "10.43.0.10"}); err == nil {
		t.Error("a service CIDR too small for a DNS IP was accepted although an address is recorded")
	}
}

// TestKubeletRendersTheDerivedDNSAddress: the resolver the kubelet actually
// renders into kubelet.conf is the one derivation's address. With a service
// range and a recorded address that disagree, the range decides — so the
// kubelet's pods and the staged CoreDNS stack (pinned to the same table in
// internal/cluster/clusterdns_test.go) hand pods one resolver, reached through
// the one exported derivation rather than two private rules.
func TestKubeletRendersTheDerivedDNSAddress(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
	st := &nodestate.NodeState{Role: "worker", Hostname: "w1",
		ServiceCIDR: "10.43.0.0/16", ClusterDNSIP: "10.99.0.10"}
	if err := writeKubeletConfig(st); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, "clusterDNS: [10.43.0.10]") {
		t.Errorf("kubelet.conf lacks the CIDR-derived resolver clusterDNS: [10.43.0.10]:\n%s", got)
	}
}
