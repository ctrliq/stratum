package nodestate

import (
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// A state written by a binary that predates Profile carries only cis: true;
// it reads as profile cis, and a save writes both spellings so a binary of
// either generation reads the profile back.
func TestProfileNameReadsTheLegacyCISField(t *testing.T) {
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })

	if err := os.MkdirAll(version.K8sConfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(), []byte("role: controller\nhostname: cp1\ncis: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.ProfileName() != CISProfile || st.Profile != CISProfile || !st.CIS {
		t.Errorf("legacy state read as profile %q (Profile %q, CIS %v), want cis", st.ProfileName(), st.Profile, st.CIS)
	}

	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1", Profile: CISProfile}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"profile: cis\n", "cis: true\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("a saved cis state lacks %q:\n%s", want, b)
		}
	}

	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1", Profile: "stig"}); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.ProfileName() != "stig" || st.CIS {
		t.Errorf("another profile read as %q (CIS %v)", st.ProfileName(), st.CIS)
	}
	if st := (&NodeState{}); st.ProfileName() != "" {
		t.Errorf("a state without a profile reports %q", st.ProfileName())
	}
}
