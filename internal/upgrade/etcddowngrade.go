package upgrade

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/k8sver"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// The etcd step of a rollback, indirected for tests.
var (
	rollbackEtcdTarget       = backedUpEtcdMinor
	rollbackEtcdStorage      = ha.EtcdStorageAt
	rollbackEtcdOffline      = offlineEtcdStorageVersion
	rollbackStopEtcd         = func() error { return staticpod.Stop("etcd", 90*time.Second) }
	rollbackReadEtcdManifest = func() ([]byte, error) { return staticpod.Read("etcd") }
	rollbackPutEtcdManifest  = func(b []byte) error { return staticpod.Put("etcd", b) }
	rollbackEtcdDowngrade    = ha.DowngradeEtcd
	etcdDowngradeWindow      = 2 * time.Minute
	etcdDowngradePollEvery   = 2 * time.Second
	etcdDowngradeSleep       = time.Sleep
)

// Kept while etcd is stopped, so a failed rollback leaves a copy to start etcd from.
func stoppedEtcdManifest() string {
	return filepath.Join(version.DataDir, "upgrade", "etcd-manifest-stopped.yaml")
}

func stoppedEtcdNote() string {
	if _, err := os.Stat(stoppedEtcdManifest()); err != nil || etcdHasManifest() {
		return ""
	}
	return fmt.Sprintf("; etcd was stopped for this rollback and has no manifest of its own: run `stratum rollback` "+
		"again, or copy %s to %s to start it as it was", stoppedEtcdManifest(), staticpod.Path("etcd"))
}

// The kept manifest is the newer etcd's; it must not outlive the restored one.
func settleStoppedEtcdManifest() error {
	if !etcdHasManifest() {
		return nil
	}
	if err := os.Remove(stoppedEtcdManifest()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not remove %s, the newer etcd's manifest kept while etcd was stopped (%v); delete it, "+
			"since etcd runs from its restored manifest now", stoppedEtcdManifest(), err)
	}
	return nil
}

func etcdHasManifest() bool {
	_, err := os.Stat(staticpod.Path("etcd"))
	return err == nil
}

const localEtcd = "127.0.0.1"

// A newer etcd migrates storage at once; its own downgrade keeps every write, a single member only.
func prepareEtcdForRollback(w io.Writer, manifestBak string, dryRun bool) error {
	if manifestBak == "" {
		return nil
	}
	target, err := rollbackEtcdTarget(manifestBak)
	if err != nil {
		return fmt.Errorf("read the etcd version this rollback restores: %w; nothing was changed", err)
	}
	if target == "" {
		return nil
	}
	cur, err := rollbackEtcdStorage(localEtcd)
	switch {
	case errors.Is(err, ha.ErrEtcdNoStatus):
		return checkStoppedEtcd(w, err, target, dryRun)
	case err != nil:
		return fmt.Errorf("this node's etcd could not be read (%w), so it is not known whether etcd %s, which this "+
			"rollback restores, can open its data; nothing was changed. Run `stratum rollback` again once "+
			"`stratum etcdctl endpoint status` answers", err, target)
	}
	above, err := minorAbove(cur.StorageVersion, target)
	if err != nil {
		return fmt.Errorf("this node's etcd reports storage version %q: %w; nothing was changed", cur.StorageVersion, err)
	}
	if !above {
		return nil
	}
	if cur.Members != 1 {
		return fmt.Errorf("this node's etcd is one of %d members whose data is stored at version %s, which etcd %s, "+
			"the version this rollback restores, cannot open; nothing was changed. Rolling back one control plane "+
			"cannot downgrade the cluster's etcd: see \"Rolling back across an etcd minor version\" in the upgrade "+
			"documentation", cur.Members, cur.StorageVersion, target)
	}
	if dryRun {
		fmt.Fprintf(w, "would downgrade etcd's storage from %s to %s, which the restored etcd can open\n", cur.StorageVersion, target)
		return nil
	}
	if cur.Downgrading {
		same, err := sameMinor(cur.DowngradeTarget, target)
		if err != nil || !same {
			return fmt.Errorf("etcd already has a downgrade to %q in progress, not to %s, the version this rollback "+
				"restores; nothing was changed. Run `stratum etcdctl downgrade cancel`, then `stratum rollback` again",
				cur.DowngradeTarget, target)
		}
		k8sutil.Step(w, fmt.Sprintf("Finish etcd's downgrade to %s, already in progress", target))
	} else {
		k8sutil.Step(w, fmt.Sprintf("Downgrade etcd's storage from %s to %s, which the restored etcd can open", cur.StorageVersion, target))
	}
	if err := downgradeUnlessStarted(cur.Downgrading, target); err != nil {
		if errors.Is(err, ha.ErrDowngradeNotValidated) {
			return fmt.Errorf("%w; nothing was changed", err)
		}
		after, rerr := rollbackEtcdStorage(localEtcd)
		switch {
		case rerr != nil:
			return fmt.Errorf("%w, and etcd's state could not be read afterwards (%v), so a downgrade may have "+
				"started; nothing else was changed. If `stratum etcdctl endpoint status -w table` shows a downgrade "+
				"enabled, run `stratum rollback` again to finish it, or `stratum etcdctl downgrade cancel` to stay on "+
				"the new version", err, rerr)
		case !after.Downgrading:
			return fmt.Errorf("%w; nothing was changed", err)
		}
		if same, serr := sameMinor(after.DowngradeTarget, target); serr != nil || !same {
			return fmt.Errorf("%w, and etcd has a downgrade to %q in progress, not to %s, the version this rollback "+
				"restores; nothing was changed. Run `stratum etcdctl downgrade cancel`, then `stratum rollback` again",
				err, after.DowngradeTarget, target)
		}
	}
	fmt.Fprintf(w, "  waiting up to %v for etcd to move its storage to %s\n", etcdDowngradeWindow, target)
	for deadline := time.Now().Add(etcdDowngradeWindow); ; {
		cur, err = rollbackEtcdStorage(localEtcd)
		if err == nil {
			if above, err = minorAbove(cur.StorageVersion, target); err == nil && !above {
				fmt.Fprintf(w, "  etcd's storage is now at %s\n", cur.StorageVersion)
				return nil
			}
		}
		if time.Now().After(deadline) {
			state := fmt.Sprintf("its storage is still at %s", cur.StorageVersion)
			if err != nil {
				state = fmt.Sprintf("its storage version could not be read (%v)", err)
			}
			return fmt.Errorf("etcd accepted the downgrade to %s, but after %v %s; nothing else was changed. "+
				"Run `stratum rollback` again to finish it, or `stratum etcdctl downgrade cancel` to stay on the "+
				"new version", target, etcdDowngradeWindow, state)
		}
		etcdDowngradeSleep(etcdDowngradePollEvery)
	}
}

