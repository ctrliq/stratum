package upgrade

import (
	"io"

	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// etcdManifest is an etcd static pod running image, as the render writes it.
func etcdManifest(image string) string {
	return "apiVersion: v1\nkind: Pod\nmetadata:\n  name: etcd\n  namespace: kube-system\nspec:\n  containers:\n" +
		"  - name: etcd\n    image: " + image + "\n    command: [etcd]\n"
}

const (
	oldEtcdImage = "ghcr.io/ctrliq/stratum/etcd:3.6.8-0"
	newEtcdImage = "ghcr.io/ctrliq/stratum/etcd:3.7.0-0"
)

func TestImageMinor(t *testing.T) {
	for _, c := range []struct {
		image        string
		major, minor int
		bad          bool
	}{
		{image: "ghcr.io/ctrliq/stratum/etcd:3.6.8-0", major: 3, minor: 6},
		{image: "registry.k8s.io/etcd:3.7.0-0@sha256:6ecefbe2510c4a30573a62a4d6dd175acf881ca67003fcd91849a16df7a724d5", major: 3, minor: 7},
		{image: "localhost:5000/etcd:v3.10.1", major: 3, minor: 10},
		{image: "localhost:5000/etcd", bad: true},
		{image: "ghcr.io/ctrliq/stratum/etcd:unknown", bad: true},
	} {
		major, minor, err := imageMinor(c.image)
		if c.bad {
			if err == nil {
				t.Errorf("imageMinor(%q) = %d.%d, want an error", c.image, major, minor)
			}
			continue
		}
		if err != nil || major != c.major || minor != c.minor {
			t.Errorf("imageMinor(%q) = %d.%d, %v; want %d.%d", c.image, major, minor, err, c.major, c.minor)
		}
	}
}

func TestMinorAbove(t *testing.T) {
	for _, c := range []struct {
		version, target string
		want            bool
	}{
		{"3.7.0", "3.6", true},
		{"3.6.0", "3.6", false},
		{"3.5.0", "3.6", false},
		{"3.10.0", "3.9", true},
		{"4.0.0", "3.9", true},
	} {
		if got, err := minorAbove(c.version, c.target); err != nil || got != c.want {
			t.Errorf("minorAbove(%q, %q) = %v, %v; want %v", c.version, c.target, got, err, c.want)
		}
	}
	if _, err := minorAbove("", "3.6"); err == nil {
		t.Error("an empty version compared without error")
	}
}

func TestBackedUpEtcdMinor(t *testing.T) {
	dir := t.TempDir()
	if got, err := backedUpEtcdMinor(dir); got != "" || err != nil {
		t.Errorf("no etcd manifest: %q, %v; want nothing to check", got, err)
	}
	writeFile(t, filepath.Join(dir, "etcd.yaml"), etcdManifest(oldEtcdImage), 0o600)
	if got, err := backedUpEtcdMinor(dir); got != "3.6" || err != nil {
		t.Errorf("got %q, %v; want 3.6", got, err)
	}
	writeFile(t, filepath.Join(dir, "etcd.yaml"), "kind: Pod\nspec:\n  containers:\n  - name: other\n    image: x:1.2\n", 0o600)
	if _, err := backedUpEtcdMinor(dir); err == nil {
		t.Error("a manifest with no etcd container read without error")
	}
}

// etcdDowngradeFixture stands in for the local etcd: storage is what each
// read returns, in turn (the last repeats), and the downgrade calls are
// recorded.
type etcdDowngradeFixture struct {
	storage   []ha.EtcdStorage
	reads     int
	downgrade []string
	offline   string
	offErr    error
	liveErr   error
	stopped   int
	putBack   [][]byte
	stopErr   error
}

func newEtcdDowngradeFixture(t *testing.T, storage ...ha.EtcdStorage) *etcdDowngradeFixture {
	t.Helper()
	f := &etcdDowngradeFixture{storage: storage}
	redirect(t, &version.DataDir, t.TempDir())
	swap(t, &rollbackEtcdStorage, func(ip string) (ha.EtcdStorage, error) {
		if ip != localEtcd {
			t.Errorf("read etcd at %s, want the local member %s", ip, localEtcd)
		}
		if f.liveErr != nil {
			return ha.EtcdStorage{}, f.liveErr
		}
		s := f.storage[min(f.reads, len(f.storage)-1)]
		f.reads++
		return s, nil
	})
	swap(t, &rollbackEtcdOffline, func() (string, error) { return f.offline, f.offErr })
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
	swap(t, &bootClock, func() (staticpod.Mark, error) { return staticpod.Mark{}, nil })
	swap(t, &rollbackReadEtcdManifest, func() ([]byte, error) { return []byte("newer etcd manifest"), nil })
	swap(t, &rollbackStopEtcd, func() error { f.stopped++; return f.stopErr })
	swap(t, &rollbackPutEtcdManifest, func(b []byte) error { f.putBack = append(f.putBack, b); return nil })
	swap(t, &rollbackEtcdDowngrade, func(ip, target string) error {
		f.downgrade = append(f.downgrade, ip+" "+target)
		return nil
	})
	sleeps := 0
	swap(t, &etcdDowngradeSleep, func(time.Duration) {
		if sleeps++; sleeps > 100 {
			t.Fatalf("still waiting for etcd's storage after %d polls", sleeps)
		}
	})
	return f
}

var errEtcdDown = fmt.Errorf("%w: connection refused", ha.ErrEtcdNoStatus)

func oldManifestBackup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "etcd.yaml"), etcdManifest(oldEtcdImage), 0o600)
	return dir
}

