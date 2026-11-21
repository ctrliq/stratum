package netcfg

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// A joiner dials the recorded VIP, else the node's public address; undialable refuse.
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
		// The empty check names the missing node address, a clearer message than "not an IP".
		{name: "no address at all is refused", vip: "", nodeIP: "", wantErr: true, errHas: "no node address"},
		{name: "no VIP: a v4,v6 node address names its IPv4 primary", vip: "", nodeIP: "10.0.0.7,fd00::7", want: "https://10.0.0.7:6443"},
		{name: "no VIP: a v6,v4 node address names its bracketed IPv6 primary", vip: "", nodeIP: "fd00::7,10.0.0.7", want: "https://[fd00::7]:6443"},
		{name: "no VIP: a loopback primary of a v4,v6 node address is refused", vip: "", nodeIP: "127.0.0.1,fd00::7", wantErr: true, errHas: "loopback"},
		// A pair with no primary meets the empty check's clearer message.
		{name: "no VIP: a v4,v6 node address with no primary is refused", vip: "", nodeIP: ",fd00::7", wantErr: true, errHas: "no node address"},
		{name: "no VIP: an unspecified IPv4 node address is refused", vip: "", nodeIP: "0.0.0.0", wantErr: true, errHas: "all-zeros", quotes: "0.0.0.0"},
		{name: "no VIP: an unspecified IPv6 node address is refused", vip: "", nodeIP: "::", wantErr: true, errHas: "all-zeros", quotes: "::"},
		{name: "an unspecified IPv4 VIP is refused", vip: "0.0.0.0", nodeIP: "192.168.99.12", wantErr: true, errHas: "all-zeros", quotes: "0.0.0.0"},
		{name: "an unspecified IPv6 VIP is refused", vip: "::", nodeIP: "192.168.99.12", wantErr: true, errHas: "all-zeros", quotes: "::"},
		// netip does not unmap, so "::ffff:0.0.0.0" is judged after unmapping.
		{name: "no VIP: an IPv4-mapped all-zeros node address is refused", vip: "", nodeIP: "::ffff:0.0.0.0", wantErr: true, errHas: "all-zeros", quotes: "::ffff:0.0.0.0"},
		{name: "no VIP: an IPv4-mapped loopback node address is refused", vip: "", nodeIP: "::ffff:127.0.0.1", wantErr: true, errHas: "loopback", quotes: "::ffff:127.0.0.1"},
		// Padding is judged before the IP parse, so the refusal names it, not a name.
		{name: "no VIP: a padded loopback node address is refused", vip: "", nodeIP: " 127.0.0.1", wantErr: true, errHas: "padded", quotes: " 127.0.0.1"},
		{name: "no VIP: a padded node address is refused", vip: "", nodeIP: " 10.0.0.7", wantErr: true, errHas: "padded", quotes: " 10.0.0.7"},
		{name: "no VIP: a node address padded on the right is refused", vip: "", nodeIP: "10.0.0.7 ", wantErr: true, errHas: "padded", quotes: "10.0.0.7 "},
		// The refusal quotes the primary it checked, not the whole pair.
		{name: "no VIP: a padded primary of a v4,v6 node address is refused", vip: "", nodeIP: " 127.0.0.1,fd00::7", wantErr: true, errHas: "padded", quotes: " 127.0.0.1"},
		{name: "a padded VIP is refused", vip: " 10.0.0.10", nodeIP: "192.168.99.12", wantErr: true, errHas: "padded", quotes: " 10.0.0.10"},
		{name: "a VIP that is a DNS name is refused", vip: "api.example", nodeIP: "192.168.99.12", wantErr: true, quotes: "api.example"},
		{name: "a VIP that is a name is refused", vip: "localhost", nodeIP: "192.168.99.12", wantErr: true, quotes: "localhost"},
		{name: "a shorthand IPv4 VIP is refused", vip: "127.1", nodeIP: "192.168.99.12", wantErr: true, quotes: "127.1"},
		{name: "no VIP: a node address that is a name is refused", vip: "", nodeIP: "localhost", wantErr: true, quotes: "localhost"},
		{name: "no VIP: a shorthand IPv4 node address is refused", vip: "", nodeIP: "127.1", wantErr: true, quotes: "127.1"},
		{name: "no VIP: a zoned IPv6 node address is refused", vip: "", nodeIP: "fe80::1%eth0", wantErr: true, errHas: "zone", quotes: "fe80::1%eth0"},
		{name: "a zoned IPv6 VIP is refused", vip: "fe80::1%eth0", nodeIP: "192.168.99.12", wantErr: true, errHas: "zone", quotes: "fe80::1%eth0"},
		// Either multicast form names a group, whichever kind is checked first.
		{name: "no VIP: an unzoned IPv6 link-local node address is refused", vip: "", nodeIP: "fe80::1", wantErr: true, errHas: "link-local", quotes: "fe80::1"},
		{name: "no VIP: an IPv4 link-local node address is refused", vip: "", nodeIP: "169.254.1.1", wantErr: true, errHas: "link-local", quotes: "169.254.1.1"},
		{name: "no VIP: an IPv6 multicast node address is refused", vip: "", nodeIP: "ff02::1", wantErr: true, errHas: "multicast", quotes: "ff02::1"},
		{name: "no VIP: an IPv4 multicast node address is refused", vip: "", nodeIP: "224.0.0.1", wantErr: true, errHas: "multicast", quotes: "224.0.0.1"},
		{name: "an unzoned IPv6 link-local VIP is refused", vip: "fe80::1", nodeIP: "192.168.99.12", wantErr: true, errHas: "link-local", quotes: "fe80::1"},
		{name: "an IPv6 multicast VIP is refused", vip: "ff02::1", nodeIP: "192.168.99.12", wantErr: true, errHas: "multicast", quotes: "ff02::1"},
		// The limited broadcast names every host on the link, not one apiserver.
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
