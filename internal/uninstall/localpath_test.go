package uninstall

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/selinux"
)

// localPathRun is a worker run whose local-path volume directory is a path in
// the test's own temporary directory, never the real /opt one, and whose
// empty-only removal is syscall.Rmdir, as Run wires it. The directory is not
// created: each test sets up what the node has.
func localPathRun(t *testing.T) (string, *hostEffects) {
	t.Helper()
	_, fx := runNode(t, "worker")
	dir := filepath.Join(t.TempDir(), "local-path-provisioner")
	fx.localPathDir = dir
	fx.removeEmptyDir = syscall.Rmdir
	return dir, fx
}

// keptLine is the whole line the run prints for a kept directory.
func keptLine(dir, reason string) string {
	return "  left " + dir + " in place: " + reason
}

// runLocalPath runs the uninstall and returns its output, failing on an error.
func runLocalPath(t *testing.T, fx *hostEffects) string {
	t.Helper()
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
	if err != nil {
		t.Fatalf("runUninstall: %v\n%s", err, out)
	}
	return out
}

// hasWholeLine fails when out has no line exactly want.
func hasWholeLine(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return
		}
	}
	t.Errorf("output has no whole line %q:\n%s", want, out)
}

// hasNoLineContaining fails when out has any line containing sub.
func hasNoLineContaining(t *testing.T, out, sub string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			t.Errorf("output has an unexpected line %q", line)
		}
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// An empty directory is removed, and nothing is said about it: nothing was
// kept that needs explaining.
func TestRunRemovesEmptyLocalPathDir(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	out := runLocalPath(t, fx)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("empty dir %s was not removed: Lstat = %v", dir, err)
	}
	hasNoLineContaining(t, out, dir)
}

// A directory holding anything may hold users' local-path volume data, so it
// is kept, with what it holds, and the run says so.
func TestRunKeepsLocalPathDirWithVolumeData(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	volume := filepath.Join(dir, "pvc-123")
	if err := os.WriteFile(volume, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runLocalPath(t, fx)
	if b, err := os.ReadFile(volume); err != nil || string(b) != "data" {
		t.Errorf("volume data %s did not survive the uninstall: %q, %v", volume, b, err)
	}
	hasWholeLine(t, out, keptLine(dir, localPathKeptReason))
}

// A mount point is left alone even when it looks empty: a disk mounted there is
// the operator's, and the run never empties or removes it.
func TestRunKeepsLocalPathDirMountPoint(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	if err := os.WriteFile(fx.mountInfoPath, []byte("36 25 0:32 / "+dir+" rw,relatime - ext4 /dev/vdb rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.removeEmptyDir = func(p string) error {
		t.Errorf("removeEmptyDir(%s) on a mount point", p)
		return nil
	}
	out := runLocalPath(t, fx)
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("mount point %s did not survive the uninstall: %v", dir, err)
	}
	hasWholeLine(t, out, keptLine(dir, localPathMountReason))
}

// A symlink is never followed: the link and its target are kept as they are.
func TestRunKeepsLocalPathDirSymlink(t *testing.T) {
	dir, fx := localPathRun(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	mkdir(t, target)
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	out := runLocalPath(t, fx)
	if fi, err := os.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink %s did not survive the uninstall: Lstat = %v", dir, err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("the symlink's target %s did not survive the uninstall: %v", target, err)
	}
	hasWholeLine(t, out, keptLine(dir, localPathNotDirReason))
}

// A plain file where the directory should be is not a plain directory either,
// so it is kept as it is.
func TestRunKeepsLocalPathDirFile(t *testing.T) {
	dir, fx := localPathRun(t)
	if err := os.WriteFile(dir, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runLocalPath(t, fx)
	if b, err := os.ReadFile(dir); err != nil || string(b) != "data" {
		t.Errorf("file %s did not survive the uninstall: %q, %v", dir, b, err)
	}
	hasWholeLine(t, out, keptLine(dir, localPathNotDirReason))
}

// A directory that cannot be inspected cannot be shown to be empty, so it is
// kept, with the reason.
func TestRunKeepsLocalPathDirItCannotInspect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root inspects any path")
	}
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	parent := filepath.Dir(dir)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	_, lstatErr := os.Lstat(dir)
	if lstatErr == nil {
		t.Fatal("Lstat succeeded under a parent with no permissions")
	}
	out := runLocalPath(t, fx)
	hasWholeLine(t, out, keptLine(dir, "it could not be inspected ("+lstatErr.Error()+"), so it cannot be shown to be empty"))
}

// A directory whose mount table cannot be read may be a mount point, so it is
// kept, with the reason. The step runs on its own: the rest of the uninstall
// reads the same table and fails without it.
func TestRemoveLocalPathDirKeepsItWhenTheMountTableIsUnreadable(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	fx.mountInfoPath = filepath.Join(t.TempDir(), "no-mountinfo")
	_, readErr := os.ReadFile(fx.mountInfoPath)
	fx.removeEmptyDir = func(p string) error {
		t.Errorf("removeEmptyDir(%s) without the mount table", p)
		return nil
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = removeLocalPathDir(w, fx) })
	if err != nil {
		t.Fatalf("removeLocalPathDir = %v, want the directory kept without an error", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("dir %s did not survive the uninstall: %v", dir, err)
	}
	hasWholeLine(t, out, keptLine(dir, "whether it is a mount point could not be told (read the mount table: "+readErr.Error()+")"))
}

