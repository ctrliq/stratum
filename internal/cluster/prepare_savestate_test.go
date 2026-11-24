package cluster

import (
	"context"
	"reflect"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/nodestate"
)

func stubSimpleAssembly(t *testing.T) {
	t.Helper()
	savedAssembly := assembleNode
	t.Cleanup(func() { assembleNode = savedAssembly })
	choice := host.NodeIPChoice{IP: "10.0.0.1", Source: "test"}
	assembleNode = func(_ context.Context, a ha.NodeAssembly, _ ha.AssembleRequest) (host.NodeIPChoice, error) {
		if _, err := a.ResolveNet(); err != nil {
			return host.NodeIPChoice{}, err
		}
		return choice, a.BeforeCerts(choice)
	}
}

// The before-certificates hook plays another writer recording fields mid-prepare.
func TestPrepareKeepsFieldsAnotherWriterRecordedMidStart(t *testing.T) {
	assemblyFixture(t)
	release := holdDataLock(t)

	seed := seedState()
	seed.EtcdState = "existing"
	seed.EtcdHasPeers = true
	seed.EtcdPromotePending = true
	seed.Profile = "cis"
	seed.CIS = true
	seed.VIP = VIP{Address: "10.0.0.254", Interface: "eth9", BGP: true}
	seed.Server = "https://10.0.0.1:6443"
	seed.ClusterDomain = "old.internal"
	if err := SaveState(seed); err != nil {
		t.Fatal(err)
	}

	savedAssembly := assembleNode
	t.Cleanup(func() { assembleNode = savedAssembly })
	choice := host.NodeIPChoice{IP: "10.0.0.1", Source: "test"}
	assembleNode = func(_ context.Context, a ha.NodeAssembly, _ ha.AssembleRequest) (host.NodeIPChoice, error) {
		if _, err := a.ResolveNet(); err != nil {
			return host.NodeIPChoice{}, err
		}
		if err := a.BeforeCerts(choice); err != nil {
			return host.NodeIPChoice{}, err
		}
		// Another writer records its fields between the start's read and final save.
		cur, err := LoadState()
		if err != nil {
			return host.NodeIPChoice{}, err
		}
		cur.ConfigPath = "/srv/other-stratum.yaml"
		cur.ClusterDNSIP = "10.43.0.99"
		if err := SaveState(cur); err != nil {
			return host.NodeIPChoice{}, err
		}
		return choice, nil
	}

	returned, err := PrepareController(&ClusterConfig{Spec: Spec{VIP: VIP{Address: "10.0.0.254"}}}, "", release)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	want := &NodeState{
		Role: "controller", Hostname: "cp1",
		NodeIP: "10.0.0.1", KubeletNodeIP: "10.0.0.1",
		Server:             kubeconfig.NodeServerURL("", "10.0.0.254"),
		EtcdName:           "cp1",
		EtcdInitialCluster: "cp1=https://10.0.0.1:2380",
		EtcdState:          "existing",
		EtcdHasPeers:       true,
		EtcdPromotePending: true,
		PodCIDR:            "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16",
		ClusterDomain: "old.internal",
		VIP:           VIP{Address: "10.0.0.254"},
		ConfigPath:    "/srv/other-stratum.yaml",
		ClusterDNSIP:  "10.43.0.99",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved node.yaml =\n %+v\nwant\n %+v", got, want)
	}
	if !reflect.DeepEqual(returned, want) {
		t.Fatalf("returned node state =\n %+v\nwant\n %+v", returned, want)
	}
}

// Owned fields are copied unconditionally, so an emptied field never survives.
func TestPrepareDoesNotResurrectOwnedFieldsItClears(t *testing.T) {
	assemblyFixture(t)
	release := holdDataLock(t)

	seed := seedState()
	seed.Profile = "cis"
	seed.CIS = true
	seed.SELinuxOptOut = true
	seed.VIP = VIP{Address: "10.0.0.10", Interface: "eth9", BGP: true}
	if err := SaveState(seed); err != nil {
		t.Fatal(err)
	}

	stubSimpleAssembly(t)

	if _, err := PrepareController(&ClusterConfig{Spec: Spec{VIP: VIP{Address: "10.0.0.10"}}}, "", release); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.VIP != (VIP{Address: "10.0.0.10"}) {
		t.Errorf("saved VIP = %+v, want the config's address with the removed interface and BGP mode gone", got.VIP)
	}
	if got.Profile != "" || got.CIS {
		t.Errorf("saved profile = %q (cis=%v), want the dropped profile cleared", got.Profile, got.CIS)
	}
	if got.SELinuxOptOut {
		t.Error("saved selinuxOptOut = true, want it cleared: a controller reads its SELinux settings from its config file, never from the state")
	}
}

// A nil release is a caller holding no lock, and an unlocked write races others.
func TestPrepareRefusesToSaveWithoutTheHeldDataLock(t *testing.T) {
	assemblyFixture(t)

	seed := seedState()
	seed.ConfigPath = "/srv/stratum.yaml"
	seed.ClusterDNSIP = "10.43.0.10"
	if err := SaveState(seed); err != nil {
		t.Fatal(err)
	}

	stubSimpleAssembly(t)

	if _, err := PrepareController(&ClusterConfig{}, "", nil); err == nil {
		t.Fatal("prepare saved node.yaml without the held etcd data lock")
	}
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, seed) {
		t.Fatalf("node.yaml =\n %+v\nwant the untouched seed\n %+v", got, seed)
	}
}

// The run phase reads its profile from the returned state, not node.yaml.
func TestPrepareReturnsTheProfileTheConfigNames(t *testing.T) {
	assemblyFixture(t)
	release := holdDataLock(t)

	stubSimpleAssembly(t)

	returned, err := PrepareController(&ClusterConfig{Spec: Spec{Profile: nodestate.CISProfile}}, "", release)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != nodestate.CISProfile || !got.CIS {
		t.Errorf("saved profile = %q (cis=%v), want the config's profile written both ways", got.Profile, got.CIS)
	}
	if returned.Profile != nodestate.CISProfile || !returned.CIS {
		t.Errorf("returned profile = %q (cis=%v), want it normalized to agree with the saved node.yaml", returned.Profile, returned.CIS)
	}
}