// TestPrepareEtcdForRollback: the restored etcd must be able to open the
// data. A single member is downgraded first; a larger cluster, or data that
// only a running etcd could downgrade, is refused before anything changes.
func TestPrepareEtcdForRollback(t *testing.T) {
	single := func(v string) ha.EtcdStorage { return ha.EtcdStorage{StorageVersion: v, Members: 1} }

	t.Run("the same minor version needs nothing", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.6.0"))
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err != nil || len(f.downgrade) != 0 {
			t.Fatalf("err = %v, downgrades %q; want neither", err, f.downgrade)
		}
	})
	t.Run("no manifest backup reads nothing", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"))
		if err := prepareEtcdForRollback(io.Discard, "", false); err != nil || f.reads != 0 {
			t.Fatalf("err = %v, reads %d; want neither", err, f.reads)
		}
	})
	t.Run("a single member is downgraded and waited for", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"), single("3.7.0"), single("3.7.0"), single("3.6.0"))
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err != nil {
			t.Fatal(err)
		}
		if len(f.downgrade) != 1 || f.downgrade[0] != localEtcd+" 3.6" {
			t.Errorf("downgrades = %q, want one of the local member to 3.6", f.downgrade)
		}
		if f.reads != 4 {
			t.Errorf("read the storage version %d times, want until it reached 3.6.0 (4)", f.reads)
		}
	})
	t.Run("a downgrade that does not land is reported, with the way back", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"))
		swap(t, &etcdDowngradeWindow, 0)
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		for _, want := range []string{"accepted the downgrade to 3.6", "still at 3.7.0", "downgrade cancel"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		if len(f.downgrade) != 1 {
			t.Errorf("downgrades = %q, want one", f.downgrade)
		}
	})
	t.Run("a refused downgrade changes nothing", func(t *testing.T) {
		newEtcdDowngradeFixture(t, single("3.7.0"))
		swap(t, &rollbackEtcdDowngrade, func(string, string) error { return errors.New("etcd refused a downgrade to 3.6: no") })
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("err = %v, want a refusal that changed nothing", err)
		}
	})
	t.Run("a downgrade whose reply was lost but which took effect is waited for", func(t *testing.T) {
		downgrading := ha.EtcdStorage{StorageVersion: "3.7.0", Members: 1, Downgrading: true, DowngradeTarget: "3.6.0"}
		newEtcdDowngradeFixture(t, single("3.7.0"), downgrading, downgrading, single("3.6.0"))
		swap(t, &rollbackEtcdDowngrade, func(string, string) error {
			return errors.New("enable etcd's downgrade to 3.6: context deadline exceeded")
		})
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err != nil {
			t.Fatalf("err = %v, want the rollback to go on once the downgrade it asked for landed", err)
		}
	})
	t.Run("a failed downgrade beside another one started meanwhile is refused", func(t *testing.T) {
		elsewhere := ha.EtcdStorage{StorageVersion: "3.7.0", Members: 1, Downgrading: true, DowngradeTarget: "3.5.0"}
		f := newEtcdDowngradeFixture(t, single("3.7.0"), elsewhere)
		swap(t, &rollbackEtcdDowngrade, func(string, string) error {
			return errors.New("enable etcd's downgrade to 3.6: a downgrade is already in progress")
		})
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		if err == nil || !strings.Contains(err.Error(), `"3.5.0" in progress`) || !strings.Contains(err.Error(), "downgrade cancel") || f.reads != 2 {
			t.Fatalf("err = %v after %d reads; want a refusal naming the other downgrade, without waiting on it", err, f.reads)
		}
	})
	t.Run("an etcd read that fails for another reason stops nothing", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"))
		f.liveErr = errors.New("etcd member list: context deadline exceeded")
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") || f.stopped != 0 || len(f.downgrade) != 0 {
			t.Fatalf("err = %v, stopped %d, downgrades %q; want a refusal that leaves etcd running", err, f.stopped, f.downgrade)
		}
		if _, serr := os.Stat(stoppedEtcdManifest()); serr == nil {
			t.Error("kept etcd's manifest as if it were stopped")
		}
	})
	t.Run("a failed downgrade whose outcome cannot be read says so", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"))
		swap(t, &rollbackEtcdDowngrade, func(string, string) error {
			f.liveErr = errEtcdDown
			return errors.New("enable etcd's downgrade to 3.6: context deadline exceeded")
		})
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		for _, want := range []string{"may have started", "endpoint status", "downgrade cancel"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		if err != nil && strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("claimed nothing was changed when a downgrade may have started: %v", err)
		}
	})
	t.Run("a cluster of several members is refused", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.7.0", Members: 3})
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		for _, want := range []string{"one of 3 members", "3.7.0", "3.6", "nothing was changed", "Rolling back across an etcd minor version"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		if len(f.downgrade) != 0 {
			t.Errorf("downgraded a cluster of several members: %q", f.downgrade)
		}
	})
	t.Run("a dry run says what it would do and does nothing", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"))
		out, err := runWriter(t, func(w io.Writer) error { return prepareEtcdForRollback(w, oldManifestBackup(t), true) })
		if err != nil || len(f.downgrade) != 0 || !strings.Contains(out, "would downgrade etcd's storage from 3.7.0 to 3.6") {
			t.Fatalf("err = %v, downgrades %q, out %q", err, f.downgrade, out)
		}
	})
	t.Run("an etcd that does not answer and cannot be stopped is left as it was", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("unused"))
		f.liveErr, f.stopErr = errEtcdDown, errors.New("still serving")
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		if err == nil || !strings.Contains(err.Error(), "stopping it to read its data failed") || len(f.putBack) != 1 ||
			!strings.Contains(err.Error(), "manifest was put back") {
			t.Fatalf("err = %v, put back %d; want the refusal and the manifest put back", err, len(f.putBack))
		}
		if _, err := os.Stat(stoppedEtcdManifest()); err == nil {
			t.Error("the kept manifest outlived its put-back")
		}
		swap(t, &rollbackPutEtcdManifest, func([]byte) error { return errors.New("read-only file system") })
		err = prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		for _, want := range []string{"putting its manifest back failed", "copy " + stoppedEtcdManifest()} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want %q", err, want)
			}
		}
		if err != nil && (strings.Contains(err.Error(), "was put back") || strings.Contains(err.Error(), "nothing was changed")) {
			t.Errorf("claimed a put-back or no change after a failed put-back: %v", err)
		}
		if b, _ := os.ReadFile(stoppedEtcdManifest()); string(b) != "newer etcd manifest" {
			t.Errorf("kept manifest = %q, want it kept for the operator to copy back", b)
		}
	})
	t.Run("a dry run reads a stopped etcd's data without stopping it", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("unused"))
		f.liveErr, f.offline = errEtcdDown, "3.6.0"
		out, err := runWriter(t, func(w io.Writer) error { return prepareEtcdForRollback(w, oldManifestBackup(t), true) })
		if err != nil || f.stopped != 0 || !strings.Contains(out, "would stop etcd") {
			t.Fatalf("err = %v, stopped %d, out %q; want a read only, saying it would stop etcd", err, f.stopped, out)
		}
		f.offline, f.offErr = "", fmt.Errorf("open db: %w", berrors.ErrTimeout)
		out, err = runWriter(t, func(w io.Writer) error { return prepareEtcdForRollback(w, oldManifestBackup(t), true) })
		if err != nil || f.stopped != 0 || !strings.Contains(out, "holds its data") {
			t.Fatalf("locked data: err = %v, out %q; want the stop the real run would make, not a failure", err, out)
		}
	})
	t.Run("a stopped etcd's kept manifest is put back by a rerun", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("unused"))
		f.liveErr, f.offline = errEtcdDown, "3.7.0"
		swap(t, &rollbackReadEtcdManifest, func() ([]byte, error) { return nil, nil })
		writeFile(t, stoppedEtcdManifest(), "kept etcd manifest", 0o600)
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err == nil {
			t.Fatal("refusal expected for newer data")
		}
		if len(f.putBack) != 1 || string(f.putBack[0]) != "kept etcd manifest" {
			t.Errorf("put back %q, want the manifest kept by the run that stopped etcd", f.putBack)
		}
	})
	t.Run("a rerun finishes a downgrade already in progress", func(t *testing.T) {
		inProgress := ha.EtcdStorage{StorageVersion: "3.7.0", Members: 1, Downgrading: true, DowngradeTarget: "3.6.0"}
		f := newEtcdDowngradeFixture(t, inProgress, inProgress, single("3.6.0"))
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err != nil {
			t.Fatalf("err = %v, want the downgrade in progress waited for", err)
		}
		if len(f.downgrade) != 0 {
			t.Errorf("asked for a second downgrade %q; etcd refuses to validate one while a downgrade is enabled", f.downgrade)
		}
	})
	t.Run("a downgrade in progress to another version is refused", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.7.0", Members: 1, Downgrading: true, DowngradeTarget: "3.5.0"})
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		if err == nil || !strings.Contains(err.Error(), "already has a downgrade") || !strings.Contains(err.Error(), "nothing was changed") || len(f.downgrade) != 0 {
			t.Fatalf("err = %v, downgrades %q; want a refusal that changed nothing", err, f.downgrade)
		}
	})
	t.Run("a downgrade etcd did not validate changes nothing and is not re-read", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, single("3.7.0"))
		swap(t, &rollbackEtcdDowngrade, func(string, string) error {
			return fmt.Errorf("%w to 3.6: context deadline exceeded", ha.ErrDowngradeNotValidated)
		})
		err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") || strings.Contains(err.Error(), "may have started") || f.reads != 1 {
			t.Fatalf("err = %v after %d reads; want nothing changed, no re-read", err, f.reads)
		}
	})
	t.Run("what cannot be read is refused before anything changes", func(t *testing.T) {
		f := newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "", Members: 1})
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("empty live storage version: err = %v", err)
		}
		swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", errors.New("parse etcd.yaml") })
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("unreadable backup: err = %v", err)
		}
		swap(t, &rollbackEtcdTarget, backedUpEtcdMinor)
		f.liveErr = errEtcdDown
		swap(t, &rollbackReadEtcdManifest, func() ([]byte, error) { return nil, errors.New("permission denied") })
		if err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false); err == nil || !strings.Contains(err.Error(), "nothing was changed") || f.stopped != 0 {
			t.Errorf("unreadable manifest: err = %v, stopped %d; want a refusal before stopping etcd", err, f.stopped)
		}
		if len(f.downgrade) != 0 {
			t.Errorf("downgraded: %q", f.downgrade)
		}
	})
	t.Run("an etcd that does not answer is read from its data", func(t *testing.T) {
		for _, c := range []struct {
			offline, want string
			offErr        error
		}{
			{offline: "3.6.0"},
			{offline: ""},
			{offline: "3.7.0", want: "Only a running etcd can downgrade"},
			{offErr: errors.New("timeout"), want: "could not be read either"},
		} {
			f := newEtcdDowngradeFixture(t, single("unused"))
			f.liveErr, f.offline, f.offErr = errEtcdDown, c.offline, c.offErr
			err := prepareEtcdForRollback(io.Discard, oldManifestBackup(t), false)
			if f.stopped != 1 {
				t.Errorf("offline %q: stopped etcd %d times, want once before reading its data", c.offline, f.stopped)
			}
			if c.want == "" {
				if err != nil {
					t.Errorf("offline %q: %v, want the rollback to go ahead", c.offline, err)
				}
				if len(f.putBack) != 0 {
					t.Errorf("offline %q: put the newer etcd manifest back, which could migrate the data before the restore", c.offline)
				}
				if b, _ := os.ReadFile(stoppedEtcdManifest()); string(b) != "newer etcd manifest" {
					t.Errorf("offline %q: kept manifest %q, want it kept while etcd stays stopped", c.offline, b)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "manifest was put back, and nothing else was changed") {
				t.Errorf("offline %q (%v): err = %v, want %q and nothing changed", c.offline, c.offErr, err, c.want)
			}
			if len(f.putBack) != 1 || string(f.putBack[0]) != "newer etcd manifest" {
				t.Errorf("offline %q (%v): refused but put back %q, want the manifest it stopped", c.offline, c.offErr, f.putBack)
			}
			if len(f.downgrade) != 0 {
				t.Errorf("downgraded with etcd not answering: %q", f.downgrade)
			}
		}
	})
}

