package profilefiles

import (
	"errors"
	"os"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// Write renders every file before it writes any: a profile whose second file
// cannot render leaves the first file's previous body in place and creates
// nothing, so a start, join or upgrade that cannot pin a file does not leave
// the node half-configured. Once every file renders, every file lands.
func TestWriteWritesEveryFileOrNothing(t *testing.T) {
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })

	previous := []byte("the first file, as the previous start left it\n")
	if err := os.WriteFile(version.ProfileConfigFile("first.yaml"), previous, 0o644); err != nil {
		t.Fatal(err)
	}
	renderErr := errors.New("the kubernetes version is unknown")
	p := profile.Profile{Name: "two-files", ConfigFiles: []profile.File{
		{Name: "first.yaml", Kind: profile.PlainFile, Render: func(profile.Context) ([]byte, error) {
			return []byte("the first file, re-rendered\n"), nil
		}},
		{Name: "second.yaml", Kind: profile.PlainFile, Render: func(profile.Context) ([]byte, error) {
			return nil, renderErr
		}},
	}}

	if err := Write(p); !errors.Is(err, renderErr) {
		t.Fatalf("Write = %v, want the second file's render error", err)
	}
	got, err := os.ReadFile(version.ProfileConfigFile("first.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(previous) {
		t.Errorf("a profile that could not render every file replaced the first one:\ngot:\n%s\nwant:\n%s", got, previous)
	}
	if _, err := os.Stat(version.ProfileConfigFile("second.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(second.yaml) = %v, want it absent after its render failed", err)
	}
	if entries, err := os.ReadDir(version.K8sConfDir); err != nil || len(entries) != 1 {
		t.Errorf("the config dir holds %d entries (err %v) after a failed render, want only first.yaml", len(entries), err)
	}

	p.ConfigFiles[1].Render = func(profile.Context) ([]byte, error) { return []byte("the second file\n"), nil }
	if err := Write(p); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"first.yaml": "the first file, re-rendered\n", "second.yaml": "the second file\n"} {
		got, err := os.ReadFile(version.ProfileConfigFile(name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s after every file rendered:\ngot:\n%s\nwant:\n%s", name, got, want)
		}
	}
}
