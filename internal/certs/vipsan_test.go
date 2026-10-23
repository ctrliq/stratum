package certs

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestRenewalKeepsTheVIPSAN pins the renewal half of the VIP-SAN rule. Renewal
// takes its SAN extras from hostNodeIP, which must carry the VIP recorded in
// node state alongside the service-network extras; extras without the VIP
// would reissue the apiserver certificate without it, and clients of the VIP
// fail TLS as soon as it lands on a renewed node.
func TestRenewalKeepsTheVIPSAN(t *testing.T) {
	origConf, origNet := version.K8sConfDir, version.NetConfigPath
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = origConf, origNet })

	netc, err := netcfg.Load()
	if err != nil {
		t.Fatal(err)
	}
	base := netc.APIServerExtraIPs()
	for _, tc := range []struct {
		name, vip string
		want      []string
	}{
		{"with a VIP", "192.168.99.10", append(slices.Clone(base), "192.168.99.10")},
		{"without one", "", base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := nodestate.SaveState(&nodestate.NodeState{
				Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
				VIP: nodestate.VIP{Address: tc.vip},
			}); err != nil {
				t.Fatal(err)
			}
			_, _, extras, _, err := hostNodeIP()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(extras.IPs, tc.want) {
				t.Errorf("renewal SAN extras = %q, want %q", extras.IPs, tc.want)
			}
		})
	}
}

// TestRenewalAddressesForADualStackNode pins renewal's certificate inputs on a
// dual-stack node: the recorded v4,v6 pair as the node address, extras equal to
// netc.APIServerSANs with the VIP — the list install and the join issue — and a
// refusal that names the missing pair when the state lost it. A renewal that
// dropped the IPv6 half, or re-detected an address for a node that already has
// state, reissues a certificate the node's own kubelet cannot use.
func TestRenewalAddressesForADualStackNode(t *testing.T) {
	origConf, origNet := version.K8sConfDir, version.NetConfigPath
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = origConf, origNet })

	if err := netcfg.Save("10.42.0.0/16,fd00:42::/48", "10.43.0.0/16,fd00:43::/108", ""); err != nil {
		t.Fatal(err)
	}
	netc, err := netcfg.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !netc.DualStack {
		t.Fatal("fixture netconfig is not dual-stack; the test proves nothing")
	}
	const vip = "192.168.99.10"

	t.Run("recorded pair and VIP", func(t *testing.T) {
		if err := nodestate.SaveState(&nodestate.NodeState{
			Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12,fd00:99::12",
			VIP: nodestate.VIP{Address: vip},
		}); err != nil {
			t.Fatal(err)
		}
		_, ip, extras, _, err := hostNodeIP()
		if err != nil {
			t.Fatal(err)
		}
		if ip != "192.168.99.12,fd00:99::12" {
			t.Errorf("renewal node address = %q, want the recorded v4,v6 pair", ip)
		}
		if want := netc.APIServerSANs(vip); !slices.Equal(extras.IPs, want) {
			t.Errorf("renewal SAN extras = %q, want %q", extras.IPs, want)
		}
	})

	t.Run("pair missing", func(t *testing.T) {
		if err := nodestate.SaveState(&nodestate.NodeState{
			Role: "controller", NodeIP: "192.168.99.12",
			VIP: nodestate.VIP{Address: vip},
		}); err != nil {
			t.Fatal(err)
		}
		_, _, _, _, err := hostNodeIP()
		if err == nil {
			t.Fatal("renewal proceeded without the recorded v4,v6 pair; it would issue for a re-detected address")
		}
		if !strings.Contains(err.Error(), "v4,v6") {
			t.Errorf("err = %v, want it to name the missing dual-stack pair", err)
		}
	})
}

// A renewal reissues the apiserver certificate with the cluster domain
// the cluster was initialised with, read from netconfig.
func TestRenewalKeepsTheClusterDomainSAN(t *testing.T) {
	origConf, origNet := version.K8sConfDir, version.NetConfigPath
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = origConf, origNet })

	if err := netcfg.Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
		t.Fatal(err)
	}
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, sans, _, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if sans.Domain != "fuzz.internal" {
		t.Errorf("renewal SAN domain = %q, want fuzz.internal", sans.Domain)
	}
}

