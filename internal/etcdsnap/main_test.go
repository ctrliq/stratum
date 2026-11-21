package etcdsnap

// TestMain redirects the node maintenance lock path away from /run/stratum-upgrade.lock.

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
