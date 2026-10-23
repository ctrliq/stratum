package upgrade

import (
	"bytes"

	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

const resumeTS = "20260928-100000"

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestResumedInstallKeepsTheOriginalBackup guards the resume case: the
// interrupted run installed the new binary, and the resumed run must not back
// that up as the version to roll back to; doing so would make a rollback
// restore the new binary.
func TestResumedInstallKeepsTheOriginalBackup(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "stratum"), filepath.Join(dir, "next")
	writeFile(t, dst, "old", 0o755)
	writeFile(t, src, "new", 0o755)

	for run := 1; run <= 2; run++ { // run 2 is the resume, same stamp
		if _, err := backupAndInstall(io.Discard, dst, src, resumeTS); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if got := readFile(t, dst+".bak-"+resumeTS); got != "old" {
		t.Errorf("backup after the resume holds %q, want the original %q", got, "old")
	}
	if got := readFile(t, dst); got != "new" {
		t.Errorf("installed %q, want %q", got, "new")
	}
}

// TestResumedManifestBackupKeepsTheOriginal: the interrupted run had already
// re-rendered a manifest; the resume's backup must still hold the old one.
func TestResumedManifestBackupKeepsTheOriginal(t *testing.T) {
	dir := t.TempDir()
	redirect(t, &version.K8sConfDir, dir)
	redirect(t, &version.DataDir, filepath.Join(dir, "data"))
	live := filepath.Join(staticpod.Dir(), "kube-apiserver.yaml")
	writeFile(t, live, "old", 0o600)

	if _, err := backupManifests(resumeTS); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, "new", 0o600) // the interrupted run's re-render
	bak, err := backupManifests(resumeTS)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(bak, "kube-apiserver.yaml")); got != "old" {
		t.Errorf("manifest backup after the resume holds %q, want the original %q", got, "old")
	}
}

// TestManifestBackupIsCompleteOrAbsent: a resume keeps whatever backup exists,
// so an interrupted one must not exist under its final name — kept, it would
// restore a control plane missing a manifest.
func TestManifestBackupIsCompleteOrAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := t.TempDir()
	redirect(t, &version.K8sConfDir, dir)
	redirect(t, &version.DataDir, filepath.Join(dir, "data"))
	writeFile(t, filepath.Join(staticpod.Dir(), "etcd.yaml"), "ok", 0o600)
	writeFile(t, filepath.Join(staticpod.Dir(), "kube-apiserver.yaml"), "unreadable", 0o000)

	if _, err := backupManifests(resumeTS); err == nil {
		t.Fatal("expected the unreadable manifest to fail the backup")
	}
	if _, err := os.Stat(manifestBakDir(resumeTS)); !os.IsNotExist(err) {
		t.Errorf("a failed backup left %s behind (stat: %v)", manifestBakDir(resumeTS), err)
	}
	entries, _ := os.ReadDir(filepath.Dir(manifestBakDir(resumeTS)))
	for _, e := range entries {
		t.Errorf("a failed backup left %s behind", e.Name())
	}
}

// TestResumedProfileBackupKeepsTheOriginal: the same for the CIS configs, which
// the interrupted run had already rewritten.
func TestResumedProfileBackupKeepsTheOriginal(t *testing.T) {
	withCISConfigs(t)
	writeFile(t, version.AdmissionConfigPath, "old", 0o644)

	if _, err := refreshProfileConfigs(io.Discard, resumeTS, cisProfile()); err != nil { // writes "new"
		t.Fatal(err)
	}
	items, err := refreshProfileConfigs(io.Discard, resumeTS, cisProfile())
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(items[0].bak, filepath.Base(version.AdmissionConfigPath))); got != "old" {
		t.Errorf("CIS backup after the resume holds %q, want the original %q", got, "old")
	}
	if got := readFile(t, version.AdmissionConfigPath); got != "new" {
		t.Errorf("the resume must still write this release's config, got %q", got)
	}
}

// TestBackupScanIgnoresInFlightWrites: a backup is written through copyBytes'
// hidden ".<name>.bak-<ts>.tmp-*" file. A crash can leave one behind, and its
// stamp — "<ts>.tmp-123" — is what keeps it out of every set.
func TestBackupScanIgnoresInFlightWrites(t *testing.T) {
	_, binDir, _ := backupFixture(t)
	writeFile(t, filepath.Join(binDir, "kubelet.bak-"+resumeTS), "old", 0o755)
	writeFile(t, filepath.Join(binDir, ".kubelet.bak-"+resumeTS+".tmp-123"), "torn", 0o600)

	ts, items := mustNewestSet(t)
	if ts != resumeTS || len(items) != 1 || items[0].name != "kubelet" {
		t.Errorf("newest set = %q, %+v; want %q with kubelet alone", ts, items, resumeTS)
	}
}

