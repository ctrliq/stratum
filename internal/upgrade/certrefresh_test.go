package upgrade

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestRefreshControlPlaneCertsReportsAReissue: the upgrade runs the PKI pass
// with this node's SANs, VIP included, so an upgraded custom-CIDR control plane
// does not keep an apiserver.crt without its Service IP, and it reports
// whether apiserver.crt changed so the apiserver can be restarted onto it.
func TestRefreshControlPlaneCertsReportsAReissue(t *testing.T) {
	oldPKI, oldNet, oldGen := version.PKIDir, version.NetConfigPath, generateCerts
	version.PKIDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.PKIDir, version.NetConfigPath, generateCerts = oldPKI, oldNet, oldGen })
	crt := filepath.Join(version.PKIDir, "apiserver.crt")
	if err := os.WriteFile(crt, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11", VIP: nodestate.VIP{Address: "10.0.0.10"}}

	var gotIP string
	var gotExtras []string
	var gotDomain string
	generateCerts = func(_ io.Writer, host, nodeIP string, sans *pki.APISANs) error {
		gotIP, gotExtras, gotDomain = nodeIP, sans.IPs, sans.Domain
		return os.WriteFile(crt, []byte("reissued"), 0o644)
	}
	// The reissue carries the cluster domain recorded in netconfig.
	if err := os.WriteFile(version.NetConfigPath, []byte(netdefault.PodCIDR+"\n"+netdefault.ServiceCIDR+"\nfuzz.internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reissued, err := refreshControlPlaneCerts(io.Discard, st, "cp1")
	if err != nil || !reissued {
		t.Fatalf("a rewritten apiserver.crt: reissued=%v err=%v", reissued, err)
	}
	if gotIP != "10.0.0.11" || len(gotExtras) == 0 || gotExtras[len(gotExtras)-1] != "10.0.0.10" {
		t.Errorf("PKI pass got node IP %q, extras %q; want the recorded address and the VIP", gotIP, gotExtras)
	}

	if gotDomain != "fuzz.internal" {
		t.Errorf("PKI pass got cluster domain %q, want fuzz.internal", gotDomain)
	}

	generateCerts = func(io.Writer, string, string, *pki.APISANs) error { return nil } // nothing to repair
	if reissued, err := refreshControlPlaneCerts(io.Discard, st, "cp1"); err != nil || reissued {
		t.Errorf("an untouched apiserver.crt: reissued=%v err=%v; want no restart", reissued, err)
	}
}

// TestUpgradeCertInputsMatchNodeState: the upgrade's PKI pass issues for the
// same addresses renewal does — the recorded KubeletNodeIP and the full
// APIServerSANs list (Service IPs, and ::1 under dual-stack, then the VIP) —
// with the netconfig's domain. An upgrade that derives a different list would
// hand the next renewal a certificate it must reissue again.
func TestUpgradeCertInputsMatchNodeState(t *testing.T) {
	oldPKI, oldNet, oldGen := version.PKIDir, version.NetConfigPath, generateCerts
	version.PKIDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.PKIDir, version.NetConfigPath, generateCerts = oldPKI, oldNet, oldGen })
	crt := filepath.Join(version.PKIDir, "apiserver.crt")

	for _, c := range []struct {
		name, podCIDR, svcCIDR, vip, kubeletNodeIP string
	}{
		{"single-stack with a VIP", "10.245.0.0/16", "10.96.0.0/12", "10.0.0.10", "10.0.0.11"},
		{"dual-stack with a recorded pair and a VIP", "10.42.0.0/16,fd00:42::/56", "10.43.0.0/16,fd00:43::/108", "10.0.0.10", "10.0.0.11,fd00:42::11"},
	} {
		if err := os.WriteFile(version.NetConfigPath, []byte(c.podCIDR+"\n"+c.svcCIDR+"\nfuzz.internal\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		netc, err := netcfg.Parse(c.podCIDR, c.svcCIDR)
		if err != nil {
			t.Fatal(err)
		}
		st := &nodestate.NodeState{Role: "controller", NodeIP: "10.9.9.99", KubeletNodeIP: c.kubeletNodeIP, VIP: nodestate.VIP{Address: c.vip}}
		var gotIP string
		var gotExtras []string
		var gotDomain string
		generateCerts = func(_ io.Writer, host, nodeIP string, sans *pki.APISANs) error {
			gotIP, gotExtras, gotDomain = nodeIP, sans.IPs, sans.Domain
			return os.WriteFile(crt, []byte("reissued"), 0o644)
		}
		if _, err := refreshControlPlaneCerts(io.Discard, st, "cp1"); err != nil {
			t.Fatalf("%s: refreshControlPlaneCerts: %v", c.name, err)
		}
		if gotIP != c.kubeletNodeIP {
			t.Errorf("%s: PKI pass got node IP %q, want the recorded KubeletNodeIP %q", c.name, gotIP, c.kubeletNodeIP)
		}
		if want := netc.APIServerSANs(c.vip); !slices.Equal(gotExtras, want) {
			t.Errorf("%s: PKI pass got extras %q, want APIServerSANs(vip) %q", c.name, gotExtras, want)
		}
		if gotDomain != "fuzz.internal" {
			t.Errorf("%s: PKI pass got cluster domain %q, want fuzz.internal", c.name, gotDomain)
		}
	}
}

// TestUpgradeRefusesADualStackNodeWithoutARecordedPair pins the rule: the
// upgrade's PKI pass issues for the same node address renewal does, and
// refuses where renewal refuses. With the recorded v4,v6 pair it must hand
// pki the certificate inputs bring-up hands it for the same node — the
// recorded pair and the full APIServerSANs list (the Service IPs, ::1, then
// the VIP) under the netconfig's domain, compared through LeafSpecs so a
// reordered or displaced Service IP fails here too. With the pair missing
// it must return renewal's refusal and never call generateCerts: falling
// back from KubeletNodeIP to NodeIP would reissue an apiserver certificate
// without the node's IPv6 address — a certificate the next renewal, which
// refuses this node, would reissue again.
func TestUpgradeRefusesADualStackNodeWithoutARecordedPair(t *testing.T) {
	oldPKI, oldNet, oldGen := version.PKIDir, version.NetConfigPath, generateCerts
	version.PKIDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.PKIDir, version.NetConfigPath, generateCerts = oldPKI, oldNet, oldGen })
	const podCIDR, svcCIDR = "10.42.0.0/16,fd00:42::/56", "10.43.0.0/16,fd00:43::/108"
	if err := os.WriteFile(version.NetConfigPath, []byte(podCIDR+"\n"+svcCIDR+"\nfuzz.internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	netc, err := netcfg.Parse(podCIDR, svcCIDR)
	if err != nil {
		t.Fatal(err)
	}
	netc.ClusterDomain = "fuzz.internal"
	const vip, pair = "10.0.0.10", "10.0.0.11,fd00:42::11"
	crt := filepath.Join(version.PKIDir, "apiserver.crt")

	t.Run("the recorded pair: bring-up's SAN purposes", func(t *testing.T) {
		if err := os.WriteFile(crt, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		st := &nodestate.NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.9.9.99",
			KubeletNodeIP: pair, VIP: nodestate.VIP{Address: vip}}
		var gotHost, gotIP string
		var gotSANs *pki.APISANs
		generateCerts = func(_ io.Writer, host, nodeIP string, sans *pki.APISANs) error {
			gotHost, gotIP, gotSANs = host, nodeIP, sans
			return os.WriteFile(crt, []byte("reissued"), 0o644)
		}
		reissued, err := refreshControlPlaneCerts(io.Discard, st, "cp1")
		if err != nil || !reissued {
			t.Fatalf("refreshControlPlaneCerts: reissued=%v err=%v", reissued, err)
		}
		if gotHost != "cp1" || gotIP != pair {
			t.Errorf("PKI pass got host %q node IP %q, want cp1 at the recorded pair %q", gotHost, gotIP, pair)
		}
		if gotSANs == nil {
			t.Fatal("PKI pass got no SANs")
		}
		// The upgrade must hand pki the same certificate inputs bring-up hands
		// it for the same node, so the SAN purposes are compared through
		// LeafSpecs rather than as a raw list.
		got := pki.LeafSpecs(gotHost, gotIP, gotSANs)
		want := pki.LeafSpecs("cp1", pair, &pki.APISANs{IPs: netc.APIServerSANs(vip), Domain: netc.ClusterDomain})
		if len(got) != len(want) {
			t.Fatalf("LeafSpecs gave %d leaves, want %d", len(got), len(want))
		}
		for i := range want {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Errorf("%s leaf = %+v, want %+v", want[i].Name, got[i], want[i])
			}
		}
	})

	t.Run("the pair missing: renewal's refusal, no reissue", func(t *testing.T) {
		generated := false
		generateCerts = func(io.Writer, string, string, *pki.APISANs) error {
			generated = true
			return os.WriteFile(crt, []byte("reissued"), 0o644)
		}
		st := &nodestate.NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.9.9.99",
			VIP: nodestate.VIP{Address: vip}}
		reissued, err := refreshControlPlaneCerts(io.Discard, st, "cp1")
		if err == nil {
			t.Fatal("the upgrade issued certificates for a dual-stack node whose state records no v4,v6 pair, where renewal refuses")
		}
		if reissued {
			t.Error("a refused PKI pass reported apiserver.crt reissued")
		}
		if generated {
			t.Error("generateCerts ran although the node-address rule refuses the node")
		}
		if !strings.Contains(err.Error(), "v4,v6") {
			t.Errorf("err = %v, want renewal's refusal naming the dual-stack pair", err)
		}
	})
}

