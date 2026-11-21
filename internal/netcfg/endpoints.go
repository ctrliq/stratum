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

// A remote joiner dials the recorded VIP, else this node's address; undialable ones refuse.
func GrantServerURL(nodeIP string) (string, error) {
	vip := ""
	if st, err := nodestate.LoadState(); err == nil {
		vip = st.VIP.Address
	} else if !errors.Is(err, fs.ErrNotExist) {
		// The state is the only source for the VIP; guessing around it strands the joiner.
		return "", fmt.Errorf("read node state for the cluster's VIP: %w", err)
	}
	if vip != "" {
		if why := undialable(vip); why != "" {
			return "", fmt.Errorf("the recorded VIP %q is %s; a remote join grant must name an address another host can dial", vip, why)
		}
		return apiserverURL(vip), nil
	}
	// A caller may pass a "v4,v6" pair; a URL carries one host, so judge its primary.
	nodeIP = host.Primary(nodeIP)
	if nodeIP == "" {
		return "", fmt.Errorf("no VIP is configured and no node address is recorded, so there is no address a remote joiner can dial")
	}
	if why := undialable(nodeIP); why != "" {
		return "", fmt.Errorf("this node's address %q is %s; without a VIP a remote join grant must name an address another host can dial", nodeIP, why)
	}
	return apiserverURL(nodeIP), nil
}

// JoinHostPort brackets an IPv6 literal; without them the port reads as another hextet.
func apiserverURL(host string) string {
	return "https://" + net.JoinHostPort(host, strconv.Itoa(version.APIServerPort))
}

// Uses host.UnreachableKind's words, so a join URL and a node IP refuse the same kinds.
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
