package etcdsnap

import (
	"bytes"
	"io"

	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	testStamp  = "20260926-120000"
	testPeer   = "https://10.0.0.11:2380"
	testLedger = "cp1=https://10.0.0.11:2380"
)

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// fault fails a fake call: on its nth occurrence, or on every one when nth is 0.
type fault struct {
	nth int
	err error
}

type fakeNode struct {
	t       *testing.T
	fx      *restoreEffects
	events  []string
	active  map[string]bool
	calls   map[string]int
	faults  map[string]fault // "stop:etcd", "run", "wait:etcd", "start:kube-apiserver", "healthz", ...
	restore func()           // what a successful etcdutl run writes
	// writesNothing: a failed etcdutl run can leave no data dir, as real etcdutl does.
	writesNothing bool
	cmd           *exec.Cmd
	snap          string
	nonces        int
	out           bytes.Buffer        // the restore run's report, for the tests to read
	manifests     map[string][]byte   // what each component's manifest held before the restore
	startedWith   map[string][][]byte // the manifest each start was given, in order
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	_, n := newFakeEffects(t)
	return n
}

func (n *fakeNode) call(key string) error {
	n.events = append(n.events, key)
	n.calls[key]++
	if f, ok := n.faults[key]; ok && (f.nth == 0 || f.nth == n.calls[key]) {
		return f.err
	}
	return nil
}

func (n *fakeNode) called(key string) bool { return n.calls[key] > 0 }

func (n *fakeNode) fail(key string, nth int) { n.faults[key] = fault{nth, errors.New("injected")} }

func (n *fakeNode) restoreErr() error {
	n.t.Helper()
	n.snap = filepath.Join(n.t.TempDir(), "snap.db")
	if err := os.WriteFile(n.snap, []byte("x"), 0o600); err != nil {
		n.t.Fatal(err)
	}
	n.out.Reset()
	return runRestore(&n.out, RestoreOptions{Snapshot: n.snap, PeerURL: testPeer, InitialCluster: testLedger, Stamp: testStamp}, n.fx)
}

func (n *fakeNode) after(key string) []string {
	n.t.Helper()
	i := slices.Index(n.events, key)
	if i < 0 {
		n.t.Fatalf("events = %q; no %s", n.events, key)
	}
	return n.events[i+1:]
}

func has(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func dataDirHolds(t *testing.T, dataDir, marker string) bool {
	t.Helper()
	return has(t, filepath.Join(dataDir, "member", marker))
}

func preRestore(n *fakeNode) string    { return n.fx.etcdDataDir + ".pre-restore-" + testStamp }
func failedRestore(n *fakeNode) string { return n.fx.etcdDataDir + ".failed-restore-" + testStamp }

// rollbackTail is what a rollback runs once etcdutl has touched the data dir.
var rollbackTail = []string{"stop:etcd", "start:etcd", "wait:etcd", "start:kube-apiserver", "healthz"}

// etcdctl snapshot only saves since etcd 3.6, so restore runs etcdutl.
func TestRestoreRunsEtcdutl(t *testing.T) {
	n := newFakeNode(t)
	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	if n.cmd.Path != n.fx.etcdutlPath || filepath.Base(n.cmd.Path) != "etcdutl" {
		t.Errorf("restore ran %q, want %q", n.cmd.Path, n.fx.etcdutlPath)
	}
	token := version.EtcdClusterToken + "-restore-" + testStamp + "-nonce1"
	want := []string{"snapshot", "restore", n.snap,
		"--data-dir", n.fx.etcdDataDir,
		"--name", version.EtcdName,
		"--initial-advertise-peer-urls", testPeer,
		"--initial-cluster", testLedger,
		"--initial-cluster-token", token,
	}
	if got := n.cmd.Args[1:]; !slices.Equal(got, want) {
		t.Errorf("etcdutl args = %q, want %q", got, want)
	}
	if token == version.EtcdClusterToken {
		t.Error("the restore reuses the bootstrap cluster token, so it reproduces the old cluster-id")
	}
}

// Stamp has second precision, so a same-second retry still needs its own cluster token.
func TestSameSecondRestoresGetDistinctTokens(t *testing.T) {
	n := newFakeNode(t)
	token := func() string {
		t.Helper()
		if err := n.restoreErr(); err != nil {
			t.Fatal(err)
		}
		i := slices.Index(n.cmd.Args, "--initial-cluster-token")
		if i < 0 || i+1 >= len(n.cmd.Args) {
			t.Fatalf("etcdutl args = %q; no cluster token", n.cmd.Args)
		}
		return n.cmd.Args[i+1]
	}
	first, second := token(), token()
	if first == second {
		t.Errorf("two restores with stamp %s both used cluster token %q", testStamp, first)
	}
	if a, b := newNonce(), newNonce(); a == b || len(a) != 8 {
		t.Errorf("newNonce() = %q, %q; want two distinct 8-character nonces", a, b)
	}
}

// a node staged by an older release has no etcdutl; found before anything is stopped.
func TestRestoreRefusesWithoutEtcdutl(t *testing.T) {
	n := newFakeNode(t)
	if err := os.Remove(n.fx.etcdutlPath); err != nil {
		t.Fatal(err)
	}
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "nothing was stopped") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if len(n.events) != 0 {
		t.Errorf("events = %q; something was stopped without an etcdutl to restore with", n.events)
	}
}