// TestApiserverRestartsOnceForAReissuedCert: on a normal
// upgrade Start changes the manifest's image, so kubelet already recreates the
// apiserver on the reissued cert, and an explicit restart on top was a second
// outage. The explicit restart is only for a resumed upgrade, where the
// manifest is re-rendered unchanged.
func TestApiserverRestartsOnceForAReissuedCert(t *testing.T) {
	oldConf, oldStart := version.K8sConfDir, startStaticPod
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir, startStaticPod = oldConf, oldStart })
	manifest := filepath.Join(version.K8sConfDir, "manifests", "kube-apiserver.yaml")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("image: apiserver:old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	startStaticPod = func(string) error { return os.WriteFile(manifest, []byte("image: apiserver:new\n"), 0o644) }
	changed, err := startReportingChange("kube-apiserver")
	if err != nil || !changed {
		t.Fatalf("an upgraded manifest: changed=%v err=%v", changed, err)
	}
	if restartForReissuedCert(true, changed) {
		t.Error("a normal upgrade restarts the apiserver a second time after Start already recreated it")
	}

	startStaticPod = func(string) error { return nil } // resumed: re-rendered byte-for-byte
	changed, err = startReportingChange("kube-apiserver")
	if err != nil || changed {
		t.Fatalf("an unchanged manifest: changed=%v err=%v", changed, err)
	}
	if !restartForReissuedCert(true, changed) {
		t.Error("a resumed upgrade leaves the apiserver on its old cert")
	}
	if restartForReissuedCert(false, changed) {
		t.Error("restarted the apiserver with no reissued cert")
	}
}

