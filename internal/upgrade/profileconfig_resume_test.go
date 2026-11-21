package upgrade

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// The marker checks only the target, so a resumed binary may declare files the backup lacks.
func TestResumeRefusesAConfigFileTheKeptBackupDoesNotCover(t *testing.T) {
	withCISConfigs(t)
	const ts = "20261018-000003"
	if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
		t.Fatal(err)
	}
	extra := version.ProfileConfigFile("extra-policy.yaml")
	if err := os.WriteFile(extra, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	wider := cisProfile()
	wider.ConfigFiles = append(wider.ConfigFiles, profile.File{
		Name: "extra-policy.yaml", Kind: profile.PlainFile,
		Render: func(profile.Context) ([]byte, error) { return []byte("new"), nil },
	})
	wrote := false
	writeProfileConfigs = func(profile.Profile) error { wrote = true; return nil }

	_, err := refreshProfileConfigs(io.Discard, ts, wider)
	if err == nil || !strings.Contains(err.Error(), "extra-policy.yaml") || !strings.Contains(err.Error(), "stratum rollback") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("a resume with an uncovered config file = %v; want a refusal naming it and the way out", err)
	}
	if wrote {
		t.Error("the refused resume wrote the profile's configs")
	}
	if b, _ := os.ReadFile(extra); string(b) != "original" {
		t.Errorf("extra-policy.yaml after the refused resume = %q, want the original", b)
	}

	wrote = false
	items, err := refreshProfileConfigs(io.Discard, ts, cisProfile())
	if err != nil || !wrote || len(items) != 1 || !items[0].profile {
		t.Fatalf("a resume with the backed-up profile = %v, %v, wrote %v; want the write through the kept backup", items, err, wrote)
	}
}
