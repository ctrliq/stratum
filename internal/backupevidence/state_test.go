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

// From here: the durable attempt-state record, the snapshot evidence file —
// the renewal-record precedent applied to the scheduled snapshot loop, read
// by the backup-evidence interface. The writer's own durability,
// independence and concurrency discipline is proven on the writer the loop
// actually uses, in internal/etcdsnap's recording tests; these tests pin
// the reader's side of the contract.

// withEvidencePaths points the snapshot evidence file and the etcd data lock
// at one temp tree: the reader loads the record from the config area, and a
// test must redirect it or the read reaches the host's /var/lib/stratum.
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

// TestStateMissingRecordIsNoAttempts: a node whose loop has never run has no
// file. The loader reports that as fs.ErrNotExist — no attempt recorded yet,
// never an invented empty record.
func TestStateMissingRecordIsNoAttempts(t *testing.T) {
	withEvidencePaths(t)
	if _, err := LoadAttempts(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadAttempts = %v, want fs.ErrNotExist on a node with no record", err)
	}
}

// TestStateSchemaVersion pins the version discipline on the reader's side:
// a record a newer binary wrote refuses to load rather than be misread — a
// newer revision may mean something else by a field. An empty but valid
// record loads as no attempts yet: a node whose loop has never run one.
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
