package cli

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
)

func TestProfileNameFoldsTheDeprecatedCISFlag(t *testing.T) {
	for _, tc := range []struct {
		o       runUpOptions
		want    string
		wantErr string
	}{
		{runUpOptions{}, "", ""},
		{runUpOptions{cis: true}, "cis", ""},
		{runUpOptions{profile: "cis"}, "cis", ""},
		{runUpOptions{profile: "cis", cis: true}, "cis", ""},
		{runUpOptions{profile: "stig"}, "stig", ""},
		{runUpOptions{profile: "stig", cis: true}, "", "two profiles"},
	} {
		got, err := tc.o.profileName()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%+v: profileName = %q, %v; want an error containing %q", tc.o, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%+v: profileName = %q, %v; want %q", tc.o, got, err, tc.want)
		}
	}
}

// A control plane's saved profile is not dropped by a restart that leaves the
// flag off, and is not swapped for another; a node without one may start with
// one, which is how a profile is turned on for a running cluster.
func TestRefuseDroppedProfile(t *testing.T) {
	cis := &cluster.NodeState{Role: "controller", Profile: "cis"}
	legacy := &cluster.NodeState{Role: "controller", CIS: true}
	none := &cluster.NodeState{Role: "controller"}
	for _, tc := range []struct {
		name    string
		prev    *cluster.NodeState
		want    string
		wantErr string
	}{
		{"same profile", cis, "cis", ""},
		{"legacy state, same profile", legacy, "cis", ""},
		{"no saved profile, none asked", none, "", ""},
		{"no saved profile, one asked", none, "cis", ""},
		{"saved profile, none asked", cis, "", "--profile cis"},
		{"legacy saved profile, none asked", legacy, "", "--profile cis"},
		{"saved profile, another asked", cis, "stig", "cannot be changed"},
	} {
		err := refuseDroppedProfile(tc.prev, tc.want)
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: refuseDroppedProfile = %v, want no refusal", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: refuseDroppedProfile = %v, want a refusal containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestUpArgvCarriesTheProfile(t *testing.T) {
	// cis keeps the spelling every release reads: an upgrade rollback
	// re-execs the previous binary with this argv, and one that predates
	// --profile would refuse the flag.
	got := cluster.UpArgv("/usr/local/bin/stratum", false, "/etc/stratum/config.yaml", "cis")
	want := "/usr/local/bin/stratum up --config /etc/stratum/config.yaml --cis"
	if strings.Join(got, " ") != want {
		t.Errorf("UpArgv = %q, want %q", strings.Join(got, " "), want)
	}
	got = cluster.UpArgv("/usr/local/bin/stratum", false, "/etc/stratum/config.yaml", "stig")
	want = "/usr/local/bin/stratum up --config /etc/stratum/config.yaml --profile stig"
	if strings.Join(got, " ") != want {
		t.Errorf("UpArgv for another profile = %q, want %q", strings.Join(got, " "), want)
	}
	if got := cluster.UpArgv("/x", false, "/c.yaml", ""); strings.Join(got, " ") != "/x up --config /c.yaml" {
		t.Errorf("UpArgv without a profile = %q", strings.Join(got, " "))
	}
	if got := cluster.UpArgv("/x", true, "/c.yaml", "cis"); strings.Join(got, " ") != "/x up --worker" {
		t.Errorf("a worker's UpArgv = %q, want bare --worker (its profile is in its state)", strings.Join(got, " "))
	}
}
