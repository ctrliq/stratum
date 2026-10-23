package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestSnapshotPruneRestoresListsBeforeItRemoves: without --yes the
// command only lists the copies; a pre-restore copy can be the only copy of the
// data from before a restore, so removing one takes --yes.
func TestSnapshotPruneRestoresListsBeforeItRemoves(t *testing.T) {
	oldData, oldEtcd := version.DataDir, version.EtcdDataDir
	t.Cleanup(func() { version.DataDir, version.EtcdDataDir = oldData, oldEtcd })
	version.DataDir = filepath.Join(t.TempDir(), "stratum")
	version.EtcdDataDir = filepath.Join(version.DataDir, "etcd")
	copyDir := version.EtcdDataDir + ".pre-restore-20260926-120000"
	if err := os.MkdirAll(filepath.Join(copyDir, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "member", "db"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := newSnapshotPruneRestoresCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("prune-restores %v: %v", args, err)
		}
		return out.String()
	}

	out := run()
	if !strings.Contains(out, copyDir) || !strings.Contains(out, "2.0 KiB") || !strings.Contains(out, "Nothing was removed") {
		t.Errorf("dry run output does not list the copy with its size:\n%s", out)
	}
	if _, err := os.Stat(copyDir); err != nil {
		t.Fatalf("the dry run removed %s: %v", copyDir, err)
	}

	out = run("--yes")
	if !strings.Contains(out, "removed "+copyDir) {
		t.Errorf("--yes output does not name what it removed:\n%s", out)
	}
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Errorf("--yes left %s: %v", copyDir, err)
	}
	if out := run(); !strings.Contains(out, "no restore copies to remove") {
		t.Errorf("after --yes, a listing = %q", out)
	}
}
