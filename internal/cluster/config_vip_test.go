package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestServerURLIsTheVIPElseTheLocalAPIServer: serverURL is the endpoint every
// controller path's kubeconfigs target — the VIP when one is configured (one
// address for in-cluster and external clients), the local apiserver otherwise.
// serverURL is a pure function, so this needs no seam and can fail before any
// kubeconfig is regenerated.
func TestServerURLIsTheVIPElseTheLocalAPIServer(t *testing.T) {
	withVIP := &NodeState{VIP: VIP{Address: "10.0.0.10"}}
	if got := serverURL(withVIP); got != fmt.Sprintf("https://10.0.0.10:%d", version.APIServerPort) {
		t.Errorf("serverURL(VIP 10.0.0.10) = %q, want the VIP endpoint", got)
	}
	if got := serverURL(&NodeState{}); got != fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort) {
		t.Errorf("serverURL(no VIP) = %q, want the local apiserver endpoint", got)
	}
}

// A spec.vip.address that is not an IP is a config error, reported like any
// other, not a panic: a panic there crash-loops the unit on it.
func TestLoadConfigRejectsAVIPThatIsNotAnIP(t *testing.T) {
	for _, addr := range []string{"10.0.0.10/24", "vip.example.com", "10.0.0"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("spec:\n  vip:\n    address: "+addr+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("LoadConfig panicked on vip.address %q: %v", addr, r)
				}
			}()
			_, err = LoadConfig(path)
		}()
		if err == nil || !strings.Contains(err.Error(), "spec.vip.address") || !strings.Contains(err.Error(), addr) {
			t.Errorf("LoadConfig(vip.address %q) = %v, want an error naming the field and value", addr, err)
		}
	}
}

func TestLoadConfigAcceptsAVIP(t *testing.T) {
	for _, addr := range []string{"10.0.0.10", "fd00::10"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("spec:\n  vip:\n    address: "+addr+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(path)
		if err != nil || c.Spec.VIP.Address != addr {
			t.Errorf("LoadConfig(vip.address %q) = %+v, %v", addr, c, err)
		}
	}
}

// spec.network.clusterDomain is validated, since it is written into
// kubelet.conf, apiserver flags and the Corefile.
func TestLoadConfigValidatesTheClusterDomain(t *testing.T) {
	load := func(domain string) (*ClusterConfig, error) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("spec:\n  network:\n    clusterDomain: "+domain+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path)
	}
	if c, err := load("fuzz.internal"); err != nil || c.Spec.Network.ClusterDomain != "fuzz.internal" {
		t.Errorf("fuzz.internal: %v, %v", c, err)
	}
	// Unset is not defaulted: PrepareController needs "not set" to tell a
	// restart that keeps the persisted domain from an explicit cluster.local.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spec: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(path); err != nil || c.Spec.Network.ClusterDomain != "" {
		t.Errorf("unset clusterDomain = %q, %v; want it left empty", c.Spec.Network.ClusterDomain, err)
	}
	if _, err := load(`"Fuzz Internal"`); err == nil || !strings.Contains(err.Error(), "spec.network.clusterDomain") {
		t.Errorf("a domain with a space = %v, want an error naming the field", err)
	}
}