func TestRestoreSucceeds(t *testing.T) {
	n := newFakeNode(t)
	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	want := []string{"read:kube-apiserver", "read:etcd", "stop:kube-apiserver", "stop:etcd", "run",
		"start:etcd", "wait:etcd", "start:kube-apiserver", "healthz",
		"restart:kubelet", "restart:kube-controller-manager", "restart:kube-scheduler", "restart:kube-vip",
		"restart:operator", "restart:cilium", "restart:coredns", "restart:local-path"}
	if !slices.Equal(n.events, want) {
		t.Errorf("events = %q, want %q", n.events, want)
	}
	if !dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
		t.Error("the data dir does not hold the restored snapshot")
	}
	if !has(t, preRestore(n)) {
		t.Error("the pre-restore data was not kept aside")
	}
}

// rolling Cilium needs the controllers' stale watches cleared before the rollout.
func TestCiliumIsRestartedAfterTheControllers(t *testing.T) {
	n := newFakeNode(t)
	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	cilium := slices.Index(n.events, "restart:cilium")
	for _, dep := range []string{"restart:kubelet", "restart:kube-controller-manager", "restart:kube-scheduler"} {
		if i := slices.Index(n.events, dep); i < 0 || i > cilium {
			t.Errorf("%s did not come before restart:cilium: %q", dep, n.events)
		}
	}
}

// a failed restart must not stop the others; reported as unfinished, not failed.
func TestRestoreTriesEveryWatcherAndNamesTheOnesThatFailed(t *testing.T) {
	n := newFakeNode(t)
	n.fail("restart:kubelet", 0)
	n.fail("restart:kube-scheduler", 0)
	err := n.restoreErr()
	if !errors.Is(err, ErrWatchersNotRestarted) ||
		!strings.Contains(err.Error(), "kubelet, kube-scheduler") || strings.Contains(err.Error(), "cilium") {
		t.Fatalf("err = %v", err)
	}
	if !n.called("restart:cilium") || !n.called("restart:kube-controller-manager") {
		t.Errorf("events = %q; a failed kubelet restart stopped the rest", n.events)
	}
	if !dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
		t.Error("the restore was rolled back because a restart failed")
	}
}

// a component this node or cluster does not have is not an error.
func TestRestoreSkipsWatchersThisNodeDoesNotRun(t *testing.T) {
	n := newFakeNode(t)
	n.fx.restoreKubelet = func() (bool, error) { return false, nil }
	n.fx.restoreWorkloads = func() []workloadResult { return nil }
	n.fx.restoreStaticPodExists = func(name string) bool { return name == "kube-scheduler" }
	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	if n.called("restart:kube-controller-manager") || !n.called("restart:kube-scheduler") {
		t.Errorf("events = %q", n.events)
	}
}

// the watchers are restarted only once the restored apiserver is healthy.
func TestNothingIsRestartedWhenTheRestoreFails(t *testing.T) {
	for _, failAt := range []string{"run", "wait:etcd", "healthz"} {
		n := newFakeNode(t)
		n.fail(failAt, 0)
		_ = n.restoreErr()
		for _, e := range n.events {
			if strings.HasPrefix(e, "restart:") {
				t.Errorf("%s failed, but %s was called", failAt, e)
			}
		}
	}
}

// a failure before etcd is up on the restored data rolls the original data back.
func TestFailedRestoreRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, failAt  string
		writesNothing bool // etcdutl fails before creating a data dir
	}{
		{"etcdutl fails", "run", false},
		{"etcdutl fails before writing anything", "run", true},
		{"etcd will not start on the restored data", "start:etcd", false},
		{"etcd does not come up on the restored data", "wait:etcd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNode(t)
			n.writesNothing = tc.writesNothing
			n.fail(tc.failAt, 1) // only the attempt on the restored data
			err := n.restoreErr()
			if err == nil {
				t.Fatal("a failed restore reported success")
			}
			if !strings.Contains(err.Error(), "rolled back: kube-apiserver and etcd restarted on the original data, which is back in place") {
				t.Errorf("error does not say it rolled back: %v", err)
			}
			if got := n.after(tc.failAt); !slices.Equal(got, rollbackTail) {
				t.Errorf("rollback events = %q, want %q", got, rollbackTail)
			}
			if !dataDirHolds(t, n.fx.etcdDataDir, "ORIGINAL") || dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
				t.Error("the original data is not back in the data dir")
			}
			if has(t, preRestore(n)) {
				t.Error("the original data was left at its pre-restore path")
			}
			kept := "the failed restore is kept at " + failedRestore(n)
			if tc.writesNothing {
				if has(t, failedRestore(n)) || strings.Contains(err.Error(), "failed restore is kept") {
					t.Errorf("a failed restore was reported or kept when etcdutl wrote nothing: %v", err)
				}
			} else {
				if !has(t, failedRestore(n)) {
					t.Error("the failed restore was not kept aside")
				}
				if !strings.Contains(err.Error(), kept) {
					t.Errorf("error does not say where the failed restore is kept: %v", err)
				}
			}
		})
	}
}

// a failed stop restarts what was already stopped; the data is untouched.
func TestFailedStopRestartsWhatWasStopped(t *testing.T) {
	for _, tc := range []struct {
		failAt, msg string
		tail        []string
	}{
		{"stop:kube-apiserver", "rolled back: kube-apiserver restarted", []string{"start:kube-apiserver", "healthz"}},
		{"stop:etcd", "rolled back: kube-apiserver and etcd restarted",
			[]string{"start:etcd", "wait:etcd", "start:kube-apiserver", "healthz"}},
	} {
		t.Run(tc.failAt, func(t *testing.T) {
			n := newFakeNode(t)
			n.fail(tc.failAt, 0)
			err := n.restoreErr()
			if err == nil || !strings.Contains(err.Error(), "aborted before touching the data dir") ||
				!strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v", err)
			}
			if got := n.after(tc.failAt); !slices.Equal(got, tc.tail) {
				t.Errorf("rollback events = %q, want %q", got, tc.tail)
			}
			if !dataDirHolds(t, n.fx.etcdDataDir, "ORIGINAL") || slices.Contains(n.events, "run") {
				t.Error("the data dir was touched after a failed stop")
			}
		})
	}
}