// TestResumeStartsFromTheInterruptedRunsBackups: a resume that fails before
// revisiting something the interrupted run changed — here a host binary it had
// already swapped — must still restore it, and bounce the host processes.
func TestResumeStartsFromTheInterruptedRunsBackups(t *testing.T) {
	stratumDir, binDir, _ := backupFixture(t)
	writeFile(t, filepath.Join(stratumDir, "stratum.bak-"+resumeTS), "old", 0o755)
	writeFile(t, filepath.Join(binDir, "containerd.bak-"+resumeTS), "old", 0o755)
	writeFile(t, filepath.Join(binDir, "containerd.bak-20260101-000000"), "older", 0o755)

	sets, err := scanBackupSets()
	if err != nil {
		t.Fatal(err)
	}
	var out string
	var ts string
	var backups []backupItem
	var hostSwapped bool
	out, err = runWriter(t, func(w io.Writer) (err error) {
		ts, backups, hostSwapped, err = startBackupSet(w, &upgradeMarker{TS: resumeTS, Target: "v1.37.9"}, "v1.37.9", sets)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if ts != resumeTS {
		t.Errorf("resume took stamp %q, want the interrupted run's %q", ts, resumeTS)
	}
	names := map[string]bool{}
	for _, b := range backups {
		names[b.name] = true
		if !strings.HasSuffix(b.bak, resumeTS) {
			t.Errorf("seeded %s from another run: %s", b.name, b.bak)
		}
	}
	if !names["stratum"] || !names["containerd"] || len(backups) != 2 {
		t.Errorf("seeded %+v, want stratum and containerd from %s", backups, resumeTS)
	}
	if !hostSwapped {
		t.Error("the interrupted run swapped containerd, so a failed resume must bounce the host processes")
	}
	if _, err := os.Stat(upgradeMarkerPath()); !os.IsNotExist(err) {
		t.Error("a resume must not rewrite the marker it resumed from")
	}
	for _, want := range []string{"keeping the interrupted run's stratum backup", "keeping the interrupted run's containerd backup"} {
		if !strings.Contains(out, want) {
			t.Errorf("a resume must say which backups it kept; missing %q in:\n%s", want, out)
		}
	}

	// And a fresh upgrade records itself before it changes anything.
	clearUpgradeMarker(io.Discard)
	ts, backups, _, err = startBackupSet(io.Discard, nil, "v1.37.9", sets)
	if err != nil {
		t.Fatal(err)
	}
	m, err := readUpgradeMarker()
	if err != nil || m == nil || m.TS != ts || m.Target != "v1.37.9" || len(backups) != 0 {
		t.Errorf("fresh upgrade: marker %+v (%v), backups %+v; want a marker for %s and no backups", m, err, backups, ts)
	}
}

// TestAddBackupDoesNotDuplicateASeededItem: the resumed run revisits what it
// was seeded with, and restoring one item twice would be harmless but a
// doubled INCOMPLETE message is not.
func TestAddBackupDoesNotDuplicateASeededItem(t *testing.T) {
	it := backupItem{name: "stratum", bak: "/x/stratum.bak-" + resumeTS}
	if got := addBackup(addBackup(nil, it), it); len(got) != 1 {
		t.Errorf("addBackup twice = %+v, want one item", got)
	}
}

// TestRollbackEndsTheUpgradeOnlyWhenComplete: a complete automatic rollback
// puts the node back, so the upgrade is over. An INCOMPLETE one must keep the
// marker, so the retry keeps the original baseline.
func TestRollbackEndsTheUpgradeOnlyWhenComplete(t *testing.T) {
	_, _, _ = backupFixture(t)
	dir := t.TempDir()
	cause := io.ErrUnexpectedEOF

	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	if err := rollbackNode(io.Discard, cause, []backupItem{{name: "stratum", bak: filepath.Join(dir, "missing"), dst: filepath.Join(dir, "stratum")}}, false, resumeTS); err == nil || !strings.Contains(err.Error(), "INCOMPLETE") {
		t.Errorf("want an INCOMPLETE rollback, got: %v", err)
	}
	if m, _ := readUpgradeMarker(); m == nil {
		t.Error("an INCOMPLETE rollback cleared the marker; the retry would take a new baseline")
	}

	bak, dst := filepath.Join(dir, "kubelet.bak"), filepath.Join(dir, "kubelet")
	writeFile(t, bak, "old", 0o755)
	writeFile(t, dst, "new", 0o755)
	if err := rollbackNode(io.Discard, cause, []backupItem{{name: "kubelet", bak: bak, dst: dst}}, false, resumeTS); err == nil || !strings.Contains(err.Error(), "the node was rolled back") {
		t.Errorf("want a complete rollback, got: %v", err)
	}
	if m, _ := readUpgradeMarker(); m != nil {
		t.Errorf("a complete rollback left the marker %+v; the next upgrade would resume a finished one", m)
	}
}

// fakeKubelet puts a kubelet reporting ver at version.BinDir: the node
// version marker.
func fakeKubelet(t *testing.T, ver string) {
	t.Helper()
	writeFile(t, filepath.Join(version.BinDir, "kubelet"), "#!/bin/sh\necho Kubernetes "+ver+"-stratum\n", 0o755)
}

// resumeFixture is a node that passes the guard and the lock, with node state.
func resumeFixture(t *testing.T) {
	t.Helper()
	backupFixture(t)
	confDir := t.TempDir()
	redirect(t, &version.K8sConfDir, confDir)
	writeFile(t, filepath.Join(confDir, "node.yaml"), "role: worker\n", 0o600)
	nodeUp(t)
	// The test binary carries no payload; answer for it as a release would.
	oldPayload := stagedPayload
	stagedPayload = func(string) error { return nil }
	t.Cleanup(func() { stagedPayload = oldPayload })
	swap(t, &bootClock, func() (staticpod.Mark, error) { return staticpod.Mark{}, nil })
}

// interruptedAt records an upgrade to target that got as far as backing up
// the stratum binary under resumeTS — one that changed something.
func interruptedAt(t *testing.T, target string) {
	t.Helper()
	writeFile(t, stratumInstallPath+".bak-"+resumeTS, "old", 0o755)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: target}); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeRefusesADifferentInterruptedUpgrade: its baseline is the node
// before THAT upgrade; neither adopting nor discarding it is safe to guess.
func TestUpgradeRefusesADifferentInterruptedUpgrade(t *testing.T) {
	resumeFixture(t)
	interruptedAt(t, "v1.37.8")
	err := UpgradeNode(io.Discard, NodeOptions{})
	for _, want := range []string{"an upgrade to v1.37.8 was interrupted", "*.bak-" + resumeTS, "stratum rollback", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestUpgradeAtTargetFinishesAnInterruptedOne: killed after the kubelet swap,
// the node reports the target but the upgrade never finished. That is not
// "nothing to do" — left there, the marker outlives the upgrade and the next
// one's rollback lands two versions back.
func TestUpgradeAtTargetFinishesAnInterruptedOne(t *testing.T) {
	// setRoots runs before resumeFixture: a SetRoots after it would recompute
	// every derived path (BinDir, K8sConfDir, StageDir) from the new roots and
	// step over the fixture's own per-var redirects, leaving the node state it
	// staged where the test no longer reads it.
	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9") // an unstamped test binary says "unknown"
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "this build", "this build")

	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{}) })
	if err != nil || !strings.Contains(out, "nothing to do") {
		t.Fatalf("with no upgrade in progress, at target on this build is a no-op: %v\n%s", err, out)
	}

	interruptedAt(t, version.K8sVersion)
	out, err = runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{}) })
	if strings.Contains(out, "nothing to do") || !strings.Contains(out, "resuming the interrupted upgrade") {
		t.Errorf("an interrupted upgrade at target must resume, not stop:\n%s", out)
	}
	// Past the at-target check and on into the upgrade itself, which stops at
	// the payload this test binary does not carry.
	if err == nil || !strings.Contains(err.Error(), "carries no payload") {
		t.Errorf("the resume should reach the payload, got: %v", err)
	}
}

