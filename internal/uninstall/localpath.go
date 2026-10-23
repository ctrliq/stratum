package uninstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"
)

// The local-path provisioner's volume directory (selinux.LocalPathDir, the
// run's localPathDir) is the one directory outside the delete targets that an
// uninstall still owns: `stratum up` creates it on SELinux hosts so it can be
// labelled, and elsewhere the provisioner creates it on the first volume. What
// it can hold is users' local-path volume data, which an uninstall must not
// delete, so it is removed only when it is empty and kept, said so, when not.

// localPathPreviewDetail is the preview's one explanation of the directory,
// shared by the CLI dry run and the MCP tool: the preview names what the run
// would decide, without reading the node.
const localPathPreviewDetail = "the local-path provisioner's volume directory; removed only when it is empty, and kept, with the reason, when it holds anything (it can hold volume data), is a mount point, or is not a plain directory"

// The reasons the run gives for keeping the directory, one copy each.
const (
	localPathKeptReason   = "the local-path provisioner keeps its volume data here, which uninstall does not delete"
	localPathMountReason  = "it is a mount point, so uninstall leaves it and what is mounted there alone"
	localPathNotDirReason = "it is a symlink or not a plain directory, so uninstall does not touch it"
)

// removeLocalPathDir removes the local-path provisioner's volume directory
// when it is an empty plain directory, and otherwise keeps it and says why
// through w. It never follows a symlink, never touches a mount point, and
// removes only with removeEmptyDir, rmdir, which refuses a directory that is
// not empty and anything that is not a directory: what arrives or replaces
// the directory after the check is kept, never deleted. A missing directory
// is silent, as is a run whose layout names none. A directory that cannot be
// read is not proven empty, so it is kept too. A removal that fails for any
// other reason is returned, with the path, a failure like any other
// target's.
func removeLocalPathDir(w io.Writer, fx *hostEffects) error {
	dir := fx.localPathDir
	if dir == "" {
		return nil
	}
	kept := func(reason string) error {
		fmt.Fprintf(w, "  left %s in place: %s\n", dir, reason)
		return nil
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return kept(fmt.Sprintf("it could not be inspected (%v), so it cannot be shown to be empty", err))
	case !fi.Mode().IsDir():
		return kept(localPathNotDirReason)
	}
	mounts, err := mountPoints(fx)
	if err != nil {
		return kept(fmt.Sprintf("whether it is a mount point could not be told (%v)", err))
	}
	if slices.Contains(mounts, dir) {
		return kept(localPathMountReason)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return kept(fmt.Sprintf("it could not be read (%v), so it cannot be shown to be empty", err))
	}
	if len(entries) > 0 {
		return kept(localPathKeptReason)
	}
	if fx.removeEmptyDir == nil {
		return unsetEffect("removeEmptyDir")
	}
	if err := fx.removeEmptyDir(dir); err != nil {
		switch {
		case errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST):
			return kept(localPathKeptReason)
		case errors.Is(err, syscall.ENOTDIR):
			return kept(localPathNotDirReason)
		}
		return &os.PathError{Op: "rmdir", Path: dir, Err: err}
	}
	return nil
}
