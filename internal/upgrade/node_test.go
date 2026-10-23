package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Kubernetes v1.36.2", "v1.36.2", true},
		{"Kubernetes v1.36.2-stratum", "v1.36.2", true},                 // SKE build suffix trimmed: Target is the bare upstream version
		{"Kubernetes v1.37.1-rc.1", "v1.37.1-rc.1", true},               // an upstream prerelease is not the release
		{"Kubernetes v1.37.1-rc.1-stratum", "v1.37.1-rc.1", true},       // ...with or without the SKE suffix
		{"Kubernetes v1.30.0-gke.1449000", "v1.30.0-gke.1449000", true}, // a vendor build is kept as reported
		{"Kubernetes v1.35.6\n", "v1.35.6", true},
		{"etcd Version: 3.5.16", "", false}, // no v-prefixed dotted token
		{"garbage", "", false},
	}
	for _, tt := range tests {
		got, err := parseVersion(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("parseVersion(%q) = %q,err=%v want %q,ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

// redirect points a package path var at v for the duration of the test.
func redirect(t *testing.T, p *string, v string) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// mustNewestSet is the newest backup set, failing the test on a scan error
// rather than reading it as "no backups".
func mustNewestSet(t *testing.T) (string, []backupItem) {
	t.Helper()
	sets, err := scanBackupSets()
	if err != nil {
		t.Fatal(err)
	}
	return newestSet(sets)
}

// backupFixture redirects every path scanBackupSets scans to temp dirs and
// returns them (stratum dir, bin dir, DataDir).
func backupFixture(t *testing.T) (stratumDir, binDir, dataDir string) {
	t.Helper()
	stratumDir, binDir, dataDir = t.TempDir(), t.TempDir(), t.TempDir()
	redirect(t, &stratumInstallPath, filepath.Join(stratumDir, "stratum"))
	redirect(t, &version.BinDir, binDir)
	redirect(t, &version.DataDir, dataDir)
	return stratumDir, binDir, dataDir
}

// TestLatestBackupSet: the newest <ts> wins, and ONLY artifacts carrying that
// exact stamp are in the set — an artifact whose newest backup is older (here
// containerd) must not be mixed in from another run.
func TestLatestBackupSet(t *testing.T) {
	stratumDir, binDir, dataDir := backupFixture(t)
	const tsOld, tsNew = "20260101-000000", "20260628-120000"

	for _, n := range []string{
		"kubelet", "kubelet.bak-" + tsOld, "kubelet.bak-" + tsNew,
		"containerd", "containerd.bak-" + tsOld, // no backup in the newest run
		"runc.tmp", // installer leftover, never a backup
	} {
		if err := os.WriteFile(filepath.Join(binDir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"stratum", "stratum.bak-" + tsOld, "stratum.bak-" + tsNew} {
		if err := os.WriteFile(filepath.Join(stratumDir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, ts := range []string{tsOld, tsNew} {
		if err := os.MkdirAll(filepath.Join(dataDir, "upgrade", "manifests-bak-"+ts), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ts, items := mustNewestSet(t)
	if ts != tsNew {
		t.Fatalf("ts = %q, want %q", ts, tsNew)
	}
	got := map[string]backupItem{}
	for _, it := range items {
		got[it.name] = it
	}
	if len(got) != 3 {
		t.Fatalf("items = %v, want exactly stratum, manifests, kubelet", items)
	}
	for _, name := range []string{"stratum", "manifests", "kubelet"} {
		it, ok := got[name]
		if !ok {
			t.Fatalf("set is missing %s: %v", name, items)
		}
		if !strings.HasSuffix(it.bak, tsNew) {
			t.Errorf("%s backup %q is not from set %s", name, it.bak, tsNew)
		}
	}
	if _, ok := got["containerd"]; ok {
		t.Error("containerd's older backup was mixed into the newest set")
	}
	if !got["manifests"].dir || got["stratum"].dir {
		t.Errorf("dir flags wrong: %v", items)
	}
	if got["stratum"].dst != stratumInstallPath {
		t.Errorf("stratum dst = %q, want %q", got["stratum"].dst, stratumInstallPath)
	}
}

// TestPruneOldBackups: pruning keeps the newest keep .bak-<ts> sets and removes
// every older one, across a stamp that spans several files in the dir; a live
// binary and an installer leftover (neither a backup) are never touched, and the
// newest set RollbackNode needs must always survive.
func TestPruneOldBackups(t *testing.T) {
	dir := t.TempDir()
	const ts1, ts2, ts3 = "20260101-000000", "20260202-000000", "20260303-000000"

	// One stamp spans two host-binary backups; three stamps in all, oldest first.
	backups := []string{
		"kubelet.bak-" + ts1, "containerd.bak-" + ts1,
		"kubelet.bak-" + ts2, "containerd.bak-" + ts2,
		"kubelet.bak-" + ts3, "containerd.bak-" + ts3,
	}
	// Non-backup entries that must be left completely alone.
	keepAlways := []string{"kubelet", "containerd", "runc.tmp", "other-tool.bak-20250101-000000", "kubelet.bak-", "kubelet.bak-aaa", "kubelet.bak-zzz", "kubelet.bak-20261301-000000", "kubelet.bak-20260230-000000"}
	for _, n := range append(append([]string{}, backups...), keepAlways...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	patterns := []backupPattern{{prefix: "kubelet.bak-"}, {prefix: "containerd.bak-"}}
	if err := pruneOldBackups(dir, 2, patterns...); err != nil {
		t.Fatalf("pruneOldBackups: %v", err)
	}

	// The oldest set (ts1) is gone; the newest two (ts2, ts3) survive intact.
	for _, n := range []string{"kubelet.bak-" + ts1, "containerd.bak-" + ts1} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("stale backup %q was not pruned (err=%v)", n, err)
		}
	}
	for _, n := range []string{
		"kubelet.bak-" + ts2, "containerd.bak-" + ts2,
		"kubelet.bak-" + ts3, "containerd.bak-" + ts3,
	} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("newest backup %q must survive: %v", n, err)
		}
	}
	// Non-backup files untouched.
	for _, n := range keepAlways {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("non-backup file %q must be left alone: %v", n, err)
		}
	}

	// Manifest backup dirs use the "manifests-bak-<ts>" scheme and are pruned
	// the same way — whole directory sets removed together.
	updir := t.TempDir()
	for _, ts := range []string{ts1, ts2, ts3} {
		if err := os.MkdirAll(filepath.Join(updir, "manifests-bak-"+ts), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifestPattern := backupPattern{prefix: "manifests-bak-", directory: true}
	if err := pruneOldBackups(updir, 2, manifestPattern); err != nil {
		t.Fatalf("pruneOldBackups (manifests): %v", err)
	}
	if _, err := os.Stat(filepath.Join(updir, "manifests-bak-"+ts1)); !os.IsNotExist(err) {
		t.Errorf("stale manifest backup set %s was not pruned (err=%v)", ts1, err)
	}
	for _, ts := range []string{ts2, ts3} {
		if _, err := os.Stat(filepath.Join(updir, "manifests-bak-"+ts)); err != nil {
			t.Errorf("newest manifest backup set %s must survive: %v", ts, err)
		}
	}

	// A dir with keep-or-fewer sets, and a missing dir, are both no-ops.
	if err := pruneOldBackups(updir, 2, manifestPattern); err != nil {
		t.Errorf("prune with <= keep sets must be a no-op: %v", err)
	}
	if err := pruneOldBackups(filepath.Join(dir, "does-not-exist"), 2, patterns...); err != nil {
		t.Errorf("prune of a missing dir must be a no-op: %v", err)
	}
}

func TestPruneOldBackupsSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"stratum.bak-20260101-000000", "stratum.bak-20260201-000000", "stratum.bak-20260301-000000"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("binary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Valid timestamps alone do not establish ownership. Neither unrelated
	// directories nor directories masquerading as binary backups may be removed.
	for _, name := range []string{"other-tool.bak-20240101-000000", "stratum.bak-20240101-000000", "manifests-bak-20240101-000000"} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep"), []byte("user data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("stratum.bak-20260201-000000", filepath.Join(dir, "stratum.bak-20250101-000000")); err != nil {
		t.Fatal(err)
	}
	if err := pruneOldBackups(dir, 2, backupPattern{prefix: "stratum.bak-"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stratum.bak-20260101-000000")); !os.IsNotExist(err) {
		t.Fatalf("old Stratum backup not removed: %v", err)
	}
	for _, name := range []string{"other-tool.bak-20240101-000000/keep", "stratum.bak-20240101-000000/keep", "manifests-bak-20240101-000000/keep", "stratum.bak-20250101-000000", "stratum.bak-20260201-000000", "stratum.bak-20260301-000000"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("protected artifact %s changed: %v", name, err)
		}
	}
}

// probeFixture forces ensureNodeUp's supervisor/unit probes for the test.
func probeFixture(t *testing.T, sup bool, active map[string]bool) {
	t.Helper()
	// Every run that passes the guard takes the node lock next.
	redirect(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	oldSup, oldAct := supervisorRunning, unitActive
	supervisorRunning = func() bool { return sup }
	unitActive = func(_ context.Context, name string) bool { return active[name] }
	t.Cleanup(func() { supervisorRunning, unitActive = oldSup, oldAct })
	runtimeFixture(t, "v1.0.0", "v1.0.0", "v1.0.0")
	oldRunning := runningSupervisor
	runningSupervisor = func() (comps.RunningSupervisor, error) {
		return comps.RunningSupervisor{PID: 1, Exe: version.StratumInstallPath, Installed: true}, nil
	}
	t.Cleanup(func() { runningSupervisor = oldRunning })
}

// testNode is the node name the gate tests look up, and kubeletProbe is the
// kubelet check on it, in the shape the probe table takes.
const testNode = "node-1"

func kubeletProbe(ctx context.Context) error { return kubeletServesInstalled(ctx, testNode) }

// runtimeFixture redirects the running-version probes: disk is what both
// binaries print for --version, ctrd what the CRI socket reports, kubelet what
// the Node's nodeInfo does. The default (all equal) is a node that is what it
// was installed to be.
func runtimeFixture(t *testing.T, disk, ctrd, kubelet string) {
	t.Helper()
	oldI, oldS, oldK, oldW := installedVersion, servingContainerd, kubeletReports, runtimeProbeWindow
	installedVersion = func(context.Context, string) (string, error) { return disk, nil }
	servingContainerd = func(context.Context, string) (string, error) { return ctrd, nil }
	kubeletReports = func(context.Context, string) (string, error) { return kubelet, nil }
	runtimeProbeWindow = 100 * time.Millisecond
	t.Cleanup(func() {
		installedVersion, servingContainerd, kubeletReports, runtimeProbeWindow = oldI, oldS, oldK, oldW
	})
}

// nodeUp is the happy-path probe state: supervisor serving, host procs active.
func nodeUp(t *testing.T) {
	t.Helper()
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
}

// TestEnsureNodeUpSupervisorDown: with no supervisor answering, both commands
// refuse before selecting or touching anything, and the error says how to
// proceed and that nothing was changed.
func TestEnsureNodeUpSupervisorDown(t *testing.T) {
	backupFixture(t)
	probeFixture(t, false, nil)
	for cmd, run := range map[string]func() error{
		"upgrade":  func() error { return UpgradeNode(io.Discard, NodeOptions{}) },
		"rollback": func() error { return RollbackNode(io.Discard, RollbackOptions{}) },
	} {
		err := run()
		if err == nil {
			t.Fatalf("%s: expected an error with the supervisor down", cmd)
		}
		for _, want := range []string{"no stratum supervisor is running", "nothing was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s error missing %q: %v", cmd, want, err)
			}
		}
	}
}

// TestEnsureNodeUpKubeletInactive: the upgrade restarts and gates on every
// host process, so it refuses while one is down. It names both causes as
// conditions, and offers rollback only when an interrupted upgrade left
// something to roll back: on a node merely still starting, a rollback would
// restore the upgrade before the last good one.
func TestEnsureNodeUpKubeletInactive(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true})
	err := UpgradeNode(io.Discard, NodeOptions{})
	for _, want := range []string{"kubelet is not active", "if `stratum up` is still bootstrapping, wait for it to finish and retry", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("upgrade error missing %q: %v", want, err)
		}
	}
	if err != nil && strings.Contains(err.Error(), "stratum rollback") {
		t.Errorf("with no interrupted upgrade, the refusal must not suggest a rollback: %v", err)
	}

	// An interrupted upgrade whose set restores no host binary: the rollback
	// would keep the full guard and refuse on the same down unit, so no hint.
	writeTestFile(t, stratumInstallPath+".bak-20260928-100000", "old")
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: "20260928-100000", Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	err = UpgradeNode(io.Discard, NodeOptions{})
	if err != nil && strings.Contains(err.Error(), "stratum rollback") {
		t.Errorf("a set with no host binary cannot recover the unit, so no rollback hint: %v", err)
	}

	// One that restores kubelet can: the hint names the interrupted target.
	writeTestFile(t, filepath.Join(version.BinDir, "kubelet.bak-20260928-100000"), "old")
	err = UpgradeNode(io.Discard, NodeOptions{})
	if err == nil || !strings.Contains(err.Error(), "crash-looping after the interrupted upgrade to v1.37.9, `stratum rollback` restores the version before it") {
		t.Errorf("with an interrupted upgrade that restores a host binary, the refusal must point at rollback: %v", err)
	}
}

// TestRollbackRunsWithAKubeletDown: a kubelet crash-looping on an
// interrupted upgrade's binaries is the state rollback exists to recover. The
// supervisor answering is what proves bootstrap's manifest write is done, so
// rollback gets past the guard to backup-set selection.
func TestRollbackRunsWithAKubeletDown(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true})
	err := RollbackNode(io.Discard, RollbackOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Errorf("rollback should get past the guard to backup-set selection, got: %v", err)
	}
}

// rollbackRestartFixture is a worker whose kubelet is down and has a backup to
// restore, with the supervisor's restart and roster stubbed: restartUnit
// records each call and, unless fail names the unit, brings it up.
func rollbackRestartFixture(t *testing.T, fail string) (restarted *[]string) {
	t.Helper()
	_, binDir, _ := backupFixture(t)
	confDir := t.TempDir()
	redirect(t, &version.K8sConfDir, confDir)
	writeTestFile(t, filepath.Join(confDir, "node.yaml"), "role: worker\n")
	writeTestFile(t, filepath.Join(binDir, "kubelet"), "new")
	writeTestFile(t, filepath.Join(binDir, "kubelet.bak-20260928-100000"), "old")

	active := map[string]bool{"containerd": true}
	probeFixture(t, true, active)
	var calls []string
	oldExists, oldRestart, oldWindow := unitExists, restartUnit, rollbackGateWindow
	unitExists = func(name string) bool { return name == "containerd" || name == "kubelet" }
	restartUnit = func(name string) error {
		calls = append(calls, name)
		if name == fail {
			return errors.New("did not come back within 90s of restart")
		}
		active[name] = true
		return nil
	}
	rollbackGateWindow = time.Second
	t.Cleanup(func() { unitExists, restartUnit, rollbackGateWindow = oldExists, oldRestart, oldWindow })
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
	swap(t, &bootClock, func() (staticpod.Mark, error) { return staticpod.Mark{}, nil })
	swap(t, &waitStaticPodReady, func(string, time.Duration) error { return nil })
	swap(t, &waitComponentReplace, func(string, staticpod.Mark, time.Duration) error { return nil })
	return &calls
}

// TestRollbackRestartsTheUnitItRecovers: after restoring, a
// rollback restarts every host unit the supervisor manages — the down kubelet
// most of all. Skipping units that were not running left the one this exists
// to recover to the supervisor's own backoff, and the gate to a coin toss.
func TestRollbackRestartsTheUnitItRecovers(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	if err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1}); err != nil {
		t.Fatalf("rollback of a node with its kubelet down: %v", err)
	}
	if got := strings.Join(*restarted, ","); got != "containerd,kubelet" {
		t.Errorf("restarted %q, want containerd,kubelet", got)
	}
	if got := readTestFile(t, filepath.Join(version.BinDir, "kubelet")); got != "old" {
		t.Errorf("kubelet = %q, want the restored %q", got, "old")
	}
}