// TestUnusableMarkerIsRefused: treating it as absent would start a fresh
// baseline over the backups it exists to protect, and its stamp is joined into
// paths that are removed and restored from, so it must be one NowStamp wrote.
func TestUnusableMarkerIsRefused(t *testing.T) {
	resumeFixture(t)
	for content, want := range map[string]string{
		"{not json":                                        "is not valid JSON (invalid character",
		`{"ts":"` + resumeTS + `"}`:                        "names no target",
		`{"ts":"../../etc","target":"v1.37.9"}`:            `invalid backup stamp "../../etc"`,
		`{"ts":"20260928-100000/../x","target":"v1.37.9"}`: "invalid backup stamp",
		`{"ts":"","target":"v1.37.9"}`:                     "invalid backup stamp",
	} {
		writeFile(t, upgradeMarkerPath(), content, 0o600)
		for cmd, run := range map[string]func() error{
			"upgrade":  func() error { return UpgradeNode(io.Discard, NodeOptions{}) },
			"rollback": func() error { return RollbackNode(io.Discard, RollbackOptions{}) },
		} {
			err := run()
			for _, w := range []string{"stratum " + cmd + ":", want, "nothing was changed"} {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("%s with marker %s: error missing %q: %v", cmd, content, w, err)
				}
			}
		}
	}
}

