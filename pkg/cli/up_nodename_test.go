package cli

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
)

// A hostname that cannot name a Kubernetes node, even lowercased, is refused
// before the join stages the payload, the first change it makes to the host:
// left to preparation, the refusal came after several hundred MB were staged.
// The refusal names the hostname and the fix.
func TestRunUpRefusesAnInvalidNodeNameBeforeItStages(t *testing.T) {
	const hostname = "k8s_wrk_01"
	old := upNodeName
	t.Cleanup(func() { upNodeName = old })
	upNodeName = func() (string, error) { return host.NodeNameOf(hostname) }

	o := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa", nodeIP: "10.80.0.4"}
	stages, err := runUpWorkerJoinReal(t, o)
	if err == nil {
		t.Fatalf("runUp on host %q = nil; want a refusal", hostname)
	}
	for _, want := range []string{`"` + hostname + `"`, "hostnamectl set-hostname", "Nothing on this host was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %s", err, want)
		}
	}
	if n := stages(); n != 0 {
		t.Errorf("the refused join staged the payload %d time(s) before refusing", n)
	}
}

// Which starts the check covers: every one that derives the node name from
// the hostname. A worker restart runs under the name it saved at its join, so
// a hostname changed since does not stop it here.
func TestPreflightNodeNameCoversTheStartsThatReadTheHostname(t *testing.T) {
	old := upNodeName
	t.Cleanup(func() { upNodeName = old })
	upNodeName = func() (string, error) { return host.NodeNameOf("bad_name") }
	for _, tc := range []struct {
		name    string
		o       runUpOptions
		refused bool
	}{
		{"control plane", runUpOptions{}, true},
		{"control-plane join", runUpOptions{join: "10.50.0.3"}, true},
		{"worker join", runUpOptions{worker: true, server: "https://10.50.0.3:6443"}, true},
		{"worker restart", runUpOptions{worker: true}, false},
	} {
		if err := preflightNodeName(tc.o); (err != nil) != tc.refused {
			t.Errorf("%s: preflightNodeName = %v, want refused=%t", tc.name, err, tc.refused)
		}
	}
}