// TestRollbackDowngradesEtcdBeforeRestoringIt: the older manifest goes back
// only after the data is at a version its etcd can open; restored first, it
// crash-loops. Refused, nothing is restored.
func TestRollbackDowngradesEtcdBeforeRestoringIt(t *testing.T) {
	setup := func(t *testing.T) (live string, backups []backupItem) {
		backupFixture(t)
		redirect(t, &version.K8sConfDir, t.TempDir())
		bak := oldManifestBackup(t)
		live = filepath.Join(staticpod.Dir(), "etcd.yaml")
		writeFile(t, live, etcdManifest(newEtcdImage), 0o600)
		return live, []backupItem{{name: "manifests", bak: bak, dst: staticpod.Dir(), dir: true}}
	}
	t.Run("single member", func(t *testing.T) {
		live, backups := setup(t)
		f := newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.7.0", Members: 1},
			ha.EtcdStorage{StorageVersion: "3.6.0", Members: 1})
		swap(t, &rollbackEtcdDowngrade, func(ip, target string) error {
			if b, _ := os.ReadFile(live); !strings.Contains(string(b), newEtcdImage) {
				t.Error("the older etcd manifest was restored before the downgrade")
			}
			f.downgrade = append(f.downgrade, target)
			return nil
		})
		err := rollbackNode(io.Discard, errors.New("kube-apiserver gate failed"), backups, false, resumeTS)
		if err == nil || !strings.Contains(err.Error(), "the node was rolled back") {
			t.Fatalf("err = %v, want the rollback done", err)
		}
		if b, _ := os.ReadFile(live); !strings.Contains(string(b), oldEtcdImage) || len(f.downgrade) != 1 {
			t.Errorf("live manifest %q after %d downgrades, want the older one after one", b, len(f.downgrade))
		}
	})
	t.Run("several members", func(t *testing.T) {
		live, backups := setup(t)
		newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.7.0", Members: 3})
		err := rollbackNode(io.Discard, errors.New("kube-apiserver gate failed"), backups, false, resumeTS)
		if err == nil || !strings.Contains(err.Error(), "rollback did not start") || !strings.Contains(err.Error(), "kube-apiserver gate failed") {
			t.Fatalf("err = %v, want the refusal and the upgrade's own failure", err)
		}
		if b, _ := os.ReadFile(live); !strings.Contains(string(b), newEtcdImage) {
			t.Errorf("live manifest %q, want it left on the new etcd", b)
		}
	})
	for _, role := range []string{"controller", "worker"} {
		t.Run("stratum rollback refuses before restoring, node.yaml role "+role, func(t *testing.T) {
			rollbackRefusalCase(t, role)
		})
	}
}