// TestMarkerThatChangedNothingIsDropped: an upgrade stopped after recording
// itself and before its first backup changed nothing, so it neither blocks a
// different upgrade nor becomes the baseline of this one. A dry run says so
// and leaves the marker.
func TestMarkerThatChangedNothingIsDropped(t *testing.T) {
	resumeFixture(t)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.8"}); err != nil {
		t.Fatal(err)
	}

	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{DryRun: true}) })
	if err != nil || !strings.Contains(out, "changed nothing") {
		t.Errorf("dry run: %v\n%s", err, out)
	}
	if m, _ := readUpgradeMarker(); m == nil {
		t.Error("a dry run removed the marker")
	}

	out, err = runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{}) })
	if err != nil && strings.Contains(err.Error(), "was interrupted") {
		t.Errorf("an upgrade that changed nothing must not block another: %v", err)
	}
	if !strings.Contains(out, "changed nothing") {
		t.Errorf("dropping the marker must be said:\n%s", out)
	}
	if m, _ := readUpgradeMarker(); m != nil {
		t.Errorf("marker %+v kept for an upgrade that changed nothing", m)
	}
}

// TestRollbackOfAnUpgradeThatChangedNothing: the newest set on disk is then
// the upgrade BEFORE the interrupted one. Restoring it would take the node a
// version further back than the run it was asked to undo; it removes the
// marker, restores nothing, and says how to go further.
func TestRollbackOfAnUpgradeThatChangedNothing(t *testing.T) {
	resumeFixture(t)
	dst := filepath.Join(version.BinDir, "containerd")
	writeFile(t, dst, "current", 0o755)
	writeFile(t, dst+".bak-20260101-000000", "previous upgrade's original", 0o755)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}

	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{}) })
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if got := readFile(t, dst); got != "current" {
		t.Errorf("rollback restored the previous upgrade's set (containerd = %q)", got)
	}
	if m, _ := readUpgradeMarker(); m != nil {
		t.Errorf("marker %+v kept", m)
	}
	for _, want := range []string{"changed nothing", "restored nothing", "run `stratum rollback` again"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRollbackRestoresTheInterruptedSet: with an upgrade in progress, its set
// is the one to restore, not whichever happens to be newest.
func TestRollbackRestoresTheInterruptedSet(t *testing.T) {
	resumeFixture(t)
	interruptedAt(t, "v1.37.9")
	writeFile(t, filepath.Join(version.BinDir, "kubelet.bak-20261231-000000"), "other", 0o755)

	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{DryRun: true}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "stratum.bak-"+resumeTS) || strings.Contains(out, "kubelet.bak-20261231-000000") {
		t.Errorf("rollback must restore the interrupted upgrade's set %s only:\n%s", resumeTS, out)
	}
}

// TestUnreadableBackupDirIsRefused: a resume restores exactly the set the scan
// returns, so a directory it cannot read must stop the run rather than shrink
// the set and let a rollback claim the whole node.
func TestUnreadableBackupDirIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	resumeFixture(t)
	if err := os.Chmod(version.BinDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(version.BinDir, 0o755) })
	for cmd, run := range map[string]func() error{
		"upgrade":  func() error { return UpgradeNode(io.Discard, NodeOptions{}) },
		"rollback": func() error { return RollbackNode(io.Discard, RollbackOptions{}) },
	} {
		err := run()
		for _, want := range []string{"stratum " + cmd + ":", "scan backups in " + version.BinDir, "nothing was changed"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error missing %q: %v", cmd, want, err)
			}
		}
	}
}

// runWriter runs fn on a fresh buffer and returns what it wrote. The ops
// under test take the writer themselves now, so the capture is the buffer,
// not an os.Stdout redirect.
func runWriter(t *testing.T, fn func(io.Writer) error) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := fn(&b)
	return b.String(), err
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	var ferr error
	func() {
		defer func() { os.Stdout = old; w.Close() }()
		ferr = fn()
	}()
	return <-done, ferr
}

// TestBackupScanRequiresARealStamp: pruning only ever recognised NowStamp
// names, and the scan must agree — any other suffix sorts as the newest set
// and a rollback would restore it.
func TestBackupScanRequiresARealStamp(t *testing.T) {
	stratumDir, binDir, dataDir := backupFixture(t)
	writeFile(t, filepath.Join(stratumDir, "stratum.bak-"+resumeTS), "old", 0o755)
	writeFile(t, filepath.Join(stratumDir, "stratum.bak-zzz"), "stray", 0o755)
	writeFile(t, filepath.Join(binDir, "kubelet.bak-99999999-999999"), "stray", 0o755)
	if err := os.MkdirAll(filepath.Join(dataDir, "upgrade", "manifests-bak-latest"), 0o700); err != nil {
		t.Fatal(err)
	}

	sets, err := scanBackupSets()
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[resumeTS]) != 1 {
		t.Errorf("sets = %+v; want only the stratum backup under %s", sets, resumeTS)
	}
}

