package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/version"
)

// The part-way failure: after netconfig persisted, at the certificate step.
var errPartialStart = errors.New("first start failed at the certificate step")

// Points every establishment decision's paths at empty temp dirs.
func prepareFixture(t *testing.T) {
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
	// The etcd data lock lives beside the data dir; redirect it off the host too.
	version.DataDir = filepath.Join(t.TempDir(), "data")
	oldHostname := hostHostname
	t.Cleanup(func() { hostHostname = oldHostname })
	hostHostname = func() (string, error) { return "cp1", nil }
}

// Takes the real etcd data lock and holds it across the test's prepares.
var heldDataLocks sync.Map // *testing.T -> func()

func holdDataLock(t *testing.T) func() {
	t.Helper()
	if held, ok := heldDataLocks.Load(t); ok {
		return held.(func())
	}
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	heldDataLocks.Store(t, release)
	t.Cleanup(func() {
		release()
		heldDataLocks.Delete(t)
	})
	return release
}

// An issued CA is establishment: certificates name the recorded network.
func establishCA(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.crt"), []byte("test cluster CA"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func establishEtcdData(t *testing.T) {
	t.Helper()
	path := filepath.Join(version.EtcdDataDir, "member", "snap", "db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stratum test: retained etcd member data"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startController(t *testing.T, cfg *ClusterConfig, choice host.NodeIPChoice, startErr error) error {
	t.Helper()
	saved := assembleNode
	assembleNode = func(_ context.Context, a ha.NodeAssembly, _ ha.AssembleRequest) (host.NodeIPChoice, error) {
		if _, err := a.ResolveNet(); err != nil {
			return host.NodeIPChoice{}, err
		}
		if err := a.BeforeCerts(choice); err != nil {
			return host.NodeIPChoice{}, err
		}
		if startErr != nil {
			return host.NodeIPChoice{}, startErr
		}
		return choice, nil
	}
	t.Cleanup(func() { assembleNode = saved })
	_, err := PrepareController(cfg, "", holdDataLock(t))
	return err
}

func runFailedFirstStart(t *testing.T, cfg *ClusterConfig, choice host.NodeIPChoice) {
	t.Helper()
	if err := startController(t, cfg, choice, errPartialStart); err == nil || !strings.Contains(err.Error(), errPartialStart.Error()) {
		t.Fatalf("the failed first start = %v, want %v", err, errPartialStart)
	}
	if _, err := LoadState(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the failed first start saved node.yaml (%v); node.yaml is written only after the whole assembly succeeds", err)
	}
}

func TestRetryWithACorrectedNetworkConvergesOnAPartialFirstStart(t *testing.T) {
	const podA, svcA, domainA = "10.42.0.0/16", "10.43.0.0/16", "first.internal"
	const podB, svcB, domainB = "10.99.0.0/16", "10.98.0.0/16", "second.internal"
	prepareFixture(t)
	choice := host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"}
	runFailedFirstStart(t, &ClusterConfig{Spec: Spec{Network: Network{PodCIDR: podA, ServiceCIDR: svcA, ClusterDomain: domainA}}}, choice)
	firstNetconfig := podA + "\n" + svcA + "\n" + domainA + "\n"
	if b, err := os.ReadFile(version.NetConfigPath); err != nil || string(b) != firstNetconfig {
		t.Fatalf("netconfig after the failed first start = %q, %v; want %q", b, err, firstNetconfig)
	}

	corrected := &ClusterConfig{Spec: Spec{Network: Network{PodCIDR: podB, ServiceCIDR: svcB, ClusterDomain: domainB}}}
	if err := startController(t, corrected, choice, nil); err != nil {
		t.Fatalf("the retry with a corrected network: %v", err)
	}
	saved, err := LoadState()
	if err != nil {
		t.Fatalf("the retry saved no node.yaml: %v", err)
	}
	if saved.PodCIDR != podB || saved.ServiceCIDR != svcB || saved.ClusterDomain != domainB {
		t.Errorf("node.yaml after the retry records pod %q, service %q, domain %q; want the corrected %q, %q, %q",
			saved.PodCIDR, saved.ServiceCIDR, saved.ClusterDomain, podB, svcB, domainB)
	}
	if saved.NodeIP != choice.IP || saved.KubeletNodeIP != choice.IP {
		t.Errorf("node.yaml after the retry records %q/%q, want the resolved %q", saved.NodeIP, saved.KubeletNodeIP, choice.IP)
	}
	if saved.EtcdState != "new" || saved.EtcdInitialCluster != "cp1=https://10.0.0.11:2380" || saved.EtcdHasPeers {
		t.Errorf("node.yaml after the retry records membership %q (%q, peers %v); want a fresh single-member state=new",
			saved.EtcdInitialCluster, saved.EtcdState, saved.EtcdHasPeers)
	}
	correctedNetconfig := podB + "\n" + svcB + "\n" + domainB + "\n"
	if b, err := os.ReadFile(version.NetConfigPath); err != nil || string(b) != correctedNetconfig {
		t.Errorf("netconfig after the retry = %q, %v; want the corrected %q", b, err, correctedNetconfig)
	}
	if err := startController(t, corrected, choice, nil); err != nil {
		t.Fatalf("the start after the retry: %v", err)
	}
	if saved, err := LoadState(); err != nil || saved.PodCIDR != podB || saved.ServiceCIDR != svcB || saved.ClusterDomain != domainB {
		t.Errorf("node.yaml after the following start = %+v, %v; want the corrected network kept", saved, err)
	}
}

func TestRetryAdoptsTheCorrectedFieldsOfAPendingNode(t *testing.T) {
	const podA, svcA, domainA = "10.42.0.0/16", "10.43.0.0/16", "first.internal"
	for _, tc := range []struct {
		name                         string
		retry                        Network
		wantPod, wantSvc, wantDomain string
	}{
		{"every field corrected",
			Network{PodCIDR: "10.99.0.0/16", ServiceCIDR: "10.98.0.0/16", ClusterDomain: "second.internal"},
			"10.99.0.0/16", "10.98.0.0/16", "second.internal"},
		{"only the pod CIDR corrected",
			Network{PodCIDR: "10.99.0.0/16", ServiceCIDR: svcA, ClusterDomain: domainA},
			"10.99.0.0/16", svcA, domainA},
		{"only the cluster domain corrected",
			Network{PodCIDR: podA, ServiceCIDR: svcA, ClusterDomain: "second.internal"},
			podA, svcA, "second.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareFixture(t)
			choice := host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"}
			runFailedFirstStart(t, &ClusterConfig{Spec: Spec{Network: Network{PodCIDR: podA, ServiceCIDR: svcA, ClusterDomain: domainA}}}, choice)

			netc, err := resolveNetConfig(tc.retry, nil)
			if err != nil {
				t.Fatalf("the retry with a corrected network: %v", err)
			}
			if netc.PodCIDR != tc.wantPod || netc.ServiceCIDR != tc.wantSvc || netc.ClusterDomain != tc.wantDomain {
				t.Fatalf("the retry resolved pod %q, service %q, domain %q; want %q, %q, %q",
					netc.PodCIDR, netc.ServiceCIDR, netc.ClusterDomain, tc.wantPod, tc.wantSvc, tc.wantDomain)
			}
			want := tc.wantPod + "\n" + tc.wantSvc + "\n"
			if tc.wantDomain != netdefault.ClusterDomain {
				want += tc.wantDomain + "\n"
			}
			if b, err := os.ReadFile(version.NetConfigPath); err != nil || string(b) != want {
				t.Errorf("netconfig after the retry = %q, %v; want %q", b, err, want)
			}
		})
	}
}

func TestCorrectedNetworkRefusedOnAnEstablishedNode(t *testing.T) {
	const pod, svc, domain = "10.42.0.0/16", "10.43.0.0/16", "cluster.internal"
	const podB, svcB = "10.99.0.0/16", "10.98.0.0/16"
	for _, tc := range []struct {
		name        string
		establish   func(t *testing.T) *NodeState
		recordedPod string
		recordedSvc string
		recordedDm  string
	}{
		{"a node state with a membership", func(t *testing.T) *NodeState {
			if err := SaveState(&NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
				EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380", EtcdState: "existing", EtcdHasPeers: true,
				PodCIDR: pod, ServiceCIDR: svc, ClusterDomain: domain}); err != nil {
				t.Fatal(err)
			}
			prev, err := LoadState()
			if err != nil {
				t.Fatal(err)
			}
			return prev
		}, pod, svc, domain},
		{"the cluster CA with no node state", func(t *testing.T) *NodeState {
			establishCA(t)
			return nil
		}, netdefault.PodCIDR, netdefault.ServiceCIDR, netdefault.ClusterDomain},
		{"retained etcd data with no node state", func(t *testing.T) *NodeState {
			establishEtcdData(t)
			return nil
		}, netdefault.PodCIDR, netdefault.ServiceCIDR, netdefault.ClusterDomain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareFixture(t)
			prev := tc.establish(t)

			_, err := resolveNetConfig(Network{PodCIDR: tc.recordedPod, ServiceCIDR: tc.recordedSvc, ClusterDomain: "other.internal"}, prev)
			if err == nil || !strings.Contains(err.Error(), "other.internal") || !strings.Contains(err.Error(), tc.recordedDm) {
				t.Errorf("a corrected cluster domain on an established node = %v; want a refusal naming both %q and %q",
					err, "other.internal", tc.recordedDm)
			}
			if _, statErr := os.Stat(version.NetConfigPath); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the refused domain left a netconfig behind: %v", statErr)
			}

			// Corrected CIDRs never take; the recorded ranges win.
			netc, err := resolveNetConfig(Network{PodCIDR: podB, ServiceCIDR: svcB, ClusterDomain: tc.recordedDm}, prev)
			if err != nil {
				t.Fatalf("the retry with corrected CIDRs: %v", err)
			}
			if netc.PodCIDR != tc.recordedPod || netc.ServiceCIDR != tc.recordedSvc {
				t.Errorf("corrected CIDRs on an established node = %s, %s; want the recorded %s, %s",
					netc.PodCIDR, netc.ServiceCIDR, tc.recordedPod, tc.recordedSvc)
			}
			want := tc.recordedPod + "\n" + tc.recordedSvc + "\n"
			if tc.recordedDm != netdefault.ClusterDomain {
				want += tc.recordedDm + "\n"
			}
			if b, err := os.ReadFile(version.NetConfigPath); err != nil || string(b) != want {
				t.Errorf("netconfig after the corrected CIDRs = %q, %v; want the recorded %q", b, err, want)
			}
		})
	}
}

