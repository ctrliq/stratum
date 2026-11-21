package ha

// the default lock path is /run/stratum-upgrade.lock, which a test run must neither create nor hold

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ha-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ha tests: temp dir for the node lock: %v\n", err)
		os.Exit(1)
	}
	version.NodeUpgradeLockPath = filepath.Join(dir, "node.lock")
	// the binary registers the cis profile for every command; a join of a cis control plane renders it
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		fmt.Fprintf(os.Stderr, "ha tests: register the cis profile: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
