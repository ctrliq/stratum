package apparmor

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// WatchPolicy sends once the file it watches turns readable again: a FIFO
// stands in for the kernel's revision file, which does the same when the
// policy changes.
func TestWatchPolicySendsOnAChange(t *testing.T) {
	old := revisionPath
	t.Cleanup(func() { revisionPath = old })
	revisionPath = filepath.Join(t.TempDir(), "revision")
	if err := syscall.Mkfifo(revisionPath, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := WatchPolicy(ctx)
	if changes == nil {
		t.Fatal("WatchPolicy() = nil for a revision file that exists")
	}
	w, err := os.OpenFile(revisionPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString("42\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchPolicy() did not send after the revision file turned readable")
	}
}

func TestWatchPolicyWithoutARevisionFileNeverSends(t *testing.T) {
	old := revisionPath
	t.Cleanup(func() { revisionPath = old })
	revisionPath = filepath.Join(t.TempDir(), "missing")
	if changes := WatchPolicy(context.Background()); changes != nil {
		t.Errorf("WatchPolicy() without a revision file = %v, want nil, which never sends", changes)
	}
}
