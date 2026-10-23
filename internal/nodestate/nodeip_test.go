package nodestate

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestNodeIPPrefersTheRecordedAddress pins the rule for code acting on a
// running node: use the address the node was brought up with (in its apiserver
// cert and etcd peer URL), and fall back to $NODE_IP / detection only when there
// is no saved state. $NODE_IP set to something else must NOT move a node that
// already runs — renewal re-issuing its cert for another NIC was the bug.
func TestNodeIPPrefersTheRecordedAddress(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	t.Setenv("NODE_IP", "10.9.9.9")

	// No state: the bare fallback.
	if ip, err := NodeIP(); err != nil || ip != "10.9.9.9" {
		t.Fatalf("without state: NodeIP() = %q, %v; want $NODE_IP", ip, err)
	}

	if err := SaveState(&NodeState{Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12,fd00::12"}); err != nil {
		t.Fatal(err)
	}
	if ip, err := NodeIP(); err != nil || ip != "192.168.99.12" {
		t.Errorf("with state: NodeIP() = %q, %v; want the recorded primary", ip, err)
	}
}

// TestNodeIPRefusesUnreadableState: only a missing node.yaml
// falls back to $NODE_IP or detection. A malformed one is an error, because
// falling back is exactly the wrong-NIC drift the saved address prevents.
func TestNodeIPRefusesUnreadableState(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	t.Setenv("NODE_IP", "10.9.9.9")
	if err := os.WriteFile(StatePath(), []byte("role: [controller\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ip, err := NodeIP(); err == nil {
		t.Errorf("NodeIP with a malformed node.yaml = %q, want an error, not the $NODE_IP fallback", ip)
	}
	if err := os.Remove(StatePath()); err != nil {
		t.Fatal(err)
	}
	if ip, err := NodeIP(); err != nil || ip != "10.9.9.9" {
		t.Errorf("with no node.yaml: %q, %v; want the $NODE_IP fallback", ip, err)
	}
}

// TestNodeIPRefusesAStateWithNoAddress: LoadState accepts any
// valid YAML, so a truncated node.yaml parses with empty fields. A node that
// has state never re-detects its address, whatever the file is missing.
func TestNodeIPRefusesAStateWithNoAddress(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	t.Setenv("NODE_IP", "10.9.9.9")
	if err := os.WriteFile(StatePath(), []byte("role: controller\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ip, err := NodeIP(); err == nil {
		t.Errorf("state with no nodeIP: NodeIP = %q, want an error, not the $NODE_IP fallback", ip)
	}
}

// TestNodeIPWithoutStateAnswersThePrimary pins the no-state fallback to one
// address: the primary, the first of $NODE_IP's kubelet "v4,v6" form, in
// whichever order the operator wrote it. Every caller uses the answer as a
// single host, so the whole pair would make an etcd restore or rejoin seed
// the peer URL https://[10.0.0.7,fd00::7]:2380, which no member can dial. A
// pair with no first address is refused, not answered as an empty host.
func TestNodeIPWithoutStateAnswersThePrimary(t *testing.T) {
	for _, tc := range []struct {
		nodeIP    string
		want      string
		wantError bool
	}{
		{"10.0.0.7,fd00::7", "10.0.0.7", false},
		{"fd00::7,10.0.0.7", "fd00::7", false},
		{",fd00::7", "", true},
	} {
		t.Run(tc.nodeIP, func(t *testing.T) {
			orig := version.K8sConfDir
			version.K8sConfDir = t.TempDir()
			t.Cleanup(func() { version.K8sConfDir = orig })
			// No SaveState: the fallback is the path under test.
			t.Setenv("NODE_IP", tc.nodeIP)

			ip, err := NodeIP()
			if (err != nil) != tc.wantError || ip != tc.want {
				t.Errorf("NODE_IP=%q without state: NodeIP() = %q, %v; want %q, error %v", tc.nodeIP, ip, err, tc.want, tc.wantError)
			}
		})
	}
}

// TestNodeIPWithoutStateRefusesAPrimaryThatIsNotAnIPLiteral pins the no-state
// fallback to an address a caller can put in a URL as it stands. $NODE_IP is
// taken verbatim, so a padded " 10.0.0.7,fd00::7" would answer " 10.0.0.7" and
// an etcd snapshot restore would seed the peer URL https:// 10.0.0.7:2380; a
// name or a zoned literal gets through the same way. The error must quote the
// offending primary, not the whole $NODE_IP, so the operator sees which part to
// fix.
func TestNodeIPWithoutStateRefusesAPrimaryThatIsNotAnIPLiteral(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodeIP  string
		primary string
	}{
		{"padded pair", " 10.0.0.7,fd00::7", " 10.0.0.7"},
		{"padded single address", " 10.0.0.7", " 10.0.0.7"},
		{"host name", "localhost", "localhost"},
		{"zoned literal", "fe80::1%eth0", "fe80::1%eth0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := version.K8sConfDir
			version.K8sConfDir = t.TempDir()
			t.Cleanup(func() { version.K8sConfDir = orig })
			// No SaveState: the fallback is the path under test.
			t.Setenv("NODE_IP", tc.nodeIP)

			ip, err := NodeIP()
			if err == nil {
				t.Fatalf("NODE_IP=%q without state: NodeIP() = %q, nil; want an error refusing the primary %q", tc.nodeIP, ip, tc.primary)
			}
			if ip != "" {
				t.Errorf("NODE_IP=%q without state: NodeIP() = %q alongside its error; want no address", tc.nodeIP, ip)
			}
			if msg := err.Error(); !strings.Contains(msg, fmt.Sprintf("%q", tc.primary)) || !strings.Contains(msg, "NODE_IP") {
				t.Errorf("NODE_IP=%q without state: error %q; want it to name $NODE_IP and quote the primary %q", tc.nodeIP, msg, tc.primary)
			}
		})
	}
}

func TestRecordedNodeIPs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		st        NodeState
		dual      bool
		want      string
		wantError bool
	}{
		{"kubelet form recorded", NodeState{NodeIP: "10.0.0.1", KubeletNodeIP: "10.0.0.1,fd00::1"}, true, "10.0.0.1,fd00::1", false},
		{"only the primary, single-stack: it is the same address", NodeState{NodeIP: "10.0.0.1"}, false, "10.0.0.1", false},
		{"only the primary, dual-stack: the pair is missing", NodeState{NodeIP: "10.0.0.1"}, true, "", true},
		{"neither", NodeState{}, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RecordedNodeIPs(&tc.st, tc.dual)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Errorf("got %q, %v; want %q, error %v", got, err, tc.want, tc.wantError)
			}
		})
	}
}
