package nodestate

import (
	"reflect"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// withStatePath points node.yaml at a temp directory for the test.
func withStatePath(t *testing.T) {
	t.Helper()
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })
}

// wantOnDisk loads node.yaml and requires it to equal want exactly: every
// field every writer recorded, still in the one file.
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

// TestNodeYAMLWritersConverge pins the retention contract the node.yaml
// writers rely on: every writer after bring-up records its fields by
// load-modify-save on the shared file (recordConfigPath, the worker DNS
// learner, the etcd growth recorder, and the HA join and restore all write
// this way), so a writer's save must keep what the other writers — and
// bring-up — recorded. Two mock writers converge on one node.yaml the way
// those do: if a save writes a fresh struct instead of the loaded one, or the
// serializer drops a field, the file stops equaling the accumulated state and
// this fails.
func TestNodeYAMLWritersConverge(t *testing.T) {
	withStatePath(t)

	// Bring-up (PrepareController, and the join that prepares a member) writes
	// the base state. Every non-zero value here is one the round-trip through
	// the file must keep.
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

	// The controller-run writer records the config file the controller was
	// started with (recordConfigPath) and that etcd grew past one member (the
	// HA join's membership record): load, record, save.
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

	// A second writer — the worker's DNS learner — records the DNS Service
	// address it learned: load, record, save. The first writer's fields, and
	// bring-up's, must all still be there.
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

	// The first writer records again on a restart (a new --config path): the
	// second writer's field survives it.
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
