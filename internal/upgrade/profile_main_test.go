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

// TestMain registers the engine's cis profile, as the binary does for every
// command: an upgrade re-renders cis nodes, and a renderer refuses a node
// whose profile is not registered.
//
// It also stops the missing-config guard reading this host's own stratum unit
// and supervisor: on a machine that is itself a stratum node, a --config it
// names that no longer exists would refuse every controller upgrade a test
// drives. A test of the guard swaps in the command lines it wants. Its load of
// each restart --config still reads the files tests name, but not this host's
// default config, which a test cannot put a value in. Likewise a
// dry run's supervisor handoff preview asks systemd about this host's own
// stratum unit; here no systemd runs the supervisor, unless a test says so.
// And the record of a supervisor the handoff cannot replace is kept out of
// this host's data directory.
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
	// Not created here: the lock tests' helper processes run TestMain too and
	// are killed, so a directory made up front would outlive them. A test
	// that writes the record makes it.
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

// cisProfile is the engine's cis profile, the one TestMain registers.
func cisProfile() profile.Profile { return profiletest.Profile() }
