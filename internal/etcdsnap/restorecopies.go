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

// A full etcd data-dir copy a restore kept aside; only RemoveRestoreCopies removes one.
type RestoreCopy struct {
	Path string
	Kind string // "pre-restore" or "failed-restore"
	// Modified is the newest mtime in the copy; a deep change does not touch the top dir's own.
	Modified time.Time
	Bytes    int64
}

var restoreCopyKinds = []string{"pre-restore", "failed-restore"}

// RestoreCopies lists the restore copies beside the etcd data dir, oldest first, with sizes.
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

// RestoreCopyPaths lists copies with only Path and Kind set, without walking into any of them.
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
	return copies, nil
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

// IsRestoreCopyPath says whether path names a restore copy, not whether it exists.
func IsRestoreCopyPath(path string) bool {
	if path == "" || filepath.Clean(path) != path {
		return false
	}
	if filepath.Dir(path) != filepath.Dir(version.EtcdDataDir) {
		return false
	}
	return restoreCopyKind(filepath.Base(version.EtcdDataDir), filepath.Base(path)) != ""
}

func restoreCopyKind(base, name string) string {
	for _, kind := range restoreCopyKinds {
		if strings.HasPrefix(name, base+"."+kind+"-") {
			return kind
		}
	}
	return ""
}

// treeStat sums the regular files under dir; any change inside moves the newest mtime.
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

// RemoveRestoreCopies removes only a shown, still-unchanged copy, under the etcd data lock.
func RemoveRestoreCopies(shown []RestoreCopy) ([]RestoreCopy, error) {
	fx := &restoreEffects{
		etcdDataDir:      version.EtcdDataDir,
		restoreRemoveAll: os.RemoveAll,
	}
	fx.restoreAcquireDataLock = func() (func(), error) { return etcddata.Lock(fx.etcdDataDir) }
	fx.restoreCopies = func() ([]RestoreCopy, error) { return restoreCopies(fx.etcdDataDir) }
	return removeRestoreCopies(shown, fx)
}

// An effect that is not set stops the run with the unset-effect sentinel.
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
