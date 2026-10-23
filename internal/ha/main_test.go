package ha

// The rejoin takes the node maintenance lock (internal/nodelock), whose lock
// file lives at version.NodeUpgradeLockPath (/run/stratum-upgrade.lock) -- a
// path a test run must neither create nor hold. The fence tests redirect it
// per test (rejoin_fence_test.go); this TestMain redirects it for the rest of
// the package, whose fixtures predate the fence and reach RejoinEtcd without
// one.

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
	// The binary registers the cis profile for every command; a join of a
	// cis control plane renders it.
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		fmt.Fprintf(os.Stderr, "ha tests: register the cis profile: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