// TestRollbackReportsAUnitThatDidNotComeBack: restored files with a host
// process that never came back on them is not a rollback that succeeded.
func TestRollbackReportsAUnitThatDidNotComeBack(t *testing.T) {
	rollbackRestartFixture(t, "kubelet")
	err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1})
	for _, want := range []string{"restored backup set 20260928-100000", "did not come back", "kubelet"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestEnsureNodeUpHappyPath: with the node fully up the guard passes and each
// sequence proceeds to its next step (node-state load / backup-set selection).
func TestEnsureNodeUpHappyPath(t *testing.T) {
	backupFixture(t)
	redirect(t, &version.K8sConfDir, t.TempDir()) // no node.yaml there
	nodeUp(t)

	err := UpgradeNode(io.Discard, NodeOptions{})
	if err == nil || !strings.Contains(err.Error(), "node state") {
		t.Errorf("upgrade should get past the guard to the node-state load, got: %v", err)
	}
	err = RollbackNode(io.Discard, RollbackOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Errorf("rollback should get past the guard to backup-set selection, got: %v", err)
	}
}

// TestUpgradeNodeDryRunSkipsGuard: --dry-run must preview even on a half-up
// node — it writes nothing, so the race the guard exists for cannot happen.
func TestUpgradeNodeDryRunSkipsGuard(t *testing.T) {
	backupFixture(t) // empty BinDir: no kubelet, so the idempotency probe is skipped
	probeFixture(t, false, nil)
	confDir := t.TempDir()
	redirect(t, &version.K8sConfDir, confDir)
	if err := os.WriteFile(filepath.Join(confDir, "node.yaml"), []byte("role: worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := UpgradeNode(&buf, NodeOptions{DryRun: true})

	if err != nil {
		t.Fatalf("dry-run must not be blocked by the guard: %v", err)
	}
	if !strings.Contains(buf.String(), "would upgrade") {
		t.Errorf("dry-run output missing the preview:\n%s", buf.String())
	}
}

// TestRollbackNodeNoBackups: with no backup set anywhere and no EtcdSnapshot,
// rollback must fail with a clear error before touching anything.
func TestRollbackNodeNoBackups(t *testing.T) {
	backupFixture(t)
	nodeUp(t)
	err := RollbackNode(io.Discard, RollbackOptions{})
	if err == nil {
		t.Fatal("expected an error with no backup set present")
	}
	if !strings.Contains(err.Error(), "nothing to roll back") {
		t.Errorf("error should say there is nothing to roll back, got: %v", err)
	}
}

// TestRollbackNodeDryRun: --dry-run prints every artifact of the selected set
// and changes nothing on disk.
func TestRollbackNodeDryRun(t *testing.T) {
	stratumDir, binDir, _ := backupFixture(t)
	const ts = "20260628-120000"
	live := map[string]string{
		filepath.Join(binDir, "kubelet"):             "new-kubelet",
		filepath.Join(stratumDir, "stratum"):         "new-stratum",
		filepath.Join(binDir, "kubelet.bak-"+ts):     "old-kubelet",
		filepath.Join(stratumDir, "stratum.bak-"+ts): "old-stratum",
	}
	for p, c := range live {
		if err := os.WriteFile(p, []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	err := RollbackNode(&buf, RollbackOptions{DryRun: true})

	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{"would restore stratum", "would restore kubelet"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("dry-run output missing %q:\n%s", want, buf.String())
		}
	}
	for p, c := range live {
		if b, _ := os.ReadFile(p); string(b) != c {
			t.Errorf("dry-run mutated %s: %q", p, b)
		}
	}
}

// TestCPComponents: the re-render order is staticpod.NamesFor verbatim — the
// kube-vip-only-with-VIP rule read from its owner, not restated here — with
// etcd first, always included. Pinned against NamesFor for a VIP and a no-VIP
// state, so the two rosters cannot drift apart again.
func TestCPComponents(t *testing.T) {
	vip := &nodestate.NodeState{Role: "controller", VIP: nodestate.VIP{Address: "192.0.2.100"}}
	if got := cpComponents(vip); !reflect.DeepEqual(got, staticpod.NamesFor(vip)) {
		t.Errorf("cpComponents(VIP) = %v, want staticpod.NamesFor(VIP) %v", got, staticpod.NamesFor(vip))
	}
	noVIP := &nodestate.NodeState{Role: "controller"}
	if got := cpComponents(noVIP); !reflect.DeepEqual(got, staticpod.NamesFor(noVIP)) {
		t.Errorf("cpComponents(no VIP) = %v, want staticpod.NamesFor(no VIP) %v — the VIP rule lives in staticpod alone", got, staticpod.NamesFor(noVIP))
	}
	if got := cpComponents(noVIP); got[0] != "etcd" {
		t.Errorf("etcd must be first (and unconditionally included), got %v", got)
	}
}

// TestHostSwapOrder: kubelet is the node version marker, so it must be
// installed LAST; backup/temp leftovers are never treated as payload.
func TestHostSwapOrder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"kubelet", "containerd", "runc", "kubelet.bak-20260101-000000", "containerd.tmp",
		// assets.extract can leave these behind if extraction is killed.
		".kubelet.tmp-3891241085", ".containerd.tmp-1234567890",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := hostSwapOrder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"containerd", "runc", "kubelet"}) {
		t.Errorf("hostSwapOrder = %v, want containerd, runc, then kubelet last", got)
	}
}

// TestUpgradeNodeTargetMismatch: the guard fires before any node state is
// read or any change is made — staging the wrong artifact must be a clean
// early error.
func TestUpgradeNodeTargetMismatch(t *testing.T) {
	err := UpgradeNode(io.Discard, NodeOptions{Target: "v9.9.9"})
	if err == nil {
		t.Fatal("expected an error for --target != the executing binary's K8sVersion")
	}
	if !strings.Contains(err.Error(), "v9.9.9") || !strings.Contains(err.Error(), version.K8sVersion) {
		t.Errorf("error should name both versions, got: %v", err)
	}
}

// TestManifestBakDirOutsideStaticPodPath: kubelet parses every file in its
// staticPodPath and would adopt a backup as a second static pod, so the
// backup dir must never live inside it.
func TestManifestBakDirOutsideStaticPodPath(t *testing.T) {
	bak := manifestBakDir("20260828-000000")
	if strings.HasPrefix(bak+string(filepath.Separator), staticpod.Dir()+string(filepath.Separator)) {
		t.Errorf("manifest backup dir %s is inside the kubelet staticPodPath %s", bak, staticpod.Dir())
	}
}

// TestRestoreAll: good items are restored byte-identically (files, and the
// manifest set into the manifest dir staticpod owns), and a failed restore is
// reported by name instead of aborting the rest.
func TestRestoreAll(t *testing.T) {
	dir := t.TempDir()

	// A binary whose backup exists.
	binBak := filepath.Join(dir, "kubelet.bak-ts")
	binDst := filepath.Join(dir, "kubelet")
	os.WriteFile(binBak, []byte("old-kubelet"), 0o755)
	os.WriteFile(binDst, []byte("new-kubelet"), 0o755)

	// A manifest set backup: its restore lands in the manifest dir, so the
	// config directory the derived files live beside must not be the real one.
	redirect(t, &version.K8sConfDir, t.TempDir())
	manBak := filepath.Join(dir, "manifests-bak")
	os.MkdirAll(manBak, 0o700)
	os.WriteFile(filepath.Join(manBak, "etcd.yaml"), []byte("old-etcd"), 0o600)
	os.WriteFile(filepath.Join(staticpod.Dir(), "etcd.yaml"), []byte("new-etcd"), 0o600)

	items := []backupItem{
		{name: "stratum", bak: filepath.Join(dir, "missing.bak"), dst: filepath.Join(dir, "stratum")},
		{name: "manifests", bak: manBak, dst: staticpod.Dir(), dir: true},
		{name: "kubelet", bak: binBak, dst: binDst},
	}
	failed := restoreAll(io.Discard, items)
	if !reflect.DeepEqual(failed, []string{"stratum"}) {
		t.Errorf("failed = %v, want [stratum] (missing backup must not abort the other restores)", failed)
	}
	if b, _ := os.ReadFile(binDst); string(b) != "old-kubelet" {
		t.Errorf("kubelet not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(staticpod.Dir(), "etcd.yaml")); string(b) != "old-etcd" {
		t.Errorf("manifest not restored: %q", b)
	}
}

// TestRollbackNodeIncompleteError: the honesty contract — a rollback whose
// restores failed must say "rollback was INCOMPLETE" and name what is still
// on the new version.
func TestRollbackNodeIncompleteError(t *testing.T) {
	dir := t.TempDir()
	cause := os.ErrDeadlineExceeded

	err := rollbackNode(io.Discard, cause, []backupItem{
		{name: "stratum", bak: filepath.Join(dir, "no-such.bak"), dst: filepath.Join(dir, "stratum")},
	}, false, "20260828-000000")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rollback was INCOMPLETE") || !strings.Contains(msg, "stratum") {
		t.Errorf("error must say INCOMPLETE and name the item, got: %v", err)
	}
	if !strings.Contains(msg, cause.Error()) {
		t.Errorf("error must wrap the original failure, got: %v", err)
	}

	// A rollback whose restores all succeed must NOT claim incompleteness.
	bak := filepath.Join(dir, "kubelet.bak")
	dst := filepath.Join(dir, "kubelet")
	os.WriteFile(bak, []byte("old"), 0o755)
	os.WriteFile(dst, []byte("new"), 0o755)
	err = rollbackNode(io.Discard, cause, []backupItem{{name: "kubelet", bak: bak, dst: dst}}, false, "ts")
	if err == nil || strings.Contains(err.Error(), "INCOMPLETE") {
		t.Errorf("clean rollback must not claim INCOMPLETE: %v", err)
	}

	// A host process that does not come back on its restored binary is not a
	// rollback done, and the marker stays for a retry.
	swap(t, &unitActive, func(context.Context, string) bool { return false })
	swap(t, &unitExists, func(name string) bool { return name == "containerd" || name == "kubelet" })
	var restartedUnits []string
	swap(t, &restartUnit, func(name string) error {
		restartedUnits = append(restartedUnits, name)
		if name == "kubelet" {
			return errors.New("did not come back within 90s of restart")
		}
		return nil
	})
	redirect(t, &version.DataDir, t.TempDir())
	marker := upgradeMarkerPath()
	writeTestFile(t, marker, "{}")
	err = rollbackNode(io.Discard, cause, []backupItem{{name: "kubelet", bak: bak, dst: dst}}, true, "ts")
	if err == nil || !strings.Contains(err.Error(), "INCOMPLETE") || !strings.Contains(err.Error(), "kubelet (did not come back") {
		t.Errorf("rollback whose kubelet did not come back = %v, want INCOMPLETE naming it", err)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Error("the marker was cleared although the rollback is incomplete")
	}
	if got := strings.Join(restartedUnits, ","); got != "containerd,kubelet" {
		t.Errorf("restarted %q, want every installed host process, the stopped ones above all", got)
	}
}

// TestCopyFileStagingName: the root cause of a staging-name collision, and
// a collision the staging must not introduce. The kubelet's staticPodPath
// file source globs `[^.]*` — it parses every NON-hidden file there
// whatever the name, and hashes the file path into the pod UID, so a
// visible "etcd.yaml.tmp" lands under etcd's own namespace/name key as
// a different pod and displaces the running one. The staging name must
// therefore be hidden — atomicfile's unique hidden temp name, whose
// uniqueness also keeps two concurrent writers from tearing a single
// manifest through one shared staging path.
//
// The staging file is renamed away (and cleaned up on error) too fast to
// observe, so dst here is a DIRECTORY: the rename fails and names the staging
// path in its os.LinkError.
func TestCopyFileStagingName(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "etcd.yaml.bak")
	if err := os.WriteFile(src, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifests := filepath.Join(dir, "manifests")
	dst := filepath.Join(manifests, "etcd.yaml")
	if err := os.MkdirAll(dst, 0o755); err != nil { // an un-renameable-onto dst
		t.Fatal(err)
	}

	staged := func() string {
		err := copyFile(io.Discard, src, dst, 0o600)
		if err == nil {
			t.Fatal("expected the rename onto a directory to fail")
		}
		var le *os.LinkError
		if !errors.As(err, &le) {
			t.Fatalf("want an os.LinkError naming the staging path, got %T: %v", err, err)
		}
		return le.Old
	}

	first, second := staged(), staged()
	for _, p := range []string{first, second} {
		if base := filepath.Base(p); !strings.HasPrefix(base, ".") {
			t.Errorf("copyFile staged %q in the staticPodPath — kubelet parses every non-hidden file there as a static pod; it must be hidden", base)
		}
	}
	if first == second {
		t.Errorf("copyFile stages at a fixed name %q — staticpod.write stages at %q in this same directory, and two writers on one name tear the manifest",
			filepath.Base(first), ".etcd.yaml.tmp")
	}
	// Nothing must be left behind for the next reader of this directory.
	if leftover, _ := filepath.Glob(filepath.Join(manifests, "*")); len(leftover) != 1 || leftover[0] != dst {
		t.Errorf("staging files left behind: %v", leftover)
	}
}

// TestCopyFileAppliesModePastUmask: os.WriteFile applies its mode only on
// create and through the umask, so the permissions have to be chmodded on
// explicitly. This is what makes the identical-content skip below able to
// match — a 0o755 install that landed 0o750 would be re-installed on every
// single rollback.
func TestCopyFileAppliesModePastUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	dir := t.TempDir()
	src := filepath.Join(dir, "kubelet.bak")
	dst := filepath.Join(dir, "kubelet")
	if err := os.WriteFile(src, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(io.Discard, src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want -rwxr-xr-x (the umask must not reach an installed binary)", fi.Mode().Perm())
	}
	// Now identical in content AND mode, so a second copy must be a no-op.
	if err := copyFile(io.Discard, src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, after) {
		t.Error("an identical file was re-installed — the no-op guard never matches, so every rollback rewrites everything")
	}
}

// TestCopyFileReplacesSymlink: the identical-content fast path must not fire
// through a symlink. Every caller promises a regular file renamed into place —
// atomicInstall installs binaries the supervisor execs — so a dst symlink
// whose target happens to hold the same bytes at the same mode must still be
// replaced by a real file, not silently left as a link.
func TestCopyFileReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "stratum.bak-ts")
	target := filepath.Join(dir, "elsewhere")
	dst := filepath.Join(dir, "stratum")
	for _, f := range []string{src, target} {
		if err := os.WriteFile(f, []byte("identical"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(io.Discard, src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("dst is still a symlink — the no-op path matched through the link instead of renaming a regular file over it")
	}
	if b, _ := os.ReadFile(target); string(b) != "identical" {
		t.Errorf("the symlink's target was overwritten through the link: %q", b)
	}
}

// TestCopyFileClearsPrivilegedBits: the no-op guard compares the whole mode,
// not Perm(), because setuid/setgid/sticky live outside Perm() and the staged
// file's chmod to exactly mode would have cleared them. A dst carrying an
// unexpected setuid bit must be replaced, not preserved by the fast path.
func TestCopyFileClearsPrivilegedBits(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "stratum.bak-ts")
	dst := filepath.Join(dir, "stratum")
	if err := os.WriteFile(src, []byte("identical"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("identical"), 0o755); err != nil {
		t.Fatal(err)
	}
	// os.ModeSetuid, NOT 0o4755: os.FileMode has its own layout for the
	// special bits, so os.Chmod silently ignores a raw 0o4000 and this test
	// would pass for the wrong reason. A non-root user cannot set setuid
	// everywhere either, so the bit is verified below rather than assumed.
	if err := os.Chmod(dst, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if fi.Mode()&os.ModeSetuid == 0 {
		t.Skipf("cannot set setuid here (got %v); nothing to test", fi.Mode())
	}
	if err := copyFile(io.Discard, src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("mode = %v — the fast path preserved a setuid bit on an installed binary instead of enforcing the requested mode", fi.Mode())
	}
}

// TestManifestSetRestoreCoversTheDerivedFiles: the manifest set the upgrade
// snapshots into manifestBakDir and restores through restoreAll carries the
// two render-derived files with it — the apiserver render writes
// admission-webhook.yaml and webhook-client.kubeconfig on every manifest
// write, and a rollback's restored manifests are never re-rendered, so nothing
// else puts them back. The restore must also leave no visible stray file in
// the directory kubelet watches.
func TestManifestSetRestoreCoversTheDerivedFiles(t *testing.T) {
	_, _, _ = backupFixture(t)
	confDir := t.TempDir()
	redirect(t, &version.K8sConfDir, confDir)
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	const ts = "20261014-000000"
	live := map[string]string{
		"etcd.yaml":           "old etcd\n",
		"kube-apiserver.yaml": "old apiserver\n",
	}
	for name, body := range live {
		if err := os.WriteFile(filepath.Join(staticpod.Dir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	derived := map[string]string{
		"admission-webhook.yaml":    "old admission-webhook\n",
		"webhook-client.kubeconfig": "old client config\n",
	}
	for name, body := range derived {
		if err := os.WriteFile(filepath.Join(version.K8sConfDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The upgrade's snapshot goes where scanBackupSets finds it.
	if err := staticpod.Snapshot(manifestBakDir(ts)); err != nil {
		t.Fatal(err)
	}

	// The upgrade re-renders every manifest, drops one, and rewrites the
	// derived pair.
	for name, body := range live {
		if err := os.WriteFile(filepath.Join(staticpod.Dir(), name), []byte(strings.Replace(body, "old", "new", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(staticpod.Dir(), "kube-apiserver.yaml")); err != nil {
		t.Fatal(err)
	}
	for name := range derived {
		if err := os.WriteFile(filepath.Join(version.K8sConfDir, name), []byte("new "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// `stratum rollback` restores exactly the set the scan finds.
	stamp, items := mustNewestSet(t)
	if stamp != ts || len(items) != 1 || items[0].name != manifestsItem {
		t.Fatalf("newest set = %s %+v; want the manifests item of %s", stamp, items, ts)
	}
	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("the restore failed for %q", failed)
	}

	for name, body := range live {
		got, err := os.ReadFile(filepath.Join(staticpod.Dir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Errorf("%s after the restore = %q, want the snapshotted %q", name, got, body)
		}
	}
	for name, body := range derived {
		got, err := os.ReadFile(filepath.Join(version.K8sConfDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Errorf("%s after the restore = %q, want the snapshotted %q — the restored manifests are not re-rendered, so nothing else puts this file back", name, got, body)
		}
	}
	entries, err := os.ReadDir(staticpod.Dir())
	if err != nil {
		t.Fatal(err)
	}
	manifests := make(map[string]bool, len(live))
	for name := range live {
		manifests[name] = true
	}
	var restored []string
	for _, e := range entries {
		restored = append(restored, e.Name())
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !manifests[e.Name()] {
			t.Errorf("the restore left %s in the manifest dir; kubelet parses it as a pod", e.Name())
		}
	}
	for name := range manifests {
		if !slices.Contains(restored, name) {
			t.Errorf("the restore dropped %s from the manifest dir", name)
		}
	}
}

// TestReplacedComponentsIgnoresTheDerivedFiles: the manifest backup carries
// admission-webhook.yaml and webhook-client.kubeconfig, which are not static
// pods. Counting them among the components a restore replaces would have a
// rollback wait out the full component gate on a pod that never exists, then
// report the rollback INCOMPLETE over a file kubelet never runs. A hidden
// staging file in the backup is an in-flight write, never a manifest.
func TestReplacedComponentsIgnoresTheDerivedFiles(t *testing.T) {
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
	live := t.TempDir()
	bak := filepath.Join(live, "manifests-bak")
	if err := os.MkdirAll(bak, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"etcd.yaml":                 "old etcd",
		"admission-webhook.yaml":    "old admission-webhook",
		"webhook-client.kubeconfig": "old client config",
		".etcd.yaml.tmp-123":        "torn",
	} {
		if err := os.WriteFile(filepath.Join(bak, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := replacedComponents([]backupItem{{name: manifestsItem, bak: bak, dst: live, dir: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !got["etcd"] {
		t.Errorf("replacedComponents = %v; want etcd, whose manifest the restore changes", got)
	}
	for _, notAPod := range []string{"admission-webhook", "webhook-client"} {
		if got[notAPod] {
			t.Errorf("replacedComponents counts %q among the static pods the restore replaces — the rollback would gate on a component that never exists", notAPod)
		}
	}
	for _, notAManifest := range []string{".etcd.yaml.tmp-123", "webhook-client.kubeconfig"} {
		if got[notAManifest] {
			t.Errorf("replacedComponents reports %q, which is no component's manifest", notAManifest)
		}
	}
}

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRollbackGateWaitsForARestartedUnit: a unit just
// restarted can still be coming up when the soak ends. One sample there fails
// a rollback that worked; the gate retries within its window instead.
func TestRollbackGateWaitsForARestartedUnit(t *testing.T) {
	rollbackRestartFixture(t, "")
	upAt := time.Now().Add(1500 * time.Millisecond) // after the 1s soak
	old := unitActive
	unitActive = func(_ context.Context, name string) bool { return name == "containerd" || time.Now().After(upAt) }
	t.Cleanup(func() { unitActive = old })
	rollbackGateWindow = 5 * time.Second

	if err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1}); err != nil {
		t.Errorf("rollback failed its gate on a unit still coming up: %v", err)
	}
}

// TestRollbackReportsAFailedOperatorRestart: the operator
// comes back last and best-effort — the node is already rolled back and a
// second rollback would not restart it either — but its failure is said, not
// dropped, and the rollback still ends the upgrade.
func TestRollbackReportsAFailedOperatorRestart(t *testing.T) {
	rollbackRestartFixture(t, "operator")
	old := unitExists
	unitExists = func(name string) bool { return true }
	t.Cleanup(func() { unitExists = old })

	var buf bytes.Buffer
	rerr := RollbackNode(&buf, RollbackOptions{SoakSec: 1})

	if rerr != nil {
		t.Errorf("a failed operator restart must not fail a rollback that restored the node: %v", rerr)
	}
	if !strings.Contains(buf.String(), "WARN: operator restart failed") {
		t.Errorf("the operator's failure was dropped:\n%s", buf.String())
	}
}

// TestSnapshotOnlyRollbackNeedsTheWholeNode: with no host
// backup to restore, a rollback cannot bring a down kubelet back, and its etcd
// restore needs kubelet to cycle the etcd pod. It refuses up front, with
// nothing changed, rather than timing out part-way.
func TestSnapshotOnlyRollbackNeedsTheWholeNode(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true})
	err := RollbackNode(io.Discard, RollbackOptions{EtcdSnapshot: "/nonexistent.db"})
	for _, want := range []string{"stratum rollback:", "kubelet is not active", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestRollbackWithoutHostBackupsNeedsTheWholeNode: a set
// that restores no host binary — here only the stratum binary — cannot bring
// a down kubelet back, so it keeps the full guard. A set that restores kubelet
// gets past it: TestRollbackRestartsTheUnitItRecovers.
func TestRollbackWithoutHostBackupsNeedsTheWholeNode(t *testing.T) {
	stratumDir, _, _ := backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true})
	writeTestFile(t, stratumInstallPath, "new")
	writeTestFile(t, filepath.Join(stratumDir, "stratum.bak-20260928-100000"), "old")
	err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1})
	for _, want := range []string{"stratum rollback:", "kubelet is not active", "nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a set with no host binary must keep the full guard; error missing %q: %v", want, err)
		}
	}
	if got := readTestFile(t, stratumInstallPath); got != "new" {
		t.Errorf("the guard let the restore through (stratum = %q)", got)
	}
}

// TestNodeGateStaysInsideItsWindow: the apiserver gets what
// is left of the window as a deadline, not a fresh 60s wait on every retry.
func TestNodeGateStaysInsideItsWindow(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	var left time.Duration
	old := waitHealthzCtx
	waitHealthzCtx = func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("healthz was given no deadline")
		}
		left = time.Until(d)
		return nil
	}
	t.Cleanup(func() { waitHealthzCtx = old })

	if err := waitNodeGate(true, testNode, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if left <= 0 || left > 10*time.Second {
		t.Errorf("healthz had %v left inside a 10s window; want 0 < left <= 10s", left)
	}
}

// TestWaitNodeGateCutsHealthzOffWhenTheWindowExpires: healthz
// starts with time remaining and is interrupted when the window ends — the gate
// returns about when the window does, not after the request or the retry sleep
// finishes.
func TestWaitNodeGateCutsHealthzOffWhenTheWindowExpires(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	runtimeFixture(t, "v1.0.0", "v1.0.0", "v1.0.0")
	started := make(chan bool, 1)
	old := waitHealthzCtx
	waitHealthzCtx = func(ctx context.Context) error {
		started <- ctx.Err() == nil // time remained when the request began
		<-ctx.Done()                // a stalled apiserver: only the window ends it
		return ctx.Err()
	}
	t.Cleanup(func() { waitHealthzCtx = old })

	const window = 300 * time.Millisecond
	begin := time.Now()
	err := waitNodeGate(true, testNode, window)
	took := time.Since(begin)
	if err == nil {
		t.Fatal("gate passed with healthz never answering")
	}
	if !<-started {
		t.Error("healthz started with no time remaining; the test does not exercise an in-flight cutoff")
	}
	if took < window || took > window+time.Second {
		t.Errorf("gate took %v; want about the %v window", took, window)
	}
}

// TestHealthzGatesFailWithinTheirBudgetOnAStalledKubectl: an apiserver
// that accepts the connection and never answers leaves kubectl hanging. Both
// upgrade gates must kill it and fail when their budget ends, not hold the
// upgrade open; through the real k8sutil wait, not a stub.
func TestHealthzGatesFailWithinTheirBudgetOnAStalledKubectl(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	runtimeFixture(t, "v1.0.0", "v1.0.0", "v1.0.0")
	writeFile(t, filepath.Join(version.BinDir, "kubectl"), "#!/bin/sh\nexec sleep 30\n", 0o755)
	const budget = 300 * time.Millisecond
	swap(t, &nodeGateHealthzWindow, budget)
	swap(t, &componentHealthzWindow, budget)
	swap(t, &waitStaticPodReady, func(string, time.Duration) error { return nil })
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
	swap(t, &bootClock, func() (staticpod.Mark, error) { return staticpod.Mark{}, nil })

	for name, gate := range map[string]func() error{
		"nodeGate":      func() error { return nodeGate(true, testNode) },
		"gateComponent": func() error { return gateComponent("kube-apiserver") },
	} {
		begin := time.Now()
		err := gate()
		took := time.Since(begin)
		if err == nil || !strings.Contains(err.Error(), "/healthz did not become ok") {
			t.Errorf("%s = %v; want the healthz failure", name, err)
		}
		if took < budget || took > budget+time.Second {
			t.Errorf("%s took %v; want about its %v budget", name, took, budget)
		}
	}
}

// TestRuntimeProbesCompareRunningToInstalled pins that a different
// containerd (an orphan) serving the CRI socket, or a nodeInfo reporting a
// different kubelet, is a mismatch; a leading "v" is not.
func TestRuntimeProbesCompareRunningToInstalled(t *testing.T) {
	for _, tc := range []struct {
		name           string
		disk, got      string
		probe          func(context.Context) error
		probeErr, want string
	}{
		{name: "containerd matches", disk: "v2.4.1+stratum", got: "v2.4.1+stratum", probe: containerdServesInstalled},
		{name: "containerd v prefix", disk: "v2.4.1+stratum", got: "2.4.1+stratum", probe: containerdServesInstalled},
		{name: "orphan containerd", disk: "v2.4.1+stratum", got: "v2.3.5+stratum", probe: containerdServesInstalled, want: "reports v2.3.5+stratum"},
		{name: "containerd silent", disk: "v2.4.1+stratum", probeErr: "connection refused", probe: containerdServesInstalled, want: "did not answer"},
		{name: "kubelet matches", disk: "v1.37.1-stratum", got: "v1.37.1-stratum", probe: kubeletProbe},
		{name: "old kubelet", disk: "v1.37.1-stratum", got: "v1.37.0-stratum", probe: kubeletProbe, want: "reports kubelet v1.37.0-stratum"},
		{name: "nodeInfo unreadable", disk: "v1.37.1-stratum", probeErr: "forbidden", probe: kubeletProbe, want: "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeFixture(t, tc.disk, tc.got, tc.got)
			if tc.probeErr != "" {
				servingContainerd = func(context.Context, string) (string, error) { return "", errors.New(tc.probeErr) }
				kubeletReports = func(context.Context, string) (string, error) { return "", errors.New(tc.probeErr) }
			}
			err := withinWindow(10*time.Millisecond, tc.probe)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("err = %v; want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestNodeGateFailsOnAnOrphanedRuntime: every unit is active, yet the runtime
// on the CRI socket is not the installed one — the upgrade's gate and
// rollback's both fail: unit state alone does not answer which runtime
// holds the socket.
func TestNodeGateFailsOnAnOrphanedRuntime(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	runtimeFixture(t, "v2.4.1+stratum", "v2.3.5+stratum", "v2.4.1+stratum")
	for name, gate := range map[string]func() error{
		"nodeGate":     func() error { return nodeGate(false, testNode) },
		"waitNodeGate": func() error { return waitNodeGate(false, testNode, time.Second) },
	} {
		if err := gate(); err == nil || !strings.Contains(err.Error(), "orphan") {
			t.Errorf("%s = %v; want a failure naming the orphaned runtime", name, err)
		}
	}
}

// TestNodeGateChecksKubeletOnEveryNode: a worker with an old kubelet fails both
// gates even though containerd matches, and so does a controller; a worker
// whose kubelet matches passes. Workers read their own Node with their kubelet
// credential, so the check is not controller-only.
func TestNodeGateChecksKubeletOnEveryNode(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	oldH := waitHealthzCtx
	waitHealthzCtx = func(context.Context) error { return nil }
	t.Cleanup(func() { waitHealthzCtx = oldH })
	for _, isCP := range []bool{false, true} {
		role := map[bool]string{false: "worker", true: "controller"}[isCP]
		gates := map[string]func() error{
			"waitNodeGate": func() error { return waitNodeGate(isCP, testNode, time.Second) },
			"nodeGate":     func() error { return nodeGate(isCP, testNode) },
		}
		for name, gate := range gates {
			runtimeFixture(t, "v1.37.1-stratum", "v1.37.1-stratum", "v1.37.0-stratum")
			if err := gate(); err == nil || !strings.Contains(err.Error(), "reports kubelet v1.37.0-stratum") {
				t.Errorf("%s %s with an old kubelet = %v; want a kubelet version failure", role, name, err)
			}
			runtimeFixture(t, "v1.37.1-stratum", "v1.37.1-stratum", "v1.37.1-stratum")
			if err := gate(); err != nil {
				t.Errorf("%s %s with a matching kubelet: %v", role, name, err)
			}
		}
	}
}

// TestWaitNodeGateFailsOnceTheWindowIsUsedUp: a probe that
// starts after the rollback window has run out fails; it does not get a fresh
// allowance. Here the containerd probe, which passes, uses the whole window, so
// the kubelet probe (and the healthz wait before it on a controller) have none.
func TestWaitNodeGateFailsOnceTheWindowIsUsedUp(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	runtimeFixture(t, "v1.0.0", "v1.0.0", "v1.0.0")
	servingContainerd = func(context.Context, string) (string, error) {
		time.Sleep(150 * time.Millisecond) // longer than the window below
		return "v1.0.0", nil
	}
	kubeletProbed := false
	kubeletReports = func(context.Context, string) (string, error) { kubeletProbed = true; return "v1.0.0", nil }
	healthzAsked := false
	oldH := waitHealthzCtx
	waitHealthzCtx = func(context.Context) error { healthzAsked = true; return nil }
	t.Cleanup(func() { waitHealthzCtx = oldH })

	for _, isCP := range []bool{false, true} {
		err := waitNodeGate(isCP, testNode, 100*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "window was used up") {
			t.Errorf("isCP=%v: err = %v; want the window used up", isCP, err)
		}
	}
	if kubeletProbed || healthzAsked {
		t.Errorf("a probe ran with no window left (kubelet %v, healthz %v)", kubeletProbed, healthzAsked)
	}
}

// TestWaitNodeGateHostPollingEndsWithTheWindow: the wait for
// host processes is not a fixed 2s sleep past the window.
func TestWaitNodeGateHostPollingEndsWithTheWindow(t *testing.T) {
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true}) // kubelet never comes up
	begin := time.Now()
	err := waitNodeGate(false, testNode, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "kubelet is not active") {
		t.Fatalf("err = %v; want kubelet not active", err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Errorf("took %v inside a 200ms window", took)
	}
}

// TestKubeletCheckReadsTheRegisteredNode: kubelet registers
// under the persisted NodeState.Hostname (--hostname-override), so both gates
// must read that Node, not the one the OS hostname names. The fake API serves
// only the registered name.
func TestKubeletCheckReadsTheRegisteredNode(t *testing.T) {
	const registered = "registered-node.example"
	if osName, _ := host.Hostname(); osName == registered {
		t.Fatalf("test needs a registered name that differs from the OS hostname %q", osName)
	}
	backupFixture(t)
	probeFixture(t, true, map[string]bool{"containerd": true, "kubelet": true})
	runtimeFixture(t, "v1.37.1-stratum", "v1.37.1-stratum", "v1.37.1-stratum")
	var asked []string
	kubeletReports = func(_ context.Context, node string) (string, error) {
		asked = append(asked, node)
		if node != registered {
			return "", errors.New("nodes " + strconv.Quote(node) + " not found")
		}
		return "v1.37.1-stratum", nil
	}
	oldH := waitHealthzCtx
	waitHealthzCtx = func(context.Context) error { return nil }
	t.Cleanup(func() { waitHealthzCtx = oldH })

	gates := map[string]func(node string) error{
		"nodeGate (worker)":         func(n string) error { return nodeGate(false, n) },
		"waitNodeGate (worker)":     func(n string) error { return waitNodeGate(false, n, time.Second) },
		"waitNodeGate (controller)": func(n string) error { return waitNodeGate(true, n, time.Second) },
	}
	for name, gate := range gates {
		asked = nil
		if err := gate(registered); err != nil {
			t.Errorf("%s with the registered node name: %v", name, err)
		}
		if len(asked) == 0 || asked[0] != registered {
			t.Errorf("%s read %v, want the registered node %q", name, asked, registered)
		}
		osName, _ := host.Hostname()
		if err := gate(osName); err == nil {
			t.Errorf("%s passed for %q, a Node kubelet does not register under", name, osName)
		}
	}
}

// TestNodeName: the registered name wins; only with no node state does the OS
// hostname stand in.
func TestNodeName(t *testing.T) {
	osName, _ := host.Hostname()
	for _, tc := range []struct {
		name string
		st   *nodestate.NodeState
		want string
	}{
		{"registered name", &nodestate.NodeState{Hostname: "cp-1.registered"}, "cp-1.registered"},
		{"state without a name", &nodestate.NodeState{}, osName},
		{"no state", nil, osName},
	} {
		if got := nodeName(tc.st); got != tc.want {
			t.Errorf("%s: nodeName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The unchanged-file skip is decided from the descriptor the copy reads, not
// from the source path: with the path swapped after open, a dst that matches
// the NEW file at that path must not stop the OLD (open) content installing.
func TestCopyOpenedDecidesSkipFromOpenFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	writeFile(t, src, "old-bytes", 0o755)
	writeFile(t, dst, "new-bytes", 0o755) // same size as the open file, matches the swapped-in one

	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	swapped := filepath.Join(dir, "swapped")
	writeFile(t, swapped, "new-bytes", 0o755)
	if err := os.Rename(swapped, src); err != nil {
		t.Fatal(err)
	}

	if err := copyOpened(io.Discard, in, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old-bytes" {
		t.Fatalf("dst = %q, want the open file's %q: the skip was decided from the path", got, "old-bytes")
	}
}

// TestRollbackRestoresManifestsAfterTheHostRestart: kubelet and containerd
// restarted while kubelet is replacing a static pod can leave the old pod half
// torn down and the restored one never started, so the manifests go back only
// once both have been restarted.
func TestRollbackRestoresManifestsAfterTheHostRestart(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", nil })
	writeTestFile(t, filepath.Join(version.DataDir, "upgrade", "manifests-bak-20260928-100000", "etcd.yaml"), "old etcd")
	writeTestFile(t, staticpod.Path("etcd"), "new etcd")
	inner := restartUnit
	swap(t, &restartUnit, func(name string) error {
		*restarted = append(*restarted, name+"@"+readTestFile(t, staticpod.Path("etcd")))
		return inner(name)
	})
	if err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1}); err != nil {
		t.Fatalf("RollbackNode: %v", err)
	}
	for _, c := range *restarted {
		if strings.Contains(c, "@old etcd") {
			t.Errorf("restarts %q: a host process restarted after the manifests were restored", *restarted)
		}
	}
	if got := readTestFile(t, staticpod.Path("etcd")); got != "old etcd" {
		t.Errorf("etcd manifest = %q, want the restored one", got)
	}
}

// TestRollbackGatesTheRestoredComponent: a component whose manifest the
// rollback changes is gated only once the process kubelet is replacing has
// exited, so the old process cannot answer for the restored one.
func TestRollbackGatesTheRestoredComponent(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", nil })
	writeTestFile(t, filepath.Join(version.K8sConfDir, "node.yaml"), "role: controller\nhostname: cp-1\nnodeIP: 10.0.0.1\n")
	writeTestFile(t, filepath.Join(version.DataDir, "upgrade", "manifests-bak-20260928-100000", "etcd.yaml"), "old etcd")
	writeTestFile(t, staticpod.Path("etcd"), "new etcd")
	writeTestFile(t, filepath.Join(version.DataDir, "upgrade", "manifests-bak-20260928-100000", "kube-scheduler.yaml"), "same")
	writeTestFile(t, staticpod.Path("kube-scheduler"), "same")
	swap(t, &waitComponentReplace, func(name string, _ staticpod.Mark, _ time.Duration) error {
		*restarted = append(*restarted, "replaced:"+name)
		return nil
	})
	swap(t, &waitStaticPodReady, func(name string, _ time.Duration) error {
		*restarted = append(*restarted, "gate:"+name)
		return nil
	})
	swap(t, &waitHealthzCtx, func(context.Context) error { return nil })
	_ = RollbackNode(io.Discard, RollbackOptions{SoakSec: 1})
	got := strings.Join(*restarted, " ")
	if !strings.Contains(got, "replaced:etcd gate:etcd") {
		t.Errorf("events = %q, want etcd's replaced process waited for before its gate", got)
	}
	if strings.Contains(got, "replaced:kube-scheduler") {
		t.Errorf("events = %q, waited on a component whose manifest the rollback did not change", got)
	}
}

// TestRollbackRefusesWhenItCannotFindWhatItReplaces: without the processes a
// restore replaces, its gates could be answered by them, so the rollback
// refuses before it changes anything.
func TestRollbackRefusesWhenItCannotFindWhatItReplaces(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", nil })
	writeTestFile(t, filepath.Join(version.DataDir, "upgrade", "manifests-bak-20260928-100000", "etcd.yaml"), "old etcd")
	writeTestFile(t, staticpod.Path("etcd"), "new etcd")
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, errors.New("read /proc: permission denied") })
	prepared := false
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { prepared = true; return "", nil })
	err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || len(*restarted) != 0 || prepared {
		t.Fatalf("err = %v, restarts %q, etcd prepared %v; want a refusal before anything, etcd included, changed", err, *restarted, prepared)
	}
	if got := readTestFile(t, staticpod.Path("etcd")); got != "new etcd" || readTestFile(t, filepath.Join(version.BinDir, "kubelet")) != "new" {
		t.Error("the rollback restored something before refusing")
	}
}

// TestRollbackGatesWhatItRestoresWhateverTheRole: a node whose saved role
// does not say controller still has every static pod the rollback changed
// waited for and gated.
func TestRollbackGatesWhatItRestoresWhateverTheRole(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", nil })
	writeTestFile(t, filepath.Join(version.DataDir, "upgrade", "manifests-bak-20260928-100000", "etcd.yaml"), "old etcd")
	writeTestFile(t, staticpod.Path("etcd"), "new etcd")
	swap(t, &waitComponentReplace, func(name string, _ staticpod.Mark, _ time.Duration) error {
		*restarted = append(*restarted, "replaced:"+name)
		return nil
	})
	swap(t, &waitStaticPodReady, func(name string, _ time.Duration) error {
		*restarted = append(*restarted, "gate:"+name)
		return nil
	})
	_ = RollbackNode(io.Discard, RollbackOptions{SoakSec: 1})
	if got := strings.Join(*restarted, " "); !strings.Contains(got, "replaced:etcd gate:etcd") {
		t.Errorf("events = %q, want the restored etcd waited for and gated on a node saved as a worker", got)
	}
}

// TestReplacedComponentsFailsClosed: a manifest backup that cannot be read
// is an error before anything changes, not a backup with nothing to restore.
func TestReplacedComponentsFailsClosed(t *testing.T) {
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
	dir := t.TempDir()
	if _, err := replacedComponents([]backupItem{{name: manifestsItem, bak: filepath.Join(dir, "gone"), dst: dir, dir: true}}); err == nil {
		t.Error("an unreadable manifest backup was taken as one with nothing changed")
	}
	bak := filepath.Join(dir, "bak")
	writeTestFile(t, filepath.Join(bak, "etcd.yaml"), "old")
	if err := os.Mkdir(filepath.Join(dir, "etcd.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := replacedComponents([]backupItem{{name: manifestsItem, bak: bak, dst: dir, dir: true}}); err == nil {
		t.Error("an unreadable live manifest was taken as one the restore does not change")
	}
	got, err := replacedComponents([]backupItem{{name: manifestsItem, bak: bak, dst: filepath.Join(dir, "empty"), dir: true}})
	if err != nil || !got["etcd"] {
		t.Errorf("a manifest with no live copy: %v, %v; want etcd replaced", got, err)
	}
}

// TestRollbackWithoutTheClockKeepsWhatItKnows: a boot clock that fails once
// the host backups are restored leaves the manifests unrestored, and the
// incomplete rollback still says what is not back.
func TestRollbackWithoutTheClockKeepsWhatItKnows(t *testing.T) {
	restarted := rollbackRestartFixture(t, "kubelet")
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", nil })
	writeTestFile(t, filepath.Join(version.DataDir, "upgrade", "manifests-bak-20260928-100000", "etcd.yaml"), "old etcd")
	writeTestFile(t, staticpod.Path("etcd"), "new etcd")
	reads := 0
	swap(t, &bootClock, func() (staticpod.Mark, error) {
		if reads++; reads > 1 {
			return staticpod.Mark{}, errors.New("read /proc/uptime: input/output error")
		}
		return staticpod.Mark{}, nil
	})
	err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1})
	if err == nil || !strings.Contains(err.Error(), "INCOMPLETE") || !strings.Contains(err.Error(), manifestsItem) ||
		!strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("err = %v (restarts %q); want INCOMPLETE naming the manifests and the host process", err, *restarted)
	}
	if got := readTestFile(t, staticpod.Path("etcd")); got != "new etcd" {
		t.Errorf("etcd manifest = %q, want it left as it was", got)
	}
}

// TestHostOnlyRollbackDoesNotNeedTheClock: a backup set with no manifests
// replaces no static pod, so it neither refuses nor reports the manifests
// unrestored when the boot clock cannot be read.
func TestHostOnlyRollbackDoesNotNeedTheClock(t *testing.T) {
	rollbackRestartFixture(t, "")
	swap(t, &bootClock, func() (staticpod.Mark, error) {
		return staticpod.Mark{}, errors.New("read /proc/uptime: input/output error")
	})
	if err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1}); err != nil {
		t.Fatalf("host-only rollback without the boot clock: %v", err)
	}
	if got := readTestFile(t, filepath.Join(version.BinDir, "kubelet")); got != "old" {
		t.Errorf("kubelet = %q, want the restored %q", got, "old")
	}
}

// TestEveryBackupKindRoundTripsThroughScanRestoreAndPrune: every kind of
// upgrade backup must survive every consumer — scanBackupSets finds it under
// its own stamp with the right destination and directory flag, restoreOrder
// puts the CIS configs first and keeps the scan's order for the rest,
// hasHostBackup counts only a host binary as one, and pruneOldBackups keeps
// the two newest stamps of every kind and removes the third. A table that
// leaves a kind out of any one of these would drop a backup from a rollback or
// relax its guard without an error.
func TestEveryBackupKindRoundTripsThroughScanRestoreAndPrune(t *testing.T) {
	const ts1, ts2, ts3 = "20260101-000000", "20260202-000000", "20260303-000000"
	kinds := []struct {
		name  string // the name scanBackupSets gives the item
		bakAt func(stratumDir, binDir, dataDir, ts string) string
		dstOf func(stratumDir, binDir, dataDir string) string
		dir   bool // the backup is a directory and dst is the manifest dir
		cis   bool // the backup is a CIS config dir, restored by restoreCISConfigs
		host  bool // hasHostBackup must count it as a host binary
		plant func(t *testing.T, stratumDir, binDir, dataDir, ts string)
		prune func(t *testing.T, stratumDir, binDir, dataDir string)
	}{
		{
			name:  "stratum",
			bakAt: func(s, _, _, ts string) string { return filepath.Join(s, "stratum.bak-"+ts) },
			dstOf: func(_, _, _ string) string { return stratumInstallPath },
			plant: func(t *testing.T, s, _, _, ts string) {
				writeFile(t, filepath.Join(s, "stratum.bak-"+ts), "old", 0o755)
			},
			prune: func(t *testing.T, s, _, _ string) {
				if err := pruneOldBackups(s, keepBackupSets, backupPattern{prefix: filepath.Base(stratumInstallPath) + ".bak-"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "kubelet",
			bakAt: func(_, b, _, ts string) string { return filepath.Join(b, "kubelet.bak-"+ts) },
			dstOf: func(_, b, _ string) string { return filepath.Join(b, "kubelet") },
			host:  true,
			plant: func(t *testing.T, _, b, _, ts string) {
				writeFile(t, filepath.Join(b, "kubelet.bak-"+ts), "old", 0o755)
			},
			prune: func(t *testing.T, _, b, _ string) {
				if err := pruneOldBackups(b, keepBackupSets, backupPattern{prefix: "kubelet.bak-"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "manifests",
			dir:   true,
			bakAt: func(_, _, d, ts string) string { return filepath.Join(d, "upgrade", "manifests-bak-"+ts) },
			dstOf: func(_, _, _ string) string { return staticpod.Dir() },
			plant: func(t *testing.T, _, _, d, ts string) {
				writeFile(t, filepath.Join(d, "upgrade", "manifests-bak-"+ts, "etcd.yaml"), "old", 0o600)
			},
			prune: func(t *testing.T, _, _, d string) {
				if err := pruneOldBackups(filepath.Join(d, "upgrade"), keepBackupSets, upgradeDirBackups...); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "profile configs",
			cis:   true,
			bakAt: func(_, _, d, ts string) string { return filepath.Join(d, "upgrade", "cisconfig-bak-"+ts) },
			dstOf: func(_, _, _ string) string { return "" }, // dst unused: restoreCISConfigs knows the live paths
			plant: func(t *testing.T, _, _, d, ts string) {
				writeFile(t, filepath.Join(d, "upgrade", "cisconfig-bak-"+ts, "admission.yaml"), "old", 0o600)
			},
			prune: func(t *testing.T, _, _, d string) {
				if err := pruneOldBackups(filepath.Join(d, "upgrade"), keepBackupSets, upgradeDirBackups...); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			stratumDir, binDir, dataDir := backupFixture(t)
			for _, ts := range []string{ts1, ts2, ts3} {
				k.plant(t, stratumDir, binDir, dataDir, ts)
			}

			// The scan finds each stamp's backup, and only it, with the right
			// destination and flags.
			sets, err := scanBackupSets()
			if err != nil {
				t.Fatal(err)
			}
			if len(sets) != 3 {
				t.Fatalf("scan found %d stamp(s), want 3: %v", len(sets), sets)
			}
			for _, ts := range []string{ts1, ts2, ts3} {
				items := sets[ts]
				if len(items) != 1 {
					t.Fatalf("set %s = %+v, want exactly one %s item", ts, items, k.name)
				}
				it := items[0]
				if it.name != k.name {
					t.Errorf("set %s item name = %q, want %q", ts, it.name, k.name)
				}
				if want := k.bakAt(stratumDir, binDir, dataDir, ts); it.bak != want {
					t.Errorf("set %s item bak = %q, want %q", ts, it.bak, want)
				}
				if want := k.dstOf(stratumDir, binDir, dataDir); it.dst != want {
					t.Errorf("set %s item dst = %q, want %q", ts, it.dst, want)
				}
				if it.dir != k.dir || it.profile != k.cis {
					t.Errorf("set %s item flags dir=%v cis=%v, want dir=%v cis=%v", ts, it.dir, it.profile, k.dir, k.cis)
				}
			}

			// Only a host binary backup relaxes the rollback's guard on a down
			// unit; the stratum binary itself does not.
			if got := hasHostBackup(sets[ts3]); got != k.host {
				t.Errorf("hasHostBackup(%s) = %v, want %v", k.name, got, k.host)
			}

			// The prune keeps the two newest stamps of the kind and removes the
			// third, so a rollback of the rollback still has something to land on.
			k.prune(t, stratumDir, binDir, dataDir)
			for _, ts := range []string{ts2, ts3} {
				if _, err := os.Stat(k.bakAt(stratumDir, binDir, dataDir, ts)); err != nil {
					t.Errorf("newest backup stamp %s must survive the prune: %v", ts, err)
				}
			}
			if _, err := os.Stat(k.bakAt(stratumDir, binDir, dataDir, ts1)); !os.IsNotExist(err) {
				t.Errorf("oldest backup stamp %s was not pruned (err=%v)", ts1, err)
			}
		})
	}

	// restoreOrder puts the CIS configs first and keeps the scan's order for
	// the other three — the order the scan appends: stratum, manifests, then
	// the BinDir binaries.
	scanOrder := []backupItem{
		{name: "stratum"},
		{name: "manifests", dir: true},
		{name: "profile configs", profile: true},
		{name: "kubelet"},
	}
	ordered := restoreOrder(scanOrder)
	got := make([]string, len(ordered))
	for i, it := range ordered {
		got[i] = it.name
	}
	want := []string{"profile configs", "stratum", "manifests", "kubelet"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("restoreOrder = %v, want %v", got, want)
	}
}

// TestHostProcsAreDerivedFromTheUnits: the upgrade swaps and restarts exactly
// the supervisor's roster minus the operator, which is restarted separately
// and LAST, in comps.Units order — and the shipped list is the derivation's
// output, not a hand-written copy of it. A literal hostProcs, a filter that
// loses the operator, or a derivation a new roster member does not flow
// through fails here: the upgrade would restart the operator before the gate,
// or never hear of a component added to comps.Units.
func TestHostProcsAreDerivedFromTheUnits(t *testing.T) {
	if !reflect.DeepEqual(hostProcs, hostProcsFrom(comps.Units)) {
		t.Errorf("hostProcs = %v, want the derivation applied to the roster: hostProcsFrom(comps.Units) = %v", hostProcs, hostProcsFrom(comps.Units))
	}

	// The relation on the real roster, restated independently: every unit but
	// the operator, in Units order.
	var want []string
	for _, u := range comps.Units {
		if u != "operator" {
			want = append(want, u)
		}
	}
	if got := hostProcsFrom(comps.Units); !reflect.DeepEqual(got, want) {
		t.Errorf("hostProcsFrom(comps.Units) = %v, want %v (comps.Units without the operator, in Units order)", got, want)
	}

	// A synthetic roster member must flow through the derivation, so adding a
	// component to comps.Units is a one-place change.
	augmented := append(slices.Clone(comps.Units), "synthetic-probe")
	wantSynthetic := append(slices.Clone(want), "synthetic-probe")
	if got := hostProcsFrom(augmented); !reflect.DeepEqual(got, wantSynthetic) {
		t.Errorf("hostProcsFrom(%v) = %v, want %v — a unit added to comps.Units must reach the derivation in Units order", augmented, got, wantSynthetic)
	}
}

// TestHostProcsIsInitializedFromTheDerivation parses node.go's declaration and
// fails when hostProcs is no longer initialized from the derivation on the
// canonical roster. A hand-written copy of the list is indistinguishable from
// the derivation by value today — it drifts only when comps.Units changes, and
// then nothing ties the two together.
func TestHostProcsIsInitializedFromTheDerivation(t *testing.T) {
	t.Parallel() // host-free: parses only its own file
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatalf("parse node.go: %v", err)
	}
	init := varInit(f, "hostProcs")
	if init == nil {
		t.Fatal("node.go declares no hostProcs; the upgrade's host-process list stays a declared value this test can pin")
	}
	if got := types.ExprString(init); got != "hostProcsFrom(comps.Units)" {
		t.Errorf("hostProcs is initialized from %s, want hostProcsFrom(comps.Units): the list must be the derivation's output on the canonical roster, not a hand-written copy that drifts from comps.Units", got)
	}
}

// varInit returns the initializing expression of a package-level var, or nil
// when the file declares no such var with a value.
func varInit(f *ast.File, name string) ast.Expr {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name == name && i < len(vs.Values) {
					return vs.Values[i]
				}
			}
		}
	}
	return nil
}

// TestGateComponentWaitsOnlyForComponentsWithAListener: the gate waits on the
// static pod's local listener for every component that has one — the set
// staticpod.HasListener declares — adds /healthz for the apiserver alone, and
// skips the listenerless ones (kube-vip today). A call wired the wrong way
// round would make every upgrade of a VIP node wait three minutes on a
// listener that never comes up and roll back, or let a listening component
// through ungated.
func TestGateComponentWaitsOnlyForComponentsWithAListener(t *testing.T) {
	var waited []string
	swap(t, &waitStaticPodReady, func(name string, _ time.Duration) error {
		waited = append(waited, name)
		return nil
	})
	var currentComponent string
	var healthz []string
	swap(t, &waitHealthzCtx, func(context.Context) error {
		healthz = append(healthz, currentComponent)
		return nil
	})

	for _, name := range staticpod.Names {
		currentComponent = name
		if err := gateComponent(name); err != nil {
			t.Errorf("gateComponent(%s): %v", name, err)
		}
	}
	var want []string
	for _, name := range staticpod.Names {
		if staticpod.HasListener(name) {
			want = append(want, name)
		}
	}
	if !reflect.DeepEqual(waited, want) {
		t.Errorf("waitStaticPodReady saw %v, want exactly the components staticpod.HasListener accepts: %v", waited, want)
	}
	if !reflect.DeepEqual(healthz, []string{"kube-apiserver"}) {
		t.Errorf("waitHealthzCtx was called for %v, want kube-apiserver alone", healthz)
	}
}

// -------------------------------------------------------- one containerd restart per upgrade

// renderFixture redirects every path the real node-config writer touches (the
// same set internal/runtime's nodeConfigsFixture redirects) and stages a
// containerd stub that prints a minimal valid `containerd config default` —
// the captured real default lives with the render tests that regenerate it
// (internal/runtime/testdata); this stub needs only the settings the render
// rewrites. registries.yaml stays absent, so the registry apply inside the
// render is a deterministic no-op.
func renderFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldBin, oldConfig, oldSock := version.BinDir, version.ContainerdConfig, version.ControlSocket
	oldReg, oldCerts, oldConfD := version.RegistriesConfig, version.CertsDir, version.ContainerdConfD
	oldConf, oldPKI := version.K8sConfDir, version.PKIDir
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket = oldBin, oldConfig, oldSock
		version.RegistriesConfig, version.CertsDir, version.ContainerdConfD = oldReg, oldCerts, oldConfD
		version.K8sConfDir, version.PKIDir = oldConf, oldPKI
	})
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	// Absent, so registry.Apply is a deterministic no-op.
	version.RegistriesConfig = filepath.Join(dir, "registries.yaml")
	version.CertsDir = filepath.Join(dir, "certs.d")
	version.ContainerdConfD = filepath.Join(dir, "conf.d", "stratum-registry.toml")
	version.K8sConfDir = filepath.Join(dir, "kubelet")
	// A joined node's PKI: the trust bundle writeKubeletConfig syncs the
	// kubelet client bundle from.
	version.PKIDir = filepath.Join(dir, "pki")
	if err := os.MkdirAll(version.PKIDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"),
		[]byte("-----BEGIN CERTIFICATE-----\nY2x1c3Rlcg==\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defaultTOML := `version = 3
root = '/var/lib/containerd'
state = '/run/containerd'
[plugins.'io.containerd.cri.v1.images']
  sandbox_image = 'registry.k8s.io/pause:3.10'
[plugins.'io.containerd.cri.v1.runtime']
  enable_selinux = false
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
    SystemdCgroup = false
`
	src := filepath.Join(dir, "default.toml")
	if err := os.WriteFile(src, []byte(defaultTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+src+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeSupervisor serves the supervisor control protocol on a temp unix socket
// and points version.ControlSocket at it, so comps sees containerd running:
// status reports pid 100 until a restart op is served, then pid 101 — the new
// pid comps.Restart's replacement check waits for. Same pattern as
// internal/runtime's fakeSupervisor. Returns a func counting the restart ops
// served.
func fakeSupervisor(t *testing.T) (restarts func() int) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	old := version.ControlSocket
	version.ControlSocket = socket
	t.Cleanup(func() { version.ControlSocket = old; ln.Close() })

	var mu sync.Mutex
	served := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req struct {
				Op   string `json:"op"`
				Name string `json:"name"`
			}
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				continue
			}
			mu.Lock()
			out := struct {
				OK    bool                    `json:"ok"`
				Error string                  `json:"error,omitempty"`
				Procs []supervisor.ProcStatus `json:"procs,omitempty"`
			}{OK: true}
			switch req.Op {
			case "status":
				pid := 100
				if served > 0 {
					pid = 101
				}
				out.Procs = []supervisor.ProcStatus{{Name: "containerd", Pid: pid, Running: true}}
			case "restart":
				served++
			}
			mu.Unlock()
			_ = json.NewEncoder(conn).Encode(out)
			conn.Close()
		}
	}()
	return func() int { mu.Lock(); defer mu.Unlock(); return served }
}

// TestApplyHostConfigsRestartsContainerdOnceViaItsOwnReload is the upgrade
// half of one containerd restart per upgrade: the node-config render must
// restart nothing, so an in-place upgrade restarts containerd at most once —
// through the upgrade's own reload, which pushes the new argv and restarts
// the replaced binary, and which is also what gates the legacy-config heal.
// The real writer runs (only the upgrade's own seams are stubbed), against a
// fake supervisor reporting containerd as running, so a render inside
// WriteNodeConfigs that restarted containerd itself would show as a second
// restart for one upgrade. A resumed upgrade that re-runs this step on
// current configs must serve no new restart either.
func TestApplyHostConfigsRestartsContainerdOnceViaItsOwnReload(t *testing.T) {
	renderFixture(t)
	restarts := fakeSupervisor(t)
	var calls []string
	oldAct, oldReload, oldRestart := unitActive, reloadUnit, restartUnit
	oldHeal, oldPresent := removeLegacyConfig, legacyConfigPresent
	t.Cleanup(func() {
		unitActive, reloadUnit, restartUnit = oldAct, oldReload, oldRestart
		removeLegacyConfig, legacyConfigPresent = oldHeal, oldPresent
	})
	unitActive = func(_ context.Context, name string) bool { return name == "containerd" || name == "kubelet" }
	reloadUnit = func(name string, _ supervisor.ProcSpec) error {
		calls = append(calls, "reload "+name)
		return nil
	}
	restartUnit = func(name string) error {
		calls = append(calls, "restart "+name)
		return nil
	}
	removeLegacyConfig = func(io.Writer) { calls = append(calls, "heal") }
	legacyConfigPresent = func() (string, bool) { return "/etc/containerd/config.toml", true }

	st := &nodestate.NodeState{Role: "controller", Hostname: "cp1", ServiceCIDR: "10.96.0.0/12"}
	if err := applyHostConfigs(io.Discard, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(version.ContainerdConfig); err != nil {
		t.Fatalf("the upgrade did not write a containerd config — the fixture is wrong: %v", err)
	}
	if n := restarts(); n != 0 {
		t.Errorf("the config render served %d containerd restart ops during the upgrade; the render must restart nothing and the upgrade's own reload is the one restart", n)
	}
	want := []string{"reload containerd", "reload kubelet", "heal"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("the upgrade's own host actions = %q, want %q — the reload is the single containerd restart, then the heal", calls, want)
	}

	// A resumed upgrade re-runs this step on current configs: still no restart
	// from the render, and the upgrade still performs its own reload (the
	// binary was swapped even though the config did not change).
	before := restarts()
	if err := applyHostConfigs(io.Discard, st); err != nil {
		t.Fatal(err)
	}
	if n := restarts(); n != before {
		t.Errorf("a re-run on current configs served %d new containerd restart ops, want 0", n-before)
	}
	if got := len(calls); got != 2*len(want) {
		t.Errorf("a re-run ran %d host actions total, want %d — the upgrade's own reload must run even when the render changed nothing", got, 2*len(want))
	}
}

// ── the stdout-guard retirement pins ─────────────────────────────────────────
//
// Every operation an MCP tool calls takes an io.Writer as its FIRST parameter
// and reports its progress through it, instead of printing to os.Stdout and
// being wrapped in internal/mcp's stdout guard. On the stdio transport
// os.Stdout is the JSON-RPC stream, so a pin that runs the operation on a
// buffer and proves its progress never touched the process's own stdout is
// what makes the guard redundant.

// captureRealStdout captures the process's ACTUAL stdout while fn runs, so a
// test can prove an operation left it alone.
func captureRealStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()

	fn()

	os.Stdout = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

// TestUpgradeNode_WritesItsProgressToTheWriter pins the node upgrade's half of
// the convention: UpgradeNode takes the writer first and its progress — the
// closing "stratum upgrade: <node> now at ..." line among it — is written
// there, never to os.Stdout.
func TestUpgradeNode_WritesItsProgressToTheWriter(t *testing.T) {
	controllerUpgradeFixture(t, "v1.37.9", 0)

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = UpgradeNode(&buf, NodeOptions{SkipSnapshot: true})
	})
	if err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	for _, want := range []string{"stratum upgrade:", "now at"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the upgrade's progress is missing %q; the writer it was handed has:\n%s", want, buf.String())
		}
	}
	if strings.Contains(onStdout, "stratum upgrade:") {
		t.Errorf("the upgrade printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

func TestPreUpgradeSnapshotWritesProgressThroughWriter(t *testing.T) {
	redirect(t, &version.BinDir, t.TempDir())
	writeFile(t, filepath.Join(version.BinDir, "etcdctl"), "#!/bin/sh\nprintf 'saved snapshot\\n'\nprintf snapshot > \"$3\"\n", 0o755)
	path := filepath.Join(t.TempDir(), "pre-upgrade.db")
	var progress bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = saveUpgradeSnapshot(&progress, path)
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if progress.String() != "saved snapshot\n" {
		t.Fatalf("snapshot progress = %q, want the child process's report", progress.String())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "snapshot" {
		t.Fatalf("snapshot at requested path = %q, %v", data, err)
	}
	if onStdout != "" {
		t.Fatalf("pre-upgrade snapshot corrupted the MCP stdout transport: %q", onStdout)
	}
}

// TestRollbackNode_WritesItsProgressToTheWriter pins the rollback's half: the
// same convention on RollbackNode — the writer first, the "restored backup
// set" progress through it, os.Stdout untouched.
func TestRollbackNode_WritesItsProgressToTheWriter(t *testing.T) {
	rollbackRestartFixture(t, "")

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = RollbackNode(&buf, RollbackOptions{SoakSec: 1})
	})
	if err != nil {
		t.Fatalf("RollbackNode: %v", err)
	}
	if !strings.Contains(buf.String(), "restored backup set") {
		t.Errorf("the rollback's progress is missing \"restored backup set\"; the writer it was handed has:\n%s", buf.String())
	}
	if strings.Contains(onStdout, "stratum rollback:") {
		t.Errorf("the rollback printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// TestUpgradeCluster_WritesItsProgressToTheWriter pins the orchestrator's
// half: UpgradeCluster takes the writer first and both the plan header and the
// closing "Cluster upgrade complete" line reach it, never os.Stdout.
func TestUpgradeCluster_WritesItsProgressToTheWriter(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	fastOrchestrator(t, nil)

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = UpgradeCluster(&buf, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs})
	})
	if err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	for _, want := range []string{"Upgrade plan", "Cluster upgrade complete"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the cluster upgrade's progress is missing %q; the writer it was handed has:\n%s", want, buf.String())
		}
	}
	if strings.Contains(onStdout, "Cluster upgrade complete") {
		t.Errorf("the cluster upgrade printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}

	// A run that upgraded nodes leaves the confinement restart steps right
	// after the complete line: pods the drain does not evict keep running
	// unconfined until they are recreated.
	complete := strings.Index(buf.String(), "Cluster upgrade complete")
	if complete < 0 || !strings.Contains(buf.String()[complete:], "rollout restart ds/cilium") {
		t.Errorf("the confinement restart steps do not follow the complete line; the writer has:\n%s", buf.String())
	}

	// A run that finds every node already current prints none. The build
	// check answers zero: the nodes run this target already. A fresh cluster,
	// so the first run's stamps cannot be what makes the nodes current.
	old := runExitCode
	t.Cleanup(func() { runExitCode = old })
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return 0, nil }
	var already bytes.Buffer
	err = UpgradeCluster(&already, ClusterOptions{Target: "v1.36.5", From: stagedBinary(t), Client: fake.NewSimpleClientset(upgradeNodesFixture()...)})
	if err != nil {
		t.Fatalf("UpgradeCluster all current: %v", err)
	}
	if out := already.String(); strings.Contains(out, "rollout restart ds/cilium") {
		t.Errorf("a run with nothing to upgrade printed the confinement restart steps:\n%s", out)
	}
}

// One upgraded node is enough for the confinement restart steps.
func TestUpgradeClusterPrintsConfinementStepsAfterOneNode(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodeFixture("cp1", true, "10.0.0.9"))
	fastOrchestrator(t, nil)
	var buf bytes.Buffer
	if err := UpgradeCluster(&buf, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs}); err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	if !strings.Contains(buf.String(), "rollout restart ds/cilium") {
		t.Errorf("a run that upgraded one node did not print the confinement restart steps:\n%s", buf.String())
	}
}

// A run that upgraded a node and then halted prints the confinement restart
// steps too: a re-run finds that node on this build and upgrades nothing, so
// it would never print them.
func TestUpgradeClusterPrintsConfinementStepsWhenItHalts(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	fastOrchestrator(t, nil)
	upgraded := runNodeJob
	runNodeJob = func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) error {
		if s.Node == "w1" {
			return errors.New("the worker's upgrade Job failed")
		}
		return upgraded(ctx, cs, s)
	}
	var buf bytes.Buffer
	if err := UpgradeCluster(&buf, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs}); err == nil {
		t.Fatal("UpgradeCluster succeeded although w1's upgrade Job failed")
	}
	if !strings.Contains(buf.String(), "rollout restart ds/cilium") {
		t.Errorf("a run that upgraded cp1 and then halted did not print the confinement restart steps:\n%s", buf.String())
	}
}

// A run that upgraded the first control plane and then halted on the second
// prints the confinement restart steps too.
func TestUpgradeClusterPrintsConfinementStepsWhenSecondControlPlaneHalts(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodeFixture("cp1", true, "10.0.0.9"), upgradeNodeFixture("cp2", true, "10.0.0.10"))
	fastOrchestrator(t, nil)
	previous := runNodeJob
	runNodeJob = func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) error {
		if s.Node == "cp2" {
			return errors.New("the second control plane's upgrade Job failed")
		}
		return previous(ctx, cs, s)
	}
	var buf bytes.Buffer
	if err := UpgradeCluster(&buf, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs}); err == nil {
		t.Fatal("UpgradeCluster succeeded although cp2's upgrade Job failed")
	}
	if !strings.Contains(buf.String(), "rollout restart ds/cilium") {
		t.Errorf("a run that upgraded cp1 and then halted on cp2 did not print the confinement restart steps:\n%s", buf.String())
	}
}

// TestUpgradeClusterConcurrentWorkerWritesAreSerialized pins the fan-out's
// writer contract: with workers_max_unavailable >= 2, runBounded's worker
// goroutines write progress concurrently into the one writer the run was
// handed, and on the MCP path that writer is a single bytes.Buffer — not safe
// for concurrent writes. The writer the operation is given must therefore make
// concurrent Write calls safe itself. Run under -race: a plain buffer fails.
func TestUpgradeClusterConcurrentWorkerWritesAreSerialized(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	fastOrchestrator(t, nil)

	var buf lockedBuffer
	err := UpgradeCluster(&buf, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs, WorkersMaxUnavailable: 2})
	if err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	if !strings.Contains(buf.String(), "Cluster upgrade complete") {
		t.Errorf("the run never reported completion:\n%s", buf.String())
	}
}

// lockedBuffer is a bare bytes.Buffer behind a mutex, exactly the shape the
// MCP handler holds: a buffer that is only safe because every writer locks.
// The pin only holds if the operation serializes its own writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// setRoots redirects the three stratum roots through version.SetRoots for the
// test and restores the previous roots — and with them every path version
// derives from them — with t.Cleanup. Unlike the per-var redirect helper, this
// moves the init-time derivations too (NodeUpgradeLockPath among them), so a
// test that needs the node's lock or manifest paths off the real /run and
// /var/lib/stratum layout must use this one.
func setRoots(t *testing.T, dataDir, confDir, runDir string) {
	t.Helper()
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	version.SetRoots(dataDir, confDir, runDir)
	t.Cleanup(func() { version.SetRoots(data, conf, run) })
}

// TestNodeLockFollowsTheRunRoot: the node maintenance lock lives at
// version.NodeUpgradeLockPath, which the version package derives from RunDir
// once at init. A SetRoots reassignment must move it — the lock file must be
// created at the redirected path and a second holder must be refused there —
// or an upgrade under redirected roots would fence on (and create) the
// compiled-in /run path.
func TestNodeLockFollowsTheRunRoot(t *testing.T) {
	run := filepath.Join(t.TempDir(), "stratum")
	setRoots(t, t.TempDir(), t.TempDir(), run)
	want := run + "-upgrade.lock"
	if got := version.NodeUpgradeLockPath; got != want {
		t.Fatalf("after SetRoots, version.NodeUpgradeLockPath = %q, want %q — the init-time derivation did not follow the reassigned RunDir", got, want)
	}

	release, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatalf("Acquire at the redirected lock path: %v", err)
	}
	defer release()
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the lock file is not at the redirected path %s: %v", want, err)
	}

	_, err = nodelock.Acquire("rollback")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a second Acquire = %v; want a refusal naming the redirected lock path %q", err, want)
	}
}

// TestMovedPathNamesDelegate: assets.DefaultRoot moved into internal/version,
// where it derives from the data root, and the staging directory has its one
// home there too (version.StageDir) — internal/upgrade declares no StageDir of
// its own (TestStagingDirHasOneHome pins that). The old assets name keeps the
// same value at the default roots — its existing callers (pkg/cli's payload
// flags) must not silently start answering a different path — so it is checked
// before any redirect. The staging half pins the CALL-TIME derivation: under a
// SetRoots redirect, Resolve must answer the redirected version.StageDir, not
// a copy frozen when this package was compiled or first read the var.
func TestMovedPathNamesDelegate(t *testing.T) {
	if assets.DefaultRoot != version.DefaultRoot {
		t.Errorf("assets.DefaultRoot = %q, want the moved version.DefaultRoot %q", assets.DefaultRoot, version.DefaultRoot)
	}

	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	bin := fakeUpgradeBinary(t, map[string]string{"bin/kubelet": "kubelet-binary"})
	src, err := Resolve(bin, []string{"kubelet"})
	if err != nil {
		t.Fatalf("Resolve under the redirected roots: %v", err)
	}
	if want := filepath.Join(version.StageDir, "bin"); src.Dir != want {
		t.Errorf("staged Dir = %q, want %q — staging must resolve version.StageDir at call time, not an init-frozen copy", src.Dir, want)
	}
}

// The upgrade's stage-then-rename writers — copyFile/copyOpened, which put a
// restored CIS config or binary back on the node, and copyBytes, which writes
// the upgrade marker — write live node paths, so each write is a durable
// single-file write: staged under a hidden unique temp name in the file's own
// directory, fsynced at its final mode, renamed into place, and the directory
// fsynced so the rename survives a power loss. That writer is
// internal/atomicfile's, and its filesystem side effects are this package's
// own seams (syncFile and syncDir, declared beside the writers in node.go and
// handed to the writer on every call — the pattern internal/host/systemd.go
// established for the same reason), so the tests below pin the durability
// contract through the delegation. A writer that skips either fsync never
// calls the seam, and the test that needs it fails on the missing call or
// error.

// TestCopyWritesSyncAtTheFinalModeThenTheDirectory: the file is fsynced at its
// final mode before the rename — a mode changed after the fsync is not covered
// by it, so a crash could leave a "successful" write at the temp file's 0600 —
// and the directory holding the file is fsynced after the rename put the new
// file in place: until that sync a power loss can bring the old file back
// under a caller that already acted on the replacement. Pinned through
// copyBytes (the upgrade marker) and copyFile (the CIS-config restore), at the
// modes those paths carry today (0600 and 0644).
func TestCopyWritesSyncAtTheFinalModeThenTheDirectory(t *testing.T) {
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })
	dir := t.TempDir()
	var syncedModes []os.FileMode
	var syncedDirs []string
	var atSync func(*testing.T)
	syncFile = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		syncedModes = append(syncedModes, fi.Mode().Perm())
		return oldSyncFile(f)
	}
	syncDir = func(d string) error {
		syncedDirs = append(syncedDirs, d)
		if atSync != nil {
			atSync(t)
		}
		return oldSyncDir(d)
	}

	// copyBytes is the upgrade marker's writer.
	marker := filepath.Join(dir, "upgrade-marker")
	atSync = func(t *testing.T) {
		if b, err := os.ReadFile(marker); err != nil || string(b) != "marker" {
			t.Errorf("the directory was synced before the rename put the marker in place (%q, %v)", b, err)
		}
	}
	if err := copyBytes(io.Discard, []byte("marker"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(syncedModes) != 1 || syncedModes[0] != 0o600 {
		t.Errorf("the marker was fsynced at modes %v, want [0600] — the final mode, before the rename", syncedModes)
	}
	if len(syncedDirs) != 1 || syncedDirs[0] != dir {
		t.Errorf("synced %v, want exactly [%s] — the rename lives in that directory's entry", syncedDirs, dir)
	}

	// copyFile is the CIS-config restore's writer.
	syncedModes, syncedDirs = nil, nil
	src := filepath.Join(dir, "admission-config.yaml.bak")
	dst := filepath.Join(dir, "admission-config.yaml")
	if err := os.WriteFile(src, []byte("restored"), 0o644); err != nil {
		t.Fatal(err)
	}
	atSync = func(t *testing.T) {
		if b, err := os.ReadFile(dst); err != nil || string(b) != "restored" {
			t.Errorf("the directory was synced before the rename put the restored config in place (%q, %v)", b, err)
		}
	}
	if err := copyFile(io.Discard, src, dst, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(syncedModes) != 1 || syncedModes[0] != 0o644 {
		t.Errorf("the restored config was fsynced at modes %v, want [0644]", syncedModes)
	}
	if len(syncedDirs) != 1 || syncedDirs[0] != dir {
		t.Errorf("synced %v, want exactly [%s]", syncedDirs, dir)
	}
	if fi, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o644 {
		t.Errorf("restored mode = %v, want the backup file's 0644 preserved", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("dir holds %d entries, want exactly the marker, the backup and the restored file (a stray staging copy?)", len(entries))
	}
}

// TestCopyWritesFailureBeforeTheRenameKeepsTheOldFile: the failure injection
// the sync seams make possible, and the whole point of the writer: a write
// that cannot make its file durable must fail the write, leave the file at dst
// byte for byte, and leave no staging copy behind — a torn CIS config or
// upgrade marker where a working one was is the incident this writer exists to
// prevent. A writer that never fsyncs never calls the seam and fails this test
// on the missing error.
func TestCopyWritesFailureBeforeTheRenameKeepsTheOldFile(t *testing.T) {
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })
	boom := errors.New("fsync: input/output error")
	syncFile = func(*os.File) error { return boom }
	var synced []string
	syncDir = func(d string) error { synced = append(synced, d); return oldSyncDir(d) }

	dir := t.TempDir()
	dst := filepath.Join(dir, "admission-config.yaml")
	if err := os.WriteFile(dst, []byte("the old working config"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyBytes(io.Discard, []byte("new"), dst, 0o644); !errors.Is(err, boom) {
		t.Errorf("copyBytes = %v, want the fsync's error — the replacement could not be made durable", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "the old working config" {
		t.Errorf("after the failed write dst = %q, want the old bytes kept", b)
	}
	if len(synced) != 0 {
		t.Errorf("the directory was synced (%v) although the write failed before the rename", synced)
	}

	src := filepath.Join(dir, "admission-config.yaml.bak")
	if err := os.WriteFile(src, []byte("the restored config"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(io.Discard, src, dst, 0o644); !errors.Is(err, boom) {
		t.Errorf("copyFile = %v, want the fsync's error", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "the old working config" {
		t.Errorf("after the failed restore dst = %q, want the old bytes kept", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("dir holds %d entries after the failed writes, want exactly the backup and the untouched config (a stray staging copy?)", len(entries))
	}
}

// TestCopyWritesReportAnUnsyncedRename: the rename is durable only once the
// directory holding it is synced, so a failed directory sync is a failed write
// reported as the writer's ErrNotDurable — the new file is already at dst, and
// the caller must learn that a power loss could still bring the old one back.
func TestCopyWritesReportAnUnsyncedRename(t *testing.T) {
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })
	boom := errors.New("fsync: input/output error")
	syncFile = oldSyncFile
	syncDir = func(string) error { return boom }

	dir := t.TempDir()
	marker := filepath.Join(dir, "upgrade-marker")
	if err := copyBytes(io.Discard, []byte("marker"), marker, 0o600); !errors.Is(err, boom) || !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("copyBytes = %v, want the directory sync's error marked atomicfile.ErrNotDurable", err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "marker" {
		t.Errorf("%s = %q, want the new contents the rename put in place", marker, b)
	}

	src := filepath.Join(dir, "admission-config.yaml.bak")
	dst := filepath.Join(dir, "admission-config.yaml")
	if err := os.WriteFile(src, []byte("restored"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(io.Discard, src, dst, 0o644); !errors.Is(err, boom) || !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("copyFile = %v, want the directory sync's error marked atomicfile.ErrNotDurable", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "restored" {
		t.Errorf("%s = %q, want the new contents the rename put in place", dst, b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("dir holds %d entries after the failed syncs, want exactly the three files (a stray staging copy?)", len(entries))
	}
}

// TestRestoreProfileConfigsWritesDurably: the restore path itself, not only the
// writer beneath it. restoreCISConfigs puts each saved CIS config back through
// copyFile, so every restored config is fsynced and the config directory
// synced — a restore that bypassed the durable writer for a raw write would
// leave the rolled-back apiserver's configs unmade-durable while every
// copyFile-level test still passed. The restored bytes are in place by the
// time a directory sync observes them, and the saved files' modes come back as
// they were (0644 and 0600, as today).
func TestRestoreProfileConfigsWritesDurably(t *testing.T) {
	withCISConfigs(t)
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })

	bak := profileConfigBakDir("20261015-000000")
	if err := os.MkdirAll(bak, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := map[string]string{
		filepath.Base(version.AdmissionConfigPath): "old admission",
		filepath.Base(version.AuditPolicyPath):     "old audit",
	}
	modes := map[string]os.FileMode{
		filepath.Base(version.AdmissionConfigPath): 0o644,
		filepath.Base(version.AuditPolicyPath):     0o600,
	}
	for name, body := range saved {
		if err := os.WriteFile(filepath.Join(bak, name), []byte(body), modes[name]); err != nil {
			t.Fatal(err)
		}
	}
	// The record names every path the backup's writer considered, so the two
	// files it did not save are read as previously absent and removed.
	var record strings.Builder
	for _, p := range profileConfigPaths(cisProfile()) {
		record.WriteString(filepath.Base(p))
		record.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(bak, profileConfigRecordName), []byte(record.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// The live files as the upgrade left them: all four exist, on the new
	// release.
	for _, p := range profileConfigPaths(cisProfile()) {
		if err := os.WriteFile(p, []byte("new "+filepath.Base(p)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restored := map[string]string{}
	for name, body := range saved {
		restored[filepath.Join(version.K8sConfDir, name)] = body
	}

	var syncedDirs []string
	var inPlace []int // how many of the restored files each directory sync saw
	syncFile = oldSyncFile
	syncDir = func(d string) error {
		syncedDirs = append(syncedDirs, d)
		n := 0
		for p, want := range restored {
			if b, err := os.ReadFile(p); err == nil && string(b) == want {
				n++
			}
		}
		inPlace = append(inPlace, n)
		return oldSyncDir(d)
	}

	if err := restoreProfileConfigs(io.Discard, bak); err != nil {
		t.Fatal(err)
	}
	if len(syncedDirs) < len(saved) {
		t.Errorf("the config directory was synced %d time(s) for %d restored configs — the restore bypassed the durable writer", len(syncedDirs), len(saved))
	}
	for _, d := range syncedDirs {
		if d != version.K8sConfDir {
			t.Errorf("synced %s, want the config directory %s", d, version.K8sConfDir)
		}
	}
	if len(inPlace) == 0 || inPlace[len(inPlace)-1] != len(saved) {
		t.Errorf("the last directory sync saw %d restored config(s) in place, want all %d — the sync must follow the renames it makes durable", inPlace, len(saved))
	}
	for p, want := range restored {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s after the restore = %q, want %q", filepath.Base(p), b, want)
		}
		if fi, err := os.Stat(p); err != nil {
			t.Fatal(err)
		} else if fi.Mode().Perm() != modes[filepath.Base(p)] {
			t.Errorf("%s mode = %v, want the saved file's %v preserved", filepath.Base(p), fi.Mode().Perm(), modes[filepath.Base(p)])
		}
	}
}
