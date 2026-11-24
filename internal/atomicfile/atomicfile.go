// Package atomicfile writes one file durably: temp file, fsync, rename, dir fsync.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/selinux"
)

var ErrNotDurable = errors.New("the file was replaced, but the replacement could not be made durable")

var ErrStrayCopy = errors.New("the temporary name could not be removed, and still holds the file's contents")

// Seams are the writer's filesystem side effects, overridable per call.
type Seams struct {
	SyncFile       func(*os.File) error
	LabelLike      func(tmp, dst string) error
	RemoveTempName func(string) error
	SyncDir        func(string) error
}

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

var (
	syncFile = func(f *os.File) error { return f.Sync() }
	// Diagnostics go to stderr: this runs under protocol handlers whose stdout is not for them.
	labelLike = func(tmp, dst string) error {
		return selinux.LabelLikeWithWriter(os.Stderr, tmp, dst)
	}
	removeTempName = os.Remove
	syncDir        = func(dir string) error {
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

// A symlink at path is replaced; a caller that must keep one checks before writing, not after.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return WriteFileWith(path, data, perm, Seams{})
}

// WriteFileWith is WriteFile with the caller's seams; nil fields mean the real implementation.
func WriteFileWith(path string, data []byte, perm os.FileMode, seams Seams) error {
	return writeFile(path, data, perm, seams.withDefaults())
}

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

// The mode is set before the fsync: a mode changed after it is not covered by the sync.
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

// Links (a rename would replace a key); FileInfo survives failed syncs, naming the inode to remove.
func CreateExclusive(path string, data []byte, perm os.FileMode) (os.FileInfo, error) {
	return CreateExclusiveWith(path, data, perm, Seams{})
}

// CreateExclusiveWith is CreateExclusive with the caller's seams.
func CreateExclusiveWith(path string, data []byte, perm os.FileMode, seams Seams) (os.FileInfo, error) {
	return createExclusive(path, data, perm, seams.withDefaults())
}

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
	// One sync makes both the new entry and the temp name's removal durable.
	if err := seams.RemoveTempName(tmp.Name()); err != nil {
		return created, fmt.Errorf("%s: %w (%v)", tmp.Name(), ErrStrayCopy, err)
	}
	return created, seams.SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs dir, so an entry change in it survives a power loss.
func SyncDir(dir string) error { return syncDir(dir) }
