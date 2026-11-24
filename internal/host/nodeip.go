package host

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

type NodeIPChoice struct {
	IP     string // "v4" or "v4,v6", as kubelet --node-ip takes it
	Source string
}

// state outranks config; every start path must use this, or a new cert follows the wrong NIC.
func ResolveNodeIP(flag, config, state string, detect func() (string, error)) (NodeIPChoice, error) {
	return resolveNodeIP([]NodeIPChoice{
		{flag, "--node-ip"},
		{state, savedNodeState},
		{config, "spec.network.nodeIP"},
		{os.Getenv("NODE_IP"), "$NODE_IP"},
	}, detect)
}

// Its hint names only the sources this start reads.
func ResolveNodeIPWithoutConfig(flag, state string, detect func() (string, error)) (NodeIPChoice, error) {
	return resolveNodeIP([]NodeIPChoice{
		{flag, "--node-ip"},
		{state, savedNodeState},
		{os.Getenv("NODE_IP"), "$NODE_IP"},
	}, detect)
}

const savedNodeState = "saved node state"

// The hint names only sources an operator can pin; saved node state is never named.
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

// Refuses what kubelet --node-ip cannot register; interface membership is the kubelet's check.
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
			// A host can hold the broadcast address, so its refusal gets its own reason.
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

// Valid addresses only (zero Addr answers "", zones left to the caller); the name reads after "is".
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

const broadcastKind = "the broadcast address"