// A directory that cannot be read cannot be shown to be empty, so it is kept,
// with the reason.
func TestRunKeepsLocalPathDirItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, readErr := os.ReadDir(dir)
	if readErr == nil {
		t.Fatal("ReadDir succeeded on a directory with no permissions")
	}
	fx.removeEmptyDir = func(p string) error {
		t.Errorf("removeEmptyDir(%s) on a directory it could not read", p)
		return nil
	}
	out := runLocalPath(t, fx)
	hasWholeLine(t, out, keptLine(dir, "it could not be read ("+readErr.Error()+"), so it cannot be shown to be empty"))
}

// A node without the directory gets no line about it.
func TestRunIsSilentWithoutLocalPathDir(t *testing.T) {
	dir, fx := localPathRun(t) // never created
	out := runLocalPath(t, fx)
	hasNoLineContaining(t, out, dir)
}

// A run whose layout names no directory touches none.
func TestRunWithoutLocalPathLayoutRemovesNothing(t *testing.T) {
	_, fx := localPathRun(t)
	fx.localPathDir = ""
	fx.removeEmptyDir = func(p string) error {
		t.Errorf("removeEmptyDir(%q) with no layout", p)
		return nil
	}
	runLocalPath(t, fx)
}

// A volume that arrives after the emptiness check is kept, not deleted: the
// removal refuses a non-empty directory, with either error POSIX allows for
// it, and the run then keeps it and says why, without failing.
func TestRunKeepsLocalPathDirFilledAfterTheCheck(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTEMPTY, syscall.EEXIST} {
		t.Run(errno.Error(), func(t *testing.T) {
			dir, fx := localPathRun(t)
			mkdir(t, dir)
			fx.removeEmptyDir = func(p string) error {
				return &os.PathError{Op: "remove", Path: p, Err: errno}
			}
			out := runLocalPath(t, fx)
			if _, err := os.Lstat(dir); err != nil {
				t.Errorf("dir %s did not survive the uninstall: %v", dir, err)
			}
			hasWholeLine(t, out, keptLine(dir, localPathKeptReason))
		})
	}
}

// A file that replaces the directory after the checks is kept, not deleted:
// rmdir refuses anything that is not a directory, and the run keeps it and
// says why, without failing.
func TestRunKeepsAFileThatReplacesTheLocalPathDir(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	fx.removeEmptyDir = func(p string) error {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		return syscall.Rmdir(p)
	}
	out := runLocalPath(t, fx)
	if b, err := os.ReadFile(dir); err != nil || string(b) != "data" {
		t.Errorf("the file that replaced %s did not survive the uninstall: %q, %v", dir, b, err)
	}
	hasWholeLine(t, out, keptLine(dir, localPathNotDirReason))
}

// Any other removal failure is a reported failure like any other target's:
// the run names the directory in its error and exits nonzero.
func TestRunReportsLocalPathDirRemovalFailure(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	fx.removeEmptyDir = func(string) error { return syscall.EBUSY }
	err := runUninstall(io.Discard, true, fx)
	want := "some paths could not be removed: rmdir " + dir + ": " + syscall.EBUSY.Error()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("runUninstall = %v, want it to carry %q", err, want)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("dir %s is gone, but its removal failed: %v", dir, err)
	}
}

// The removal failure is reported even after another target already failed:
// the run prints nothing for it, so the error is where the directory is named.
func TestRunReportsLocalPathDirRemovalFailureAfterAnEarlierOne(t *testing.T) {
	dir, fx := localPathRun(t)
	mkdir(t, dir)
	earlier := errors.New("a target could not be removed")
	fx.removeAll = func(string) error { return earlier }
	fx.removeEmptyDir = func(string) error { return syscall.EBUSY }
	err := runUninstall(io.Discard, true, fx)
	for _, want := range []string{
		"some paths could not be removed: " + earlier.Error(),
		"some paths could not be removed: rmdir " + dir + ": " + syscall.EBUSY.Error(),
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("runUninstall = %v, want it to carry %q", err, want)
		}
	}
}

// The preview names the directory and the rule the run applies, without
// reading the node: the dry run prints it, and Preview() carries it for the
// MCP tool, from the one detail text.
func TestPreviewNamesTheLocalPathDirRule(t *testing.T) {
	items := previewItemsByKind(Preview().Items, "local-path-dir")
	if want := []PreviewItem{{Kind: "local-path-dir", Path: selinux.LocalPathDir, Detail: localPathPreviewDetail}}; !reflect.DeepEqual(items, want) {
		t.Errorf("Preview() local-path-dir items = %+v, want %+v", items, want)
	}
	_, fx := localPathRun(t)
	fx.removeAll = func(string) error {
		t.Error("the dry run removed a target")
		return nil
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, false, fx) })
	if err != nil {
		t.Fatal(err)
	}
	hasWholeLine(t, out, "  - "+selinux.LocalPathDir+"  ("+localPathPreviewDetail+")")
}

// The MCP schema's list of item kinds names the new one.
func TestPreviewSchemaNamesTheLocalPathDirKind(t *testing.T) {
	if f, ok := reflect.TypeOf(PreviewItem{}).FieldByName("Kind"); !ok || !strings.Contains(f.Tag.Get("jsonschema"), "local-path-dir") {
		t.Errorf("PreviewItem.Kind's jsonschema does not name local-path-dir: %q", f.Tag.Get("jsonschema"))
	}
}