// TestFreshUpgradeNeverTakesAStampInUse: stamps have one-second precision, so
// a run started in the same second as another run's would otherwise adopt
// that run's backups as its own originals.
func TestFreshUpgradeNeverTakesAStampInUse(t *testing.T) {
	backupFixture(t)
	stamps := []string{resumeTS, resumeTS, "20260928-100001"}
	old := nowStamp
	nowStamp = func() string { s := stamps[0]; stamps = stamps[1:]; return s }
	t.Cleanup(func() { nowStamp = old })

	sets := map[string][]backupItem{resumeTS: {{name: "stratum", bak: "x"}}}
	ts, _, _, err := startBackupSet(io.Discard, nil, "v1.37.9", sets)
	if err != nil {
		t.Fatal(err)
	}
	if ts != "20260928-100001" {
		t.Errorf("fresh upgrade took stamp %q, which another run's backups use", ts)
	}
}

// TestSnapshotRollbackKeepsTheMarkerUntilEtcdIsRestored: the marker goes only
// once the whole rollback, etcd included, has succeeded. Removed before an
// etcd restore that then fails, a retry would find no marker and roll back
// the upgrade before this one. This fixture's etcd step
// fails (no node IP), so the marker must survive.
func TestSnapshotRollbackKeepsTheMarkerUntilEtcdIsRestored(t *testing.T) {
	resumeFixture(t)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{EtcdSnapshot: "/nonexistent.db"}) })
	if err == nil {
		t.Fatal("want the etcd step to fail in this fixture")
	}
	if m, _ := readUpgradeMarker(); m == nil {
		t.Error("the marker was removed before the etcd restore succeeded")
	}
	if strings.Contains(out, "removed its marker") {
		t.Errorf("claimed a removal before the rollback finished:\n%s", out)
	}
}

// etcdRollbackFixture stands in for the node IP, the single-member guard and
// the etcd restore that RollbackNode's snapshot step calls, and leaves a marker
// for the rollback to clear.
func etcdRollbackFixture(t *testing.T, restore func(io.Writer, etcdsnap.RestoreOptions) error) {
	t.Helper()
	resumeFixture(t)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	oldIP, oldMembers, oldRestore := rollbackNodeIP, rollbackMemberIPs, rollbackEtcdRestore
	rollbackNodeIP = func() (string, error) { return "192.0.2.10", nil }
	rollbackMemberIPs = func(string) ([]string, error) { return []string{"192.0.2.10"}, nil }
	rollbackEtcdRestore = restore
	t.Cleanup(func() { rollbackNodeIP, rollbackMemberIPs, rollbackEtcdRestore = oldIP, oldMembers, oldRestore })
}

// Snapshot rollback must give etcdutl the same bracketed IPv6 peer URL used
// by the running member and its single-member initial cluster.
func TestSnapshotRollbackUsesAnIPv6PeerURL(t *testing.T) {
	stop := errors.New("stop after inspecting the restore arguments")
	called := false
	etcdRollbackFixture(t, func(w io.Writer, o etcdsnap.RestoreOptions) error {
		called = true
		wantPeer := "https://[fd00::9]:2380"
		if o.PeerURL != wantPeer || o.InitialCluster != version.EtcdName+"="+wantPeer {
			t.Errorf("restore PeerURL=%q, InitialCluster=%q; want bracketed IPv6 membership", o.PeerURL, o.InitialCluster)
		}
		return stop
	})
	rollbackNodeIP = func() (string, error) { return "fd00::9", nil }
	rollbackMemberIPs = func(ip string) ([]string, error) {
		if ip != "fd00::9" {
			t.Errorf("quorum guard got node IP %q", ip)
		}
		return []string{ip}, nil
	}
	_, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{EtcdSnapshot: "/snap.db"}) })
	if !called || !errors.Is(err, stop) {
		t.Fatalf("restore called=%v, err=%v; want the checked restore arguments", called, err)
	}
}

// TestSnapshotRollbackWarnsWhenOnlyTheWatchersWereNotRestarted: the
// data is restored, so the rollback is finished: it warns, succeeds and clears
// the marker. Failing here would leave the marker and have a retry restore the
// snapshot again.
func TestSnapshotRollbackWarnsWhenOnlyTheWatchersWereNotRestarted(t *testing.T) {
	etcdRollbackFixture(t, func(_ io.Writer, _ etcdsnap.RestoreOptions) error {
		return fmt.Errorf("%w: kubelet", etcdsnap.ErrWatchersNotRestarted)
	})
	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{EtcdSnapshot: "/snap.db"}) })
	if err != nil {
		t.Fatalf("rollback failed after the data was restored: %v", err)
	}
	if !strings.Contains(out, "WARN: ") || !strings.Contains(out, "kubelet") {
		t.Errorf("no warning naming what was not restarted:\n%s", out)
	}
	otherNodeSteps(t, out)
	if m, _ := readUpgradeMarker(); m != nil {
		t.Error("the marker survived a finished rollback")
	}
}

