package cluster

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/version"
)

// Pins the retry contract after a first start fails part-way: netconfig is
// persisted during the network resolution and node.yaml only after the whole
// assembly succeeds, and a refused start writes nothing. A partial first
// start journals its preparation (journalPartialPreparation, written when
// the assembly fails after the network resolution persisted netconfig), and
// the node is PENDING, not established, so the retry with a corrected
// spec.network converges onto the correction. The corrected-network
// recovery and refusal acceptance is pinned end to end in prepare_test.go
// (TestRetryWithACorrectedNetworkConvergesOnAPartialFirstStart,
// TestCorrectedNetworkRefusedOnAnEstablishedNode).

// errCertificates is the part-way failure of the first start: after the
// network resolution saved netconfig, at the certificate step, before SaveState
// ran.
var errCertificates = errors.New("certificate generation failed")

// runFirstStart replaces the assembly seam with one that runs the real
// ResolveNet and BeforeCerts hooks PrepareController wires, and then reports
// assembleErr (nil for a start that reaches SaveState).
func runFirstStart(t *testing.T, choice host.NodeIPChoice, assembleErr error) {
	t.Helper()
	saved := assembleNode
	assembleNode = func(_ context.Context, a ha.NodeAssembly, _ ha.AssembleRequest) (host.NodeIPChoice, error) {
		if _, err := a.ResolveNet(); err != nil {
			return host.NodeIPChoice{}, err
		}
		if err := a.BeforeCerts(choice); err != nil {
			return host.NodeIPChoice{}, err
		}
		if assembleErr != nil {
			return host.NodeIPChoice{}, assembleErr
		}
		return choice, nil
	}
	t.Cleanup(func() { assembleNode = saved })
}

// failFirstStart runs the failed first start and returns the netconfig bytes
// it left. The failure must not have saved node.yaml: that write follows the
// whole assembly.
func failFirstStart(t *testing.T, cfg *ClusterConfig, choice host.NodeIPChoice) string {
	t.Helper()
	runFirstStart(t, choice, errCertificates)
	if _, err := PrepareController(cfg, "", nil); err == nil || !strings.Contains(err.Error(), errCertificates.Error()) {
		t.Fatalf("the failed first start = %v, want %v", err, errCertificates)
	}
	if _, err := LoadState(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed first start saved node.yaml (%v); node.yaml is written only after the whole assembly succeeds", err)
	}
	b, err := os.ReadFile(version.NetConfigPath)
	if err != nil {
		t.Fatalf("read netconfig after the failed first start: %v", err)
	}
	return string(b)
}

// TestRetryAfterAFailedFirstStart pins the retry: a first start that failed
// part-way is a PENDING node, so the retry adopts spec.network — the same spec
// re-persists the recorded network, a corrected CIDR or a corrected domain
// converges onto the correction and is persisted — while the missing node.yaml
// stays the ordinary first-boot case for the address and the etcd membership,
// and every retry persists a node.yaml carrying the network it adopted.
func TestRetryAfterAFailedFirstStart(t *testing.T) {
	const pod, svc, domain = "10.42.0.0/16", "10.43.0.0/16", "first.internal"
	for _, tc := range []struct {
		name                     string
		retry                    Network
		wantPod, wantSvc, wantDm string
	}{
		{"the same spec persists the recorded network", Network{PodCIDR: pod, ServiceCIDR: svc, ClusterDomain: domain}, pod, svc, domain},
		{"a corrected CIDR converges", Network{PodCIDR: "10.99.0.0/16", ServiceCIDR: "10.98.0.0/16", ClusterDomain: domain}, "10.99.0.0/16", "10.98.0.0/16", domain},
		{"a corrected domain converges", Network{PodCIDR: pod, ServiceCIDR: svc, ClusterDomain: "second.internal"}, pod, svc, "second.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assemblyFixture(t)
			cfg := &ClusterConfig{Spec: Spec{Network: Network{PodCIDR: pod, ServiceCIDR: svc, ClusterDomain: domain}}}
			choice := host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"}
			netconfig := failFirstStart(t, cfg, choice)
			if want := pod + "\n" + svc + "\n" + domain + "\n"; netconfig != want {
				t.Fatalf("netconfig after the failed first start = %q, want %q", netconfig, want)
			}

			runFirstStart(t, choice, nil)
			if _, err := PrepareController(&ClusterConfig{Spec: Spec{Network: tc.retry}}, "", holdDataLock(t)); err != nil {
				t.Fatalf("the retry after a failed first start: %v", err)
			}

			// The saved node.yaml carries the network the retry adopted, with
			// the resolved address and a fresh single-member state=new: the
			// missing node.yaml was the ordinary first boot, so the corrected
			// network changes neither.
			saved, err := LoadState()
			if err != nil {
				t.Fatalf("the retry saved no node.yaml: %v", err)
			}
			if saved.PodCIDR != tc.wantPod || saved.ServiceCIDR != tc.wantSvc || saved.ClusterDomain != tc.wantDm {
				t.Errorf("node.yaml after the retry records pod %q, service %q, domain %q; want %q, %q, %q",
					saved.PodCIDR, saved.ServiceCIDR, saved.ClusterDomain, tc.wantPod, tc.wantSvc, tc.wantDm)
			}
			if saved.NodeIP != choice.IP || saved.KubeletNodeIP != choice.IP {
				t.Errorf("node.yaml after the retry records %q/%q, want the resolved %q", saved.NodeIP, saved.KubeletNodeIP, choice.IP)
			}
			if saved.EtcdState != "new" || saved.EtcdInitialCluster != "cp1=https://10.0.0.11:2380" || saved.EtcdHasPeers {
				t.Errorf("node.yaml after the retry records membership %q (%q, peers %v); want a fresh single-member state=new",
					saved.EtcdInitialCluster, saved.EtcdState, saved.EtcdHasPeers)
			}

			// The retried start completed, so the node is established on the
			// network it converged onto: netconfig says the same.
			want := tc.wantPod + "\n" + tc.wantSvc + "\n" + tc.wantDm + "\n"
			if b, rerr := os.ReadFile(version.NetConfigPath); rerr != nil || string(b) != want {
				t.Errorf("netconfig after the retry = %q, %v; want %q", b, rerr, want)
			}
		})
	}
}

// TestWALOnlyMemberDataHoldsTheRecordedNetwork pins the journal's conservative
// reading of etcd member history: a node whose member directory holds only a
// write-ahead log segment — the database gone — is not a node whose history
// can be dismissed. The journal exists so a corrected spec.network can
// converge after a start that stopped before anything depended on the
// recorded values; WAL-only member data is the opposite of that, so the node
// keeps the recorded network whatever the journal says.
func TestWALOnlyMemberDataHoldsTheRecordedNetwork(t *testing.T) {
	assemblyFixture(t)
	origEtcd := version.EtcdDataDir
	version.EtcdDataDir = filepath.Join(t.TempDir(), "etcd")
	t.Cleanup(func() { version.EtcdDataDir = origEtcd })
	wal := filepath.Join(version.EtcdDataDir, "member", "wal")
	if err := os.MkdirAll(wal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wal, "0000000000000000-0000000000000000.wal"), []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := holdsRecordedNetworkDependents(nil)
	if err != nil || !held {
		t.Errorf("holdsRecordedNetworkDependents over WAL-only member data = %v, %v; want true — a member whose WAL survives is not a first start", held, err)
	}
}
