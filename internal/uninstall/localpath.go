package uninstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"
)

// the one directory outside the targets the uninstall owns; stratum up or the provisioner makes it.

// one explanation shared by the CLI dry run and the MCP tool.
const localPathPreviewDetail = "the local-path provisioner's volume directory; removed only when it is empty, and kept, with the reason, when it holds anything (it can hold volume data), is a mount point, or is not a plain directory"

const (
	localPathKeptReason   = "the local-path provisioner keeps its volume data here, which uninstall does not delete"
	localPathMountReason  = "it is a mount point, so uninstall leaves it and what is mounted there alone"
	localPathNotDirReason = "it is a symlink or not a plain directory, so uninstall does not touch it"
)

// rmdir refuses a non-empty or non-directory path, so what arrives or replaces it is kept.
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