// otherNodeSteps asserts out carries the list of what is left on the other nodes
// that `stratum snapshot restore` prints: the worker
// kubelets and the controllers the operator installed. A rollback restores a
// single-member cluster, so the control-plane rejoin items are not in it.
func otherNodeSteps(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{"Still to do:", "sudo pkill -x kubelet", "stratum kubectl get pods -A -o wide",
		"stratum kubectl -n <namespace> rollout restart deploy/<name>"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rollback's output lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Rejoin every OTHER control plane", "once EVERY one has rejoined"} {
		if strings.Contains(out, bad) {
			t.Errorf("a rollback has no control planes to rejoin, but its output has %q:\n%s", bad, out)
		}
	}
}

// TestSnapshotRollbackPrintsTheOtherNodeStepsAfterACleanRestore: a restore that
// restarted everything still leaves the other nodes' kubelets and third-party
// controllers, and said nothing about them on the rollback path before.
func TestSnapshotRollbackPrintsTheOtherNodeStepsAfterACleanRestore(t *testing.T) {
	etcdRollbackFixture(t, func(_ io.Writer, _ etcdsnap.RestoreOptions) error { return nil })
	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{EtcdSnapshot: "/snap.db"}) })
	if err != nil {
		t.Fatal(err)
	}
	otherNodeSteps(t, out)
	if strings.Contains(out, "WARN: ") {
		t.Errorf("a clean restore warned:\n%s", out)
	}
}

// TestSnapshotRollbackFailsOnAnyOtherRestoreError: an ordinary restore failure
// fails the rollback and keeps the marker, so a retry restores the same set.
func TestSnapshotRollbackFailsOnAnyOtherRestoreError(t *testing.T) {
	etcdRollbackFixture(t, func(_ io.Writer, _ etcdsnap.RestoreOptions) error { return errors.New("etcdutl exploded") })
	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{EtcdSnapshot: "/snap.db"}) })
	if err == nil || !strings.Contains(err.Error(), "etcdutl exploded") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out, "WARN: ") || strings.Contains(out, "Still to do:") {
		t.Errorf("an ordinary failure was reported as a warning, or told the operator what is left:\n%s", out)
	}
	if m, _ := readUpgradeMarker(); m == nil {
		t.Error("the marker was removed though the etcd restore failed")
	}
}

// TestMarkerThatWillNotGoIsReported: past the health gate a marker that
// survives would make the next run resume a finished upgrade into a
// rollback under new configs, so its removal must be able to fail loudly.
// A non-empty directory in its place cannot be removed.
func TestMarkerThatWillNotGoIsReported(t *testing.T) {
	backupFixture(t)
	writeFile(t, filepath.Join(upgradeMarkerPath(), "stuck"), "x", 0o600)
	if err := removeUpgradeMarker(); err == nil || !strings.Contains(err.Error(), "clear the in-progress upgrade marker") {
		t.Errorf("want an error for a marker that cannot be removed, got: %v", err)
	}
	if err := os.RemoveAll(upgradeMarkerPath()); err != nil {
		t.Fatal(err)
	}
	if err := removeUpgradeMarker(); err != nil {
		t.Errorf("an absent marker is already removed, got: %v", err)
	}
}

