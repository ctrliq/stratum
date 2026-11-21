package certs

import (
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

func TestRenewalReusesTheRecordedNodeIP(t *testing.T) {
	origConf, origNet := version.K8sConfDir, version.NetConfigPath
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig") // absent: the IPv4 defaults
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = origConf, origNet })
	t.Setenv("NODE_IP", "10.9.9.9")

	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
	}); err != nil {
		t.Fatal(err)
	}
	_, ip, _, _, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.99.12" {
		t.Errorf("renewal would reissue for %q; want the recorded 192.168.99.12, not $NODE_IP", ip)
	}
}
