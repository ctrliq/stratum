package cluster

import (
	"context"
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

func TestWorkerRestartRefusesAProfileOtherThanItsJoin(t *testing.T) {
	for _, tc := range []struct {
		name, saved, given, wantErr string
	}{
		{"cis on a default worker", "", "cis", `this worker joined with no profile, and a restart cannot change it to profile "cis"`},
		{"default given to a cis worker keeps cis", "cis", "", ""},
		{"cis given to a cis worker", "cis", "cis", ""},
		{"another profile on a cis worker", "cis", "fips", `this worker joined with profile "cis", and a restart cannot change it to profile "fips"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workerScratchHost(t)
			stubWorkerStartHost(t)
			if err := SaveState(&NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8", KubeletNodeIP: "192.0.2.8", Profile: tc.saved}); err != nil {
				t.Fatal(err)
			}
			_, err := PrepareWorker(context.Background(), WorkerOptions{Profile: tc.given})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("PrepareWorker = %v, want the restart to run", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("PrepareWorker = %v, want a refusal containing %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "stratum token create") || !strings.Contains(err.Error(), "--profile "+tc.given) {
					t.Errorf("refusal %q does not name the re-join with --profile %s", err, tc.given)
				}
			}
			if got := mustLoadState(t).ProfileName(); got != tc.saved {
				t.Errorf("saved profile = %q, want %q kept", got, tc.saved)
			}
		})
	}
}
