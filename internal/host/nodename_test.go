package host_test

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
)

// The kubelet registers its Node under the lowercased hostname, whatever
// --hostname-override says, so the name stratum derives must be that same
// lowercased short name: a mixed-case one put into the kubelet's certificate
// and flag left the Node authorizer refusing the registration.
func TestNodeNameOfLowercasesTheShortHostname(t *testing.T) {
	for _, tc := range []struct{ hostname, want string }{
		{"HK-k8s-ctrl-01", "hk-k8s-ctrl-01"},
		{"HK-k8s-ctrl-01.Corp.Example.COM", "hk-k8s-ctrl-01"},
		{"cp1.example.com", "cp1"},
		{"cp1", "cp1"},
		{"NODE7", "node7"},
	} {
		got, err := host.NodeNameOf(tc.hostname)
		if err != nil {
			t.Errorf("NodeNameOf(%q) = %v, want %q", tc.hostname, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("NodeNameOf(%q) = %q, want %q", tc.hostname, got, tc.want)
		}
	}
}

// A hostname that is still no Node name once lowercased is refused, and the
// refusal names the hostname and says how to fix it, so `stratum up` can
// refuse before the host changes instead of a kubelet that never registers.
func TestNodeNameOfRefusesANameNoNodeCanTake(t *testing.T) {
	for _, hostname := range []string{
		"k8s_ctrl_01",
		"-cp1",
		"cp1-",
		"",
		".example.com",
		"cp 1",
		strings.Repeat("a", 254),
	} {
		got, err := host.NodeNameOf(hostname)
		if err == nil {
			t.Errorf("NodeNameOf(%q) = %q, want a refusal", hostname, got)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, `"`+hostname+`"`) || !strings.Contains(msg, "hostnamectl set-hostname") {
			t.Errorf("NodeNameOf(%q) refused with %q; want it to name the hostname and the hostnamectl fix", hostname, msg)
		}
	}
}

// ShortName names what this host keeps for itself the way NodeName does,
// lowercased and cut at the first dot, but a name no Node can take is no
// reason to stop a snapshot or its prune: on a host renamed after it joined
// to such a name, they still run under it rather than under "".
func TestShortNameIsTheNodeNameWithoutTheCheck(t *testing.T) {
	short, err := host.ShortName()
	if err != nil {
		t.Skip(err)
	}
	if node, err := host.NodeName(); err == nil && node != short {
		t.Errorf("ShortName = %q, NodeName = %q; want the same name on a host with a valid one", short, node)
	}
	if short == "" || short != strings.ToLower(short) || strings.Contains(short, ".") {
		t.Errorf("ShortName = %q, want the lowercased short hostname", short)
	}
}
