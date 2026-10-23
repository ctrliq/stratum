package etcdsnap

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// restoreCopiesNode lays out a data dir parent with two restore copies and
// the things beside them that must never be listed or removed: the live data
// dir, a file and a symlink named like copies, and an unrelated dir.
func restoreCopiesNode(t *testing.T) (pre, failed string) {
	t.Helper()
	root := t.TempDir()
	oldData := version.EtcdDataDir
	t.Cleanup(func() { version.EtcdDataDir = oldData })
	version.EtcdDataDir = filepath.Join(root, "etcd")

	write := func(path string, n int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(version.EtcdDataDir, "member", "snap", "db"), 10)
	pre = version.EtcdDataDir + ".pre-restore-20260926-120000"
	failed = version.EtcdDataDir + ".failed-restore-20260926-120100-1"
	write(filepath.Join(pre, "member", "snap", "db"), 100)
	write(filepath.Join(pre, "member", "wal", "0.wal"), 20)
	write(filepath.Join(failed, "member", "snap", "db"), 50)
	write(version.EtcdDataDir+".pre-restore-not-a-dir", 5)
	write(filepath.Join(root, "elsewhere", "keep"), 7)
	if err := os.Symlink(filepath.Join(root, "elsewhere"), version.EtcdDataDir+".failed-restore-link"); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "etcd.backup", "keep"), 3)

	setTreeTime(t, failed, time.Now().Add(-2*time.Hour))
	return pre, failed
}

// setTreeTime gives dir and everything under it the modification time at.
func setTreeTime(t *testing.T, dir string, at time.Time) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, at, at)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRestoreCopiesListsOnlyWhatRestoreKept: the listing finds the
// pre-restore and failed-restore dirs, oldest first with their sizes, and
// nothing else beside the data dir.
func TestRestoreCopiesListsOnlyWhatRestoreKept(t *testing.T) {
	pre, failed := restoreCopiesNode(t)
	copies, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %+v, want the failed and the pre-restore copy only", copies)
	}
	if c := copies[0]; c.Path != failed || c.Kind != "failed-restore" || c.Bytes != 50 {
		t.Errorf("oldest copy = %+v, want %s, failed-restore, 50 bytes", c, failed)
	}
	if c := copies[1]; c.Path != pre || c.Kind != "pre-restore" || c.Bytes != 120 {
		t.Errorf("newest copy = %+v, want %s, pre-restore, 120 bytes", c, pre)
	}
}

// TestRemoveRestoreCopiesRemovesOnlyThemUnderTheLock: removal takes
// the etcd data lock, so it cannot race a restore, and leaves the live data
// dir and everything that is not a restore copy alone.
func TestRemoveRestoreCopiesRemovesOnlyThemUnderTheLock(t *testing.T) {
	pre, failed := restoreCopiesNode(t)
	held := false
	var removedUnlocked []string
	dataDir := version.EtcdDataDir
	fx := &restoreEffects{
		restoreCopies: func() ([]RestoreCopy, error) { return restoreCopies(dataDir) },
		restoreAcquireDataLock: func() (func(), error) {
			held = true
			return func() { held = false }, nil
		},
		restoreRemoveAll: func(p string) error {
			if !held {
				removedUnlocked = append(removedUnlocked, p)
			}
			return os.RemoveAll(p)
		},
	}
	shown, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}
	removed, err := removeRestoreCopies(shown, fx)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("the data lock was not released")
	}
	if len(removedUnlocked) > 0 {
		t.Errorf("removed %q without holding the data lock", removedUnlocked)
	}
	if len(removed) != 2 {
		t.Errorf("removed = %+v, want both copies", removed)
	}
	for _, p := range []string{pre, failed} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", p)
		}
	}
	root := filepath.Dir(version.EtcdDataDir)
	for _, p := range []string{
		filepath.Join(version.EtcdDataDir, "member", "snap", "db"),
		version.EtcdDataDir + ".pre-restore-not-a-dir",
		version.EtcdDataDir + ".failed-restore-link",
		filepath.Join(root, "elsewhere", "keep"),
		filepath.Join(root, "etcd.backup", "keep"),
	} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

