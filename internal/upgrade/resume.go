package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/version"
)

// The interrupted run's stamp: its kept backups stay the rollback baseline.
type upgradeMarker struct {
	TS     string `json:"ts"`
	Target string `json:"target"`
}

func upgradeMarkerPath() string {
	return filepath.Join(version.DataDir, "upgrade", "in-progress.json")
}

// An unreadable marker is an error, not absent: resuming without it discards the kept backups.
func readUpgradeMarker() (*upgradeMarker, error) {
	path := upgradeMarkerPath()
	unusable := func(why string) error {
		return fmt.Errorf("the in-progress upgrade marker %s %s; inspect the *.bak-<ts> backups, then remove it by hand; nothing was changed", path, why)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the in-progress upgrade marker %s: %w; nothing was changed", path, err)
	}
	var m upgradeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, unusable(fmt.Sprintf("is not valid JSON (%v)", err))
	}
	if m.Target == "" {
		return nil, unusable("names no target")
	}
	if !validStamp(m.TS) {
		return nil, unusable(fmt.Sprintf("has an invalid backup stamp %q", m.TS))
	}
	return &m, nil
}

func (m *upgradeMarker) changedNothing(sets map[string][]backupItem) bool {
	return m != nil && len(sets[m.TS]) == 0
}

// Written atomically: a crash must not leave a torn marker.
func writeUpgradeMarker(w io.Writer, m upgradeMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(upgradeMarkerPath()), 0o700); err != nil {
		return fmt.Errorf("record the in-progress upgrade: %w", err)
	}
	if err := copyBytes(w, data, upgradeMarkerPath(), 0o600); err != nil {
		return fmt.Errorf("record the in-progress upgrade: %w", err)
	}
	return nil
}

// Best-effort: after a complete rollback the kept backups are the node's state.
func clearUpgradeMarker(w io.Writer) {
	if err := removeUpgradeMarker(); err != nil {
		fmt.Fprintf(w, "  WARN: %v\n"+
			"         the next `stratum upgrade` will treat this upgrade as unfinished; remove %s by hand\n", err, upgradeMarkerPath())
	}
}

func removeUpgradeMarker() error {
	if err := os.Remove(upgradeMarkerPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear the in-progress upgrade marker: %w", err)
	}
	return nil
}

func refuseOtherInterruptedUpgrade(m *upgradeMarker, target string) error {
	if m == nil || m.Target == target {
		return nil
	}
	return fmt.Errorf("stratum upgrade: an upgrade to %s was interrupted on this node (backups *.bak-%s); "+
		"finish it by re-running `stratum upgrade` with that upgrade binary, or undo it with `stratum rollback`, "+
		"then retry; nothing was changed", m.Target, m.TS)
}

// A path a rollback could not restore from is an error, never "kept".
func keptBackup(bak string, dir bool) (bool, error) {
	fi, err := os.Lstat(bak)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case dir && !fi.IsDir():
		return false, fmt.Errorf("%s is not a directory", bak)
	case dir:
		entries, err := os.ReadDir(bak)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if _, err := keptBackup(filepath.Join(bak, e.Name()), false); err != nil {
				return false, err
			}
		}
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("%s is not a regular file", bak)
	default:
		f, err := os.Open(bak)
		if err != nil {
			return false, err
		}
		_ = f.Close()
	}
	return true, nil
}

var nowStamp = etcdsnap.NowStamp

func startBackupSet(w io.Writer, m *upgradeMarker, target string, sets map[string][]backupItem) (ts string, backups []backupItem, hostSwapped bool, err error) {
	if m != nil {
		backups = sets[m.TS]
		for _, b := range backups {
			if _, err := keptBackup(b.bak, b.dir || b.profile); err != nil {
				return "", nil, false, fmt.Errorf("stratum upgrade: the interrupted run's %s backup is unusable: %w; nothing was changed", b.name, err)
			}
			fmt.Fprintf(w, "  keeping the interrupted run's %s backup %s\n", b.name, b.bak)
		}
		return m.TS, backups, hasHostBackup(backups), nil
	}
	// Stamps have one-second precision; a used stamp is another run's originals.
	for ts = nowStamp(); len(sets[ts]) > 0; ts = nowStamp() {
		time.Sleep(100 * time.Millisecond)
	}
	if err := writeUpgradeMarker(w, upgradeMarker{TS: ts, Target: target}); err != nil {
		return "", nil, false, fmt.Errorf("stratum upgrade: %w; nothing was changed", err)
	}
	return ts, nil, false, nil
}

func hasHostBackup(items []backupItem) bool {
	for _, it := range items {
		if !it.dir && !it.profile && it.dst != stratumInstallPath {
			return true
		}
	}
	return false
}

func addBackup(items []backupItem, it backupItem) []backupItem {
	for _, have := range items {
		if have.bak == it.bak {
			return items
		}
	}
	return append(items, it)
}
