// Package atomicfile is stratum's one durable single-file writer: a file is
// written to a unique hidden temp file in its own directory, labelled for its
// destination, fsynced at its final mode, renamed into place, and the
// directory is fsynced so the rename survives a power loss. WriteFile replaces
// whatever is at path; CreateExclusive refuses to. Both report how a failure
// left the filesystem through ErrNotDurable and ErrStrayCopy.
//
// The package is a leaf: it may import internal/selinux, and nothing that
// imports internal/host.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/selinux"
)

// ErrNotDurable matches a WriteFile whose rename already put the new file at
// path, but whose directory sync failed: the new file is in place, and a power
// loss could still bring back the old one.
var ErrNotDurable = errors.New("the file was replaced, but the replacement could not be made durable")

// ErrStrayCopy matches a write whose temp name could not be removed. After
// CreateExclusive's link, it is a second name for the created file; before the
// link, or before WriteFile's rename, it is a file holding all or part of the
// contents.
var ErrStrayCopy = errors.New("the temporary name could not be removed, and still holds the file's contents")

// Seams are the writer's filesystem side effects, overridable per call. A
// package that delegates here and keeps its own test seams for these effects
// hands them in with WriteFileWith and CreateExclusiveWith, so its tests keep
// pinning the same behavior through the delegation. Every nil field means the
// real implementation for that step.
type Seams struct {
	// SyncFile is (*os.File).Sync, the file fsync before the rename.
	SyncFile func(*os.File) error
	// LabelLike gives the temp file the SELinux label of its destination.
	LabelLike func(tmp, dst string) error
	// RemoveTempName removes the temp file's name.
	RemoveTempName func(string) error
	// SyncDir fsyncs a directory.
	SyncDir func(string) error
}

// withDefaults fills the nil fields with the writer's seams, which it reads at
// call time.
func (s Seams) withDefaults() Seams {
	if s.SyncFile == nil {
		s.SyncFile = syncFile
	}
	if s.LabelLike == nil {
		s.LabelLike = labelLike
	}
	if s.RemoveTempName == nil {
		s.RemoveTempName = removeTempName
	}
	if s.SyncDir == nil {
		s.SyncDir = syncDir
	}
	return s
}

// The writer's seams, vars so this package's tests can see and fail each step.
var (
	// syncFile is (*os.File).Sync behind a seam, so a test can see what the
	// file looks like at the moment it is made durable.
	syncFile = func(f *os.File) error { return f.Sync() }
	// labelLike gives the temp file the SELinux label of its destination, with
	// its warnings on stderr: this writer runs under protocol handlers (the
	// MCP server's stdio among them) whose stdout is not for diagnostics, and
	// callers with their own report channel hand their own LabelLike seam.
	labelLike = func(tmp, dst string) error {
		return selinux.LabelLikeWithWriter(os.Stderr, tmp, dst)
	}
	// removeTempName is os.Remove behind a seam, so a test can fail it.
	removeTempName = os.Remove
	// syncDir fsyncs a directory behind a seam, so a test can see it is
	// called, and with which directory.
	syncDir = func(dir string) error {
		d, err := os.Open(dir)
		if err != nil {
			return fmt.Errorf("open %s to sync it: %w", dir, err)
		}
		if err := d.Sync(); err != nil {
			d.Close()
			return fmt.Errorf("sync %s: %w", dir, err)
		}
		return d.Close()
	}
)

// WriteFile writes data to path by creating a temp file in the SAME directory,
// fsyncing it, then renaming it into place. The temp file is created 0600 and
// only chmod'ed to perm before the rename, so it is never readable at a wider
// mode than path will have, even mid-write. A same-directory rename is atomic
// on the same filesystem: whatever fails along the way — the write, the fsync,
// a crash mid-write — the file already at path is untouched until the rename
// itself, so a failed write can never leave a corrupt or truncated file in
// place of a working one. A symlink at path is replaced, not written
// through: the rename puts a regular file at path and leaves the link's target
// untouched, so a caller that must keep something at path special (a masked
// unit is a symlink to /dev/null) checks for it before writing, never after —
// the rename would silently undo it.
//
// The files written here are labelled for their destination path while still
// under the temp name: a file renamed over its destination otherwise keeps the
// label of the temp name, and a label that cannot be set stops the write
// before the rename, so a file never reaches path with the wrong label.
//
// The rename is then made durable by fsyncing the directory: a rename lives in
// the directory's entry, and until the directory is synced a power loss can
// undo it after this function has returned success, bringing back the old file
// that the caller has already acted on replacing.
//
// A failure of that sync is returned matching ErrNotDurable: the new file is
// already at path. A failure before the rename whose temp file cannot be
// removed is returned matching ErrStrayCopy, naming the temp file.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return WriteFileWith(path, data, perm, Seams{})
}