func TestPrepareControllerRefusesAnUnreadableNodeState(t *testing.T) {
	prepareFixture(t)
	if err := os.WriteFile(filepath.Join(version.K8sConfDir, "node.yaml"), []byte("{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := startController(t, &ClusterConfig{}, host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"}, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to start from a guessed address") {
		t.Fatalf("a start over an unreadable node state = %v; want a refusal to start from a guessed address", err)
	}
	if _, statErr := os.Stat(version.NetConfigPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused start wrote a netconfig: %v", statErr)
	}
}

func TestFailedStartKeepsUnprovenNetworkAuthoritative(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(fmt.Sprint("network-resolved-", resolve), func(t *testing.T) {
			prepareFixture(t)
			if err := netcfg.Save("10.42.0.0/16", "10.43.0.0/16", "existing.internal"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(version.NetConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			saved := assembleNode
			t.Cleanup(func() { assembleNode = saved })
			assembleNode = func(_ context.Context, a ha.NodeAssembly, _ ha.AssembleRequest) (host.NodeIPChoice, error) {
				if resolve {
					if _, err := a.ResolveNet(); err != nil {
						return host.NodeIPChoice{}, err
					}
				}
				return host.NodeIPChoice{}, errPartialStart
			}
			if _, err := PrepareController(&ClusterConfig{}, "", holdDataLock(t)); !errors.Is(err, errPartialStart) {
				t.Fatalf("failure = %v", err)
			}
			if partialPreparationJournaled() {
				t.Error("failure reclassified existing authoritative network as pending")
			}
			if _, err := resolveNetConfig(Network{ClusterDomain: "changed.internal"}, nil); err == nil {
				t.Error("retry replaced unproven network")
			}
			after, err := os.ReadFile(version.NetConfigPath)
			if err != nil || string(after) != string(before) {
				t.Errorf("network changed: %q, %v", after, err)
			}
		})
	}
}

func TestUnreadableEstablishmentEvidenceRefusesNetworkWrite(t *testing.T) {
	for _, evidence := range []string{"ca", "etcd", "network", "journal"} {
		t.Run(evidence, func(t *testing.T) {
			prepareFixture(t)
			path := map[string]string{
				"ca":      filepath.Join(version.PKIDir, "ca.crt"),
				"etcd":    filepath.Join(version.EtcdDataDir, "member", "snap", "db"),
				"network": version.NetConfigPath,
				"journal": partialPreparationPath(),
			}[evidence]
			if evidence == "journal" {
				if err := netcfg.Save("10.42.0.0/16", "10.43.0.0/16", "existing.internal"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(path), path); err != nil {
				t.Fatal(err)
			}
			domain := "guessed.internal"
			if evidence == "journal" {
				domain = "existing.internal"
			}
			if _, err := resolveNetConfig(Network{ClusterDomain: domain}, nil); err == nil {
				t.Error("unreadable evidence authorized network resolution")
			}
			if evidence != "network" && evidence != "journal" {
				if _, err := os.Stat(version.NetConfigPath); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("network written despite unreadable evidence: %v", err)
				}
			}
		})
	}
}
