package certinputs

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestNewProducesThePinnedSANPurposes pins the certificate-input values this
// package owns. New builds the inputs for a node whose address is already
// resolved — the form the shared node assembly (bring-up and the HA join) has
// when it issues certificates — and hands pki the SAN list the cluster's
// network facts call for: netc.APIServerSANs with this node's VIP, under the
// netconfig's domain. The want lists are written out, not recomputed, so a
// drift in netcfg or pki shows here as a disagreement rather than a
// tautology. The LeafSpecs comparison is the acceptance pin: a lifecycle path
// that builds its inputs through this constructor issues certificates with
// the same SAN purposes as the hand-built construction bring-up, the join and
// renewal are pinned to today — the kubernetes Service IP first, the node's
// own IPs, the VIP as an extra after them, and the cluster domain in the DNS
// names. The node state on disk is a fresh empty directory: first
// preparation constructs the inputs from in-memory state before any state
// file exists, so the arguments alone must decide.
func TestNewProducesThePinnedSANPurposes(t *testing.T) {
	origConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir() // the inputs come from the arguments, never from disk
	t.Cleanup(func() { version.K8sConfDir = origConf })

	for _, tc := range []struct {
		name             string
		podCIDR, svcCIDR string
		vip              string
		nodeIP           string
		wantSANs         []string // the cluster's extras New must hand pki, in order
		wantAPIServerIPs []string // the composed apiserver IP SANs LeafSpecs must give
	}{
		{"single-stack without a VIP", "10.245.0.0/16", "10.96.0.0/12", "", "10.0.0.11",
			[]string{"10.96.0.1"},
			[]string{"10.96.0.1", "127.0.0.1", "10.0.0.11"}},
		{"single-stack with a VIP", "10.245.0.0/16", "10.96.0.0/12", "10.0.0.10", "10.0.0.11",
			[]string{"10.96.0.1", "10.0.0.10"},
			[]string{"10.96.0.1", "127.0.0.1", "10.0.0.11", "10.0.0.10"}},
		{"dual-stack with a VIP", "10.245.0.0/16,fd00:245::/64", "10.96.0.0/12,fd00:96::/108", "10.0.0.10", "10.0.0.11,fd00::11",
			[]string{"10.96.0.1", "fd00:96::1", "::1", "10.0.0.10"},
			[]string{"10.96.0.1", "127.0.0.1", "10.0.0.11", "fd00::11", "fd00:96::1", "::1", "10.0.0.10"}},
		// A custom service range replaces the default Service IP rather than
		// adding to it: 10.96.0.1 is in no Service's range here.
		{"single-stack on a custom range", "10.42.0.0/16", "10.43.0.0/16", "", "10.0.0.11",
			[]string{"10.43.0.1"},
			[]string{"10.43.0.1", "127.0.0.1", "10.0.0.11"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netc, err := netcfg.Parse(tc.podCIDR, tc.svcCIDR)
			if err != nil {
				t.Fatal(err)
			}
			netc.ClusterDomain = "fuzz.internal"

			in := New("cp1", tc.nodeIP, netc, tc.vip)
			if in.Host != "cp1" {
				t.Errorf("Host = %q, want cp1", in.Host)
			}
			if in.NodeIP != tc.nodeIP {
				t.Errorf("NodeIP = %q, want the resolved address %q", in.NodeIP, tc.nodeIP)
			}
			if in.SANs == nil {
				t.Fatal("New returned no SANs")
			}
			if !slices.Equal(in.SANs.IPs, tc.wantSANs) {
				t.Errorf("SAN IPs = %q, want %q", in.SANs.IPs, tc.wantSANs)
			}
			if in.SANs.Domain != "fuzz.internal" {
				t.Errorf("SAN domain = %q, want the netconfig's fuzz.internal", in.SANs.Domain)
			}
			api := apiserverLeaf(t, pki.LeafSpecs(in.Host, in.NodeIP, in.SANs))
			if !slices.Equal(api.IPs, tc.wantAPIServerIPs) {
				t.Errorf("apiserver IP SANs = %q, want %q", api.IPs, tc.wantAPIServerIPs)
			}
			if !slices.Contains(api.DNS, "kubernetes.default.svc.fuzz.internal") {
				t.Errorf("apiserver DNS SANs = %q, want kubernetes.default.svc.fuzz.internal from the netconfig's domain", api.DNS)
			}

			// The constructor reproduces, certificate for certificate, the
			// hand-built construction the landed pins hold the lifecycle paths
			// to: the netconfig's APIServerSANs with the VIP, under its domain.
			got := pki.LeafSpecs(in.Host, in.NodeIP, in.SANs)
			want := pki.LeafSpecs("cp1", tc.nodeIP,
				&pki.APISANs{IPs: netc.APIServerSANs(tc.vip), Domain: netc.ClusterDomain})
			compareLeafSpecs(t, got, want)
		})
	}
}

