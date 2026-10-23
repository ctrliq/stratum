package host

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// NodeIPChoice is the node IP a start path settled on, and where it came from.
type NodeIPChoice struct {
	IP     string // "v4" or "v4,v6", as kubelet --node-ip takes it
	Source string // human-readable origin, for the startup log
}

// ResolveNodeIP is the one precedence every start path uses to decide this
// node's IP, except that a start that does not read spec.network.nodeIP skips
// step 3 through ResolveNodeIPWithoutConfig:
//
//  1. flag   — --node-ip on this invocation
//  2. state  — the address this node was brought up with (node.yaml)
//  3. config — spec.network.nodeIP
//  4. $NODE_IP
//  5. detect — the default-route source address, else the single global IPv4
//
// state outranks config and $NODE_IP: the reboot-persistence
// unit's ExecStart never carries --node-ip, so a restart's only explicit
// source is whatever spec.network.nodeIP/$NODE_IP happen to say NOW — which
// can legitimately differ from what the node was actually brought up with
// (an operator editing the config for the NEXT node, not this one). Config
// outranking state would let a restart resolve a different address
// than the one this node's etcd membership and certificates name, which
// nodeIPMoveRefusal (internal/cluster/prepare.go) then correctly refuses to
// start on — turning a config edit for some other purpose into a node that
// cannot reboot. state is empty on a true first boot, so config decides
// that case; --node-ip still outranks state for the
// deliberate-move workflow nodeIPMoveRefusal documents.
//
// Every start path must use this one precedence: a path that picks its own
// sources can re-issue a node's apiserver cert for whichever NIC holds the
// default route on a host with a management NIC and a fast fabric.
//
// Explicit sources are used verbatim (they may carry "v4,v6"). detect is only
// called when every other source is empty, so a node on an air-gapped network
// with a pinned address never needs a route to 8.8.8.8.
func ResolveNodeIP(flag, config, state string, detect func() (string, error)) (NodeIPChoice, error) {
	return resolveNodeIP([]NodeIPChoice{
		{flag, "--node-ip"},
		{state, savedNodeState},
		{config, "spec.network.nodeIP"},
		{os.Getenv("NODE_IP"), "$NODE_IP"},
	}, detect)
}

// ResolveNodeIPWithoutConfig is ResolveNodeIP without its config source, for a
// start that does not read spec.network.nodeIP: flag, then state, then
// $NODE_IP, then detect. Such a start uses it so that the hint it adds to a
// detection failure names only sources the start reads.
func ResolveNodeIPWithoutConfig(flag, state string, detect func() (string, error)) (NodeIPChoice, error) {
	return resolveNodeIP([]NodeIPChoice{
		{flag, "--node-ip"},
		{state, savedNodeState},
		{os.Getenv("NODE_IP"), "$NODE_IP"},
	}, detect)
}

// savedNodeState is the Source of the address a node was brought up with.
const savedNodeState = "saved node state"

// resolveNodeIP returns the first of sources, in their order, that names an
// address, and the detected address when none does. A detection failure's hint
// is built from the sources it was given, so it names exactly the ones its
// caller reads; it leaves out saved node state, which an operator cannot pin.
// With no other source there is nothing to name, and the failure is returned
// as it is.
func resolveNodeIP(sources []NodeIPChoice, detect func() (string, error)) (NodeIPChoice, error) {
	var pins []string
	for _, c := range sources {
		if c.IP != "" {
			return c, nil
		}
		if c.Source != savedNodeState {
			pins = append(pins, c.Source)
		}
	}
	ip, err := detect()
	if err != nil {
		if len(pins) == 0 {
			return NodeIPChoice{}, err
		}
		hint := pins[len(pins)-1]
		if len(pins) > 1 {
			hint = strings.Join(pins[:len(pins)-1], ", ") + " or " + hint
		}
		return NodeIPChoice{}, fmt.Errorf("%w (pin the node IP with %s)", err, hint)
	}
	return NodeIPChoice{ip, "autodetected"}, nil
}

