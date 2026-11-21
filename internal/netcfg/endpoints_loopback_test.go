// External because kubeconfig imports pki, which imports netcfg: an internal one would cycle.
package netcfg_test

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// A local kubeconfig may target loopback, exactly what a remote grant must never name.
func TestGrantServerURLNeverInheritsTheLocalLoopbackEndpoint(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	if err := nodestate.SaveState(&nodestate.NodeState{NodeIP: "192.168.99.12"}); err != nil {
		t.Fatal(err)
	}

	local := kubeconfig.NodeServerURL("", "")
	if local != "https://127.0.0.1:6443" {
		t.Fatalf("local kubeconfig endpoint = %q, want the loopback apiserver this pin is written against", local)
	}
	_, err := netcfg.GrantServerURL("127.0.0.1")
	if err == nil {
		t.Fatalf("a remote grant accepted the local kubeconfigs' endpoint %q", local)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %v does not say the address is loopback", err)
	}
}