// WriteFileWith is WriteFile with the caller's seams for the writer's
// filesystem side effects; nil fields mean the real implementation for that
// step. It exists for a delegating package whose own tests hold seams for
// these effects and keep pinning them through the delegation.
func WriteFileWith(path string, data []byte, perm os.FileMode, seams Seams) error {
	return writeFile(path, data, perm, seams.withDefaults())
}

// writeFile is WriteFile's body, with the seams it uses made explicit.
func writeFile(path string, data []byte, perm os.FileMode, seams Seams) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if renamed {
			return
		}
		if rmErr := seams.RemoveTempName(tmp.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = fmt.Errorf("%w; %s: %w (%v)", err, tmp.Name(), ErrStrayCopy, rmErr)
		}
	}()
	if err := seams.LabelLike(tmp.Name(), path); err != nil {
		tmp.Close()
		return err
	}
	if err := writeSynced(tmp, data, perm, seams.SyncFile); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	renamed = true
	if err := seams.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %w", ErrNotDurable, err)
	}
	return nil
}

// writeSynced writes data to tmp, sets perm, fsyncs and closes it. The mode is
// set before the fsync: a mode changed after it is not covered by it, so a
// crash could leave a "successful" write at the temp file's 0600 instead of
// perm.
func writeSynced(tmp *os.File, data []byte, perm os.FileMode, syncFile func(*os.File) error) error {
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := syncFile(tmp); err != nil {
		tmp.Close()
		return err
	}
	return tmp.Close()
}

// CreateExclusive is WriteFile for a file that must never replace one already
// at path. The at-rest key is minted on an observation, made earlier, that no
// key exists; a rename would silently replace a key that appeared since -- one
// an operator put there, whose Secrets would then be unreadable. A hard link
// of the finished temp file is create-only: it fails with an error matching
// fs.ErrExist instead. The temp name is removed on every path but one, a
// removal that fails after the link (ErrStrayCopy, below), and once it is gone
// the directory is synced, so the new entry is durable before this returns.
//
// It returns the created file's FileInfo once the link exists -- even when the
// directory sync then fails -- so a caller that has to take the file back off
// can remove exactly the inode this call created, and nothing that replaced
// it. After the link the temp name is a second name for that file, so if it
// cannot be removed the error matches ErrStrayCopy and names it: taking path
// back off would not remove what the file holds.
func CreateExclusive(path string, data []byte, perm os.FileMode) (os.FileInfo, error) {
	return CreateExclusiveWith(path, data, perm, Seams{})
}

// CreateExclusiveWith is CreateExclusive with the caller's seams, matching
// WriteFileWith.
func CreateExclusiveWith(path string, data []byte, perm os.FileMode, seams Seams) (os.FileInfo, error) {
	return createExclusive(path, data, perm, seams.withDefaults())
}

// createExclusive is CreateExclusive's body, with the seams it uses made
// explicit.
func createExclusive(path string, data []byte, perm os.FileMode, seams Seams) (_ os.FileInfo, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, err
	}
	linked := false
	defer func() {
		if linked {
			return
		}
		if rmErr := seams.RemoveTempName(tmp.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = fmt.Errorf("%w; %s: %w (%v)", err, tmp.Name(), ErrStrayCopy, rmErr)
		}
	}()
	if err := writeSynced(tmp, data, perm, seams.SyncFile); err != nil {
		return nil, err
	}
	created, err := os.Stat(tmp.Name())
	if err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return nil, err
	}
	linked = true
	// Drop the temp name before the directory sync, so the one sync makes both
	// the new entry and the temp name's removal durable: synced first, a power
	// loss could bring the temp name back as a second, stray copy of a secret.
	if err := seams.RemoveTempName(tmp.Name()); err != nil {
		return created, fmt.Errorf("%s: %w (%v)", tmp.Name(), ErrStrayCopy, err)
	}
	return created, seams.SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs dir, so an entry added to or removed from it survives a power
// loss.
func SyncDir(dir string) error { return syncDir(dir) }