// TestKeptBackupMustBeABackup: a path that exists is kept only if a rollback
// can restore from it — a regular file, or a real, readable directory. Kept
// otherwise, the run would change the node and the rollback fail.
func TestKeptBackupMustBeABackup(t *testing.T) {
	dir := t.TempDir()
	file, sub := filepath.Join(dir, "file"), filepath.Join(dir, "sub")
	writeFile(t, file, "x", 0o600)
	writeFile(t, filepath.Join(sub, "m.yaml"), "x", 0o600)
	fileLink, dirLink := filepath.Join(dir, "file-link"), filepath.Join(dir, "dir-link")
	if err := os.Symlink(file, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, dirLink); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		path    string
		dir     bool
		kept    bool
		wantErr bool
	}{
		{"absent file", filepath.Join(dir, "none"), false, false, false},
		{"absent dir", filepath.Join(dir, "none"), true, false, false},
		{"regular file", file, false, true, false},
		{"real directory", sub, true, true, false},
		{"directory where a file belongs", sub, false, false, true},
		{"file where a directory belongs", file, true, false, true},
		{"symlinked file", fileLink, false, false, true},
		{"symlinked directory", dirLink, true, false, true},
	} {
		kept, err := keptBackup(c.path, c.dir)
		if kept != c.kept || (err != nil) != c.wantErr {
			t.Errorf("%s: keptBackup = %v, %v; want kept=%v err=%v", c.name, kept, err, c.kept, c.wantErr)
		}
	}
	// A backup directory is usable only if everything in it is: a restore
	// copies each entry.
	withLink, withDir := filepath.Join(dir, "with-link"), filepath.Join(dir, "with-dir")
	writeFile(t, filepath.Join(withLink, "etcd.yaml"), "x", 0o600)
	if err := os.Symlink("/etc/passwd", filepath.Join(withLink, "kube-apiserver.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(withDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"a symlink inside": withLink, "a directory inside": withDir} {
		if kept, err := keptBackup(p, true); kept || err == nil {
			t.Errorf("backup dir with %s: keptBackup = %v, %v; want an error", name, kept, err)
		}
	}
	if os.Geteuid() != 0 {
		unreadable := filepath.Join(sub, "m.yaml")
		if err := os.Chmod(unreadable, 0o000); err != nil {
			t.Fatal(err)
		}
		if kept, err := keptBackup(sub, true); kept || err == nil {
			t.Errorf("backup dir with an unreadable entry: keptBackup = %v, %v; want an error", kept, err)
		}
		if err := os.Chmod(unreadable, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
		if kept, err := keptBackup(sub, true); kept || err == nil {
			t.Errorf("unreadable directory: keptBackup = %v, %v; want an error", kept, err)
		}
	}
}

// TestInstallRefusesAnUnusableBackupPath: rather than keep it and install
// over the live binary with nothing to restore.
func TestInstallRefusesAnUnusableBackupPath(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "stratum"), filepath.Join(dir, "next")
	writeFile(t, dst, "old", 0o755)
	writeFile(t, src, "new", 0o755)
	if err := os.MkdirAll(dst+".bak-"+resumeTS, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backupAndInstall(io.Discard, dst, src, resumeTS); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want a refusal, got: %v", err)
	}
	if got := readFile(t, dst); got != "old" {
		t.Errorf("installed over the live binary with no usable backup (dst = %q)", got)
	}
}

// TestResumeRefusesAnUnreadableKeptBackup: checked before anything changes,
// not discovered as an INCOMPLETE rollback after.
func TestResumeRefusesAnUnreadableKeptBackup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	backupFixture(t)
	bak := manifestBakDir(resumeTS)
	writeFile(t, filepath.Join(bak, "etcd.yaml"), "x", 0o600)
	sets, err := scanBackupSets()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bak, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bak, 0o755) })
	_, err = runWriter(t, func(w io.Writer) error {
		_, _, _, err := startBackupSet(w, &upgradeMarker{TS: resumeTS, Target: "v1.37.9"}, "v1.37.9", sets)
		return err
	})
	for _, want := range []string{"manifests backup is unusable", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestSnapshotRollbackDryRunRemovesNothing: a preview says what it would do.
func TestSnapshotRollbackDryRunRemovesNothing(t *testing.T) {
	resumeFixture(t)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	out, _ := runWriter(t, func(w io.Writer) error {
		return RollbackNode(w, RollbackOptions{DryRun: true, EtcdSnapshot: "/nonexistent.db"})
	})
	if !strings.Contains(out, "would remove the marker") || strings.Contains(out, "removed its marker") {
		t.Errorf("dry run must only say what it would do:\n%s", out)
	}
	if m, _ := readUpgradeMarker(); m == nil {
		t.Error("a dry run removed the marker")
	}
}

// TestRollbackRefusesAnUnusableBackupBeforeRestoring: found part-way through,
// the backups before it would already be restored and the node left mixed;
// checked first, nothing changes.
func TestRollbackRefusesAnUnusableBackupBeforeRestoring(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	resumeFixture(t)
	writeFile(t, stratumInstallPath, "new", 0o755)
	writeFile(t, stratumInstallPath+".bak-"+resumeTS, "old", 0o755)
	bak := manifestBakDir(resumeTS)
	writeFile(t, filepath.Join(bak, "etcd.yaml"), "x", 0o600)
	if err := os.Chmod(bak, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bak, 0o755) })

	err := RollbackNode(io.Discard, RollbackOptions{})
	for _, want := range []string{"stratum rollback:", "manifests backup of set " + resumeTS + " is unusable", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if got := readFile(t, stratumInstallPath); got != "new" {
		t.Errorf("rollback restored the stratum binary before finding the unusable backup (stratum = %q)", got)
	}
}

