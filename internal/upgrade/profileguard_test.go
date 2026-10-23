package upgrade

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// TestUpgradeRefusesANodeWhoseProfileThisBinaryDoesNotRegister: a cis worker
// upgraded with an engine binary is refused before the binary installs
// itself, with --check and without, and the refusal carries the cis note.
// A worker is never resolved by the control-plane
// refresh, so this guard is the only one it meets.
func TestUpgradeRefusesANodeWhoseProfileThisBinaryDoesNotRegister(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check bool
	}{
		{"upgrade", false},
		{"upgrade --check", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumeFixture(t)
			writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"), "role: worker\nprofile: cis\n", 0o600)
			writeFile(t, stratumInstallPath, "installed", 0o755)
			prev := profile.All()
			if err := profile.SetRegistered(nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = profile.SetRegistered(prev) })

			err := UpgradeNode(io.Discard, NodeOptions{Check: tc.check})
			for _, want := range []string{profile.CISUnregisteredNote, `profile "cis"`, "nothing was changed"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			if m, err := readUpgradeMarker(); err != nil || m != nil {
				t.Errorf("marker after the refusal = %+v, %v; want none", m, err)
			}
			if sets, err := scanBackupSets(); err != nil || len(sets) != 0 {
				t.Errorf("backup sets after the refusal = %v, %v; want none", sets, err)
			}
			if got, err := os.ReadFile(stratumInstallPath); err != nil || string(got) != "installed" {
				t.Errorf("installed stratum after the refusal = %q, %v; want it untouched", got, err)
			}
		})
	}
}
