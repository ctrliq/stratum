package nodestate

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"

	"github.com/ctrliq/stratum/internal/host"
)

// NodeIP is the recorded primary for a running node; re-detecting can pick the default-route NIC.
func NodeIP() (string, error) {
	st, err := LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		ip, err := host.NodeIP()
		if err != nil {
			return "", err
		}
		primary := host.Primary(ip)
		if primary == "" {
			return "", fmt.Errorf("node IP %q names no primary address before its comma; set NODE_IP=<v4>[,<v6>]", ip)
		}
		a, err := netip.ParseAddr(primary)
		switch {
		case err != nil:
			return "", fmt.Errorf("$NODE_IP's primary address %q is not an IP literal; set NODE_IP=<v4>[,<v6>] "+
				"with no spaces or host names", primary)
		case a.Zone() != "":
			return "", fmt.Errorf("$NODE_IP's primary address %q carries an interface zone, which a node IP "+
				"cannot; set NODE_IP=<v4>[,<v6>] without one", primary)
		}
		return primary, nil
	}
	if err != nil {
		return "", fmt.Errorf("read node state: %w", err)
	}
	if st.NodeIP == "" {
		return "", fmt.Errorf("node state %s records no nodeIP; refusing to re-detect an address for a node that "+
			"already has state -- repair the file", StatePath())
	}
	return st.NodeIP, nil
}

// RecordedNodeIPs answers the recorded kubelet "v4[,v6]" address, or the primary alone.
func RecordedNodeIPs(st *NodeState, dualStack bool) (string, error) {
	switch {
	case st.KubeletNodeIP != "":
		return st.KubeletNodeIP, nil
	case st.NodeIP != "" && !dualStack:
		// On a single-stack node the primary is the kubelet address.
		return st.NodeIP, nil
	}
	need := ""
	if dualStack {
		need = " (dual-stack needs the v4,v6 pair)"
	}
	return "", fmt.Errorf("node state %s records no kubelet node IP%s; refusing to re-detect an address for a "+
		"node that already has state -- repair the file", StatePath(), need)
}
