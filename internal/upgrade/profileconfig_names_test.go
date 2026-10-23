package upgrade

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

// The backup record holds one name per line and the restore reads each back
// as a path element, so a profile file name with a newline would read as two
// coverage entries (and a rollback would remove a file it never backed up),
// and one with a backslash would make the record reader refuse the rollback.
// Registration refuses such names; the upgrade refuses them too, before it
// writes anything, on a fresh run and on a resume.
func TestRefreshProfileConfigsRefusesANameTheRecordCannotRoundTrip(t *testing.T) {
	for _, bad := range []string{"policy.yaml\nnode.yaml", `policy\node.yaml`, "policy\x01.yaml"} {
		t.Run(strings.NewReplacer("\n", `\n`, "\x01", `\x01`).Replace(bad), func(t *testing.T) {
			withCISConfigs(t)
			prof := profile.Profile{Name: "odd", ConfigFiles: []profile.File{
				{Name: bad, Kind: profile.PlainFile, Render: func(profile.Context) ([]byte, error) { return []byte("new"), nil }},
			}}
			wrote := false
			writeProfileConfigs = func(profile.Profile) error { wrote = true; return nil }
			const ts = "20261018-000001"
			if _, err := refreshProfileConfigs(io.Discard, ts, prof); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("refreshProfileConfigs = %v, want a refusal", err)
			}
			if wrote {
				t.Error("the refused refresh wrote the profile's configs")
			}
			if _, err := os.Stat(profileConfigBakDir(ts)); !os.IsNotExist(err) {
				t.Errorf("the refused refresh left a backup: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(profileConfigBakDir(ts)), ".cisconfig-staging-"+ts)); !os.IsNotExist(err) {
				t.Errorf("the refused refresh left a staging dir: %v", err)
			}

			// The resume path: a kept backup from an earlier run must not
			// let the name through to the write either.
			if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
				t.Fatal(err)
			}
			wrote = false
			if _, err := refreshProfileConfigs(io.Discard, ts, prof); err == nil || wrote {
				t.Fatalf("on a resume, refreshProfileConfigs = %v, wrote = %v; want a refusal before the write", err, wrote)
			}
		})
	}
}

// A record carrying a name with a control character is refused like one
// carrying a path, before anything is restored or removed.
func TestRestoreProfileConfigsRefusesARecordWithAControlCharacter(t *testing.T) {
	withCISConfigs(t)
	const ts = "20261018-000002"
	if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
		t.Fatal(err)
	}
	bak := profileConfigBakDir(ts)
	record := filepath.Join(bak, profileConfigRecordName)
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, append(raw, []byte("odd\x01.yaml\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreProfileConfigs(io.Discard, bak); err == nil {
		t.Fatal("restore accepted a record naming a file with a control character")
	}
}