// With netconfig lost and node state on fuzz.internal, the upgrade
// refresh reissues apiserver.crt with the fuzz.internal SAN rather than leaving
// it on cluster.local, the domain Load alone gives here.
func TestRefreshControlPlaneCertsKeepsTheClusterDomainWithoutNetConfig(t *testing.T) {
	oldPKI, oldNet, oldConf, oldGen := version.PKIDir, version.NetConfigPath, version.K8sConfDir, generateCerts
	version.PKIDir, version.K8sConfDir = t.TempDir(), t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig") // never written: lost
	generateCerts = pki.GenerateAllWithWriter
	t.Cleanup(func() {
		version.PKIDir, version.NetConfigPath, version.K8sConfDir, generateCerts = oldPKI, oldNet, oldConf, oldGen
	})
	st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11", ClusterDomain: "fuzz.internal"}
	if err := nodestate.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if err := pki.GenerateAll("cp1", "10.0.0.11", nil); err != nil { // a cert on the default domain
		t.Fatal(err)
	}
	var progress bytes.Buffer
	var reissued bool
	var err error
	stdout := captureRealStdout(t, func() { reissued, err = refreshControlPlaneCerts(&progress, st, "cp1") })
	if err != nil || !reissued {
		t.Fatalf("refreshControlPlaneCerts: reissued=%v err=%v; want apiserver.crt reissued", reissued, err)
	}
	if stdout != "" {
		t.Errorf("PKI refresh leaked to stdout: %q", stdout)
	}
	if !strings.Contains(progress.String(), "kubernetes.default.svc.fuzz.internal; reissuing it") {
		t.Errorf("PKI refresh progress absent: %q", progress.String())
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
		t.Errorf("reissued apiserver.crt SANs = %v, want kubernetes.default.svc.fuzz.internal", crt.DNSNames)
	}
}
