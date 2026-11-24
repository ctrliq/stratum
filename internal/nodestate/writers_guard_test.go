package nodestate

import (
	"reflect"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// Points node.yaml at a temp directory for the test.
func withStatePath(t *testing.T) {
	t.Helper()
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })
}

// wantOnDisk fails unless node.yaml equals want exactly.
func wantOnDisk(t *testing.T, want *NodeState) {
	t.Helper()
	got, err := LoadState()
	if err != nil {
		t.Fatalf("read node.yaml back: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node.yaml holds\n\t%+v\nwant\n\t%+v\n— a save dropped a field another writer recorded", got, want)
	}
}

// Every node.yaml writer records its fields by load-modify-save on the shared file.
func TestNodeYAMLWritersConverge(t *testing.T) {
	withStatePath(t)

	// Bring-up writes the base state; the round-trip must keep every value here.
	base := &NodeState{
		Role:               "controller",
		Hostname:           "cp1",
		NodeIP:             "192.0.2.10",
		KubeletNodeIP:      "192.0.2.10",
		Server:             "https://192.0.2.10:6443",
		EtcdName:           "cp1",
		EtcdInitialCluster: "cp1=https://192.0.2.10:2380",
		EtcdState:          "new",
		DualStack:          true,
		PodCIDR:            "10.245.0.0/16,fd00:245::/56",
		ServiceCIDR:        "10.96.0.0/12,fd00:96::/108",
		ClusterDomain:      "cluster.internal",
		CIS:                true,
		SELinuxOptOut:      true,
		VIP:                VIP{Address: "192.0.2.1", Interface: "eth0", BGP: true},
	}
	if err := SaveState(base); err != nil {
		t.Fatal(err)
	}
	wantOnDisk(t, base)

	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.ConfigPath = "/etc/stratum/cluster.yaml"
	st.EtcdHasPeers = true
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	after := *base
	after.ConfigPath = "/etc/stratum/cluster.yaml"
	after.EtcdHasPeers = true
	wantOnDisk(t, &after)

	// A second writer records the DNS Service address it learned.
	st, err = LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.ClusterDNSIP = "10.96.0.10"
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	after.ClusterDNSIP = "10.96.0.10"
	wantOnDisk(t, &after)

	// The first writer records again on a restart.
	st, err = LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.ConfigPath = "/etc/stratum/other.yaml"
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	after.ConfigPath = "/etc/stratum/other.yaml"
	wantOnDisk(t, &after)
}