// TestScanRefusesAWrongKindOfBackup: an entry named as a backup, with a real
// stamp, that is the wrong kind is refused, not skipped. Skipped, the marker's
// set could come out empty, read as an upgrade that changed nothing, and have
// its marker and baseline discarded.
func TestScanRefusesAWrongKindOfBackup(t *testing.T) {
	for name, plant := range map[string]func(stratumDir, binDir, dataDir string) error{
		"directory as the stratum backup": func(s, _, _ string) error { return os.MkdirAll(filepath.Join(s, "stratum.bak-"+resumeTS), 0o700) },
		"symlink as a host-binary backup": func(_, b, _ string) error {
			return os.Symlink("/etc/passwd", filepath.Join(b, "containerd.bak-"+resumeTS))
		},
		"file as the manifests backup": func(_, _, d string) error {
			p := filepath.Join(d, "upgrade", "manifests-bak-"+resumeTS)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			return os.WriteFile(p, []byte("x"), 0o600)
		},
		"file as the CIS config backup": func(_, _, d string) error {
			p := filepath.Join(d, "upgrade", "cisconfig-bak-"+resumeTS)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			return os.WriteFile(p, []byte("x"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			resumeFixture(t)
			if err := plant(filepath.Dir(stratumInstallPath), version.BinDir, version.DataDir); err != nil {
				t.Fatal(err)
			}
			if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.8"}); err != nil {
				t.Fatal(err)
			}
			err := UpgradeNode(io.Discard, NodeOptions{})
			for _, want := range []string{"is named as a backup but is not", "nothing was changed"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			if m, _ := readUpgradeMarker(); m == nil {
				t.Error("the marker was dropped as if its upgrade changed nothing")
			}
		})
	}
}

// TestInstallRefusesAnUnreadableLiveFile: only an absent file has nothing to
// back up; any other read error must stop the install, or the file is
// replaced with no way back.
func TestInstallRefusesAnUnreadableLiveFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "kubelet"), filepath.Join(dir, "next")
	writeFile(t, dst, "old", 0o000)
	writeFile(t, src, "new", 0o755)
	if _, err := backupAndInstall(io.Discard, dst, src, resumeTS); err == nil {
		t.Fatal("want a refusal for an unreadable live file")
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dst); got != "old" {
		t.Errorf("installed over an unreadable live file with no backup (dst = %q)", got)
	}

	// Absent is the one case with nothing to back up.
	fresh := filepath.Join(dir, "added")
	if bak, err := backupAndInstall(io.Discard, fresh, src, resumeTS); err != nil || bak != "" {
		t.Errorf("a file the payload adds must install with no backup: bak %q, %v", bak, err)
	}
}

// TestRollbackSaysSoWhenTheMarkerWillNotGo: "removed its marker", and "run it
// again", are only true once it is gone; otherwise the next run lands in the
// same place. A read-only upgrade dir keeps the marker
// readable and unremovable.
func TestRollbackSaysSoWhenTheMarkerWillNotGo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a read-only directory")
	}
	// Without a snapshot: with one, the marker goes only after an etcd
	// restore, which no unit test reaches.
	for name, opts := range map[string]RollbackOptions{
		"no snapshot": {},
	} {
		t.Run(name, func(t *testing.T) {
			resumeFixture(t)
			if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(upgradeMarkerPath())
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, opts) })
			for _, want := range []string{"stratum rollback:", "clear the in-progress upgrade marker", "nothing was changed"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			if strings.Contains(out, "removed its marker") {
				t.Errorf("claimed a removal that failed:\n%s", out)
			}
		})
	}
}

// TestFreshMarkerWriteFailureChangesNothing: the marker is written before the
// first change, so a failure to write it says so, like every other refusal
// before one.
func TestFreshMarkerWriteFailureChangesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	backupFixture(t)
	dir := filepath.Dir(upgradeMarkerPath())
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, _, _, err := startBackupSet(io.Discard, nil, "v1.37.9", nil)
	for _, want := range []string{"stratum upgrade:", "record the in-progress upgrade", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestInstallReportsTheBackupItTook: the caller records exactly the backup
// backupAndInstall reports — taken or kept — rather than asking the
// filesystem again.
func TestInstallReportsTheBackupItTook(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "kubelet"), filepath.Join(dir, "next")
	writeFile(t, dst, "old", 0o755)
	writeFile(t, src, "new", 0o755)
	want := dst + ".bak-" + resumeTS
	for run := 1; run <= 2; run++ { // taken, then kept
		bak, err := backupAndInstall(io.Discard, dst, src, resumeTS)
		if err != nil || bak != want {
			t.Errorf("run %d: backupAndInstall = %q, %v; want %q", run, bak, err, want)
		}
	}
}
