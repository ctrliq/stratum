package nodestate

import (
	"net"
	"strconv"

	"github.com/ctrliq/stratum/internal/version"
)

// ServerURL is the apiserver endpoint every kubeconfig on a controller
// targets, and the Server its node state records: the VIP when one is
// configured (one address for in-cluster and external clients), the local
// apiserver otherwise. The controller start and the HA join both take it from
// here, so a joined controller's first restart writes what the join wrote.
// Only the address decides: the interface and BGP mode say how kube-vip
// announces the VIP, not where clients reach it. kube-vip's own kubeconfig is
// not covered: it stays on the local apiserver either way. net.JoinHostPort
// brackets an IPv6 VIP, because net/url, and so client-go, rejects a server
// URL whose IPv6 host is bare.
func ServerURL(vip VIP) string {
	port := strconv.Itoa(version.APIServerPort)
	if vip.Address != "" {
		return "https://" + net.JoinHostPort(vip.Address, port)
	}
	return "https://" + net.JoinHostPort("127.0.0.1", port)
}
