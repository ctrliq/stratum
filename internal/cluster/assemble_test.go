package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// Controller bootstrap must run its common node steps through the ONE shared
// assembly the HA join uses (ha.AssembleNode), not a second inline copy of
// the sequence: a change to one path's node rendering must reach the other.
// Stubbing the assembly behind its seam short-circuits every common step —
// host dirs, prerequisites, PKI, kubeconfigs, CIS configs, runtime configs —
// so what stays observable is the wiring: the request PrepareController
// derives from the cluster config and saved state, the caller-owned hooks
// the assembly invokes, and the assembled node state flowing out to
// SaveState and the caller. The join side of the same contract is pinned in
// internal/ha/join_test.go against the recording join effects.

// assemblyFixture points the state paths PrepareController reads and writes
// before the assembly runs at temporary ones, and stands in for the hostname
// and the etcd join-state read the CIS key gate makes. The assembly itself
// is stubbed per test, so the host-touching effects never run here.
func assemblyFixture(t *testing.T) {
	t.Helper()
	origConf, origPKI := version.K8sConfDir, version.PKIDir
	origEnc, origNet := version.EncryptionConfigPath, version.NetConfigPath
	origData, origRoot := version.EtcdDataDir, version.DataDir
	t.Cleanup(func() {
		version.K8sConfDir, version.PKIDir = origConf, origPKI
		version.EncryptionConfigPath, version.NetConfigPath = origEnc, origNet
		version.EtcdDataDir, version.DataDir = origData, origRoot
	})
	version.K8sConfDir = t.TempDir()
	version.PKIDir = t.TempDir()
	version.EncryptionConfigPath = filepath.Join(version.PKIDir, "encryption-config.yaml")
	version.NetConfigPath = filepath.Join(version.PKIDir, "netconfig")
	version.EtcdDataDir = filepath.Join(t.TempDir(), "etcd")
	version.DataDir = filepath.Join(t.TempDir(), "data")
	oldHostname, oldJoinState := hostHostname, readEtcdJoinState
	t.Cleanup(func() { hostHostname, readEtcdJoinState = oldHostname, oldJoinState })
	hostHostname = func() (string, error) { return "cp1", nil }
	readEtcdJoinState = func() (etcdJoinState, error) { return etcdJoinState{Members: 1}, nil }
}

// runThroughRecordingAssembly replaces the assembly seam with one that
// records the call, runs the caller-owned ResolveNet and BeforeCerts hooks
// the way the real assembly would, and answers with a fixed detected node
// IP. The returned accessors report what the assembly received.
func runThroughRecordingAssembly(t *testing.T, choice host.NodeIPChoice, assembleErr error) (
	calls func() int, request func() ha.AssembleRequest, effects func() ha.NodeAssembly,
) {
	t.Helper()
	var n int
	var gotReq ha.AssembleRequest
	var gotFX ha.NodeAssembly
	saved := assembleNode
	assembleNode = func(_ context.Context, a ha.NodeAssembly, req ha.AssembleRequest) (host.NodeIPChoice, error) {
		n++
		gotReq, gotFX = req, a
		if assembleErr != nil {
			return host.NodeIPChoice{}, assembleErr
		}
		if a.ResolveNet == nil {
			return host.NodeIPChoice{}, errors.New("the assembly got no network resolution")
		}
		if _, err := a.ResolveNet(); err != nil {
			return host.NodeIPChoice{}, err
		}
		if a.BeforeCerts == nil {
			return host.NodeIPChoice{}, errors.New("the assembly got no before-certificates hook")
		}
		if err := a.BeforeCerts(choice); err != nil {
			return host.NodeIPChoice{}, err
		}
		return choice, nil
	}
	t.Cleanup(func() { assembleNode = saved })
	return func() int { return n }, func() ha.AssembleRequest { return gotReq }, func() ha.NodeAssembly { return gotFX }
}

// assemblyFieldSet reports whether the named exported func field of the
// assembly is wired. reflect keeps the wiring check name-by-name without
// fourteen nil comparisons.
func assemblyFieldSet(a ha.NodeAssembly, name string) bool {
	v := reflect.ValueOf(a).FieldByName(name)
	return v.IsValid() && !v.IsNil()
}

