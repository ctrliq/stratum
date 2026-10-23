package cluster

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// A fresh worker join refuses spec.selinux: false from the selected profile's
// own declaration, not from its name, and before it touches the host: the
// refusal comes ahead of the hostname, address and directory steps, so this
// test needs no host stubs.
func TestWorkerJoinRefusesAnOptOutTheProfileForbids(t *testing.T) {
	registerProfilesForTest(t,
		profile.Profile{Name: "cis", RequiresOperator: true, RequiresSELinux: true},
		profile.Profile{Name: "strict", RequiresSELinux: true},
	)
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })
	oldPrereqs := hostPrereqs
	t.Cleanup(func() { hostPrereqs = oldPrereqs })
	hostPrereqs = func(host.PrereqOptions) error {
		t.Fatal("the host was prepared although the profile forbids the opt-out")
		return nil
	}

	optOut := true
	for _, name := range []string{"strict", "cis"} {
		_, err := PrepareWorker(t.Context(), WorkerOptions{Server: "https://cp:6443", Token: "abcdef.0123456789abcdef", CAHash: "sha256:aa", Profile: name, SELinuxOptOut: &optOut})
		if err == nil || !strings.Contains(err.Error(), "spec.selinux: false") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s with spec.selinux: false: err = %v; want a refusal naming the profile and the setting", name, err)
		}
	}
}