// once etcd is up on the restored data, an apiserver failure keeps the restored data.
func TestApiserverFailureKeepsASuccessfulRestore(t *testing.T) {
	for _, failAt := range []string{"start:kube-apiserver", "healthz"} {
		t.Run(failAt, func(t *testing.T) {
			n := newFakeNode(t)
			n.fail(failAt, 0)
			err := n.restoreErr()
			if err == nil || !strings.Contains(err.Error(), "etcd is running on the restored data") ||
				!strings.Contains(err.Error(), "the original data is kept at "+preRestore(n)) {
				t.Fatalf("err = %v", err)
			}
			if !dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
				t.Error("a successful restore was rolled back because the apiserver failed")
			}
			if slices.Contains(n.after("wait:etcd"), "stop:etcd") {
				t.Errorf("events = %q; etcd was stopped after coming up on the restored data", n.events)
			}
		})
	}
}

// when the rollback itself fails, the error says so and no apiserver starts without etcd.
func TestUnfinishedRollbackSaysSo(t *testing.T) {
	n := newFakeNode(t)
	n.fail("run", 0)
	n.fail("start:etcd", 0)
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK DID NOT FINISH") || !strings.Contains(err.Error(), "restart etcd") ||
		!strings.Contains(err.Error(), "kube-apiserver not restarted") ||
		!strings.Contains(err.Error(), "the original data is back in "+n.fx.etcdDataDir) ||
		!strings.Contains(err.Error(), "the failed restore is kept at "+failedRestore(n)) ||
		strings.Contains(err.Error(), "still in") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(n.after("run"), "start:kube-apiserver") {
		t.Errorf("events = %q; the apiserver was started with etcd down", n.events)
	}
}

// a same-second retry's rollback must not collide with the previous attempt's directories.
func TestRollbackAvoidsAnEarlierAttemptsPaths(t *testing.T) {
	n := newFakeNode(t)
	for _, p := range []string{preRestore(n), failedRestore(n)} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n.fail("run", 0)
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "which is back in place") ||
		!strings.Contains(err.Error(), "the failed restore is kept at "+failedRestore(n)+"-1") {
		t.Fatalf("err = %v", err)
	}
	if !dataDirHolds(t, n.fx.etcdDataDir, "ORIGINAL") {
		t.Error("the original data is not back in the data dir")
	}
	if !has(t, failedRestore(n)+"-1") {
		t.Error("the failed restore was not kept at a fresh path")
	}
	for _, p := range []string{preRestore(n), failedRestore(n)} {
		if !has(t, p) {
			t.Errorf("the earlier attempt's %s was disturbed", p)
		}
	}
}

// with etcd unstoppable, the data dir is not moved and neither etcd nor the apiserver restarts.
func TestRollbackLeavesDataWhenEtcdWillNotStop(t *testing.T) {
	n := newFakeNode(t)
	n.fail("run", 0)
	n.fail("stop:etcd", 2)
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK DID NOT FINISH") ||
		!strings.Contains(err.Error(), "etcd may still be running on the failed restore") ||
		!strings.Contains(err.Error(), "the original data is at "+preRestore(n)) ||
		!strings.Contains(err.Error(), "the failed restore is still in "+n.fx.etcdDataDir) {
		t.Fatalf("err = %v", err)
	}
	if !has(t, preRestore(n)) {
		t.Error("the original data was moved while etcd would not stop")
	}
	if got := n.after("run"); !slices.Equal(got, []string{"stop:etcd"}) {
		t.Errorf("rollback events = %q; want only the failed stop", got)
	}
}

// when the original cannot go back, etcd must not start on whatever the data dir holds.
func TestRollbackDoesNotStartEtcdOffTheOriginalData(t *testing.T) {
	n := newFakeNode(t)
	n.fail("run", 0)
	fake := n.fx.restoreRun
	n.fx.restoreRun = func(cmd *exec.Cmd) error {
		err := fake(cmd)
		// Occupy the failed-restore path, so moving it aside fails and the original cannot go back.
		if e := os.MkdirAll(filepath.Join(failedRestore(n), "x"), 0o700); e != nil {
			t.Fatal(e)
		}
		return err
	}
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK DID NOT FINISH") ||
		!strings.Contains(err.Error(), "kube-apiserver not restarted") ||
		!strings.Contains(err.Error(), "the failed restore is still in "+n.fx.etcdDataDir) {
		t.Fatalf("err = %v", err)
	}
	if got := n.after("run"); !slices.Equal(got, []string{"stop:etcd"}) {
		t.Errorf("rollback events = %q; want only the stop", got)
	}
	if !has(t, preRestore(n)) {
		t.Error("the original data was not left where the error says it is")
	}
}