// etcd refuses a second downgrade; a rerun after a lost reply only needs to wait.
func downgradeUnlessStarted(started bool, target string) error {
	if started {
		return nil
	}
	return rollbackEtcdDowngrade(localEtcd, target)
}

func sameMinor(a, b string) (bool, error) {
	aMaj, aMin, err := parseMinor(a)
	if err != nil {
		return false, err
	}
	bMaj, bMin, err := parseMinor(b)
	if err != nil {
		return false, err
	}
	return aMaj == bMaj && aMin == bMin, nil
}

// Nothing may start the newer etcd on this data before the restore, or it migrates the data.
func checkStoppedEtcd(w io.Writer, liveErr error, target string, dryRun bool) error {
	verdict := func(stored string, offErr error) (error, string) {
		if offErr != nil {
			return fmt.Errorf("this node's etcd does not answer (%v), and its data could not be read either (%w), so "+
					"it is not known whether etcd %s, which this rollback restores, can open it", liveErr, offErr, target),
				"Run `stratum rollback` again once etcd answers"
		}
		if stored == "" {
			return nil, ""
		}
		above, err := minorAbove(stored, target)
		if err != nil {
			return fmt.Errorf("this node's etcd data reports storage version %q: %w", stored, err), ""
		}
		if above {
			return fmt.Errorf("this node's etcd does not answer (%v), and its data is stored at version %s, which "+
					"etcd %s, the version this rollback restores, cannot open", liveErr, stored, target),
				"Only a running etcd can downgrade its data: see \"Rolling back across an etcd minor version\" in the " +
					"upgrade documentation"
		}
		return nil, ""
	}
	refuse := func(reason error, status, advice string) error {
		if advice == "" {
			return fmt.Errorf("%w; %s", reason, status)
		}
		return fmt.Errorf("%w; %s. %s", reason, status, advice)
	}
	if dryRun {
		stored, offErr := rollbackEtcdOffline()
		if errors.Is(offErr, berrors.ErrTimeout) {
			fmt.Fprintf(w, "would stop etcd, which does not answer (%v) but holds its data, and read its storage version "+
				"from disk\n", liveErr)
			return nil
		}
		if reason, advice := verdict(stored, offErr); reason != nil {
			return refuse(reason, "nothing was changed", advice)
		}
		fmt.Fprintf(w, "would stop etcd, which does not answer (%v), and keep it stopped for the restore\n", liveErr)
		return nil
	}

	manifest, err := rollbackReadEtcdManifest()
	if err != nil {
		return fmt.Errorf("this node's etcd does not answer (%v), and its manifest could not be read (%w); nothing "+
			"was changed", liveErr, err)
	}
	if manifest == nil {
		if kept, err := os.ReadFile(stoppedEtcdManifest()); err == nil {
			manifest = kept
		}
	}
	if manifest != nil {
		if err := os.MkdirAll(filepath.Dir(stoppedEtcdManifest()), 0o755); err != nil {
			return fmt.Errorf("keep etcd's manifest before stopping it: %w; nothing was changed", err)
		}
		if err := atomicfile.WriteFile(stoppedEtcdManifest(), manifest, 0o600); err != nil {
			return fmt.Errorf("keep etcd's manifest before stopping it: %w; nothing was changed", err)
		}
	}
	putBack := func(reason error, advice string) error {
		if manifest == nil {
			return refuse(reason, "etcd has no manifest, so it stays stopped", advice)
		}
		if err := rollbackPutEtcdManifest(manifest); err != nil {
			return refuse(reason, fmt.Sprintf("etcd is stopped, and putting its manifest back failed (%v): copy %s "+
				"to %s to start it as it was", err, stoppedEtcdManifest(), staticpod.Path("etcd")), advice)
		}
		_ = os.Remove(stoppedEtcdManifest())
		return refuse(reason, "etcd's manifest was put back, and nothing else was changed", advice)
	}

	k8sutil.Step(w, fmt.Sprintf("Stop etcd, which does not answer (%v), to read its storage version from disk", liveErr))
	if err := rollbackStopEtcd(); err != nil {
		return putBack(fmt.Errorf("this node's etcd does not answer (%v), and stopping it to read its data failed "+
			"(%w)", liveErr, err), "")
	}
	stored, offErr := rollbackEtcdOffline()
	if reason, advice := verdict(stored, offErr); reason != nil {
		return putBack(reason, advice)
	}
	shown := stored
	if shown == "" {
		shown = "no version recorded"
	}
	fmt.Fprintf(w, "  etcd's data is stored at %s, which etcd %s opens; etcd stays stopped until the restore puts its "+
		"manifest back\n", shown, target)
	return nil
}