// TestPrepareControllerRunsTheSharedNodeAssembly pins the bootstrap side of
// the shared assembly, case by case: the assembly runs exactly once per
// prepare, with the request the config and saved state resolve to, and the
// node state it hands back — carrying the node IP the caller's
// before-certificates hook filled from the resolved choice — is what gets
// persisted and returned.
func TestPrepareControllerRunsTheSharedNodeAssembly(t *testing.T) {
	restart := &NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380,cp2=https://10.0.0.12:2380",
		EtcdState: "existing", EtcdHasPeers: true,
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", ClusterDomain: "cluster.local"}

	for _, tc := range []struct {
		name       string
		cfg        *ClusterConfig
		nodeIPFlag string
		seed       func(t *testing.T)
		wantCIS    bool
		wantVIP    string
		wantSaved  string
		wantServer string
		wantSaved_ func(t *testing.T, saved *NodeState)
	}{
		{"a fresh controller without a VIP", &ClusterConfig{}, "192.0.2.7", nil,
			false, "", "", fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort),
			func(t *testing.T, saved *NodeState) {
				if saved.EtcdInitialCluster != "cp1=https://10.0.0.11:2380" || saved.EtcdState != "new" {
					t.Errorf("saved membership = %q (%q), want a fresh single-member state=new",
						saved.EtcdInitialCluster, saved.EtcdState)
				}
			}},
		{"a fresh controller with a VIP and spec.cis", &ClusterConfig{Spec: Spec{CIS: true, VIP: VIP{Address: "10.0.0.10"},
			Network: Network{NodeIP: "192.0.2.7"}}}, "192.0.2.7", nil,
			true, "10.0.0.10", "", fmt.Sprintf("https://10.0.0.10:%d", version.APIServerPort), nil},
		{"a restart over a two-member state", &ClusterConfig{}, "", func(t *testing.T) {
			if err := SaveState(restart); err != nil {
				t.Fatal(err)
			}
		}, false, "", "10.0.0.11", fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort),
			func(t *testing.T, saved *NodeState) {
				if saved.EtcdInitialCluster != restart.EtcdInitialCluster || saved.EtcdState != "existing" {
					t.Errorf("saved membership = %q (%q), want the carried two-member state=existing",
						saved.EtcdInitialCluster, saved.EtcdState)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assemblyFixture(t)
			if tc.seed != nil {
				tc.seed(t)
			}
			choice := host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"}
			calls, request, effects := runThroughRecordingAssembly(t, choice, nil)
			st, err := PrepareController(tc.cfg, tc.nodeIPFlag, holdDataLock(t))
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if calls() != 1 {
				t.Fatalf("PrepareController ran the shared assembly %d times, want once", calls())
			}
			req := request()
			if req.Hostname != "cp1" {
				t.Errorf("the assembly's hostname was %q, want cp1", req.Hostname)
			}
			if got := req.PrereqOptions.Profile.Name == nodestate.CISProfile; got != tc.wantCIS {
				t.Errorf("the assembly ran with profile %q, want cis %v", req.PrereqOptions.Profile.Name, tc.wantCIS)
			}
			if req.VIPAddress != tc.wantVIP {
				t.Errorf("the assembly's VIP was %q, want %q", req.VIPAddress, tc.wantVIP)
			}
			if req.NodeIPFlag != tc.nodeIPFlag {
				t.Errorf("the assembly got node-IP flag %q, want %q", req.NodeIPFlag, tc.nodeIPFlag)
			}
			if want := tc.cfg.Spec.Network.NodeIP; req.ConfigNodeIP != want {
				t.Errorf("the assembly got config node IP %q, want %q", req.ConfigNodeIP, want)
			}
			if req.SavedIP != tc.wantSaved {
				t.Errorf("the assembly got saved IP %q, want %q", req.SavedIP, tc.wantSaved)
			}
			if req.APIServer != tc.wantServer {
				t.Errorf("the assembly's apiserver endpoint was %q, want %q", req.APIServer, tc.wantServer)
			}
			if req.State == nil || req.State.Role != "controller" || req.State.Hostname != "cp1" {
				t.Errorf("the assembly got node state %+v, want a controller state for cp1", req.State)
			}
			// The caller's own hooks ride in the assembly, beside the common
			// effects: none of the fields may be left unwired.
			fx := effects()
			for _, name := range []string{"HostDirs", "Prereqs", "ResolveNet", "TrustPodCIDRs",
				"ResolveNodeIP", "DetectNodeIPs", "BeforeCerts", "IssueCerts", "WriteKubeconfigs",
				"WriteRuntimeConfigs"} {
				if !assemblyFieldSet(fx, name) {
					t.Errorf("the assembly's %s was not wired", name)
				}
			}
			if st == nil || st.NodeIP != choice.IP || st.KubeletNodeIP != choice.IP {
				t.Fatalf("PrepareController returned %+v, want the assembled state at %q", st, choice.IP)
			}
			saved, err := LoadState()
			if err != nil {
				t.Fatalf("no node state was persisted: %v", err)
			}
			if saved.NodeIP != choice.IP || saved.KubeletNodeIP != choice.IP {
				t.Errorf("persisted node IPs %q/%q, want the assembled %q", saved.NodeIP, saved.KubeletNodeIP, choice.IP)
			}
			if tc.wantSaved_ != nil {
				tc.wantSaved_(t, saved)
			}
		})
	}
}

