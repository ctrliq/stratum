package certs

import (
	"errors"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

// The finalize preflight skips control planes by what the cluster's profile
// declares, not by its name: an importer's profile that bootstraps serving
// certs is skipped like cis, and one that does not is probed like none.
func TestControlPlanesBootstrapServingReadsTheProfile(t *testing.T) {
	prev, prevConf := profile.All(), version.K8sConfDir
	t.Cleanup(func() {
		version.K8sConfDir = prevConf
		if err := profile.SetRegistered(prev); err != nil {
			t.Fatal(err)
		}
	})
	if err := profile.SetRegistered([]profile.Profile{
		profiletest.Profile(),
		{Name: "bootstraps", KubeletServerTLSBootstrap: true},
		{Name: "static"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		state   nodestate.NodeState
		want    bool
		wantErr error
	}{
		{name: "no profile", state: nodestate.NodeState{Role: "controller"}},
		{name: "the cis profile", state: nodestate.NodeState{Role: "controller", Profile: profile.CISName}, want: true},
		{name: "the legacy cis flag", state: nodestate.NodeState{Role: "controller", CIS: true}, want: true},
		{name: "another bootstrapping profile", state: nodestate.NodeState{Role: "controller", Profile: "bootstraps"}, want: true},
		{name: "a profile with static serving certs", state: nodestate.NodeState{Role: "controller", Profile: "static"}},
		{name: "an unregistered profile refuses", state: nodestate.NodeState{Role: "controller", Profile: "gone"}, wantErr: profile.ErrUnregistered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.K8sConfDir = t.TempDir()
			st := tc.state
			if err := nodestate.SaveState(&st); err != nil {
				t.Fatal(err)
			}
			got, err := controlPlanesBootstrapServing()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("controlPlanesBootstrapServing = %v, %v; want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("controlPlanesBootstrapServing = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
