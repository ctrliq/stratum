package netcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func useTempNetConfig(t *testing.T) string {
	t.Helper()
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })
	return version.NetConfigPath
}

// TestSaveReplacesTheFileAtomically: netconfig is authoritative after
// first boot, so a crash mid-write must not leave a truncated file. Save writes a
// temp file and renames it over the old one, which a hard link to the old file
// shows: the link keeps the old bytes, where an in-place write would change them.
func TestSaveReplacesTheFileAtomically(t *testing.T) {
	path := useTempNetConfig(t)
	if err := Save("10.42.0.0/16", "10.43.0.0/16", ""); err != nil {
		t.Fatal(err)
	}
	old := path + ".old"
	if err := os.Link(path, old); err != nil {
		t.Fatal(err)
	}
	if err := Save("10.52.0.0/16", "10.53.0.0/16", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(old); string(b) != "10.42.0.0/16\n10.43.0.0/16\n" {
		t.Errorf("old file was rewritten in place: %q", b)
	}
	if b, _ := os.ReadFile(path); string(b) != "10.52.0.0/16\n10.53.0.0/16\n" {
		t.Errorf("netconfig = %q, want the new CIDRs", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("netconfig mode = %v, want 0644", fi.Mode().Perm())
	}
}

// A failed Save leaves the file it was replacing, and no temp file behind.
func TestSaveFailureLeavesNoTempFile(t *testing.T) {
	path := useTempNetConfig(t)
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil { // a rename onto this fails
		t.Fatal(err)
	}
	if err := Save("10.42.0.0/16", "10.43.0.0/16", ""); err == nil {
		t.Fatal("Save onto a directory succeeded")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || entries[0].Name() != "netconfig" {
		t.Errorf("directory holds %v, want only the original netconfig", entries)
	}
}

// A damaged netconfig stops the node (it is authoritative, so nothing guesses),
// and the error names the file and says to restore it from another control
// plane, not to remove it: only a controller start restores CIDRs without it.
func TestLoadDamagedFileNamesIt(t *testing.T) {
	for name, content := range map[string]string{
		"empty":          "",
		"one line":       "10.42.0.0/16\n",
		"truncated CIDR": "10.42.0.0/16\nnot-a-cidr\n",
		"three lines":    "a\nb\nc\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := useTempNetConfig(t)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("Load accepted a damaged file")
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "another control plane") {
				t.Errorf("error %q does not name %s and say to restore it from another control plane", err, path)
			}
			if strings.Contains(err.Error(), "remove") {
				t.Errorf("error %q still advises removing the file", err)
			}
		})
	}
}