// an error other than not-existing is returned, not treated as "taken" forever.
func TestFreePathReturnsErrors(t *testing.T) {
	dir := t.TempDir()
	if p, err := freePath(filepath.Join(dir, "a"), pathExists); err != nil || p != filepath.Join(dir, "a") {
		t.Fatalf("free path = %q, %v", p, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A path under a regular file fails with ENOTDIR, not ENOENT.
	if _, err := freePath(filepath.Join(dir, "file", "a"), pathExists); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("freePath under a regular file = %v, want the ENOTDIR it hit", err)
	}
}

// with no original data, the whole rollback is moving the failed restore aside.
func TestRestoreOntoAMissingDataDir(t *testing.T) {
	missing := func(t *testing.T) *fakeNode {
		n := newFakeNode(t)
		if err := os.RemoveAll(n.fx.etcdDataDir); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("succeeds", func(t *testing.T) {
		n := missing(t)
		if err := n.restoreErr(); err != nil {
			t.Fatal(err)
		}
		if !dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
			t.Error("the data dir does not hold the restored snapshot")
		}
	})
	t.Run("fails", func(t *testing.T) {
		n := missing(t)
		n.fail("run", 0)
		err := n.restoreErr()
		if err == nil || !strings.Contains(err.Error(), "nothing to roll back to: there was no etcd data dir before the restore") ||
			strings.Contains(err.Error(), "ROLLBACK DID NOT FINISH") {
			t.Fatalf("err = %v", err)
		}
		if got := n.after("run"); !slices.Equal(got, []string{"stop:etcd"}) {
			t.Errorf("rollback events = %q; want etcd stopped and nothing started", got)
		}
		if has(t, n.fx.etcdDataDir) {
			t.Error("the failed restore was left in the data dir")
		}
		if !has(t, failedRestore(n)) {
			t.Error("the failed restore was not kept aside")
		}
	})
	t.Run("apiserver fails after the restore", func(t *testing.T) {
		n := missing(t)
		n.fail("healthz", 0)
		err := n.restoreErr()
		if err == nil || !strings.Contains(err.Error(), "there was no data dir before the restore") ||
			strings.Contains(err.Error(), ".pre-restore-") {
			t.Fatalf("err = %v; it must not name a pre-restore dir that was never created", err)
		}
	})
}

// an error other than not-existing is not taken to mean the data dir is absent.
func TestUnreadableDataDirAbortsBeforeTouchingIt(t *testing.T) {
	n := newFakeNode(t)
	exists := n.fx.restorePathExists
	n.fx.restorePathExists = func(p string) (bool, error) {
		if p == n.fx.etcdDataDir {
			return false, fmt.Errorf("check %s: %w", p, syscall.EIO)
		}
		return exists(p)
	}
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "aborted before touching the data dir") ||
		!errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v", err)
	}
	if !dataDirHolds(t, n.fx.etcdDataDir, "ORIGINAL") || slices.Contains(n.events, "run") {
		t.Errorf("events = %q; etcdutl ran against an unreadable data dir", n.events)
	}
	if !n.active["etcd"] || !n.active["kube-apiserver"] {
		t.Error("the stopped components were not restarted")
	}
}

// re-rendering would put the newer binary's images into an upgrade rollback's older manifests.
func TestRestorePutsBackTheManifestsItStopped(t *testing.T) {
	for _, tc := range []struct{ name, failAt string }{
		{"a restore that succeeds", ""},
		{"a restore that is rolled back", "run"},
		{"a restore rolled back after etcd would not start on it", "start:etcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNode(t)
			if tc.failAt != "" {
				n.fail(tc.failAt, 1)
			}
			_ = n.restoreErr()
			for name, want := range n.manifests {
				got := n.startedWith[name]
				if len(got) == 0 {
					t.Fatalf("%s was never started; events = %q", name, n.events)
				}
				for _, m := range got {
					if string(m) != string(want) {
						t.Errorf("%s started with %q, want the manifest it had before the restore, %q", name, m, want)
					}
				}
			}
		})
	}
}

// a component with no manifest before the restore is rendered as before.
func TestRestoreRendersWhenThereWasNoManifest(t *testing.T) {
	n := newFakeNode(t)
	n.manifests["etcd"] = nil
	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	if got := n.startedWith["etcd"]; len(got) != 1 || got[0] != nil {
		t.Errorf("etcd started with %q, want one start with no manifest (render)", got)
	}
}

// a manifest that cannot be read cannot be put back, so the restore refuses first.
func TestUnreadableManifestStopsNothing(t *testing.T) {
	n := newFakeNode(t)
	n.fail("read:etcd", 0)
	err := n.restoreErr()
	if err == nil || !strings.Contains(err.Error(), "read the etcd manifest") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("err = %v", err)
	}
	for _, e := range n.events {
		if strings.HasPrefix(e, "stop:") {
			t.Errorf("events = %q; a component was stopped", n.events)
		}
	}
}

// startManifest puts a saved manifest back byte for byte, rather than rendering.
func TestStartManifestWritesTheSavedBytes(t *testing.T) {
	swap(t, &version.K8sConfDir, t.TempDir())
	saved := []byte("apiVersion: v1\nkind: Pod\nspec:\n  containers:\n  - image: registry.example/etcd:3.6.0-old\n")
	if err := startManifest("etcd", saved); err != nil {
		t.Fatal(err)
	}
	got, err := staticpod.Read("etcd")
	if err != nil || string(got) != string(saved) {
		t.Fatalf("etcd manifest after start = %q, %v; want the saved bytes %q", got, err, saved)
	}
}

// A directory passes os.Stat; it is refused before anything is stopped.
func TestRestoreRefusesADirectory(t *testing.T) {
	n := newFakeNode(t)
	err := runRestore(io.Discard, RestoreOptions{Snapshot: t.TempDir(), PeerURL: testPeer, InitialCluster: testLedger, Stamp: testStamp}, n.fx)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v", err)
	}
	if len(n.events) != 0 {
		t.Errorf("events = %q; something was stopped for a directory", n.events)
	}
}

