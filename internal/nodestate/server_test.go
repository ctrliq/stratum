package nodestate

import (
	"fmt"
	"net/url"
	"strconv"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestServerURLIsTheVIPElseTheLocalAPIServer pins the one rule every
// controller's kubeconfigs, and its recorded server, follow: the VIP when one
// is configured (one address for in-cluster and external clients), the local
// apiserver otherwise. The controller start and the HA join both take it from
// here, so a joined controller's first restart writes what the join wrote.
// Only the address decides: the interface and BGP mode say how kube-vip
// announces the VIP, not where clients reach it. An IPv6 VIP is bracketed, and
// every URL must parse back to the address it was built from: client-go
// cannot build a client from a server URL that net/url rejects.
func TestServerURLIsTheVIPElseTheLocalAPIServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		vip  VIP
		want string
	}{
		{"a VIP", VIP{Address: "10.0.0.10"}, fmt.Sprintf("https://10.0.0.10:%d", version.APIServerPort)},
		{"a VIP announced over BGP on a named interface", VIP{Address: "192.0.2.50", Interface: "eth1", BGP: true},
			fmt.Sprintf("https://192.0.2.50:%d", version.APIServerPort)},
		{"no VIP", VIP{}, fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)},
		{"an interface but no VIP address", VIP{Interface: "eth1"}, fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)},
		{"an IPv6 VIP", VIP{Address: "fd00::10"}, fmt.Sprintf("https://[fd00::10]:%d", version.APIServerPort)},
		{"an IPv6 VIP announced over BGP on a named interface", VIP{Address: "2001:db8::50", Interface: "eth1", BGP: true},
			fmt.Sprintf("https://[2001:db8::50]:%d", version.APIServerPort)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ServerURL(tc.vip)
			if got != tc.want {
				t.Errorf("ServerURL(%+v) = %q, want %q", tc.vip, got, tc.want)
			}
			wantHost := tc.vip.Address
			if wantHost == "" {
				wantHost = "127.0.0.1"
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(ServerURL(%+v)) = %v, want a URL a kubeconfig can carry", tc.vip, err)
			}
			if u.Scheme != "https" || u.Hostname() != wantHost || u.Port() != strconv.Itoa(version.APIServerPort) {
				t.Errorf("url.Parse(%q) = scheme %q, host %q, port %q; want https, %q, %d",
					got, u.Scheme, u.Hostname(), u.Port(), wantHost, version.APIServerPort)
			}
		})
	}
}
