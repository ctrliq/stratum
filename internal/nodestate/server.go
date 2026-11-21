package nodestate

import (
	"net"
	"strconv"

	"github.com/ctrliq/stratum/internal/version"
)

// ServerURL is the controller's apiserver endpoint: the VIP when set, else the local apiserver.
func ServerURL(vip VIP) string {
	port := strconv.Itoa(version.APIServerPort)
	if vip.Address != "" {
		return "https://" + net.JoinHostPort(vip.Address, port)
	}
	return "https://" + net.JoinHostPort("127.0.0.1", port)
}