// With netconfig lost and node state on fuzz.internal, a rotation still
// reissues apiserver.crt with the fuzz.internal SAN. Deriving the domain from
// load alone gives cluster.local, and rotation force-reissues leaves, so the
// SAN would be lost.
func TestRotationKeepsTheClusterDomainSANWithoutNetConfig(t *testing.T) {
	origConf, origNet, origPKI := version.K8sConfDir, version.NetConfigPath, version.PKIDir
	version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig") // never written: lost
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath, version.PKIDir = origConf, origNet, origPKI })
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12", ClusterDomain: "fuzz.internal",
	}); err != nil {
		t.Fatal(err)
	}
	hostname, nodeIP, sans, _, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if sans.Domain != "fuzz.internal" {
		t.Fatalf("hostNodeIP domain = %q, want fuzz.internal", sans.Domain)
	}
	if err := pki.GenerateAll(hostname, nodeIP, nil); err != nil { // a cert on the default domain
		t.Fatal(err)
	}
	if err := pki.RotateLeaves(hostname, nodeIP, sans); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.PKIDir, "apiserver.crt"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(crt.DNSNames, "kubernetes.default.svc.fuzz.internal") {
		t.Errorf("rotated apiserver.crt SANs = %v, want kubernetes.default.svc.fuzz.internal", crt.DNSNames)
	}
}

// TestRotationDerivesTheServiceSANFromTheRecordedRangeWithoutNetConfig: with
// netconfig lost on a custom-range node, rotation derives the apiserver's
// service-IP SAN from the recorded service range, not the default 10.96.0.1.
func TestRotationDerivesTheServiceSANFromTheRecordedRangeWithoutNetConfig(t *testing.T) {
	origConf, origNet, origPKI := version.K8sConfDir, version.NetConfigPath, version.PKIDir
	version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig") // never written: lost
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath, version.PKIDir = origConf, origNet, origPKI })
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
		PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "fuzz.internal",
	}); err != nil {
		t.Fatal(err)
	}
	hostname, nodeIP, sans, _, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sans.IPs, "10.43.0.1") || slices.Contains(sans.IPs, "10.96.0.1") {
		t.Fatalf("hostNodeIP IPs = %v, want 10.43.0.1 (recorded service range) and not 10.96.0.1", sans.IPs)
	}
	if err := pki.GenerateAll(hostname, nodeIP, nil); err != nil { // a cert on the default range
		t.Fatal(err)
	}
	if err := pki.RotateLeaves(hostname, nodeIP, sans); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.PKIDir, "apiserver.crt"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	var ips []string
	for _, ip := range crt.IPAddresses {
		ips = append(ips, ip.String())
	}
	if !slices.Contains(ips, "10.43.0.1") {
		t.Errorf("rotated apiserver.crt IP SANs = %v, want 10.43.0.1", ips)
	}
}

// Node state that cannot be read is not "no VIP": rotating a VIP node as one
// without would drop the VIP SAN and leave kube-vip on the superseded leaf. A
// node with no node state at all has no VIP.
func TestNodeVIPRefusesUnreadableNodeState(t *testing.T) {
	origConf, origNet := version.K8sConfDir, version.NetConfigPath
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = origConf, origNet })

	if vip, err := nodeVIP(); err != nil || vip != "" {
		t.Fatalf("no node state: nodeVIP = %q, %v; want no VIP and no error", vip, err)
	}
	if err := os.WriteFile(nodestate.StatePath(), []byte("vip: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeVIP(); err == nil || !strings.Contains(err.Error(), "node.yaml") {
		t.Errorf("malformed node state: nodeVIP error = %v, want one naming node.yaml", err)
	}
	if err := os.Remove(nodestate.StatePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nodestate.StatePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeVIP(); err == nil {
		t.Error("unreadable node state read as no VIP")
	}
}

// regenFromNodeState regenerates the kubeconfigs as a CA-rotation phase does,
// reading the hostname and VIP once.
func regenFromNodeState() error {
	hostname, _, _, vip, err := hostNodeIP()
	if err != nil {
		return err
	}
	return regenKubeconfigs(hostname, vip)
}
