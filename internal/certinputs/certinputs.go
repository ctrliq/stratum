// Package certinputs defines the certificate inputs every control-plane
// lifecycle issues leaves from — the host, the node's address, and the named
// SAN purposes the cluster's network facts call for — and builds them through
// one constructor. Bring-up and the HA join resolve the address first and
// construct through New; renewal and the upgrade answer for a node that already
// has state through ForNode, which applies the recorded-address rule renewal
// pins, refusal included. The one constructor exists because per-path assembly
// of the triple drifted: a path that fell back from the recorded pair to the
// primary reissued an apiserver certificate without the node's IPv6 address —
// a certificate the next renewal, which refuses that node, would reissue again.
//
// The package is a leaf of value types above pki: it may import pki (its
// values name the SAN purposes pki issues) and the cluster's state and network
// facts, but never the lifecycle packages that construct through it, and pki
// never imports it back — the reverse edge would be an import cycle.
package certinputs

import (
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
)

// Inputs are the certificate inputs a node's control-plane leaves are issued
// from, in the shape pki's leaf builders take. Host is the node's short
// hostname. NodeIP is its address in the kubelet form, "v4" or the "v4,v6"
// pair under dual-stack — pki.LeafSpecs splits it and makes each family a SAN
// on the node-bound certificates. SANs are the apiserver-cert names that come
// from the cluster rather than the node: the `kubernetes` Service IP first
// (LeafSpecs takes the first IPv4 entry as the Service IP), the IPv6 service
// IP and "::1" under dual-stack, then the VIP as an ordinary extra, under the
// cluster's DNS domain.
type Inputs struct {
	Host   string
	NodeIP string
	SANs   *pki.APISANs
}

// New builds the inputs for a node whose address is already resolved — the
// form the shared node assembly (bring-up and the HA join) has when it issues
// certificates, where first preparation works from in-memory state before any
// state file exists, so the arguments alone must decide. The SANs are the
// netconfig's APIServerSANs with this node's VIP, under the netconfig's
// cluster domain: the same named purposes every path is pinned to.
func New(host, nodeIP string, netc netcfg.Config, vip string) Inputs {
	return Inputs{
		Host:   host,
		NodeIP: nodeIP,
		SANs:   &pki.APISANs{IPs: netc.APIServerSANs(vip), Domain: netc.ClusterDomain},
	}
}

// ForNode builds the inputs for a node that already has state, applying the
// recorded-address rule renewal pins (nodestate.RecordedNodeIPs): the recorded
// KubeletNodeIP (the "v4,v6" pair under dual-stack), or the recorded primary
// standing in when only it is recorded on a single-stack node. A dual-stack
// node whose state lost the pair is refused — naming the pair — rather than
// re-detected or fallen back to the primary, and a state with no address at
// all is refused too: no path through the constructor may re-detect an
// address for a node that already has state.
// The SANs and the VIP come from the same state read, so they cannot disagree.
func ForNode(st *nodestate.NodeState, netc netcfg.Config, host string) (Inputs, error) {
	nodeIP, err := nodestate.RecordedNodeIPs(st, netc.DualStack)
	if err != nil {
		return Inputs{}, err
	}
	return New(host, nodeIP, netc, st.VIP.Address), nil
}
