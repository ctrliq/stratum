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

// upgradeMarker records an upgrade that has started changing this node and
// not yet finished. It exists so a re-run resumes the SAME upgrade
// rather than starting a new one.
//
// Every artifact is backed up immediately before it is changed, under the
// run's stamp. A re-run that took a fresh stamp therefore backed up whatever
// the interrupted run had already changed — the new stratum binary, the
// re-rendered manifests — and a resume that then failed "rolled back" onto the
// new version and said it had not. Reusing the interrupted run's stamp, and
// keeping any backup already taken under it, makes the baseline the node as it
// was before the first run touched it: a backup that exists is the original,
// and an artifact the upgrade backs up but has no backup for has not been
// changed yet.
//
// Two things were never covered and still are not: a host binary the payload
// adds, which had nothing to back up, stays installed after a rollback; and a
// resume, finding no backup of such a binary, backs up the new file it finds
// installed as that binary's "original" — the one case where a kept backup
// cannot exist. Images and reissued
// certificates are not backed up at all (both are kept deliberately; see
// UpgradeNode).
//
// The marker covers only the part of the upgrade a rollback can undo. It is
// cleared before the host-process configs and argv are rewritten, which no
// rollback undoes, so a run killed there is not "resumed" into one.
type upgradeMarker struct {
	TS     string `json:"ts"`
	Target string `json:"target"`
}

func upgradeMarkerPath() string {
	return filepath.Join(version.DataDir, "upgrade", "in-progress.json")
}

// readUpgradeMarker returns nil when no upgrade is in progress. A marker that
// cannot be read is an error, not "none": treating it as absent would start a
// fresh baseline over the very backups it exists to protect. Its stamp is
// joined into paths that are removed and restored from, so anything but a
// stamp NowStamp could have written is refused.
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

// changedNothing reports an upgrade that recorded itself and was stopped
// before its first backup: nothing under its stamp, so nothing it could have
// changed. Its marker is then only in the way — of a different upgrade, and of
// a rollback that would otherwise restore the upgrade BEFORE it.
func (m *upgradeMarker) changedNothing(sets map[string][]backupItem) bool {
	return m != nil && len(sets[m.TS]) == 0
}

// writeUpgradeMarker records m before the upgrade changes anything, through a
// rename so a crash cannot leave a torn marker.
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

// clearUpgradeMarker ends the upgrade: it finished, or the node is back on its
// original version. Anything else must leave the marker, so the next run keeps
// the original baseline.
//
// Best-effort, for the places a leftover marker is harmless: after a complete
// rollback its backups ARE the node's state, so a resume from them is right.
// Where it is not harmless, use removeUpgradeMarker and stop on its error.
func clearUpgradeMarker(w io.Writer) {
	if err := removeUpgradeMarker(); err != nil {
		fmt.Fprintf(w, "  WARN: %v\n"+
			"         the next `stratum upgrade` will treat this upgrade as unfinished; remove %s by hand\n", err, upgradeMarkerPath())
	}
}

// removeUpgradeMarker removes the marker and reports whether it is gone.
func removeUpgradeMarker() error {
	if err := os.Remove(upgradeMarkerPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear the in-progress upgrade marker: %w", err)
	}
	return nil
}

// refuseOtherInterruptedUpgrade refuses an upgrade to a different version
// while one is still in progress: its baseline is the node before THAT
// upgrade, and silently adopting it, or silently discarding it, would each be
// wrong for someone.
func refuseOtherInterruptedUpgrade(m *upgradeMarker, target string) error {
	if m == nil || m.Target == target {
		return nil
	}
	return fmt.Errorf("stratum upgrade: an upgrade to %s was interrupted on this node (backups *.bak-%s); "+
		"finish it by re-running `stratum upgrade` with that upgrade binary, or undo it with `stratum rollback`, "+
		"then retry; nothing was changed", m.Target, m.TS)
}

// keptBackup reports whether bak is a backup to keep: absent is false, and
// present must be what a backup is — a regular file, or a real directory, and
// readable. Anything else there is an error, never "kept": a rollback could
// not restore from it, or would follow a symlink out of the backup set.
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
		// And every entry in it, which a restore copies: only ever regular
		// files, so a symlink would be followed out of the set and an
		// unreadable one fail after its siblings were restored.
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

// nowStamp is etcdsnap.NowStamp, indirected for tests.
var nowStamp = etcdsnap.NowStamp

// startBackupSet opens this run's backup set. A resume continues the
// interrupted run's set: its stamp, every backup it took, and whether it had
// swapped host binaries. A fresh upgrade takes a new stamp and records itself
// before its first change.
func startBackupSet(w io.Writer, m *upgradeMarker, target string, sets map[string][]backupItem) (ts string, backups []backupItem, hostSwapped bool, err error) {
	if m != nil {
		backups = sets[m.TS]
		// Every kept backup is checked before this run changes anything: one
		// the rollback could not read would turn a refusal now into an
		// INCOMPLETE rollback later.
		for _, b := range backups {
			if _, err := keptBackup(b.bak, b.dir || b.profile); err != nil {
				return "", nil, false, fmt.Errorf("stratum upgrade: the interrupted run's %s backup is unusable: %w; nothing was changed", b.name, err)
			}
			fmt.Fprintf(w, "  keeping the interrupted run's %s backup %s\n", b.name, b.bak)
		}
		return m.TS, backups, hasHostBackup(backups), nil
	}
	// A stamp with backups under it already is another run's: taking it would
	// make that run's backups this one's "originals" — they are kept, not
	// retaken — so wait for the next second.
	for ts = nowStamp(); len(sets[ts]) > 0; ts = nowStamp() {
		time.Sleep(100 * time.Millisecond)
	}
	if err := writeUpgradeMarker(w, upgradeMarker{TS: ts, Target: target}); err != nil {
		return "", nil, false, fmt.Errorf("stratum upgrade: %w; nothing was changed", err)
	}
	return ts, nil, false, nil
}

// hasHostBackup reports whether items include a host binary, so a resumed run
// that fails before its own host swap still bounces the host processes the
// interrupted run swapped.
func hasHostBackup(items []backupItem) bool {
	for _, it := range items {
		if !it.dir && !it.profile && it.dst != stratumInstallPath {
			return true
		}
	}
	return false
}

// addBackup appends it unless a resumed run already seeded it.
func addBackup(items []backupItem, it backupItem) []backupItem {
	for _, have := range items {
		if have.bak == it.bak {
			return items
		}
	}
	return append(items, it)
}