// rollbackRefusalCase is a rollback whose backup restores an older etcd over
// data a larger cluster stores above it. The check reads the backup, not the
// role node.yaml records, which may be missing or wrong.
func rollbackRefusalCase(t *testing.T, role string) {
	t.Helper()
	newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.7.0", Members: 3})
	resumeFixture(t)
	writeFile(t, filepath.Join(staticpod.Dir(), "..", "node.yaml"), "role: "+role+"\n", 0o600)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(manifestBakDir(resumeTS), "etcd.yaml"), etcdManifest(oldEtcdImage), 0o600)
	live := filepath.Join(staticpod.Dir(), "etcd.yaml")
	writeFile(t, live, etcdManifest(newEtcdImage), 0o600)
	err := RollbackNode(io.Discard, RollbackOptions{})
	if err == nil || !strings.Contains(err.Error(), "one of 3 members") {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if b, _ := os.ReadFile(live); !strings.Contains(string(b), newEtcdImage) {
		t.Errorf("live manifest %q, want it left on the new etcd", b)
	}
	if m, _ := readUpgradeMarker(); m == nil {
		t.Error("the marker was removed by a rollback that restored nothing")
	}
}

// TestOfflineEtcdStorageVersion reads a database written by the pinned etcd.
func TestOfflineEtcdStorageVersion(t *testing.T) {
	b, err := os.ReadFile("../cluster/testdata/etcd/members.db")
	if err != nil {
		t.Fatal(err)
	}
	redirect(t, &version.EtcdDataDir, t.TempDir())
	writeFile(t, filepath.Join(version.EtcdDataDir, "member", "snap", "db"), string(b), 0o600)
	got, err := offlineEtcdStorageVersion()
	if err != nil || got == "" {
		t.Fatalf("got %q, %v; want the storage version the pinned etcd recorded", got, err)
	}
	if _, err := minorAbove(got, "3.6"); err != nil {
		t.Errorf("recorded storage version %q does not parse: %v", got, err)
	}

	redirect(t, &version.EtcdDataDir, t.TempDir())
	if _, err := offlineEtcdStorageVersion(); err == nil {
		t.Error("a missing database read without error")
	}
}

