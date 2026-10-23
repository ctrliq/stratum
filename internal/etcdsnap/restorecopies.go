package etcdsnap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/version"
)

// RestoreCopy is an etcd data dir that a restore kept beside the live one: the
// data from before a restore (pre-restore), or what a failed restore wrote
// (failed-restore). Each is a full copy of etcd's data, and nothing removes it
// but RemoveRestoreCopies.
type RestoreCopy struct {
	Path string
	Kind string // "pre-restore" or "failed-restore"
	// Modified is the newest modification time anywhere in the copy, the
	// directory itself and everything under it: a change deep inside
	// (member/snap/db) does not touch the top directory's own time.
	Modified time.Time
	Bytes    int64
}

// restoreCopyKinds are the suffixes Restore gives the dirs it keeps.
var restoreCopyKinds = []string{"pre-restore", "failed-restore"}

// RestoreCopies lists the restore copies beside the etcd data dir, oldest
// first, with their sizes. Only real directories named the way Restore names
// them are listed: not the live data dir, not its lock, and not a symlink.
func RestoreCopies() ([]RestoreCopy, error) {
	return restoreCopies(version.EtcdDataDir)
}

func restoreCopies(dataDir string) ([]RestoreCopy, error) {
	named, err := restoreCopyPaths(dataDir)
	if err != nil {
		return nil, err
	}
	copies := make([]RestoreCopy, 0, len(named))
	for _, c := range named {
		c, err := StatRestoreCopy(c)
		if err != nil {
			return nil, err
		}
		copies = append(copies, c)
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].Modified.Before(copies[j].Modified) })
	return copies, nil
}

// RestoreCopyPaths lists the restore copies with only Path and Kind set, in
// path order, without walking into any of them: cheap enough to page over
// before sizing just one page.
func RestoreCopyPaths() ([]RestoreCopy, error) {
	return restoreCopyPaths(version.EtcdDataDir)
}

func restoreCopyPaths(dataDir string) ([]RestoreCopy, error) {
	parent, base := filepath.Dir(dataDir), filepath.Base(dataDir)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list restore copies in %s: %w", parent, err)
	}
	var copies []RestoreCopy
	for _, e := range entries {
		kind := restoreCopyKind(base, e.Name())
		if kind == "" || !e.IsDir() {
			continue
		}
		copies = append(copies, RestoreCopy{Path: filepath.Join(parent, e.Name()), Kind: kind})
	}
	return copies, nil // os.ReadDir returns entries sorted by name
}

// StatRestoreCopy fills in c's size and newest modification time.
func StatRestoreCopy(c RestoreCopy) (RestoreCopy, error) {
	size, newest, err := treeStat(c.Path)
	if err != nil {
		return RestoreCopy{}, fmt.Errorf("restore copy %s: %w", c.Path, err)
	}
	c.Bytes, c.Modified = size, newest
	return c, nil
}

// IsRestoreCopyPath reports whether path names a restore copy: a clean path
// beside the etcd data dir, named the way Restore names the copies it keeps.
// It says nothing about whether the copy exists.
func IsRestoreCopyPath(path string) bool {
	if path == "" || filepath.Clean(path) != path {
		return false
	}
	if filepath.Dir(path) != filepath.Dir(version.EtcdDataDir) {
		return false
	}
	return restoreCopyKind(filepath.Base(version.EtcdDataDir), filepath.Base(path)) != ""
}

// restoreCopyKind is the kind a dir named name is, or "" if it is not one.
func restoreCopyKind(base, name string) string {
	for _, kind := range restoreCopyKinds {
		if strings.HasPrefix(name, base+"."+kind+"-") {
			return kind
		}
	}
	return ""
}

// treeStat is the total size of the regular files under dir, and the newest
// modification time of dir or anything under it. A file written or added
// changes its own time and a file removed changes its directory's, so any
// change to the copy moves the newest time.
func treeStat(dir string) (total int64, newest time.Time, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if d.Type().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, newest, err
}

// RemoveRestoreCopies removes the restore copies in shown, the ones the
// operator was shown, and returns those it removed. It holds the etcd data lock,
// so it cannot race a restore that is moving the data dir aside or putting it
// back, and it removes only a shown copy that is still there unchanged (same
// path, still a directory, same newest modification time anywhere in it): a
// copy a restore created or touched after the listing is one nobody has
// verified, so it stays. On an error it stops, having removed only what it returns.
//
// It is the one place a production effects value for the removal is built: one
// per call, the data lock and the removal wired inline, and the run itself
// consults nothing else.
func RemoveRestoreCopies(shown []RestoreCopy) ([]RestoreCopy, error) {
	fx := &restoreEffects{
		etcdDataDir:      version.EtcdDataDir,
		restoreRemoveAll: os.RemoveAll,
	}
	fx.restoreAcquireDataLock = func() (func(), error) { return etcddata.Lock(fx.etcdDataDir) }
	fx.restoreCopies = func() ([]RestoreCopy, error) { return restoreCopies(fx.etcdDataDir) }
	return removeRestoreCopies(shown, fx)
}

// removeRestoreCopies is the removal sequence RemoveRestoreCopies hands its
// effects value to. An effect that is not set stops the run with the
// unset-effect sentinel before its host call is made, so a zero value removes
// nothing.
func removeRestoreCopies(shown []RestoreCopy, fx *restoreEffects) ([]RestoreCopy, error) {
	release, err := fx.acquireDataLock()
	if err != nil {
		return nil, err
	}
	defer release()
	if fx.restoreRemoveAll == nil {
		return nil, unsetEffect("restoreRemoveAll")
	}
	if fx.restoreCopies == nil {
		return nil, unsetEffect("restoreCopies")
	}
	current, err := fx.restoreCopies()
	if err != nil {
		return nil, err
	}
	unchanged := map[string]time.Time{}
	for _, c := range current {
		unchanged[c.Path] = c.Modified
	}
	var removed []RestoreCopy
	for _, c := range shown {
		if mod, ok := unchanged[c.Path]; !ok || !mod.Equal(c.Modified) {
			continue
		}
		if err := fx.removeAll(c.Path); err != nil {
			return removed, fmt.Errorf("remove restore copy %s: %w", c.Path, err)
		}
		delete(unchanged, c.Path) // a second entry for the same path finds it gone
		removed = append(removed, c)
	}
	return removed, nil
}

// HumanBytes formats n in binary units, e.g. "1.5 GiB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
