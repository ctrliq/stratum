package cluster

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

// TestNodeIPMoveRefusal: an etcd member restarted with a new
// explicit address while keeping its recorded membership would render etcd
// on the new address and could not reconnect. The move is refused.
func TestNodeIPMoveRefusal(t *testing.T) {
	member := &NodeState{NodeIP: "192.168.99.12", EtcdInitialCluster: "cp1=https://192.168.99.12:2380"}
	kubeletOnly := &NodeState{KubeletNodeIP: "192.168.99.12,fd00::12", EtcdInitialCluster: member.EtcdInitialCluster}
	for _, tc := range []struct {
		name    string
		prev    *NodeState
		primary string
		refuse  bool
	}{
		{"first boot", nil, "192.168.99.12", false},
		{"restart on the same address", member, "192.168.99.12", false},
		{"state without membership (never started etcd)", &NodeState{NodeIP: "10.0.0.1"}, "192.168.99.12", false},
		{"a member moved to another NIC", member, "10.99.0.1", true},
		{"kubelet-only member stays on its recorded primary", kubeletOnly, "192.168.99.12", false},
		{"kubelet-only member moved to another NIC", kubeletOnly, "10.99.0.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := nodeIPMoveRefusal(tc.prev, tc.primary, "spec.network.nodeIP")
			if (err != nil) != tc.refuse {
				t.Fatalf("err = %v, want refusal: %v", err, tc.refuse)
			}
			if tc.refuse && !strings.Contains(err.Error(), "etcd member at 192.168.99.12") {
				t.Errorf("refusal does not name the recorded address: %v", err)
			}
			// The refusal must point at the supported
			// remedy (remove and rejoin), matching installation/_index.md —
			// not the etcdctl member update/renew-certificates procedure
			// that was removed because it does not work.
			if tc.refuse && !strings.Contains(err.Error(), "stratum uninstall") {
				t.Errorf("refusal does not say to remove the node: %v", err)
			}
			if tc.refuse && strings.Contains(err.Error(), "etcdctl member update") {
				t.Errorf("refusal still points at the non-working move procedure: %v", err)
			}
			// An HA control plane's stale etcd member must
			// be removed from a remaining control plane first, before this
			// node is uninstalled and rejoined — not left for whoever
			// notices the cluster is one member short after the fact.
			if tc.refuse && !strings.Contains(err.Error(), "stratum etcdctl member remove") {
				t.Errorf("refusal does not say to remove the stale etcd member first: %v", err)
			}
		})
	}
}

