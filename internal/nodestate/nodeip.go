package nodestate

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"

	"github.com/ctrliq/stratum/internal/host"
)

// NodeIP returns the primary IP this node was brought up with — the address in
// its apiserver certificate and its etcd peer URL — or, when there is no saved
// state, the primary of host.NodeIP's answer ($NODE_IP, then detection),
// validated as an IP literal.
//
// The fallback takes the primary because $NODE_IP may be in the kubelet's
// "v4,v6" form, and every caller uses the answer as one host: the whole pair
// would seed an etcd restore or rejoin with the peer URL
// https://[10.0.0.7,fd00::7]:2380, which no member can dial. A pair with no
// first address is refused rather than answered as an empty host.
//
// The primary must also be an IP literal without a zone. $NODE_IP is taken
// verbatim, so a padded " 10.0.0.7", a host name or a zoned "fe80::1%eth0"
// would otherwise reach a URL as it stands, such as the peer URL
// https:// 10.0.0.7:2380. Either family may come first: an IPv6 primary is
// still answered.
//
// Everything that acts on a node that already runs (cert renewal, `token
// create`, etcd rejoin and snapshot restore, the upgrade roster) must use this,
// not host.NodeIP: re-detecting on a multi-NIC host picks whichever NIC holds the
// default route, which is how a rejoin could add an etcd member at the wrong
// address.
//
// Only a missing state file falls back. An unreadable or malformed one is an
// error: falling back there would re-detect, which is exactly the wrong-NIC
// drift this exists to prevent.
//
// A state that parses but records no address is refused as well. Every writer
// (bring-up, join, worker join) records one, so an empty field is a damaged
// file, and LoadState alone cannot tell: any valid YAML loads, with missing
// fields left empty.
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

// RecordedNodeIPs is the recorded-address rule for a node that has state, in
// kubelet's "v4[,v6]" form: the recorded KubeletNodeIP, or the recorded
// primary standing in for it on a single-stack node. A dual-stack node without
// the pair, or a state with neither, is refused rather than re-detected. Pure:
// the state is the argument, so a caller answers for in-memory state the same
// way it answers for a state read from disk.
func RecordedNodeIPs(st *NodeState, dualStack bool) (string, error) {
	switch {
	case st.KubeletNodeIP != "":
		return st.KubeletNodeIP, nil
	case st.NodeIP != "" && !dualStack:
		// The primary and the kubelet address are the same on a single-stack
		// node, so older state that records only the primary is complete there.
		return st.NodeIP, nil
	}
	need := ""
	if dualStack {
		need = " (dual-stack needs the v4,v6 pair)"
	}
	return "", fmt.Errorf("node state %s records no kubelet node IP%s; refusing to re-detect an address for a "+
		"node that already has state -- repair the file", StatePath(), need)
}
