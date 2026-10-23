package comps

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestSupervisorComparesItsBinaryWithTheInstalledOne: a supervisor is current
// only while it runs the very file installed at StratumInstallPath. An upgrade
// replaces that file, and the old supervisor keeps running the old inode.
func TestSupervisorComparesItsBinaryWithTheInstalledOne(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "stratum")
	other := filepath.Join(dir, "stratum.old")
	sameBuild := filepath.Join(dir, "stratum.restored")
	for p, b := range map[string]string{installed: "new build", other: "old build", sameBuild: "new build"} {
		if err := os.WriteFile(p, []byte(b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	savedPath, savedPeer, savedExe, savedPath2 := version.StratumInstallPath, socketPeerPID, procExe, procExePath
	t.Cleanup(func() {
		version.StratumInstallPath, socketPeerPID, procExe, procExePath = savedPath, savedPeer, savedExe, savedPath2
	})
	version.StratumInstallPath = installed
	socketPeerPID = func(string) (int, error) { return 42, nil }

	for _, c := range []struct {
		name, exe, runs string
		installed       bool
	}{
		{"runs the installed file", installed, installed, true},
		{"runs another file", other, other, false},
		{"runs a file since deleted", installed + " (deleted)", other, false},
		{"runs the same build from another file", sameBuild, sameBuild, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			procExe = func(pid int) (string, error) { return c.exe, nil }
			procExePath = func(int) string { return c.runs }
			got, err := Supervisor()
			if err != nil {
				t.Fatal(err)
			}
			if got.PID != 42 || got.Exe != c.exe || got.Installed != c.installed || got.Stale() == c.installed {
				t.Errorf("got %+v, want pid 42, exe %q, installed %v", got, c.exe, c.installed)
			}
		})
	}

	socketPeerPID = func(string) (int, error) { return 0, errors.New("no supervisor") }
	if _, err := Supervisor(); err == nil {
		t.Error("a supervisor that cannot be found was reported")
	}
}
