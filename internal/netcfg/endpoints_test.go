package netcfg

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// TestGrantServerURLNamesTheVIPOrThePublicNodeAddress pins the remote join
// grant's endpoint rule: the URL a joiner is told to dial names the cluster's
// VIP when this node records one, else this node's own public address. On a
// VIP cluster the join line must therefore survive the controller that minted
// the token losing the VIP; on a cluster without a VIP it names the one
// control plane there is.
//
// nodestate.NodeIP answers the primary, but a caller can still pass the
// kubelet's "v4,v6" pair (the MCP tests inject one through deps.nodeIP), so
// the grant takes the primary itself as a guard: a joiner handed
// "https://[10.0.0.7,fd00::7]:6443" dials nothing.
//
// An address that does not name one apiserver every joiner can dial is refused
// in either role, VIP or node address. A loopback or all-zeros one (0.0.0.0,
// ::) makes a Linux joiner dial itself. A multicast one (ff02::1, 224.0.0.1)
// or the broadcast address (255.255.255.255) names many hosts rather than one.
// An IPv4 link-local one (169.254.1.1) is not routed beyond one link, so only
// a joiner on that link can dial it. A Linux joiner cannot dial an unzoned
// IPv6 link-local one (fe80::1) even on the same link: the connect needs a
// zone that names the joiner's own interface, and the URL carries none.
// host.CheckNodeIP refuses the same kinds for a worker start, because the
// kubelet never registers a loopback, all-zeros, multicast or link-local
// address as a node IP and the broadcast address names every host on the link
// rather than one node. The refusal names them as host.UnreachableKind does,
// so the two lists cannot drift apart. An IPv4-mapped literal
// (::ffff:127.0.0.1) is judged as the IPv4 address it names. A
// whitespace-padded one (" 127.0.0.1") is refused for its padding, which no
// URL host may carry, and the refusal quotes the address so the padding shows.
//
// Both the VIP and the node address must be IP literals without a zone. The
// node address comes from this node's own state or $NODE_IP and is always
// meant to be an IP. The VIP matches what config loading accepts for
// spec.vip.address, which is only an IP, and a joining control plane inherits
// it from the join bundle's authenticated metadata, so a VIP that is a name
// comes only from a hand-edited node state. Either one that is not an IP
// literal is refused rather than printed as a name: a joiner resolves
// "localhost" to itself, and the shorthand "127.1", which netip.ParseAddr
// rejects, is 127.0.0.1 to an inet_aton-style resolver. A zoned literal
// ("fe80::1%eth0") is refused in either role: the zone names an interface on
// this host only, and url.Parse rejects the raw % the URL would carry, so the
// joiner cannot even read its join line.
func TestGrantServerURLNamesTheVIPOrThePublicNodeAddress(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })

	for _, tc := range []struct {
		name    string
		vip     string // the VIP this node's state records; "" means none
		nodeIP  string // the node address the caller passes: nodestate.NodeIP's primary, or a "v4,v6" pair
		want    string
		wantErr bool
		errHas  string // a word the refusal must contain; "" checks only that it refuses
		quotes  string // the address the refusal must quote with %q, so padding shows; "" skips the check
	}{
		{name: "a recorded VIP wins over the node address", vip: "10.0.0.10", nodeIP: "192.168.99.12", want: "https://10.0.0.10:6443"},
		{name: "an IPv6 VIP is bracketed", vip: "fd00::10", nodeIP: "192.168.99.12", want: "https://[fd00::10]:6443"},
		{name: "a VIP covers a loopback node address", vip: "10.0.0.10", nodeIP: "127.0.0.1", want: "https://10.0.0.10:6443"},
		{name: "no VIP: the node's own public address", vip: "", nodeIP: "192.168.99.12", want: "https://192.168.99.12:6443"},
		{name: "no VIP: an IPv6 node address is bracketed", vip: "", nodeIP: "fd00::99", want: "https://[fd00::99]:6443"},
		{name: "no VIP: loopback is refused", vip: "", nodeIP: "127.0.0.1", wantErr: true},
		{name: "a loopback VIP is refused", vip: "127.0.0.1", nodeIP: "192.168.99.12", wantErr: true},
		// The empty check names the missing node address, a clearer message
		// than calling "" an address that is not an IP literal.
		{name: "no address at all is refused", vip: "", nodeIP: "", wantErr: true, errHas: "no node address"},
		{name: "no VIP: a v4,v6 node address names its IPv4 primary", vip: "", nodeIP: "10.0.0.7,fd00::7", want: "https://10.0.0.7:6443"},
		{name: "no VIP: a v6,v4 node address names its bracketed IPv6 primary", vip: "", nodeIP: "fd00::7,10.0.0.7", want: "https://[fd00::7]:6443"},
		{name: "no VIP: a loopback primary of a v4,v6 node address is refused", vip: "", nodeIP: "127.0.0.1,fd00::7", wantErr: true, errHas: "loopback"},
		// The primary is taken before the empty check, so a pair with no
		// primary gets the empty check's clearer message naming the missing
		// node address.
		{name: "no VIP: a v4,v6 node address with no primary is refused", vip: "", nodeIP: ",fd00::7", wantErr: true, errHas: "no node address"},
		{name: "no VIP: an unspecified IPv4 node address is refused", vip: "", nodeIP: "0.0.0.0", wantErr: true, errHas: "all-zeros", quotes: "0.0.0.0"},
		{name: "no VIP: an unspecified IPv6 node address is refused", vip: "", nodeIP: "::", wantErr: true, errHas: "all-zeros", quotes: "::"},
		{name: "an unspecified IPv4 VIP is refused", vip: "0.0.0.0", nodeIP: "192.168.99.12", wantErr: true, errHas: "all-zeros", quotes: "0.0.0.0"},
		{name: "an unspecified IPv6 VIP is refused", vip: "::", nodeIP: "192.168.99.12", wantErr: true, errHas: "all-zeros", quotes: "::"},
		// An IPv4-mapped literal is judged as the IPv4 address it names.
		// netip's IsUnspecified does not unmap, so "::ffff:0.0.0.0" is refused
		// only because the address is unmapped before its kind is judged.
		{name: "no VIP: an IPv4-mapped all-zeros node address is refused", vip: "", nodeIP: "::ffff:0.0.0.0", wantErr: true, errHas: "all-zeros", quotes: "::ffff:0.0.0.0"},
		{name: "no VIP: an IPv4-mapped loopback node address is refused", vip: "", nodeIP: "::ffff:127.0.0.1", wantErr: true, errHas: "loopback", quotes: "::ffff:127.0.0.1"},
		// A padded address is refused for its padding, not as a name: the IP
		// parse rejects it too, so without the padding check first it would
		// still be refused, but the refusal would wrongly call it a name.
		{name: "no VIP: a padded loopback node address is refused", vip: "", nodeIP: " 127.0.0.1", wantErr: true, errHas: "padded", quotes: " 127.0.0.1"},
		{name: "no VIP: a padded node address is refused", vip: "", nodeIP: " 10.0.0.7", wantErr: true, errHas: "padded", quotes: " 10.0.0.7"},
		{name: "no VIP: a node address padded on the right is refused", vip: "", nodeIP: "10.0.0.7 ", wantErr: true, errHas: "padded", quotes: "10.0.0.7 "},
		// The refusal names the primary it checked, as the loopback-primary
		// refusal does, so it quotes " 127.0.0.1" rather than the whole pair.
		{name: "no VIP: a padded primary of a v4,v6 node address is refused", vip: "", nodeIP: " 127.0.0.1,fd00::7", wantErr: true, errHas: "padded", quotes: " 127.0.0.1"},
		{name: "a padded VIP is refused", vip: " 10.0.0.10", nodeIP: "192.168.99.12", wantErr: true, errHas: "padded", quotes: " 10.0.0.10"},
		{name: "a VIP that is a DNS name is refused", vip: "api.example", nodeIP: "192.168.99.12", wantErr: true, quotes: "api.example"},
		{name: "a VIP that is a name is refused", vip: "localhost", nodeIP: "192.168.99.12", wantErr: true, quotes: "localhost"},
		{name: "a shorthand IPv4 VIP is refused", vip: "127.1", nodeIP: "192.168.99.12", wantErr: true, quotes: "127.1"},
		{name: "no VIP: a node address that is a name is refused", vip: "", nodeIP: "localhost", wantErr: true, quotes: "localhost"},
		{name: "no VIP: a shorthand IPv4 node address is refused", vip: "", nodeIP: "127.1", wantErr: true, quotes: "127.1"},
		{name: "no VIP: a zoned IPv6 node address is refused", vip: "", nodeIP: "fe80::1%eth0", wantErr: true, errHas: "zone", quotes: "fe80::1%eth0"},
		{name: "a zoned IPv6 VIP is refused", vip: "fe80::1%eth0", nodeIP: "192.168.99.12", wantErr: true, errHas: "zone", quotes: "fe80::1%eth0"},
		// Both multicast examples are also link-local multicast, so they pin
		// "multicast": the reason a joiner cannot dial them is that they name a
		// group, whichever kind the refusal checks first.
		{name: "no VIP: an unzoned IPv6 link-local node address is refused", vip: "", nodeIP: "fe80::1", wantErr: true, errHas: "link-local", quotes: "fe80::1"},
		{name: "no VIP: an IPv4 link-local node address is refused", vip: "", nodeIP: "169.254.1.1", wantErr: true, errHas: "link-local", quotes: "169.254.1.1"},
		{name: "no VIP: an IPv6 multicast node address is refused", vip: "", nodeIP: "ff02::1", wantErr: true, errHas: "multicast", quotes: "ff02::1"},
		{name: "no VIP: an IPv4 multicast node address is refused", vip: "", nodeIP: "224.0.0.1", wantErr: true, errHas: "multicast", quotes: "224.0.0.1"},
		{name: "an unzoned IPv6 link-local VIP is refused", vip: "fe80::1", nodeIP: "192.168.99.12", wantErr: true, errHas: "link-local", quotes: "fe80::1"},
		{name: "an IPv6 multicast VIP is refused", vip: "ff02::1", nodeIP: "192.168.99.12", wantErr: true, errHas: "multicast", quotes: "ff02::1"},
		// The limited broadcast names every host on the link, not one
		// apiserver.
		{name: "a broadcast VIP is refused", vip: "255.255.255.255", nodeIP: "192.168.99.12", wantErr: true, errHas: "broadcast", quotes: "255.255.255.255"},
		{name: "no VIP: a broadcast node address is refused", vip: "", nodeIP: "255.255.255.255", wantErr: true, errHas: "broadcast", quotes: "255.255.255.255"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := nodestate.SaveState(&nodestate.NodeState{
				NodeIP: "192.168.99.12",
				VIP:    nodestate.VIP{Address: tc.vip},
			}); err != nil {
				t.Fatal(err)
			}

			got, err := GrantServerURL(tc.nodeIP)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GrantServerURL(%q) = %q, want a refusal", tc.nodeIP, got)
				}
				if !strings.Contains(err.Error(), tc.errHas) {
					t.Errorf("GrantServerURL(%q) refused with %v, want it to say %q", tc.nodeIP, err, tc.errHas)
				}
				if q := strconv.Quote(tc.quotes); tc.quotes != "" && !strings.Contains(err.Error(), q) {
					t.Errorf("GrantServerURL(%q) refused with %v, want it to quote the address as %s", tc.nodeIP, err, q)
				}
				return
			}
			if err != nil {
				t.Fatalf("GrantServerURL(%q): %v", tc.nodeIP, err)
			}
			if got != tc.want {
				t.Errorf("GrantServerURL(%q) = %q, want %q", tc.nodeIP, got, tc.want)
			}
		})
	}
}