// newFakeEffects builds a complete fake restoreEffects over a recording fakeNode.
func newFakeEffects(t *testing.T) (*restoreEffects, *fakeNode) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "etcd")
	binDir := filepath.Join(root, "bin")
	etcdutlPath := filepath.Join(binDir, "etcdutl")
	if err := os.MkdirAll(filepath.Join(dataDir, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "member", "ORIGINAL"), []byte("live data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(etcdutlPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	n := &fakeNode{t: t, active: map[string]bool{"etcd": true, "kube-apiserver": true},
		calls: map[string]int{}, faults: map[string]fault{}}
	n.restore = func() {
		_ = os.MkdirAll(filepath.Join(dataDir, "member"), 0o700)
		_ = os.WriteFile(filepath.Join(dataDir, "member", "RESTORED"), []byte("snapshot"), 0o600)
	}
	n.manifests = map[string][]byte{"etcd": []byte("etcd manifest"), "kube-apiserver": []byte("kube-apiserver manifest")}
	n.startedWith = map[string][][]byte{}

	fx := &restoreEffects{
		etcdDataDir:              dataDir,
		etcdutlPath:              etcdutlPath,
		restoreAcquireNodeLock:   func() (func(), error) { return func() {}, nil },
		restoreStat:              os.Stat,
		restoreCopies:            func() ([]RestoreCopy, error) { return restoreCopies(dataDir) },
		restoreAcquireDataLock:   func() (func(), error) { return func() {}, nil },
		restoreSupervisorRunning: func() bool { return true },
		restoreStartLockHeld:     func() (bool, error) { return true, nil },
		restoreStop: func(name string) error {
			if err := n.call("stop:" + name); err != nil {
				return err
			}
			n.active[name] = false
			return nil
		},
		restoreReadManifest: func(name string) ([]byte, error) {
			if err := n.call("read:" + name); err != nil {
				return nil, err
			}
			return n.manifests[name], nil
		},
		restoreStart: func(name string, manifest []byte) error {
			n.startedWith[name] = append(n.startedWith[name], manifest)
			if err := n.call("start:" + name); err != nil {
				return err
			}
			n.active[name] = true
			return nil
		},
		restoreWaitActive:  func(name string) error { return n.call("wait:" + name) },
		restoreWaitHealthz: func() error { return n.call("healthz") },
		restorePathExists:  pathExists,
		restoreRun: func(cmd *exec.Cmd) error {
			n.cmd = cmd
			if err := n.call("run"); err != nil {
				if !n.writesNothing {
					// A failed restore can leave a data dir behind.
					_ = os.MkdirAll(filepath.Join(dataDir, "member"), 0o700)
				}
				return err
			}
			n.restore()
			return nil
		},
		restoreNonce:    func() string { n.nonces++; return fmt.Sprintf("nonce%d", n.nonces) },
		restoreKubelet:  func() (bool, error) { return true, n.call("restart:kubelet") },
		restoreOperator: func() (bool, error) { return true, n.call("restart:operator") },
		restoreWorkloads: func() []workloadResult {
			return []workloadResult{
				{name: "cilium agent + operator", present: true, err: n.call("restart:cilium")},
				{name: "coredns", present: true, err: n.call("restart:coredns")},
				{name: "local-path provisioner", present: true, err: n.call("restart:local-path")},
			}
		},
		restoreStaticPodExists: func(string) bool { return true },
		restoreStaticPod:       func(name string) error { return n.call("restart:" + name) },
		// The data-dir moves rename for real on the temp paths and record no event.
		restoreMoveDir: os.Rename,
	}
	n.fx = fx
	// seedKubectl, baseKubectl, compsStatus, compsRestart unset on purpose; the sentinel catches a use.
	return fx, n
}

// runFakeRestore drives runRestore with a written snapshot and the shared test options.
func runFakeRestore(t *testing.T, fx *restoreEffects, callerHoldsNodeLock bool) error {
	t.Helper()
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return runRestore(io.Discard, RestoreOptions{
		Snapshot:       snap,
		PeerURL:        testPeer,
		InitialCluster: testLedger,
		Stamp:          testStamp,
		NodeLockHeld:   callerHoldsNodeLock,
	}, fx)
}

// a harness that forgot a stub must hit the sentinel, never a real host effect.
func TestUnsetRestoreEffectsFailClosed(t *testing.T) {
	t.Parallel()
	err := runRestore(io.Discard, RestoreOptions{
		Snapshot: "snap.db", PeerURL: testPeer, InitialCluster: testLedger, NodeLockHeld: true,
	}, &restoreEffects{})
	if err == nil {
		t.Fatal("runRestore with every effect unset succeeded; want the unset-effect sentinel")
	}
	if !errors.Is(err, errUnsetEffect) {
		t.Fatalf("runRestore = %v, want it to wrap the unset-effect sentinel", err)
	}
	if !strings.Contains(err.Error(), "restoreAcquireDataLock") {
		t.Errorf("the sentinel does not name the effect it stopped at, so the harness cannot say which stub is missing: %v", err)
	}
}

// the refusal probes are host reads; a missing stub must hit the sentinel, not a panic.
func TestUnsetRefusalProbesFailClosed(t *testing.T) {
	t.Parallel()
	o := RestoreOptions{Snapshot: "snap.db", PeerURL: testPeer, InitialCluster: testLedger, NodeLockHeld: true}
	fx := &restoreEffects{restoreAcquireDataLock: func() (func(), error) { return func() {}, nil }}
	err := runRestore(io.Discard, o, fx)
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "restoreSupervisorRunning") {
		t.Fatalf("runRestore without a supervisor probe = %v; want the sentinel naming restoreSupervisorRunning", err)
	}
	fx.restoreSupervisorRunning = func() bool { return false }
	err = runRestore(io.Discard, o, fx)
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "restoreStartLockHeld") {
		t.Fatalf("runRestore without a start-lock probe = %v; want the sentinel naming restoreStartLockHeld", err)
	}
}