func backedUpEtcdMinor(manifestBak string) (string, error) {
	path := filepath.Join(manifestBak, "etcd.yaml")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(b, &pod); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != "etcd" {
			continue
		}
		major, minor, err := imageMinor(c.Image)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		return fmt.Sprintf("%d.%d", major, minor), nil
	}
	return "", fmt.Errorf("%s has no etcd container", path)
}

// The tag is <major>.<minor>.<patch>-<build>.
func imageMinor(image string) (int, int, error) {
	ref, _, _ := strings.Cut(image, "@")
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return 0, 0, fmt.Errorf("etcd image %q has no tag", image)
	}
	major, minor, err := parseMinor(ref[i+1:])
	if err != nil {
		return 0, 0, fmt.Errorf("etcd image %q: %w", image, err)
	}
	return major, minor, nil
}

func minorAbove(version, target string) (bool, error) {
	vMaj, vMin, err := parseMinor(version)
	if err != nil {
		return false, err
	}
	tMaj, tMin, err := parseMinor(target)
	if err != nil {
		return false, err
	}
	return vMaj > tMaj || (vMaj == tMaj && vMin > tMin), nil
}

func parseMinor(v string) (int, int, error) {
	p, ok := k8sver.Parse(v)
	if !ok {
		return 0, 0, fmt.Errorf("version %q is not major.minor", v)
	}
	return p.Major, p.Minor, nil
}

// A held lock means etcd is running; bbolt panics on a corrupt page, so both are errors.
func offlineEtcdStorageVersion() (stored string, err error) {
	path := filepath.Join(version.EtcdDataDir, "member", "snap", "db")
	defer func() {
		if r := recover(); r != nil {
			stored, err = "", fmt.Errorf("read %s: the database is corrupt: %v", path, r)
		}
	}()
	db, err := bolt.Open(path, 0o400, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	err = db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte("meta")); b != nil {
			stored = string(b.Get([]byte("storageVersion")))
		}
		return nil
	})
	return stored, err
}
