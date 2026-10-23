package netcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// GrantServerURL is the apiserver endpoint a remote join grant names: the
// --server of the join lines `stratum token create` prints, and the server the
// MCP token_create returns. Both front ends derive it here so they cannot
// print different answers for the same node.
//
// The rule is purpose-specific, and its purpose is remote. The joiner dials
// this address from another host, so it must survive the controller that
// minted the token going away: the cluster's VIP when this node records one,
// else this node's own public address. Local component kubeconfigs follow a
// different rule — kubeconfig.NodeServerURL, the recorded endpoint, else the
// local apiserver — and may target loopback on a node without a VIP, which is
// exactly the address a remote grant must never name: a joiner handed
// 127.0.0.1 dials itself.
//
// nodeIP is this node's own address, the primary nodestate.NodeIP answers. A
// caller can still pass the kubelet's "v4,v6" pair (the MCP tests inject one
// through deps.nodeIP), so host.Primary takes the first address again as a
// guard for those callers: a URL holds one host, and a joiner handed
// "https://[10.0.0.7,fd00::7]:6443" dials nothing. A recorded VIP wins
// over nodeIP, so the grant also holds when this node's own view of itself is
// not dialable from outside.
//
// Loopback, all-zeros, link-local, multicast, broadcast, zoned and
// whitespace-padded addresses are refused in either role, VIP or node address,
// rather than printed. A loopback or all-zeros address (0.0.0.0, ::) makes a
// Linux joiner dial itself, so the grant would join the worker to whichever
// node runs the command. A multicast address (ff02::1, 224.0.0.1) or the
// broadcast address (255.255.255.255) names many hosts rather than one
// apiserver. An IPv4 link-local address (169.254.1.1) is not routed beyond
// one link, so only a joiner on that link can dial it. A Linux joiner cannot
// dial an unzoned IPv6 link-local address (fe80::1) even on the same link:
// the connect needs a zone that names the joiner's own interface, and the URL
// carries none. Those kinds are host.UnreachableKind's, the same list
// host.CheckNodeIP refuses for a worker start, because the kubelet never
// registers a loopback, all-zeros, multicast or link-local address as a node
// IP and the broadcast address names every host on the link rather than one
// node. The refusal uses UnreachableKind's names for them. A zone
// ("fe80::1%eth0") names an interface on this host only, and url.Parse
// rejects the raw % the URL would carry, so the joiner cannot even read its
// join line. A padded address (" 127.0.0.1") is refused for its padding,
// which no URL host may carry; each refusal of an address quotes it so the
// padding shows.
//
// Both the VIP and the node address must be IP literals without a zone. The
// node address comes from this node's own state or $NODE_IP and is always
// meant to be an IP. The VIP matches what config loading accepts for
// spec.vip.address, which is only an IP, and a joining control plane inherits
// it from the join bundle's authenticated metadata, so a VIP that is a name
// comes only from a hand-edited node state. A name in either role is refused
// rather than printed: a joiner resolves "localhost" to itself, and an
// inet_aton-style resolver reads the shorthand "127.1", which the IP parse
// rejects, as 127.0.0.1.
func GrantServerURL(nodeIP string) (string, error) {
	vip := ""
	if st, err := nodestate.LoadState(); err == nil {
		vip = st.VIP.Address
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A state file that exists but cannot be read is refused rather than
		// answered from the node address alone: the record is the only source
		// for the VIP, and guessing around it prints a join line that strands
		// the joiner on a cluster it cannot reach.
		return "", fmt.Errorf("read node state for the cluster's VIP: %w", err)
	}
	if vip != "" {
		if why := undialable(vip); why != "" {
			return "", fmt.Errorf("the recorded VIP %q is %s; a remote join grant must name an address another host can dial", vip, why)
		}
		return apiserverURL(vip), nil
	}
	// The guard for a caller that passes a "v4,v6" pair: the primary is taken
	// before the node-address checks, so they judge the one host the URL would
	// carry rather than the whole pair being bracketed into a URL no joiner can
	// dial. A pair with no primary (",fd00::7") then meets the empty check.
	// undialable would refuse "" as well, as not an IP literal; the empty check
	// gives the clearer message, naming the missing node address.
	nodeIP = host.Primary(nodeIP)
	if nodeIP == "" {
		return "", fmt.Errorf("no VIP is configured and no node address is recorded, so there is no address a remote joiner can dial")
	}
	if why := undialable(nodeIP); why != "" {
		return "", fmt.Errorf("this node's address %q is %s; without a VIP a remote join grant must name an address another host can dial", nodeIP, why)
	}
	return apiserverURL(nodeIP), nil
}

// apiserverURL is the https URL an apiserver serves at host. net.JoinHostPort
// brackets an IPv6 literal; without the brackets the port reads as another
// hextet of the address.
func apiserverURL(host string) string {
	return "https://" + net.JoinHostPort(host, strconv.Itoa(version.APIServerPort))
}

// undialable says why a remote joiner cannot dial addr, a VIP or a node
// address, or is "" when it can; the answer reads after "is". Padding is
// checked first because the IP parse rejects a padded literal, so the refusal
// names the padding rather than calling the address a name. The zone is
// checked before the kind, so a zoned link-local literal is refused for its
// zone, the part the URL cannot carry. The kind is host.UnreachableKind's, the
// one list host.CheckNodeIP also refuses for a worker's node IP, so a join URL
// and a node IP cannot disagree on which kinds of address they refuse; it
// judges an IPv4-mapped literal (::ffff:127.0.0.1) as the IPv4 it names.
func undialable(addr string) string {
	if strings.TrimSpace(addr) != addr {
		return "padded with whitespace, which no URL host may carry"
	}
	ip, err := netip.ParseAddr(addr)
	switch {
	case err != nil:
		return "not an IP literal, and a joiner resolves a name by its own lookup, so it may dial itself"
	case ip.Zone() != "":
		return "an IPv6 literal with a zone, which names an interface on this host only and puts a raw % in the URL that url.Parse rejects"
	}
	return host.UnreachableKind(ip)
}