// The snapshot is deliberately absent: reaching it would mean the gate ran late.
func TestRestoreRunnerRefusesAStartInProgress(t *testing.T) {
	t.Parallel()
	released := false
	fx := &restoreEffects{
		restoreAcquireDataLock:   func() (func(), error) { return func() { released = true }, nil },
		restoreSupervisorRunning: func() bool { return false },
		restoreStartLockHeld:     func() (bool, error) { return true, nil },
	}
	err := runRestore(io.Discard, RestoreOptions{
		Snapshot: "/nonexistent/snap.db", PeerURL: testPeer, InitialCluster: testLedger, NodeLockHeld: true,
	}, fx)
	if err == nil || !strings.Contains(err.Error(), "a stratum node is starting") {
		t.Fatalf("runRestore during a node start = %v", err)
	}
	if !released {
		t.Error("the data lock was not released on the refusal")
	}
}

// pins the same order as TestRestoreSucceeds, driven only by the effects value.
func TestRestoreThroughTheEffectsValueSucceeds(t *testing.T) {
	fx, n := newFakeEffects(t)
	if err := runFakeRestore(t, fx, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"read:kube-apiserver", "read:etcd", "stop:kube-apiserver", "stop:etcd", "run",
		"start:etcd", "wait:etcd", "start:kube-apiserver", "healthz",
		"restart:kubelet", "restart:kube-controller-manager", "restart:kube-scheduler", "restart:kube-vip",
		"restart:operator", "restart:cilium", "restart:coredns", "restart:local-path"}
	if !slices.Equal(n.events, want) {
		t.Errorf("events = %q, want %q", n.events, want)
	}
	if !dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
		t.Error("the data dir does not hold the restored snapshot")
	}
	if !has(t, preRestore(n)) {
		t.Error("the pre-restore data was not kept aside")
	}
}

// a failed etcdutl run rolls back through the same effects value.
func TestRollbackRunsThroughTheEffectsValue(t *testing.T) {
	fx, n := newFakeEffects(t)
	n.fail("run", 0)
	err := runFakeRestore(t, fx, true)
	if err == nil {
		t.Fatal("a failed restore reported success")
	}
	if got := n.after("run"); !slices.Equal(got, rollbackTail) {
		t.Errorf("rollback events = %q, want %q", got, rollbackTail)
	}
	if !dataDirHolds(t, n.fx.etcdDataDir, "ORIGINAL") || dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
		t.Error("the original data is not back in the data dir")
	}
	if !has(t, failedRestore(n)) {
		t.Error("the failed restore was not kept aside")
	}
}

// whatever field is unset, the run stops there with the sentinel naming it.
func TestMissingStubStopsTheRunAtItsEffect(t *testing.T) {
	for _, tc := range []struct {
		name, field, forbid string
		unset               func(fx *restoreEffects)
	}{
		{"an unreadable manifest stops nothing", "restoreReadManifest", "stop:",
			func(fx *restoreEffects) { fx.restoreReadManifest = nil }},
		{"a forgotten stop never runs etcdutl", "restoreStop", "run",
			func(fx *restoreEffects) { fx.restoreStop = nil }},
		{"a forgotten nonce stops no component", "restoreNonce", "stop:",
			func(fx *restoreEffects) { fx.restoreNonce = nil }},
		{"a forgotten data-dir move never runs etcdutl", "restoreMoveDir", "run",
			func(fx *restoreEffects) { fx.restoreMoveDir = nil }},
		{"a forgotten health gate restarts no watcher", "restoreWaitHealthz", "restart:kubelet",
			func(fx *restoreEffects) { fx.restoreWaitHealthz = nil }},
		{"a forgotten workload restart reaches no kubectl", "restoreWorkloads", "restart:cilium",
			func(fx *restoreEffects) { fx.restoreWorkloads = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, n := newFakeEffects(t)
			tc.unset(fx)
			err := runFakeRestore(t, fx, true)
			if err == nil || !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("runRestore with %s unset = %v; want the sentinel naming the field", tc.field, err)
			}
			if slices.Contains(n.events, tc.forbid) {
				t.Errorf("events = %q; %s was reached with %s unset", n.events, tc.forbid, tc.field)
			}
		})
	}
}

// the node lock must be held when the data lock is taken, and released after.
func TestRestoreRunnerTakesTheNodeLockAroundTheDataLock(t *testing.T) {
	t.Parallel()
	fx, _ := newFakeEffects(t)
	held := false
	fx.restoreAcquireNodeLock = func() (func(), error) { held = true; return func() { held = false }, nil }
	heldAtDataLock := false
	fx.restoreAcquireDataLock = func() (func(), error) {
		heldAtDataLock = held
		return func() {}, nil
	}
	if err := runFakeRestore(t, fx, false); err != nil {
		t.Fatal(err)
	}
	if !heldAtDataLock {
		t.Error("the node maintenance lock was not held when the data lock was taken: the runner takes the locks in the wrong order, or not at all")
	}
	if held {
		t.Error("the node maintenance lock was still held after the run returned")
	}
}

// a caller-held node lock lifts only the maintenance lock; the data lock is still taken.
func TestRestoreRunnerSkipsTheNodeLockWhenTheCallerHoldsIt(t *testing.T) {
	t.Parallel()
	fx, _ := newFakeEffects(t)
	fx.restoreAcquireNodeLock = nil
	dataLocks := 0
	fx.restoreAcquireDataLock = func() (func(), error) {
		dataLocks++
		return func() {}, nil
	}
	if err := runFakeRestore(t, fx, true); err != nil {
		t.Fatal(err)
	}
	if dataLocks != 1 {
		t.Errorf("data lock taken %d times; want once while caller holds the node lock", dataLocks)
	}
}

