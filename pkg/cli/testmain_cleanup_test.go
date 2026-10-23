package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run the package entrypoint in another process: os.Exit skips defers, so a
// cleanup assertion inside the parent process cannot catch a TestMain leak.
func TestTestMainRemovesNodeLockDirectory(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("KUBEBUILDER_ASSETS", "") // this fixture needs no control plane
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package TestMain: %v\n%s", err, out)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "cli-test-lock-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("TestMain left lock directories after exiting: %v", leftovers)
	}
}
