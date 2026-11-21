package backupevidence

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// Redirected, or the read reaches the host's config area.
func withEvidencePaths(t *testing.T) {
	t.Helper()
	oldUID := readerNodeUID
	readerNodeUID = func(context.Context, string) (string, error) { return "test-node-uid", nil }
	t.Cleanup(func() { readerNodeUID = oldUID })
	oldConf, oldData, oldEtcd := version.K8sConfDir, version.DataDir, version.EtcdDataDir
	root := t.TempDir()
	version.K8sConfDir = filepath.Join(root, "config")
	version.DataDir = filepath.Join(root, "data")
	version.EtcdDataDir = filepath.Join(root, "etcd")
	t.Cleanup(func() {
		version.K8sConfDir, version.DataDir, version.EtcdDataDir = oldConf, oldData, oldEtcd
	})
}

func TestStateMissingRecordIsNoAttempts(t *testing.T) {
	withEvidencePaths(t)
	if _, err := LoadAttempts(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadAttempts = %v, want fs.ErrNotExist on a node with no record", err)
	}
}

func TestStateSchemaVersion(t *testing.T) {
	withEvidencePaths(t)
	if err := os.MkdirAll(version.K8sConfDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(EvidencePath(), []byte("schemaVersion: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAttempts(); err == nil {
		t.Fatal("a record from a newer schema loaded; its fields may mean something this binary does not know")
	} else if !strings.Contains(err.Error(), "schema") {
		t.Errorf("the refusal does not say what it refused: %v", err)
	}

	if err := os.WriteFile(EvidencePath(), []byte("schemaVersion: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAttempts()
	if err != nil {
		t.Fatalf("an empty but valid record must load: %v", err)
	}
	if !got.Save.AttemptedAt.IsZero() || !got.Upload.AttemptedAt.IsZero() || !got.Prune.AttemptedAt.IsZero() ||
		!got.LastSaveSuccess.IsZero() || !got.LastUploadSuccess.IsZero() || got.Warnings != nil {
		t.Errorf("an empty record loaded as %+v, want no attempts recorded", got)
	}
}
