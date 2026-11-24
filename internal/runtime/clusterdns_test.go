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

// internal/cluster/clusterdns_test.go pins the CoreDNS side to this same table.
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
	// An underivable range is an error even with a recorded address to fall back on.
	if _, err := ClusterDNSIP(&nodestate.NodeState{ServiceCIDR: "10.43.0.0/30", ClusterDNSIP: "10.43.0.10"}); err == nil {
		t.Error("a service CIDR too small for a DNS IP was accepted although an address is recorded")
	}
}

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
