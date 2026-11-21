package upgrade

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

// The cis profile is registered as the binary does: a renderer refuses an unregistered profile.
func TestMain(m *testing.M) {
	unitArgv = func() ([]string, error) { return nil, fs.ErrNotExist }
	supervisorArgv = func() ([]string, string, error) { return nil, "", fs.ErrNotExist }
	serviceMainPID = func() (int, error) { return 0, nil }
	serviceExecPath = func() (string, error) { return "", fs.ErrNotExist }
	load := restartConfigLoads
	restartConfigLoads = func(path string) error {
		if path == cluster.DefaultConfigPath {
			return nil
		}
		return load(path)
	}
	// Helper processes run TestMain too and are killed; only a test that writes the record makes it.
	recordDir := filepath.Join(os.TempDir(), fmt.Sprintf("stratum-upgrade-test-%d", os.Getpid()))
	unreplaceableSupervisorPath = func() string { return filepath.Join(recordDir, "unreplaceable-supervisor.json") }
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		fmt.Fprintf(os.Stderr, "register the cis profile: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(recordDir)
	os.Exit(code)
}

func cisProfile() profile.Profile { return profiletest.Profile() }
