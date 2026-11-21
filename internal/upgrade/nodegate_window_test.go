package upgrade

import "testing"

// The node gate follows a full control-plane restart, as disruptive as each component restart.
func TestNodeGateHealthzWindowCoversARestartedNode(t *testing.T) {
	if nodeGateHealthzWindow < componentHealthzWindow {
		t.Fatalf("node gate /healthz window %s < component window %s; the node gate "+
			"follows a containerd+kubelet restart and needs at least the component budget",
			nodeGateHealthzWindow, componentHealthzWindow)
	}
}
