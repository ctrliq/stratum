package etcdsnap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/version"
)

// A bundle is collected under the node maintenance lock, so it never reads a
// certificate and its key from either side of a rotation. It waits a short
// while for the lock, and takes no bundle when an operation holds it past
// that.
func TestNodeStateLockWaitsThenGivesUp(t *testing.T) {
	oldPath, oldWait, oldEvery := version.NodeUpgradeLockPath, nodeStateLockWait, nodeStateLockEvery
	t.Cleanup(func() {
		version.NodeUpgradeLockPath, nodeStateLockWait, nodeStateLockEvery = oldPath, oldWait, oldEvery
	})
	version.NodeUpgradeLockPath = filepath.Join(t.TempDir(), "node.lock")
	nodeStateLockWait, nodeStateLockEvery = 300*time.Millisecond, 20*time.Millisecond

	held, err := nodelock.Acquire("certs rotate")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := nodeStateLock(context.Background()); err == nil || !strings.Contains(err.Error(), "certs rotate") {
		t.Fatalf("with the lock held past the wait: error = %v, want one naming the holder", err)
	}
	if took := time.Since(start); took < nodeStateLockWait {
		t.Errorf("gave up after %s, before the %s wait", took, nodeStateLockWait)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		held()
	}()
	release, err := nodeStateLock(context.Background())
	if err != nil {
		t.Fatalf("with the lock released during the wait: %v", err)
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	again, err := nodelock.Acquire("certs rotate")
	if err != nil {
		t.Fatal(err)
	}
	defer again()
	cancel()
	if _, err := nodeStateLock(ctx); err == nil {
		t.Error("a cancelled save went on waiting for the lock")
	}
}