// pins the effects inventory, timing knobs included; extra fields are allowed.
func TestRestoreEffectsCoverTheInventory(t *testing.T) {
	t.Parallel()
	effectFields := []string{
		"restoreAcquireNodeLock", "restoreStat", "restoreCopies", "restoreAcquireDataLock", "restoreSupervisorRunning", "restoreStartLockHeld",
		"restoreStop", "restoreReadManifest", "restoreStart", "restoreWaitActive",
		"restoreWaitHealthz", "restorePathExists", "restoreRun", "restoreNonce",
		"restoreKubelet", "restoreOperator", "compsStatus", "compsRestart",
		"restoreWorkloads", "seedKubectl", "baseKubectl", "restoreStaticPod",
		"restoreStaticPodExists", "restoreRemoveAll",
	}
	durationFields := []string{"kubectlCallTimeout", "healthzWindow", "rolloutWait"}
	fields := map[string]reflect.StructField{}
	ty := reflect.TypeOf(restoreEffects{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		fields[f.Name] = f
	}
	for _, name := range effectFields {
		f, ok := fields[name]
		if !ok {
			t.Errorf("restoreEffects has no %s field; the inventoried effect would stay a package variable", name)
			continue
		}
		if f.Type.Kind() != reflect.Func {
			t.Errorf("restoreEffects.%s is a %s, want a host effect function", name, f.Type.Kind())
		}
	}
	for _, name := range durationFields {
		f, ok := fields[name]
		if !ok {
			t.Errorf("restoreEffects has no %s field; the timing knob would stay a package variable", name)
			continue
		}
		if f.Type != reflect.TypeOf(time.Duration(0)) {
			t.Errorf("restoreEffects.%s is a %s, want a time.Duration", name, f.Type)
		}
	}
}

// parseProduction reads the package's non-test Go files, for structure pins on production code.
func parseProduction(t *testing.T) (map[string]*ast.File, map[string]*ast.FuncDecl) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	funcs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	return files, funcs
}

// no restore effect or timing knob may remain a package-level variable in production.
func TestRestoreEffectVariablesAreGone(t *testing.T) {
	t.Parallel()
	gone := []string{
		"restoreAcquireNodeLock", "restoreStat", "restoreCopies", "restoreAcquireDataLock", "restoreSupervisorRunning", "restoreStartLockHeld",
		"restoreStop", "restoreReadManifest", "restoreStart", "restoreWaitActive",
		"restoreWaitHealthz", "restorePathExists", "restoreRun", "restoreNonce",
		"restoreKubelet", "restoreOperator", "compsStatus", "compsRestart",
		"restoreWorkloads", "seedKubectl", "baseKubectl", "restoreStaticPod",
		"restoreStaticPodExists", "restoreRemoveAll", "restoreMoveDir",
		"kubectlCallTimeout", "healthzWindow", "rolloutWait",
	}
	declared := map[string]string{} // variable name -> declaring file
	files, _ := parseProduction(t)
	for name, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					declared[id.Name] = name
				}
			}
		}
	}
	for _, name := range gone {
		if file, ok := declared[name]; ok {
			t.Errorf("%s is still a package-level variable (%s); the effect belongs on the effects value", name, file)
		}
	}
}

