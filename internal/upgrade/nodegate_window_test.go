package upgrade

import "testing"

// The node gate runs after the host binary swap and the containerd+kubelet
// restart, which is at least as disruptive to the local control plane as the
// single component restart each 2m component window was sized for — and the
// component gates, including kube-apiserver's, have already passed by the time
// it runs. Its /healthz budget therefore never starts below the component
// window: a node that answered its component gate but needs longer to settle
// after the full restart must not be rolled back for having less slack.
func TestNodeGateHealthzWindowCoversARestartedNode(t *testing.T) {
	if nodeGateHealthzWindow < componentHealthzWindow {
		t.Fatalf("node gate /healthz window %s < component window %s; the node gate "+
			"follows a containerd+kubelet restart and needs at least the component budget",
			nodeGateHealthzWindow, componentHealthzWindow)
	}
}