// TestRollbackNodeDowngradesASingleMemberFirst: `stratum rollback` itself, not
// only the upgrade's own rollback, downgrades a single member's etcd before
// restoring the older manifest, and its dry run does neither.
func TestRollbackNodeDowngradesASingleMemberFirst(t *testing.T) {
	setup := func(t *testing.T) (string, *etcdDowngradeFixture) {
		f := newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.7.0", Members: 1},
			ha.EtcdStorage{StorageVersion: "3.6.0", Members: 1})
		rollbackRestartFixture(t, "")
		writeFile(t, filepath.Join(staticpod.Dir(), "..", "node.yaml"), "role: controller\n", 0o600)
		if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(manifestBakDir(resumeTS), "etcd.yaml"), etcdManifest(oldEtcdImage), 0o600)
		live := filepath.Join(staticpod.Dir(), "etcd.yaml")
		writeFile(t, live, etcdManifest(newEtcdImage), 0o600)
		swap(t, &waitStaticPodReady, func(string, time.Duration) error { return nil })
		swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
		swap(t, &bootClock, func() (staticpod.Mark, error) { return staticpod.Mark{}, nil })
		swap(t, &waitHealthzCtx, func(context.Context) error { return nil })
		return live, f
	}
	t.Run("real run", func(t *testing.T) {
		live, f := setup(t)
		swap(t, &rollbackEtcdDowngrade, func(ip, target string) error {
			if b, _ := os.ReadFile(live); !strings.Contains(string(b), newEtcdImage) {
				t.Error("the older etcd manifest was restored before the downgrade")
			}
			f.downgrade = append(f.downgrade, target)
			return nil
		})
		_ = RollbackNode(io.Discard, RollbackOptions{SoakSec: -1})
		if len(f.downgrade) != 1 {
			t.Errorf("downgrades = %q, want one", f.downgrade)
		}
		if b, _ := os.ReadFile(live); !strings.Contains(string(b), oldEtcdImage) {
			t.Errorf("live manifest %q, want the older one restored after the downgrade", b)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		live, f := setup(t)
		out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{DryRun: true}) })
		if err != nil || len(f.downgrade) != 0 || f.stopped != 0 || !strings.Contains(out, "would downgrade etcd's storage") {
			t.Fatalf("err = %v, downgrades %q, stopped %d; want only the plan:\n%s", err, f.downgrade, f.stopped, out)
		}
		if b, _ := os.ReadFile(live); !strings.Contains(string(b), newEtcdImage) {
			t.Errorf("a dry run restored the older manifest")
		}
	})
}