// pins the seam's structure in the production files: the entry points own the effects value.
func TestRestoreOwnsTheProductionRestoreEffects(t *testing.T) {
	t.Parallel()
	files, funcs := parseProduction(t)
	isType := func(e ast.Expr, name string) bool {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name == name
		}
		// A qualified type such as io.Writer is a selector, not an ident.
		if sel, ok := e.(*ast.SelectorExpr); ok {
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name+"."+sel.Sel.Name == name
		}
		return false
	}
	isPointerTo := func(e ast.Expr, name string) bool {
		star, ok := e.(*ast.StarExpr)
		return ok && isType(star.X, name)
	}
	isSliceOf := func(e ast.Expr, name string) bool {
		arr, ok := e.(*ast.ArrayType)
		return ok && isType(arr.Elt, name)
	}

	restore := funcs["Restore"]
	if restore == nil {
		t.Fatal("no Restore in the production files; the entry point must keep its exported signature")
	}
	// The writer convention: the writer leads, then the options.
	if restore.Recv != nil || restore.Type.Params.NumFields() != 2 || !isType(restore.Type.Params.List[0].Type, "io.Writer") ||
		!isType(restore.Type.Params.List[1].Type, "RestoreOptions") ||
		restore.Type.Results.NumFields() != 1 || !isType(restore.Type.Results.List[0].Type, "error") {
		t.Error("Restore's signature changed; the writer convention is Restore(w io.Writer, o RestoreOptions) error and it must not")
	}

	declared := false
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "restoreEffects" {
					continue
				}
				declared = true
				if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
					t.Errorf("restoreEffects is %T, want a struct of function fields", ts.Type)
				}
			}
		}
	}
	if !declared {
		t.Fatal("no restoreEffects struct in the production files; the one effects value is the seam")
	}

	run := funcs["runRestore"]
	if run == nil {
		t.Fatal("no unexported runRestore; the runner is what takes the effects value")
	}
	// The writer convention: the writer leads, then the options, then the effects value.
	if run.Recv != nil || run.Type.Params.NumFields() != 3 ||
		!isType(run.Type.Params.List[0].Type, "io.Writer") ||
		!isType(run.Type.Params.List[1].Type, "RestoreOptions") ||
		!isPointerTo(run.Type.Params.List[2].Type, "restoreEffects") ||
		run.Type.Results.NumFields() != 1 || !isType(run.Type.Results.List[0].Type, "error") {
		t.Error("runRestore's signature changed; want func runRestore(w io.Writer, o RestoreOptions, fx *restoreEffects) error")
	}

	delegates := false
	ast.Inspect(restore.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "runRestore" {
			delegates = true
		}
		return true
	})
	if !delegates {
		t.Error("Restore never mentions runRestore; the runner must be what the entry point calls")
	}

	// RemoveRestoreCopies keeps its exported signature; pkg/cli and the MCP tool call it.
	removeCopies := funcs["RemoveRestoreCopies"]
	if removeCopies == nil {
		t.Fatal("no RemoveRestoreCopies; the copies removal is an exported entry point")
	}
	if removeCopies.Recv != nil || removeCopies.Type.Params.NumFields() != 1 ||
		!isSliceOf(removeCopies.Type.Params.List[0].Type, "RestoreCopy") ||
		removeCopies.Type.Results.NumFields() != 2 ||
		!isSliceOf(removeCopies.Type.Results.List[0].Type, "RestoreCopy") ||
		!isType(removeCopies.Type.Results.List[1].Type, "error") {
		t.Error("RemoveRestoreCopies's signature changed; pkg/cli calls RemoveRestoreCopies(shown []RestoreCopy) ([]RestoreCopy, error) and it must not")
	}
	removes := funcs["removeRestoreCopies"]
	if removes == nil {
		t.Fatal("no unexported removeRestoreCopies; the copies removal must go through the effects value too")
	}
	if removes.Recv != nil || removes.Type.Params.NumFields() != 2 ||
		!isSliceOf(removes.Type.Params.List[0].Type, "RestoreCopy") ||
		!isPointerTo(removes.Type.Params.List[1].Type, "restoreEffects") {
		t.Error("removeRestoreCopies's signature changed; want func removeRestoreCopies(shown []RestoreCopy, fx *restoreEffects) ([]RestoreCopy, error)")
	}

	// a production restoreEffects is built only in an exported entry point, never shared.
	for name, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Name.IsExported() || d.Body == nil {
					continue
				}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if cl, ok := n.(*ast.CompositeLit); ok && isType(cl.Type, "restoreEffects") {
						t.Errorf("%s constructs a production restoreEffects; only the exported entry points do", name)
					}
					return true
				})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if vs.Type != nil && (isType(vs.Type, "restoreEffects") || isPointerTo(vs.Type, "restoreEffects")) {
						t.Errorf("package-level variable %s is a restoreEffects; the effects value is per-run, built at the entry point", vs.Names[0].Name)
					}
					for _, v := range vs.Values {
						ast.Inspect(v, func(n ast.Node) bool {
							if cl, ok := n.(*ast.CompositeLit); ok && isType(cl.Type, "restoreEffects") {
								t.Errorf("a package-level variable is initialized with a restoreEffects literal; the production value is built at the entry point")
							}
							return true
						})
					}
				}
			}
		}
	}
}

// a zero value or a missing removal stub refuses at the data lock, removing nothing.
func TestRemoveRestoreCopiesFailsClosedOnUnsetEffects(t *testing.T) {
	pre, failed := restoreCopiesNode(t)
	shown, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}
	removed, err := removeRestoreCopies(shown, &restoreEffects{})
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "restoreAcquireDataLock") || len(removed) != 0 {
		t.Fatalf("removeRestoreCopies with every effect unset = %+v, %v; want the sentinel at the data lock", removed, err)
	}
	if !has(t, pre) || !has(t, failed) {
		t.Error("a copy was removed without the data lock being taken")
	}
	held := false
	removed, err = removeRestoreCopies(shown, &restoreEffects{
		restoreAcquireDataLock: func() (func(), error) { held = true; return func() { held = false }, nil },
	})
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "restoreRemoveAll") || len(removed) != 0 {
		t.Fatalf("removeRestoreCopies without a removal stub = %+v, %v; want the sentinel naming restoreRemoveAll", removed, err)
	}
	if held {
		t.Error("the data lock was not released")
	}
	if !has(t, pre) || !has(t, failed) {
		t.Error("a copy was removed without a removal effect to do it")
	}
}

// with lock and removal faked, each shown copy is removed under the lock and reported.
func TestRemoveRestoreCopiesRunsThroughTheEffectsValue(t *testing.T) {
	pre, failed := restoreCopiesNode(t)
	shown, err := RestoreCopies()
	if err != nil {
		t.Fatal(err)
	}
	held := false
	var removedPaths []string
	dataDir := version.EtcdDataDir
	fx := &restoreEffects{
		restoreCopies:          func() ([]RestoreCopy, error) { return restoreCopies(dataDir) },
		restoreAcquireDataLock: func() (func(), error) { held = true; return func() { held = false }, nil },
		restoreRemoveAll: func(p string) error {
			if !held {
				t.Errorf("%s was removed without holding the data lock", p)
			}
			removedPaths = append(removedPaths, p)
			return os.RemoveAll(p)
		},
	}
	removed, err := removeRestoreCopies(shown, fx)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != len(shown) {
		t.Errorf("removed = %+v, want each of the %d shown copies", removed, len(shown))
	}
	for _, p := range []string{pre, failed} {
		if !slices.Contains(removedPaths, p) || has(t, p) {
			t.Errorf("%s was not removed through the effects value", p)
		}
	}
}
