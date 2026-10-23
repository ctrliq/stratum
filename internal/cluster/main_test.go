package cluster

// A start with no explicit or saved node address falls back to $NODE_IP and
// then to address detection, which dials out and lists this host's
// interfaces. A test that reached either would pass or fail with the
// developer's environment and network, and save the host's real address into
// its node state. This TestMain clears $NODE_IP and makes detection answer
// with documentation addresses (RFC 5737, RFC 3849) for the whole package; a
// test that pins either source sets its own with t.Setenv or a seam stub and
// restores these on cleanup.
//
// It also takes the host side effects of a start away from every test in the
// package: hostDirs creates the node's directories, /var/lib/kubelet and
// /etc/cni/net.d among them, which no redirect of the version paths reaches;
// hostTrustPodCIDRs runs firewall-cmd, which on a test host with firewalld
// running would change its real trusted zone and write the production
// ownership record; and hostPrereqs loads modules and writes sysctls. A test
// that asserts on any of them installs its own recorder and restores these
// no-ops; one that forgets can no longer reach the host.
//
// And it registers the engine's cis profile, as the binary does for every
// command: the tests here render cis nodes, and a renderer refuses a node
// whose profile is not registered.

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestMain(m *testing.M) {
	if err := os.Unsetenv("NODE_IP"); err != nil {
		fmt.Fprintf(os.Stderr, "cluster tests: clear $NODE_IP: %v\n", err)
		os.Exit(1)
	}
	hostDetectNodeIPs = func(dualStack bool) (string, error) {
		if dualStack {
			return "192.0.2.1,2001:db8::1", nil
		}
		return "192.0.2.1", nil
	}
	hostDirs = func() error { return nil }
	hostTrustPodCIDRs = func(string) {}
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		fmt.Fprintf(os.Stderr, "cluster tests: register the cis profile: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// Every seam the comment above says TestMain takes away from the host must be
// something other than the host function it defaults to, or a test that
// forgets its own stub reaches the host after all. The seams are compared by
// identity rather than called: the real host.Dirs would create directories
// outside any temp dir before a failure could report it.
func TestTestMainStubsTheHostSeams(t *testing.T) {
	for _, s := range []struct {
		name       string
		seam, real any
	}{
		{"hostDetectNodeIPs", hostDetectNodeIPs, host.DetectNodeIPs},
		{"hostTrustPodCIDRs", hostTrustPodCIDRs, host.TrustPodCIDRs},
		{"hostPrereqs", hostPrereqs, host.Prereqs},
		{"hostDirs", hostDirs, host.Dirs},
	} {
		if reflect.ValueOf(s.seam).Pointer() == reflect.ValueOf(s.real).Pointer() {
			t.Errorf("%s is still the real host function; TestMain must stub it for the whole package", s.name)
		}
	}
}
