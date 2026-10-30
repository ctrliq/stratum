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

// stubSimpleAssembly replaces assembleNode with a stub that resolves net,
// runs the before-certificates hook, and reports the fixture's node IP, the
// way a real start does; the original is restored at cleanup.
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

// TestPrepareKeepsFieldsAnotherWriterRecordedMidStart pins the write rule for
// the save at the end of a controller start: another writer's record lands in
// node.yaml while the start runs, and the save must apply bring-up's fields to
// the state read back under the etcd data lock — never replace the file with
// the fresh struct the start built, which would drop every field bring-up does
// not own (recordConfigPath's ConfigPath, learnClusterDNS's ClusterDNSIP) and
// whatever any other writer recorded in between.
//
// The stubbed assembly's before-certificates hook plays the other writer: it
// records ConfigPath and ClusterDNSIP on disk mid-prepare, the way
// recordConfigPath and learnClusterDNS write them around a start. The seed
// deliberately diverges from what this start sets — a profile this config does
// not name, VIP interface and BGP settings the config dropped, a stale endpoint
// record no kubeconfig file corroborates, a domain the config does not carry —
// so the saved state proves a real write happened and that owned fields are
// copied unconditionally, zero values included: nothing bring-up means to set
// or clear is resurrected from disk.
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
		// Another writer records its fields while the start runs, between the
		// start's own read and its final save.
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
		// The established node keeps the domain its state records (netcfg.ResolveDomain).
		ClusterDomain: "old.internal",
		VIP:           VIP{Address: "10.0.0.254"},
		// The fields the mid-prepare writer recorded; the save keeps them.
		ConfigPath:   "/srv/other-stratum.yaml",
		ClusterDNSIP: "10.43.0.99",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved node.yaml =\n %+v\nwant\n %+v", got, want)
	}
	// The state handed to RunController reports what was actually saved, so
	// recordConfigPath's early return and learnClusterDNS read the same file's
	// truth rather than the start's stale copy.
	if !reflect.DeepEqual(returned, want) {
		t.Fatalf("returned node state =\n %+v\nwant\n %+v", returned, want)
	}
}

// TestPrepareDoesNotResurrectOwnedFieldsItClears pins the zero-value half of
// the save: the closure copies each owned field unconditionally, so an owned
// field the current config leaves empty does not survive from disk. A save
// that copied a field only when the start's value is non-empty would bring a
// removed VIP interface and BGP mode, a dropped profile, or an opt-out the
// state happened to record back past what the config says.
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

// TestPrepareRefusesToSaveWithoutTheHeldDataLock pins the refusal half of the
// lock contract: the save must write only under the caller's held etcd data
// lock. A nil release is a caller that holds no lock — an unlocked write would
// land between another operation's read and write — so the save refuses and
// node.yaml keeps exactly what it had.
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

// TestPrepareReturnsTheProfileTheConfigNames pins the profile the start hands
// to the run phase: the save normalizes the state written to node.yaml
// (SaveState's normalizeProfile), but the returned struct is the one this start
// built, so a config that names the profile — spec.profile: cis — without the
// legacy spec.cis flag leaves its CIS flag false while the file records it
// true. The returned state must agree with the saved file on Profile and CIS:
// the run phase reads its profile back from the returned state, not from
// node.yaml.
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