// TestRemoveRestoreCopiesRemovesNothingWithoutTheLock: while another operation
// holds the data lock, nothing is removed.
func TestRemoveRestoreCopiesRemovesNothingWithoutTheLock(t *testing.T) {
	pre, failed := restoreCopiesNode(t)
	shown, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := version.EtcdDataDir
	fx := &restoreEffects{
		restoreCopies:          func() ([]RestoreCopy, error) { return restoreCopies(dataDir) },
		restoreAcquireDataLock: func() (func(), error) { return nil, errors.New("another operation is changing it") },
		restoreRemoveAll:       os.RemoveAll,
	}
	if removed, err := removeRestoreCopies(shown, fx); err == nil || len(removed) != 0 {
		t.Fatalf("RemoveRestoreCopies with the lock held = %+v, %v", removed, err)
	}
	for _, p := range []string{pre, failed} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed without the lock: %v", p, err)
		}
	}
}

// TestRemoveRestoreCopiesRemovesEachPathOnce: a copy named
// twice is removed once and reported once.
func TestRemoveRestoreCopiesRemovesEachPathOnce(t *testing.T) {
	restoreCopiesNode(t)
	dataDir := version.EtcdDataDir
	fx := &restoreEffects{
		restoreCopies:          func() ([]RestoreCopy, error) { return restoreCopies(dataDir) },
		restoreAcquireDataLock: func() (func(), error) { return func() {}, nil },
		restoreRemoveAll:       os.RemoveAll,
	}
	shown, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}
	removed, err := removeRestoreCopies(append(shown, shown[0]), fx)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != len(shown) {
		t.Errorf("removed = %+v; want each of the %d copies once", removed, len(shown))
	}
}

// TestIsRestoreCopyPath: only a clean path beside the data dir, named the way
// Restore names its copies, counts.
func TestIsRestoreCopyPath(t *testing.T) {
	restoreCopiesNode(t)
	for path, want := range map[string]bool{
		version.EtcdDataDir + ".pre-restore-20260926-120000":      true,
		version.EtcdDataDir + ".failed-restore-20260926-120000-1": true,
		"":                              false,
		version.EtcdDataDir:             false,
		version.EtcdDataDir + ".backup": false,
		filepath.Dir(version.EtcdDataDir) + "/./etcd.pre-restore-20260926-120000": false,
		filepath.Join(t.TempDir(), "etcd.pre-restore-20260926-120000"):            false,
	} {
		if got := IsRestoreCopyPath(path); got != want {
			t.Errorf("IsRestoreCopyPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536 << 20: "1.5 GiB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestRemoveRestoreCopiesKeepsWhatChangedAfterTheListing: a
// restore can run between the listing and the removal. The copy it creates,
// or one it changes, is one nobody has verified, so only the copies the
// operator was shown, still unchanged, are removed.
func TestRemoveRestoreCopiesKeepsWhatChangedAfterTheListing(t *testing.T) {
	pre, failed := restoreCopiesNode(t)
	dataDir := version.EtcdDataDir
	fx := &restoreEffects{
		restoreCopies:          func() ([]RestoreCopy, error) { return restoreCopies(dataDir) },
		restoreAcquireDataLock: func() (func(), error) { return func() {}, nil },
		restoreRemoveAll:       os.RemoveAll,
	}
	deep := version.EtcdDataDir + ".pre-restore-20260926-110000"
	deepDB := filepath.Join(deep, "member", "snap", "db")
	if err := os.MkdirAll(filepath.Dir(deepDB), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deepDB, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setTreeTime(t, deep, time.Now().Add(-3*time.Hour))
	shown, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}

	// After the listing: a restore creates a new copy, the failed-restore
	// copy's top directory changes, and a file deep inside another copy
	// changes, which leaves that copy's top directory's own time untouched.
	fresh := version.EtcdDataDir + ".pre-restore-20260926-130000"
	if err := os.MkdirAll(filepath.Join(fresh, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(failed, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(deepDB, later, later); err != nil {
		t.Fatal(err)
	}

	removed, err := removeRestoreCopies(shown, fx)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Path != pre {
		t.Errorf("removed = %+v, want only the unchanged %s", removed, pre)
	}
	for _, p := range []string{fresh, failed, deep} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s changed or appeared after the listing and was removed: %v", p, err)
		}
	}
}