// TestForNodeUsesRenewalsNodeAddressRule pins the recorded-address rule the
// constructor applies to a node state: the recorded KubeletNodeIP ("v4,v6"
// under dual-stack), the recorded primary standing in when only it is recorded
// on a single-stack node, and renewal's refusal — naming the dual-stack pair —
// wherever renewal refuses. No path through the constructor may re-detect an
// address for a node that already has state, and no path may fall back to the
// primary on a dual-stack node: that fallback reissues an apiserver
// certificate without the node's IPv6 address. With the rule answered,
// ForNode builds exactly the inputs New builds.
func TestForNodeUsesRenewalsNodeAddressRule(t *testing.T) {
	origConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = origConf })

	dual, err := netcfg.Parse("10.42.0.0/16,fd00:42::/56", "10.43.0.0/16,fd00:43::/108")
	if err != nil {
		t.Fatal(err)
	}
	dual.ClusterDomain = "fuzz.internal"
	single, err := netcfg.Parse("10.245.0.0/16", "10.96.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	single.ClusterDomain = "fuzz.internal"

	t.Run("the recorded pair and a VIP", func(t *testing.T) {
		const pair, vip = "10.0.0.11,fd00::11", "10.0.0.10"
		st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.11",
			KubeletNodeIP: pair, VIP: nodestate.VIP{Address: vip}}
		in, err := ForNode(st, dual, "cp1")
		if err != nil {
			t.Fatalf("ForNode: %v", err)
		}
		compareInputs(t, in, New("cp1", pair, dual, vip))
	})
	t.Run("single-stack with only the primary recorded", func(t *testing.T) {
		st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.11"}
		in, err := ForNode(st, single, "cp1")
		if err != nil {
			t.Fatalf("ForNode: %v", err)
		}
		compareInputs(t, in, New("cp1", "10.0.0.11", single, ""))
	})
	t.Run("a dual-stack node whose state lost the pair is refused", func(t *testing.T) {
		st := &nodestate.NodeState{Role: "controller", NodeIP: "10.9.9.99",
			VIP: nodestate.VIP{Address: "10.0.0.10"}}
		in, err := ForNode(st, dual, "cp1")
		if err == nil {
			t.Fatalf("ForNode issued for %+v; renewal refuses a dual-stack node whose state records no v4,v6 pair", in)
		}
		if !strings.Contains(err.Error(), "v4,v6") {
			t.Errorf("err = %v, want renewal's refusal naming the dual-stack pair", err)
		}
	})
	t.Run("a state with no address at all is refused", func(t *testing.T) {
		st := &nodestate.NodeState{Role: "controller"}
		if _, err := ForNode(st, single, "cp1"); err == nil || !strings.Contains(err.Error(), "kubelet node IP") {
			t.Errorf("err = %v, want a refusal naming the missing kubelet node IP", err)
		}
	})
}

