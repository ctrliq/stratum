package atomicfile

import (
	"errors"
	"github.com/ctrliq/stratum/internal/version"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestWriteFileSyncsTheDirectoryAfterTheRename: the rename is durable only
// once the directory holding it is synced, so the sync has to happen, on that
// directory, after the file is in place -- and a failed sync is a failed
// write, since the caller cannot know the rename will last. That failure is
// marked ErrNotDurable: the new file is already at path.
func TestWriteFileSyncsTheDirectoryAfterTheRename(t *testing.T) {
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	var synced []string
	syncDir = func(d string) error {
		if b, err := os.ReadFile(path); err != nil || string(b) != "new" {
			t.Errorf("directory synced before the rename put the file in place (%q, %v)", b, err)
		}
		synced = append(synced, d)
		return saved(d)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("synced %q, want exactly the file's directory %q", synced, dir)
	}

	boom := errors.New("EIO")
	syncDir = func(string) error { return boom }
	if err := WriteFile(path, []byte("again"), 0o600); !errors.Is(err, boom) || !errors.Is(err, ErrNotDurable) {
		t.Fatalf("err = %v, want the directory sync's error, marked ErrNotDurable", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "again" {
		t.Fatalf("%s = %q, want the new contents, which the rename put in place", path, b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries after a failed directory sync, want only the renamed file", len(entries))
	}
}

// TestWriteFileFailureBeforeTheRename: the file at path is left byte for
// byte, the temp file is removed, and a temp file that cannot be removed is
// named in the error as ErrStrayCopy.
func TestWriteFileFailureBeforeTheRename(t *testing.T) {
	savedSync, savedRemove := syncFile, removeTempName
	t.Cleanup(func() { syncFile, removeTempName = savedSync, savedRemove })
	boom := errors.New("EIO")
	syncFile = func(*os.File) error { return boom }
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFile(path, []byte("new"), 0o600); !errors.Is(err, boom) || errors.Is(err, ErrStrayCopy) || errors.Is(err, ErrNotDurable) {
		t.Fatalf("err = %v, want the sync's error alone", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("%s = %q after a failed write, want %q", path, b, "old")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries after a failed write, want only %s", len(entries), path)
	}

	removeTempName = func(string) error { return errors.New("EROFS") }
	err := WriteFile(path, []byte("new"), 0o600)
	if !errors.Is(err, boom) || !errors.Is(err, ErrStrayCopy) || !strings.Contains(err.Error(), "EROFS") {
		t.Fatalf("err = %v, want the sync's error, ErrStrayCopy and the removal's error", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "f" && !strings.Contains(err.Error(), filepath.Join(dir, e.Name())) {
			t.Errorf("error does not name the temp file %s: %v", e.Name(), err)
		}
	}
}

// TestAtomicWritesSetTheModeBeforeTheSync: a mode changed after the fsync is
// not covered by it, so a crash could leave a write that reported success at
// the temp file's 0600. Both writers must have the final mode on the file
// when it is synced, and the file at path must carry that mode afterwards --
// not the 0600 the temp file is created with.
func TestAtomicWritesSetTheModeBeforeTheSync(t *testing.T) {
	saved := syncFile
	t.Cleanup(func() { syncFile = saved })
	var modes []os.FileMode
	syncFile = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		modes = append(modes, fi.Mode().Perm())
		return saved(f)
	}
	dir := t.TempDir()
	if err := WriteFile(filepath.Join(dir, "unit"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "unit"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644, not the temp file's 0600", fi.Mode().Perm())
	}
	if _, err := CreateExclusive(filepath.Join(dir, "key"), []byte("k"), 0o640); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	if len(modes) != 2 || modes[0] != 0o644 || modes[1] != 0o640 {
		t.Errorf("modes at sync time = %v, want [-rw-r--r-- -rw-r-----]", modes)
	}
}

// TestWriteFileLabelsBeforeTheRename: the files written atomically are
// labelled for their destination path while still under the temp name -- a
// file renamed over its destination otherwise keeps the label of the temp
// name. The temp file is the hidden, unique name in the file's own directory.
func TestWriteFileLabelsBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stratum.service")
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	var labelled []string
	labelLike = func(tmp, dst string) error {
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s labelled after it was renamed into place", dst)
		}
		if _, err := os.Stat(tmp); err != nil {
			t.Errorf("labelled %s before the temp file existed: %v", tmp, err)
		}
		if filepath.Dir(tmp) != dir || !strings.HasPrefix(filepath.Base(tmp), ".stratum.service.tmp-") {
			t.Errorf("labelled %s, want the hidden same-directory temp file", tmp)
		}
		labelled = append(labelled, dst)
		return nil
	}
	if err := WriteFile(path, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(labelled) != 1 || labelled[0] != path {
		t.Errorf("labelled %q, want [%s]", labelled, path)
	}
}

// TestWriteFileStopsOnALabelFailure: a file whose label cannot be set must
// never reach path with the wrong label. The old file is kept and the temp
// file is cleaned up.
func TestWriteFileStopsOnALabelFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stratum.service")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	boom := errors.New("chcon: Operation not permitted")
	labelLike = func(string, string) error { return boom }
	if err := WriteFile(path, []byte("new"), 0o644); !errors.Is(err, boom) {
		t.Fatalf("WriteFile = %v, want the label error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("%s = %q, want the old contents kept", path, b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries after a failed label, want only %s", len(entries), path)
	}
}

// TestWriteFileReplacesASymlinkDestination: the rename replaces a symlink at
// path rather than writing through it, so the file at path is afterwards a
// regular file holding the new contents and the link's target is untouched.
// This order of events is what the reboot unit's mask guard relies on: a
// masked unit is a symlink to /dev/null, and InstallUnit checks for it before
// writing because a rename over it would silently undo the mask.
func TestWriteFileReplacesASymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-unit")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "unit")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file after the write; the rename followed the symlink instead of replacing it", path)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "new" {
		t.Errorf("%s = %q, %v; want the new contents behind a regular file", path, b, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "target" {
		t.Errorf("the symlink's target now holds %q, want it untouched", b)
	}
}

// TestWriteFileTempNamesAreUniqueUnderConcurrentWriters: the temp name is
// unique per write. A fixed temp name -- the shared-name hazard this package
// exists to retire -- makes concurrent writers to one path fail on each
// other's temp file; unique names let every write through and leave exactly
// the file behind.
func TestWriteFileTempNamesAreUniqueUnderConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	const writers = 16
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = WriteFile(path, []byte(strconv.Itoa(i)), 0o600)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	written := map[string]bool{}
	for i := range writers {
		written[strconv.Itoa(i)] = true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !written[string(b)] {
		t.Errorf("%s holds %q, want the contents of one of the %d writers", path, b, writers)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries after %d concurrent writes, want only %s", len(entries), writers, path)
	}
}

// TestCreateExclusive: the at-rest key is minted on an earlier look at its
// path, so the write must never replace a file that appeared since; and no
// temp file is left behind unreported.
func TestCreateExclusive(t *testing.T) {
	savedSync := syncDir
	t.Cleanup(func() { syncDir = savedSync })
	onlyEntries := func(t *testing.T, dir string, want ...string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("directory holds %v, want %v (a temp file left behind?)", got, want)
		}
	}

	t.Run("creates the file at its mode, durably, and returns it", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		var synced []string
		syncDir = func(d string) error {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("directory synced before the file was linked in: %v", err)
			}
			// The temp name is gone before the sync, so the sync covers its
			// removal too.
			if entries, _ := os.ReadDir(d); len(entries) != 1 {
				t.Errorf("directory synced with %d entries, want only the file (temp name still there?)", len(entries))
			}
			synced = append(synced, d)
			return savedSync(d)
		}
		created, err := CreateExclusive(path, []byte("k"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		cur, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(created, cur) {
			t.Fatal("the returned FileInfo is not the file at path")
		}
		if cur.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", cur.Mode().Perm())
		}
		if len(synced) != 1 || synced[0] != dir {
			t.Errorf("synced %v, want exactly %s", synced, dir)
		}
		onlyEntries(t, dir, "key")
	})
	t.Run("refuses to replace a file that is already there", func(t *testing.T) {
		syncDir = savedSync
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := CreateExclusive(path, []byte("mine"), 0o600)
		if !errors.Is(err, fs.ErrExist) || created != nil {
			t.Fatalf("got %v, %v; want nil and an fs.ErrExist error", created, err)
		}
		if b, _ := os.ReadFile(path); string(b) != "theirs" {
			t.Errorf("the existing file now holds %q", b)
		}
		onlyEntries(t, dir, "key")
	})
	t.Run("a failed directory sync still reports what it created", func(t *testing.T) {
		boom := errors.New("EIO")
		syncDir = func(string) error { return boom }
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		created, err := CreateExclusive(path, []byte("k"), 0o600)
		// Nothing was replaced -- the file was linked in create-only -- so the
		// failure is the sync's error alone, not ErrNotDurable.
		if !errors.Is(err, boom) || created == nil || errors.Is(err, ErrNotDurable) {
			t.Fatalf("got %v, %v; want the created file and the sync's error alone", created, err)
		}
		onlyEntries(t, dir, "key")
	})
	t.Run("a temp file that outlives a failed link is reported", func(t *testing.T) {
		savedRemove := removeTempName
		removeTempName = func(string) error { return errors.New("EIO") }
		t.Cleanup(func() { removeTempName = savedRemove })
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := CreateExclusive(path, []byte("mine"), 0o600)
		if !errors.Is(err, fs.ErrExist) || !errors.Is(err, ErrStrayCopy) || created != nil || !strings.Contains(err.Error(), "EIO") {
			t.Fatalf("got %v, %v; want nil, and fs.ErrExist with ErrStrayCopy and the removal's error", created, err)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "key" && !strings.Contains(err.Error(), filepath.Join(dir, e.Name())) {
				t.Errorf("error does not name the temp file %s: %v", e.Name(), err)
			}
		}
	})
	t.Run("a temp name that outlives the link is reported, not retried silently", func(t *testing.T) {
		synced := 0
		syncDir = func(string) error { synced++; return nil }
		savedRemove := removeTempName
		removeTempName = func(string) error { return errors.New("EIO") }
		t.Cleanup(func() { removeTempName = savedRemove })
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		created, err := CreateExclusive(path, []byte("k"), 0o600)
		if !errors.Is(err, ErrStrayCopy) || created == nil || !strings.Contains(err.Error(), "EIO") {
			t.Fatalf("got %v, %v; want the created file and ErrStrayCopy with the removal's error", created, err)
		}
		if synced != 0 {
			t.Errorf("synced the directory %d times with a stray copy outstanding", synced)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 2 {
			t.Errorf("dir holds %d entries, want the key and its temp name", len(entries))
		}
		for _, e := range entries {
			if e.Name() != "key" && !strings.Contains(err.Error(), filepath.Join(dir, e.Name())) {
				t.Errorf("error does not name the temp file %s: %v", e.Name(), err)
			}
		}
	})
}

// TestSyncDirReportsAnUnopenableDirectory: the real sync, not the seam, fails
// on a directory it cannot open rather than reporting a sync that never ran,
// through both the seam and the exported SyncDir.
func TestSyncDirReportsAnUnopenableDirectory(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir reported success for a directory that does not exist")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a real directory: %v", err)
	}
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir on a real directory: %v", err)
	}
}

