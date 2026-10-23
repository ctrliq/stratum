package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/atomicfile"
)

// The durable single-file writer now lives in internal/atomicfile; the host
// helpers are thin delegating wrappers kept so existing callers are
// untouched. These tests pin what delegation must preserve for those
// callers: the sentinels they match on are the ones the writer actually
// returns, and the wrappers write exactly what atomicfile.WriteFile writes.

// TestHostWriteFileAtomicallySurfacesAtomicfileErrNotDurable: a caller that
// matches host.ErrNotDurable (ha/certs.go's take-the-file-back-off path) must
// keep matching through the wrapper, and the sentinel the wrapper returns
// must be atomicfile.ErrNotDurable, not a divergent copy of it.
func TestHostWriteFileAtomicallySurfacesAtomicfileErrNotDurable(t *testing.T) {
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	boom := errors.New("EIO")
	syncDir = func(string) error { return boom }
	dir := t.TempDir()
	path := filepath.Join(dir, "unit")
	if err := WriteFileAtomically(path, []byte("unit"), 0o644); !errors.Is(err, boom) {
		t.Fatalf("WriteFileAtomically = %v, want the directory sync's error", err)
	} else if !errors.Is(err, ErrNotDurable) || !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("err = %v, want it to match both host.ErrNotDurable and atomicfile.ErrNotDurable", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "unit" {
		t.Errorf("%s = %q, want the new contents, which the rename put in place", path, b)
	}
}

// TestHostCreateFileExclusivelySurfacesAtomicfileErrStrayCopy: a caller that
// matches host.ErrStrayCopy (cluster/ciskey.go's key rollback) must keep
// matching through the wrapper, and the sentinel the wrapper returns must be
// atomicfile.ErrStrayCopy, not a divergent copy of it.
func TestHostCreateFileExclusivelySurfacesAtomicfileErrStrayCopy(t *testing.T) {
	saved := removeTempName
	t.Cleanup(func() { removeTempName = saved })
	removeTempName = func(string) error { return errors.New("EROFS") }
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := CreateFileExclusively(path, []byte("mine"), 0o600)
	if created != nil || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("got %v, %v; want nil and an fs.ErrExist error", created, err)
	}
	if !errors.Is(err, ErrStrayCopy) || !errors.Is(err, atomicfile.ErrStrayCopy) {
		t.Fatalf("err = %v, want it to match both host.ErrStrayCopy and atomicfile.ErrStrayCopy", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "theirs" {
		t.Errorf("the existing file now holds %q", b)
	}
}

// TestHostWriteFileAtomicallyMatchesAtomicfileWriteFile: the wrapper writes
// what the moved writer writes -- the same contents at the same mode, one
// entry per directory -- and fails for the same reason when the write cannot
// even start.
func TestHostWriteFileAtomicallyMatchesAtomicfileWriteFile(t *testing.T) {
	hostDir, atomicDir := t.TempDir(), t.TempDir()
	hostPath := filepath.Join(hostDir, "f")
	atomicPath := filepath.Join(atomicDir, "f")
	if err := WriteFileAtomically(hostPath, []byte("same"), 0o640); err != nil {
		t.Fatalf("WriteFileAtomically: %v", err)
	}
	if err := atomicfile.WriteFile(atomicPath, []byte("same"), 0o640); err != nil {
		t.Fatalf("atomicfile.WriteFile: %v", err)
	}
	hostData, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	atomicData, err := os.ReadFile(atomicPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(hostData) != string(atomicData) || string(hostData) != "same" {
		t.Errorf("wrapper wrote %q, atomicfile wrote %q, want both %q", hostData, atomicData, "same")
	}
	hostFi, err := os.Stat(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	atomicFi, err := os.Stat(atomicPath)
	if err != nil {
		t.Fatal(err)
	}
	if hostFi.Mode().Perm() != 0o640 || atomicFi.Mode().Perm() != 0o640 {
		t.Errorf("modes = %v and %v, want 0640 for both", hostFi.Mode().Perm(), atomicFi.Mode().Perm())
	}
	hostEntries, _ := os.ReadDir(hostDir)
	atomicEntries, _ := os.ReadDir(atomicDir)
	if len(hostEntries) != 1 || len(atomicEntries) != 1 {
		t.Errorf("dirs hold %d and %d entries, want the written file alone in each", len(hostEntries), len(atomicEntries))
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if err := WriteFileAtomically(filepath.Join(missing, "a"), []byte("x"), 0o600); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("WriteFileAtomically into a missing directory = %v, want fs.ErrNotExist", err)
	}
	if err := atomicfile.WriteFile(filepath.Join(missing, "b"), []byte("x"), 0o600); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("atomicfile.WriteFile into a missing directory = %v, want fs.ErrNotExist", err)
	}
}

// TestHostSyncDirMatchesAtomicfileSyncDir: SyncDir keeps working for its own
// callers (the ha and cluster key-directory syncs hold it behind seams) and
// answers the same as atomicfile.SyncDir: a real directory syncs, a missing
// one is an error.
func TestHostSyncDirMatchesAtomicfileSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir on a real directory: %v", err)
	}
	if err := atomicfile.SyncDir(t.TempDir()); err != nil {
		t.Errorf("atomicfile.SyncDir on a real directory: %v", err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("SyncDir reported success for a directory that does not exist")
	}
	if err := atomicfile.SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("atomicfile.SyncDir reported success for a directory that does not exist")
	}
}
