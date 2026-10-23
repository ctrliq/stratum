package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// An unregistered profile is refused before the payload is staged or the
// SELinux labels are forgotten, from whichever place the start would read
// it: the flag, a control plane's config, or the saved state.
func TestPreStageProfileRefusesAnUnregisteredProfileFromEverySource(t *testing.T) {
	registerCISForTest(t)
	dir := t.TempDir()
	old := version.K8sConfDir
	version.K8sConfDir = dir
	t.Cleanup(func() { version.K8sConfDir = old })
	cfg := filepath.Join(dir, "config.yaml")
	write := func(body string) {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("spec: {}\n")
	for _, o := range []runUpOptions{
		{configPath: cfg},
		{configPath: cfg, profile: "cis"},
		{configPath: cfg, cis: true},
		{configPath: cfg, worker: true, server: "https://cp:6443", profile: "cis"},
	} {
		if err := preStageProfile(o); err != nil {
			t.Errorf("%+v: preStageProfile = %v, want no refusal", o, err)
		}
	}
	for _, o := range []runUpOptions{
		{configPath: cfg, profile: "stig"},
		{configPath: cfg, worker: true, server: "https://cp:6443", profile: "stig"},
	} {
		if err := preStageProfile(o); !errors.Is(err, profile.ErrUnregistered) {
			t.Errorf("%+v: preStageProfile = %v, want a refusal", o, err)
		}
	}

	write("spec:\n  profile: stig\n")
	if err := preStageProfile(runUpOptions{configPath: cfg}); !errors.Is(err, profile.ErrUnregistered) {
		t.Errorf("a control plane whose config names stig = %v, want a refusal", err)
	}
	if err := preStageProfile(runUpOptions{configPath: cfg, worker: true}); err != nil {
		t.Errorf("a worker does not read the config's profile; preStageProfile = %v", err)
	}

	write("spec: {}\n")
	if err := cluster.SaveState(&cluster.NodeState{Role: "worker", Hostname: "w1", Profile: "stig"}); err != nil {
		t.Fatal(err)
	}
	if err := preStageProfile(runUpOptions{configPath: cfg, worker: true}); !errors.Is(err, profile.ErrUnregistered) {
		t.Errorf("a worker whose saved state names stig = %v, want a refusal", err)
	}
	if err := cluster.SaveState(&cluster.NodeState{Role: "controller", Hostname: "cp1", CIS: true}); err != nil {
		t.Fatal(err)
	}
	if err := preStageProfile(runUpOptions{configPath: cfg}); err != nil {
		t.Errorf("a control plane whose legacy state says cis = %v, want no refusal", err)
	}
}

// The wiring: a worker restart whose saved state names a profile this binary
// does not register stops in runUp before the join, so nothing is staged and
// no state is touched.
func TestRunUpWorkerRefusesAnUnregisteredSavedProfileBeforeTheJoin(t *testing.T) {
	registerCISForTest(t)
	saved := &cluster.NodeState{Role: "worker", Hostname: "w1", Server: "https://cp:6443", Profile: "stig"}
	call, err := runUpWorker(t, runUpOptions{}, "spec: {}\n", saved)
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("runUp(--worker) with a saved stig profile = %v, want a refusal", err)
	}
	if call.called {
		t.Error("runUp reached workerJoin although the profile is not registered")
	}
	st, err := cluster.LoadState()
	if err != nil || st.ProfileName() != "stig" {
		t.Errorf("the saved state changed: %+v, %v", st, err)
	}
}