func TestSavedControllerIP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   *NodeState
		want    string
		wantErr bool
	}{
		{"missing state is first boot", nil, "", false},
		{"kubelet address wins", &NodeState{NodeIP: "10.0.0.1", KubeletNodeIP: "10.0.0.1,fd00::1"}, "10.0.0.1,fd00::1", false},
		{"older primary-only state", &NodeState{NodeIP: "10.0.0.1"}, "10.0.0.1", false},
		{"addressless partial state", &NodeState{Role: "controller"}, "", true},
		{"addressless etcd member", &NodeState{EtcdInitialCluster: "cp1=https://10.0.0.1:2380"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := savedControllerIP(tc.state)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("savedControllerIP() = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestPrepareControllerWritesTheSameNode pins what controller assembly writes,
// field by field, for the node shapes the start paths rebuild it for: a fresh
// controller without a VIP, a fresh controller with a VIP and spec.cis, and a
// restart over a saved two-member state — which must keep its etcd membership
// instead of being reset to a state=new seed. PKI and kubeconfigs are generated
// for real in redirected temp dirs, with the host behind its seams, so any
// change in a saved field, a kubeconfig endpoint, the apiserver SAN list or
// which files are written fails here.
func TestPrepareControllerWritesTheSameNode(t *testing.T) {
	t.Setenv("NODE_IP", "")
	origConf, origPKI, origKubeconfig, origAdmin := version.K8sConfDir, version.PKIDir, version.KubeconfigDir, version.AdminConf
	origEnc, origNet, origAudit, origAdmission := version.EncryptionConfigPath, version.NetConfigPath, version.AuditPolicyPath, version.AdmissionConfigPath
	origContainerd, origK8sVersion, origBinDir := version.ContainerdConfig, version.K8sVersion, version.BinDir
	origRegistries, origCerts, origConfD := version.RegistriesConfig, version.CertsDir, version.ContainerdConfD
	origRoot, origState := version.ContainerdRootDir, version.ContainerdStateDir
	origData, origDataRoot := version.EtcdDataDir, version.DataDir
	t.Cleanup(func() {
		version.K8sConfDir, version.PKIDir, version.KubeconfigDir, version.AdminConf = origConf, origPKI, origKubeconfig, origAdmin
		version.EncryptionConfigPath, version.NetConfigPath, version.AuditPolicyPath, version.AdmissionConfigPath = origEnc, origNet, origAudit, origAdmission
		version.ContainerdConfig, version.K8sVersion, version.BinDir = origContainerd, origK8sVersion, origBinDir
		version.RegistriesConfig, version.CertsDir, version.ContainerdConfD = origRegistries, origCerts, origConfD
		version.ContainerdRootDir, version.ContainerdStateDir = origRoot, origState
		version.EtcdDataDir, version.DataDir = origData, origDataRoot
	})
	oldHostname, oldDirs, oldPrereqs := hostHostname, hostDirs, hostPrereqs
	oldTrust, oldDetect, oldJoin := hostTrustPodCIDRs, hostDetectNodeIPs, readEtcdJoinState
	t.Cleanup(func() {
		hostHostname, hostDirs, hostPrereqs = oldHostname, oldDirs, oldPrereqs
		hostTrustPodCIDRs, hostDetectNodeIPs, readEtcdJoinState = oldTrust, oldDetect, oldJoin
	})
	hostHostname = func() (string, error) { return "cp1", nil }
	hostDirs = func() error { return nil }
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	var trustedPodCIDRs []string
	hostTrustPodCIDRs = func(cidr string) { trustedPodCIDRs = append(trustedPodCIDRs, cidr) }
	hostDetectNodeIPs = func(bool) (string, error) { return "10.0.0.11", nil }
	readEtcdJoinState = func() (etcdJoinState, error) { return etcdJoinState{Members: 1}, nil }

	restart := &NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380,cp2=https://10.0.0.12:2380",
		EtcdState: "existing", EtcdHasPeers: true,
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", ClusterDomain: "cluster.local"}

	for _, tc := range []struct {
		name string
		cfg  *ClusterConfig
		seed func()
		want NodeState
		cis  bool
	}{
		{"a fresh controller without a VIP", &ClusterConfig{}, func() {},
			NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
				Server:   fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort),
				EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380", EtcdState: "new",
				PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", ClusterDomain: "cluster.local"},
			false},
		{"a fresh controller with a VIP and spec.cis", &ClusterConfig{Spec: Spec{CIS: true, VIP: VIP{Address: "10.0.0.10"}}}, func() {},
			NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
				Server:   fmt.Sprintf("https://10.0.0.10:%d", version.APIServerPort),
				EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380", EtcdState: "new",
				PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", ClusterDomain: "cluster.local",
				Profile: "cis", CIS: true, VIP: VIP{Address: "10.0.0.10"}},
			true},
		{"a restart over a two-member state", &ClusterConfig{}, func() {
			if err := SaveState(restart); err != nil {
				t.Fatal(err)
			}
		},
			NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
				Server:   fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort),
				EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380,cp2=https://10.0.0.12:2380",
				EtcdState: "existing", EtcdHasPeers: true,
				PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", ClusterDomain: "cluster.local"},
			false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trustedPodCIDRs = nil
			version.K8sConfDir = t.TempDir()
			version.PKIDir = t.TempDir()
			version.KubeconfigDir = t.TempDir()
			version.EtcdDataDir = filepath.Join(t.TempDir(), "etcd")
			version.DataDir = filepath.Join(t.TempDir(), "data")
			version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
			version.EncryptionConfigPath = filepath.Join(version.PKIDir, "encryption-config.yaml")
			version.NetConfigPath = filepath.Join(version.PKIDir, "netconfig")
			version.AuditPolicyPath = filepath.Join(version.K8sConfDir, "audit-policy.yaml")
			version.AdmissionConfigPath = filepath.Join(version.K8sConfDir, "admission-config.yaml")
			version.K8sVersion = "v1.31.4"
			// EnsureConfig also applies registry files and checks ownership of
			// the legacy host config. Keep registry effects in this fixture;
			// unique root/state paths prevent it from claiming a real host file.
			version.RegistriesConfig = filepath.Join(version.K8sConfDir, "registries.yaml")
			version.CertsDir = filepath.Join(version.K8sConfDir, "certs.d")
			version.ContainerdConfD = filepath.Join(version.K8sConfDir, "conf.d", "registry.toml")
			version.ContainerdRootDir = filepath.Join(version.K8sConfDir, "containerd-root")
			version.ContainerdStateDir = filepath.Join(version.K8sConfDir, "containerd-state")
			// A payload marker (never executed: the rendered config below
			// short-circuits ensureConfig's content check) satisfies the
			// payload check WriteConfig makes before rendering.
			version.BinDir = t.TempDir()
			if err := os.WriteFile(filepath.Join(version.BinDir, "containerd"), []byte("#!true\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// The containerd config a node's payload has already rendered
			// short-circuits ensureConfig (its content check) without the
			// containerd binary a unit test has no payload to run.
			version.ContainerdConfig = filepath.Join(version.K8sConfDir, "containerd.toml")
			driver := "SystemdCgroup = false"
			if runtime.CgroupDriver() == "systemd" {
				driver = "SystemdCgroup = true"
			}
			if err := os.WriteFile(version.ContainerdConfig, []byte(driver+"\nsandbox_image = "+runtime.SandboxImage+"\nenable_selinux = false\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			tc.seed()
			if _, err := PrepareController(tc.cfg, "", holdDataLock(t)); err != nil {
				t.Fatal(err)
			}

			// The saved node state, field by field.
			saved, err := LoadState()
			if err != nil {
				t.Fatal(err)
			}
			if *saved != tc.want {
				t.Errorf("saved node state = %+v, want %+v", *saved, tc.want)
			}
			if !slices.Contains(trustedPodCIDRs, tc.want.PodCIDR) {
				t.Errorf("the pod CIDRs trusted on the host were %v, want %q among them", trustedPodCIDRs, tc.want.PodCIDR)
			}

			// Every kubeconfig targets the recorded endpoint; kube-vip's, only
			// written with a VIP, keeps the local apiserver it must reach.
			for _, f := range []string{"admin.kubeconfig", "controller-manager.kubeconfig", "scheduler.kubeconfig", "kubelet.kubeconfig", version.SchedulerKubeconfig()} {
				if !filepath.IsAbs(f) {
					f = filepath.Join(version.KubeconfigDir, f)
				}
				cfg, err := clientcmd.LoadFromFile(f)
				if err != nil {
					t.Fatalf("load %s: %v", f, err)
				}
				if got := cfg.Clusters[version.ClusterName].Server; got != tc.want.Server {
					t.Errorf("%s server = %q, want %q", f, got, tc.want.Server)
				}
			}
			cfg, err := clientcmd.LoadFromFile(version.AdminConf)
			if err != nil {
				t.Fatalf("load admin.conf: %v", err)
			}
			if got := cfg.Clusters[version.ClusterName].Server; got != tc.want.Server {
				t.Errorf("admin.conf server = %q, want %q", got, tc.want.Server)
			}
			_, vipErr := os.Stat(version.KubeVipKubeconfig())
			if tc.want.VIP.Address == "" {
				if vipErr == nil {
					t.Error("kube-vip.kubeconfig exists without a VIP")
				}
			} else if vipErr != nil {
				t.Errorf("kube-vip.kubeconfig is missing with a VIP: %v", vipErr)
			}

			// The apiserver cert carries the SAN list bring-up derives: the
			// service IPs, localhost, the node's addresses, then the VIP.
			netc, err := netcfg.Parse(tc.want.PodCIDR, tc.want.ServiceCIDR)
			if err != nil {
				t.Fatal(err)
			}
			sans := netc.APIServerSANs(tc.want.VIP.Address)
			wantIPs := append([]string{sans[0], "127.0.0.1"}, strings.Split(tc.want.KubeletNodeIP, ",")...)
			wantIPs = append(wantIPs, sans[1:]...)
			cert, err := readCert(t, filepath.Join(version.PKIDir, "apiserver.crt"))
			if err != nil {
				t.Fatal(err)
			}
			var gotIPs []string
			for _, ip := range cert.IPAddresses {
				gotIPs = append(gotIPs, ip.String())
			}
			if !slices.Equal(gotIPs, wantIPs) {
				t.Errorf("apiserver.crt IP SANs = %v, want %v", gotIPs, wantIPs)
			}

			// Every node runs kubelet+containerd.
			if _, err := os.Stat(filepath.Join(version.K8sConfDir, "kubelet.conf")); err != nil {
				t.Errorf("kubelet.conf is missing: %v", err)
			}
			// The CIS configs exist exactly when spec.cis is set.
			for _, p := range []string{version.AuditPolicyPath, version.AdmissionConfigPath, version.EncryptionConfigPath} {
				_, err := os.Stat(p)
				if tc.cis && err != nil {
					t.Errorf("%s is missing with spec.cis: %v", p, err)
				}
				if !tc.cis && err == nil {
					t.Errorf("%s exists without spec.cis", p)
				}
			}
		})
	}
}

// readCert parses the first PEM block of a certificate file.
func readCert(t *testing.T, path string) (*x509.Certificate, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}
