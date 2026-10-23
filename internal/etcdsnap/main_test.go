package etcdsnap

// The restore takes the node maintenance lock (internal/nodelock), whose lock
// file lives at version.NodeUpgradeLockPath (/run/stratum-upgrade.lock) -- a
// path a test run must neither create nor hold. The fence tests redirect it
// per test (restore_fence_test.go); this TestMain redirects it for the rest of
// the package, whose fixtures predate the fence and reach Restore without one.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "etcdsnap-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcdsnap tests: temp dir for the node lock: %v\n", err)
		os.Exit(1)
	}
	version.NodeUpgradeLockPath = filepath.Join(dir, "node.lock")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