// CheckNodeIP refuses a node IP kubelet --node-ip could not register for a
// node of the given stack, in the form every start path saves: an IPv4
// address on a single-stack node, because a cluster's primary family is IPv4;
// on a dual-stack node its pair, the IPv4 first and then the IPv6. It also
// refuses a loopback, all-zeros, multicast or link-local address, which the
// kubelet never registers as a node IP on any host, and the broadcast
// address, which names every host on the link rather than one node
// (UnreachableKind judges an IPv4-mapped IPv6 as the IPv4 it names). Whether
// the address is on one of this host's interfaces is left to the kubelet.
// ResolveNodeIP and ResolveNodeIPWithoutConfig return an explicit source
// verbatim, so a worker's join and its restart check here the address they
// resolved before they save it, or a value that is not an IP address, or one
// that drops or adds an address family, outlives the start that was given it.
// Controller starts do not call it.
func CheckNodeIP(ip string, dualStack bool) error {
	parts := strings.Split(ip, ",")
	switch {
	case dualStack && len(parts) != 2:
		return fmt.Errorf(`a dual-stack node's IP is its "v4,v6" pair, got %q`, ip)
	case !dualStack && len(parts) != 1:
		return fmt.Errorf("a single-stack node takes one address, got %q", ip)
	}
	addrs := make([]netip.Addr, len(parts))
	for i, p := range parts {
		a, err := netip.ParseAddr(p)
		if err != nil {
			return fmt.Errorf("%q is not an IP address", p)
		}
		if a.Zone() != "" {
			return fmt.Errorf("%q carries an interface zone, which a node IP cannot", p)
		}
		if kind := UnreachableKind(a); kind != "" {
			// The kubelet's own check refuses every other kind, but a host can
			// hold the broadcast address on an interface, so its refusal gives
			// its own reason.
			why := "which the kubelet does not register as a node IP"
			if kind == broadcastKind {
				why = "which names every host on the link rather than one node"
			}
			return fmt.Errorf("%q is %s, %s", p, kind, why)
		}
		addrs[i] = a
	}
	// Is6 holds for an IPv4-mapped IPv6 too, which names the IPv4 family again.
	if dualStack && (!addrs[0].Is4() || !addrs[1].Is6() || addrs[1].Is4In6()) {
		return fmt.Errorf("a dual-stack node's pair lists its IPv4 first, then its IPv6, got %q", ip)
	}
	if !dualStack && !addrs[0].Is4() {
		return fmt.Errorf("a single-stack node's IP is an IPv4 address, got %q", ip)
	}
	return nil
}

// UnreachableKind names the kind of address a is when, on any network, it
// cannot name one node or one apiserver that every other host can reach, and
// is "" for any other address. It is the one list of such kinds: CheckNodeIP
// refuses them as a worker's node IP, and a join URL must not name them. An
// all-zeros or loopback address makes the host that dials it reach itself, a
// multicast or broadcast one names many hosts rather than one, and a
// link-local one is not forwarded past a single network link. An IPv4-mapped
// IPv6 address is unmapped first, so it is judged as the IPv4 it names. Only
// the limited broadcast, 255.255.255.255, is a kind: a subnet's own broadcast
// (10.0.0.255 in 10.0.0.0/24) depends on a prefix length that a bare address
// does not carry. A zone is not judged; a caller that cannot carry one
// refuses it itself. The address a must be valid: the zero Addr is none of
// these kinds and answers "", so a caller refuses an address that does not
// parse before it calls this. The name reads after "is", as in
// `"127.0.0.1" is a loopback address`.
func UnreachableKind(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsUnspecified():
		return "the all-zeros address"
	case a.IsLoopback():
		return "a loopback address"
	case a.IsMulticast():
		return "a multicast address"
	case a.IsLinkLocalUnicast():
		return "a link-local address"
	case a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return broadcastKind
	}
	return ""
}

// broadcastKind is UnreachableKind's name for the limited broadcast address.
// CheckNodeIP tells it apart by this name, so the address is tested in one
// place only.
const broadcastKind = "the broadcast address"
