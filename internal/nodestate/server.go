package nodestate

import (
	"net"
	"strconv"

	"github.com/ctrliq/stratum/internal/version"
)

// ServerURL returns the VIP when set, else the local apiserver; JoinHostPort brackets an IPv6 VIP.
func ServerURL(vip VIP) string {
	port := strconv.Itoa(version.APIServerPort)
	if vip.Address != "" {
		return "https://" + net.JoinHostPort(vip.Address, port)
	}
	return "https://" + net.JoinHostPort("127.0.0.1", port)
}
