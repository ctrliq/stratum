//go:build unix

package upgrade

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestBackupStagingENOENTStopsTheInstall: only a missing live file at open may
// skip the backup. An ENOENT while staging the backup (its temp file gone
// before the chmod) is a failed backup, and installing over the live binary
// then would leave nothing to roll back to.
//
// The live path is a FIFO so the backup copy blocks reading it, which leaves
// the test time to remove the copy's temp file mid-flight.
func TestBackupStagingENOENTStopsTheInstall(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "stratum"), filepath.Join(dir, "next")
	if err := syscall.Mkfifo(dst, 0o755); err != nil {
		t.Skipf("no fifos here: %v", err)
	}
	writeFile(t, src, "new", 0o755)
	w, err := os.OpenFile(dst, os.O_RDWR, 0) // O_RDWR does not block on a fifo
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	done := make(chan error, 1)
	go func() {
		_, err := backupAndInstall(io.Discard, dst, src, resumeTS)
		done <- err
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		tmps, _ := filepath.Glob(filepath.Join(dir, ".stratum.bak-*.tmp-*"))
		if len(tmps) == 1 {
			if err := os.Remove(tmps[0]); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backup never started staging")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Close() // EOF: the copy finishes, then chmods a temp file that is gone

	select {
	case err := <-done:
		if err == nil {
			t.Error("a failed backup must stop the install")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("backupAndInstall did not return")
	}
	fi, err := os.Lstat(dst)
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the live file was replaced despite the failed backup: %v, %v", fi, err)
	}
}