// TestStoppedEtcdNote: a restore that fails while etcd is stopped for the
// rollback says so, and where its manifest is kept.
func TestStoppedEtcdNote(t *testing.T) {
	redirect(t, &version.DataDir, t.TempDir())
	if n := stoppedEtcdNote(); n != "" {
		t.Errorf("note with no kept manifest: %q", n)
	}
	writeFile(t, stoppedEtcdManifest(), "m", 0o600)
	if n := stoppedEtcdNote(); !strings.Contains(n, "etcd was stopped") || !strings.Contains(n, stoppedEtcdManifest()) {
		t.Errorf("note = %q", n)
	}
	redirect(t, &version.K8sConfDir, t.TempDir())
	if err := settleStoppedEtcdManifest(); err != nil {
		t.Fatal(err)
	}
	if n := stoppedEtcdNote(); n == "" {
		t.Error("a restore that did not put etcd's manifest back dropped the kept one etcd still needs")
	}
	writeFile(t, staticpod.Path("etcd"), "restored", 0o600)
	if n := stoppedEtcdNote(); n != "" {
		t.Errorf("etcd has its own manifest again, but the note still says: %q", n)
	}
	if err := settleStoppedEtcdManifest(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stoppedEtcdManifest()); err == nil {
		t.Error("etcd's manifest was restored, but the newer kept one is still there")
	}
}

