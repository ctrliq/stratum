package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/objstore"
)

func unreadableSnapshotConfig(t *testing.T) error {
	t.Helper()
	useSnapshotDir(t)
	path := filepath.Join(realTempDir(t), "snapshot.conf")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, readErr := os.ReadFile(path)
	if readErr == nil {
		t.Fatal("reading a directory unexpectedly succeeded")
	}
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.DefaultConfig(), readErr }
	return readErr
}

func writeSnapshots(t *testing.T, dir string, count int) []string {
	t.Helper()
	h, err := host.Hostname()
	if err != nil {
		t.Skip(err)
	}
	names := make([]string, count)
	for i := range names {
		names[i] = etcdsnap.ScheduledName(h, fmt.Sprintf("202501%02d-000000", i+1))
		if err := os.WriteFile(filepath.Join(dir, names[i]), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return names
}

func TestSnapshotPruneRefusesToDeleteWhenConfigIsUnreadable(t *testing.T) {
	readErr := unreadableSnapshotConfig(t)
	dir := snapshotDir()
	names := writeSnapshots(t, dir, 6)

	cmd := newSnapshotPruneCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "snapshot config") || !strings.Contains(err.Error(), readErr.Error()) {
		t.Fatalf("err = %v, want the config read error %q", err, readErr)
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("prune deleted %s despite refusing: %v", n, err)
		}
	}
}

func TestSnapshotPruneWithExplicitRetainPrunesLocalWhenConfigIsUnreadable(t *testing.T) {
	readErr := unreadableSnapshotConfig(t)
	dir := snapshotDir()
	names := writeSnapshots(t, dir, 5)

	var errOut bytes.Buffer
	var out string
	var err error
	out = captureStdout(t, func() {
		cmd := newSnapshotPruneCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"--retain", "3"})
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("prune returned %v; an explicit --retain must prune", err)
	}
	if !strings.Contains(errOut.String(), readErr.Error()) || !strings.Contains(errOut.String(), "WARN") {
		t.Errorf("stderr does not warn with the config read error:\n%s", errOut.String())
	}
	for i, n := range names {
		_, statErr := os.Stat(filepath.Join(dir, n))
		if i < 2 && statErr == nil {
			t.Errorf("prune kept %s beyond the --retain 3", n)
		}
		if i >= 2 && statErr != nil {
			t.Errorf("prune removed kept snapshot %s: %v", n, statErr)
		}
	}
	if !strings.Contains(out, "pruned to newest 3 snapshot(s)") {
		t.Errorf("stdout does not carry the prune summary:\n%s", out)
	}
}

func TestSnapshotRestoreS3ReportsTheConfigError(t *testing.T) {
	readErr := unreadableSnapshotConfig(t)
	oldDL, oldRestore := s3Download, restoreLocalSeed
	t.Cleanup(func() { s3Download, restoreLocalSeed = oldDL, oldRestore })
	downloaded := false
	s3Download = func(*objstore.Target, context.Context, *os.Root, string) error { downloaded = true; return nil }
	restoreLocalSeed = func(io.Writer, string) error { return nil }

	cmd := newSnapshotRestoreCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"s3://b/x.db", "--yes"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), readErr.Error()) {
		t.Fatalf("err = %v, want the config read error %q", err, readErr)
	}
	if strings.Contains(err.Error(), "needs an S3 target") {
		t.Errorf("error misnames the problem as a missing S3 target: %v", err)
	}
	if downloaded {
		t.Error("downloaded despite the config read error")
	}
}