// TestDefaultLabelerKeepsDiagnosticsOffStdout pins the default label route's
// protocol safety: this writer runs under handlers whose stdout is not for
// diagnostics (the MCP server's stdio among them), so when the real selinux
// route has something to say — a policy-gone warning, on a labelled node
// whose tools are gone — the file's own stdout carries none of it. The test
// creates the emitting condition itself: a labelling stamp under a redirected
// data dir, and no matchpathcon on PATH, which is exactly the host the
// warning fires on. Where the kernel exposes no selinuxfs at all, the route
// says nothing and the pin below degenerates to a stdout smoke; the warning's
// delivery to the caller's writer is pinned against the fake policy host in
// the selinux package.
func TestDefaultLabelerKeepsDiagnosticsOffStdout(t *testing.T) {
	saved := labelLike
	t.Cleanup(func() { labelLike = saved })
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	dataDir := t.TempDir()
	version.DataDir = dataDir
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "selinux-labels"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "") // no matchpathcon: the labelling lookup is policy-gone

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		wOut.Close()
		wErr.Close()
		rOut.Close()
		rErr.Close()
	})
	if err := WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	wOut.Close()
	wErr.Close()
	stdout, _ := io.ReadAll(rOut)
	stderr, _ := io.ReadAll(rErr)
	if len(stdout) != 0 {
		t.Errorf("the default labeler wrote to the process's stdout: %q", stdout)
	}
	if len(stderr) == 0 {
		t.Logf("the label route said nothing on this host (no selinuxfs?); only stdout-cleanliness is pinned here")
	}
}