// TestForNodeAnswersEveryRecordedAddressShape pins ForNode's answer, address
// and refusal text alike, for each combination of what a node state records
// and the cluster's stack: the rule is nodestate's, shared with every other
// reader of a recorded address, so a change to it shows here as a change to
// what renewal, the rotations and the upgrade issue for.
func TestForNodeAnswersEveryRecordedAddressShape(t *testing.T) {
	origConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = origConf })

	dual, err := netcfg.Parse("10.42.0.0/16,fd00:42::/56", "10.43.0.0/16,fd00:43::/108")
	if err != nil {
		t.Fatal(err)
	}
	single, err := netcfg.Parse("10.245.0.0/16", "10.96.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	refusal := func(need string) string {
		return "node state " + nodestate.StatePath() + " records no kubelet node IP" + need +
			"; refusing to re-detect an address for a node that already has state -- repair the file"
	}
	const dualNeed = " (dual-stack needs the v4,v6 pair)"

	for _, tc := range []struct {
		name    string
		st      nodestate.NodeState
		netc    netcfg.Config
		want    string
		wantErr string
	}{
		{"the kubelet address recorded, single-stack", nodestate.NodeState{NodeIP: "10.0.0.1", KubeletNodeIP: "10.0.0.1"}, single, "10.0.0.1", ""},
		{"the kubelet pair recorded, dual-stack", nodestate.NodeState{NodeIP: "10.0.0.1", KubeletNodeIP: "10.0.0.1,fd00::1"}, dual, "10.0.0.1,fd00::1", ""},
		{"only the primary recorded, single-stack", nodestate.NodeState{NodeIP: "10.0.0.1"}, single, "10.0.0.1", ""},
		{"only the primary recorded, dual-stack", nodestate.NodeState{NodeIP: "10.0.0.1"}, dual, "", refusal(dualNeed)},
		{"no address recorded, single-stack", nodestate.NodeState{}, single, "", refusal("")},
		{"no address recorded, dual-stack", nodestate.NodeState{}, dual, "", refusal(dualNeed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := ForNode(&tc.st, tc.netc, "cp1")
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if in.NodeIP != tc.want || gotErr != tc.wantErr {
				t.Errorf("ForNode = %q, %q; want %q, %q", in.NodeIP, gotErr, tc.want, tc.wantErr)
			}
		})
	}
}

// TestCertinputsImportsStayALeafAbovePki pins the import direction the
// constructor's design stands on. certinputs may import pki — its values name
// the SAN purposes pki issues — but pki must never import certinputs: the
// reverse edge is an import cycle. And certinputs must never import the
// lifecycle packages that construct through it (internal/cluster, internal/ha,
// internal/certs, internal/upgrade): it owns values, those own lifecycles.
func TestCertinputsImportsStayALeafAbovePki(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // the module root, two levels above internal/certinputs

	pkiImports := goListImports(t, root, "github.com/ctrliq/stratum/internal/pki")
	if pkiImports["github.com/ctrliq/stratum/internal/certinputs"] {
		t.Error("internal/pki imports internal/certinputs; the constructor may import pki, never the reverse")
	}
	certimports := goListImports(t, root, "github.com/ctrliq/stratum/internal/certinputs")
	for _, lifecycle := range []string{"cluster", "ha", "certs", "upgrade"} {
		if certimports["github.com/ctrliq/stratum/internal/"+lifecycle] {
			t.Errorf("internal/certinputs imports internal/%s; the value package above pki imports no lifecycle package", lifecycle)
		}
	}
}

// apiserverLeaf returns the apiserver leaf of specs, failing the test when
// LeafSpecs gave none.
func apiserverLeaf(t *testing.T, specs []pki.CertSpec) *pki.CertSpec {
	t.Helper()
	for i := range specs {
		if specs[i].Name == "apiserver" {
			return &specs[i]
		}
	}
	t.Fatal("LeafSpecs has no apiserver leaf")
	return nil
}

// compareLeafSpecs fails the test when two LeafSpecs results differ, naming
// the first leaf that does.
func compareLeafSpecs(t *testing.T, got, want []pki.CertSpec) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("LeafSpecs gave %d leaves, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s leaf = %+v, want %+v", want[i].Name, got[i], want[i])
		}
	}
}

// compareInputs fails the test when two certificate inputs differ.
func compareInputs(t *testing.T, got, want Inputs) {
	t.Helper()
	if got.Host != want.Host || got.NodeIP != want.NodeIP || !reflect.DeepEqual(got.SANs, want.SANs) {
		t.Errorf("certificate inputs = %+v, want %+v", got, want)
	}
}

// goListImports lists the non-test packages pkg imports directly, as go list
// reports them.
func goListImports(t *testing.T, dir, pkg string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", pkg)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v: %s", pkg, err, out)
	}
	imports := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			imports[line] = true
		}
	}
	return imports
}