// TestSettleStoppedEtcdManifestReportsAFailedRemove: a kept manifest that
// cannot be removed once etcd's is restored is reported, not left silently.
func TestSettleStoppedEtcdManifestReportsAFailedRemove(t *testing.T) {
	redirect(t, &version.DataDir, t.TempDir())
	redirect(t, &version.K8sConfDir, t.TempDir())
	writeFile(t, staticpod.Path("etcd"), "restored", 0o600)
	writeFile(t, filepath.Join(stoppedEtcdManifest(), "child"), "x", 0o600)
	if err := settleStoppedEtcdManifest(); err == nil || !strings.Contains(err.Error(), stoppedEtcdManifest()) {
		t.Fatalf("err = %v, want the kept manifest named", err)
	}
}

// TestOfflineEtcdStorageVersionOnACorruptDatabase: bbolt panics on a corrupt
// page; the read returns an error instead, so a refusal can put etcd's
// manifest back.
func TestOfflineEtcdStorageVersionOnACorruptDatabase(t *testing.T) {
	src := filepath.Join(t.TempDir(), "db")
	db, err := bolt.Open(src, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket([]byte("meta"))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte("storageVersion"), []byte("3.7.0")); err != nil {
			return err
		}
		for i := range 64 {
			b, err := tx.CreateBucket(fmt.Appendf(nil, "bucket-%02d", i))
			if err != nil {
				return err
			}
			for k := range 64 {
				if err := b.Put(fmt.Appendf(nil, "key-%04d", k), make([]byte, 128)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	const page = 4096
	failed := 0
	for p := 2; p*page < len(good); p++ {
		b := append([]byte(nil), good...)
		for i := p * page; i < (p+1)*page && i < len(b); i++ {
			b[i] = 0xff
		}
		redirect(t, &version.EtcdDataDir, t.TempDir())
		writeFile(t, filepath.Join(version.EtcdDataDir, "member", "snap", "db"), string(b), 0o600)
		if _, err := offlineEtcdStorageVersion(); err != nil {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("no corrupted page made the read fail; the test does not exercise a corrupt database")
	}
}

func TestWriteBytesAtomicKeepsTheOldCopyOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "kept.yaml")
	writeFile(t, dst, "old", 0o600)
	if err := writeBytesAtomic(dst, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Fatalf("content = %q, want new", b)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := writeBytesAtomic(dst, []byte("lost")); err == nil {
		t.Fatal("write into a read-only directory succeeded")
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("a failed write changed the kept copy to %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("left %d entries behind, want only the kept copy", len(ents))
	}
}
