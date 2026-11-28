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

// A control plane that already registered restarts under the name it saved
// only if the hostname still names it, so a hostname no Node can take is
// refused with that saved name as the fix, not a new one: a new hostname
// would register a new Node and etcd member. A first start, a join and a
// worker's join have no such name, and keep the general advice.
func TestPreflightNodeNameNamesAControlPlanesSavedName(t *testing.T) {
	oldName, oldSaved := upNodeName, upSavedNodeName
	t.Cleanup(func() { upNodeName, upSavedNodeName = oldName, oldSaved })
	upNodeName = func() (string, error) { return host.NodeNameOf("cp_1") }
	upSavedNodeName = func() string { return "cp-1" }
	err := preflightNodeName(runUpOptions{})
	for _, want := range []string{`"cp_1"`, `registered as Node "cp-1"`, "hostnamectl set-hostname cp-1", "Nothing on this host was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("control plane restart refused with %v; want it to say %s", err, want)
		}
	}
	if err != nil && strings.Contains(err.Error(), "k8s-ctrl-01") {
		t.Errorf("control plane restart refused with %v, which advises a new hostname", err)
	}
	for _, o := range []runUpOptions{{join: "10.50.0.3"}, {worker: true, server: "https://10.50.0.3:6443"}} {
		if err := preflightNodeName(o); err == nil || !strings.Contains(err.Error(), "k8s-ctrl-01") {
			t.Errorf("%+v refused with %v; want the general advice", o, err)
		}
	}
	upSavedNodeName = func() string { return "" }
	if err := preflightNodeName(runUpOptions{}); err == nil || !strings.Contains(err.Error(), "k8s-ctrl-01") {
		t.Errorf("first control-plane start refused with %v; want the general advice", err)
	}
}
