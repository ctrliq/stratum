// Package certinputs builds the certificate inputs each control-plane lifecycle issues from.
package certinputs

import (
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
)

type Inputs struct {
	Host   string
	NodeIP string       // kubelet form: the v4 address, or the v4,v6 pair under dual-stack
	SANs   *pki.APISANs // LeafSpecs takes the first IPv4 entry as the Service IP
}

// New builds inputs for a node whose address is already resolved; args alone decide.
func New(host, nodeIP string, netc netcfg.Config, vip string) Inputs {
	return Inputs{
		Host:   host,
		NodeIP: nodeIP,
		SANs:   &pki.APISANs{IPs: netc.APIServerSANs(vip), Domain: netc.ClusterDomain},
	}
}

// ForNode applies the recorded-address rule renewal pins; it never re-detects an address.
func ForNode(st *nodestate.NodeState, netc netcfg.Config, host string) (Inputs, error) {
	nodeIP, err := nodestate.RecordedNodeIPs(st, netc.DualStack)
	if err != nil {
		return Inputs{}, err
	}
	return New(host, nodeIP, netc, st.VIP.Address), nil
}