func TestPrepareControllerKeepsAnInheritedVIPAcrossRestarts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured VIP
		want       VIP
		repointed  string
	}{
		{"unspecified keeps the inherited address", VIP{}, VIP{Address: "10.0.0.10"}, ""},
		{"an explicit address moves the VIP", VIP{Address: "10.0.0.20"}, VIP{Address: "10.0.0.20"}, ""},
		{"local interface and BGP options remain configured", VIP{Interface: "ens3", BGP: true}, VIP{Address: "10.0.0.10", Interface: "ens3", BGP: true}, ""},
		{"interface change keeps a distinct repointed endpoint", VIP{Address: "10.0.0.10", Interface: "ens3"}, VIP{Address: "10.0.0.10", Interface: "ens3"}, "https://10.0.0.99:6443"},
		{"BGP change keeps a distinct repointed endpoint", VIP{Address: "10.0.0.10", BGP: true}, VIP{Address: "10.0.0.10", BGP: true}, "https://10.0.0.99:6443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assemblyFixture(t)
			oldKC, oldAdmin := version.KubeconfigDir, version.AdminConf
			t.Cleanup(func() { version.KubeconfigDir, version.AdminConf = oldKC, oldAdmin })
			version.KubeconfigDir = t.TempDir()
			version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
			originalServer := tc.repointed
			if originalServer == "" {
				originalServer = "https://10.0.0.10:6443"
			}
			kc := "apiVersion: v1\nkind: Config\nclusters:\n- name: stratum\n  cluster:\n    server: " + originalServer + "\ncontexts:\n- name: default\n  context:\n    cluster: stratum\ncurrent-context: default\n"
			for _, file := range recordKubeconfigs() {
				if err := os.WriteFile(file, []byte(kc), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveState(&NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11",
				KubeletNodeIP: "10.0.0.11", VIP: VIP{Address: "10.0.0.10"}, Server: originalServer,
				EtcdName: "cp1", EtcdState: "existing", EtcdInitialCluster: "cp1=https://10.0.0.11:2380"}); err != nil {
				t.Fatal(err)
			}
			_, request, _ := runThroughRecordingAssembly(t, host.NodeIPChoice{IP: "10.0.0.11", Source: "state"}, nil)
			st, err := PrepareController(&ClusterConfig{Spec: Spec{VIP: tc.configured}}, "", holdDataLock(t))
			if err != nil {
				t.Fatal(err)
			}
			wantServer := "https://" + tc.want.Address + ":6443"
			if tc.repointed != "" {
				wantServer = tc.repointed
			}
			if st.VIP != tc.want || st.Server != wantServer {
				t.Errorf("restart state VIP=%+v server=%q, want VIP=%+v server=%q", st.VIP, st.Server, tc.want, wantServer)
			}
			if req := request(); req.VIPAddress != tc.want.Address || req.APIServer != wantServer {
				t.Errorf("restart assembly VIP=%q server=%q, want VIP=%q server=%q", req.VIPAddress, req.APIServer, tc.want.Address, wantServer)
			}
			saved, err := LoadState()
			if err != nil {
				t.Fatal(err)
			}
			if saved.VIP != tc.want || saved.Server != wantServer {
				t.Errorf("saved restart state VIP=%+v server=%q, want VIP=%+v server=%q", saved.VIP, saved.Server, tc.want, wantServer)
			}
		})
	}
}

// TestPrepareControllerRefusesThroughTheSharedAssembly pins the two refusal
// paths the shared assembly must keep: an assembly that fails fails the
// prepare, and the caller's before-certificates hook — the IP-move refusal —
// runs at the assembly's hook point and refuses a restart on a moved
// address. A refused or failed prepare saves no node state.
func TestPrepareControllerRefusesThroughTheSharedAssembly(t *testing.T) {
	prev := &NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380",
		EtcdState: "existing", EtcdHasPeers: true,
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", ClusterDomain: "cluster.local"}

	for _, tc := range []struct {
		name        string
		choice      host.NodeIPChoice
		assembleErr error
		wantErr     string
	}{
		{"an assembly that fails", host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"},
			errors.New("an unstamped binary cannot pin Pod Security"), "an unstamped binary"},
		{"a restart on a moved address", host.NodeIPChoice{IP: "10.0.0.99", Source: "flag"},
			nil, "is an etcd member at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assemblyFixture(t)
			if err := SaveState(prev); err != nil {
				t.Fatal(err)
			}
			calls, _, _ := runThroughRecordingAssembly(t, tc.choice, tc.assembleErr)
			_, err := PrepareController(&ClusterConfig{}, "", holdDataLock(t))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want a refusal carrying %q", err, tc.wantErr)
			}
			if calls() != 1 {
				t.Errorf("PrepareController ran the shared assembly %d times, want once", calls())
			}
			if saved, loadErr := LoadState(); loadErr != nil || saved.NodeIP != prev.NodeIP {
				t.Errorf("a refused prepare changed the saved state: %+v, %v", saved, loadErr)
			}
		})
	}
}
