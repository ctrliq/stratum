package etcdsnap

import (
	"io"

	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

func TestRestoreMissingRecoveryEffectsStopsNoPod(t *testing.T) {
	for _, tc := range []struct {
		field string
		unset func(*restoreEffects)
	}{
		{"restoreStart", func(fx *restoreEffects) { fx.restoreStart = nil }},
		{"restoreWaitActive", func(fx *restoreEffects) { fx.restoreWaitActive = nil }},
		{"restoreWaitHealthz", func(fx *restoreEffects) { fx.restoreWaitHealthz = nil }},
		{"restoreMoveDir", func(fx *restoreEffects) { fx.restoreMoveDir = nil }},
		{"restoreRun", func(fx *restoreEffects) { fx.restoreRun = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			fx, n := newFakeEffects(t)
			tc.unset(fx)
			err := runFakeRestore(t, fx, true)
			if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("restore = %v; want missing %s", err, tc.field)
			}
			if slices.ContainsFunc(n.events, func(e string) bool { return strings.HasPrefix(e, "stop:") }) || n.called("run") {
				t.Errorf("missing recovery effect stopped the node: %v", n.events)
			}
			if !n.active["etcd"] || !n.active["kube-apiserver"] || !dataDirHolds(t, fx.etcdDataDir, "ORIGINAL") || has(t, preRestore(n)) {
				t.Errorf("missing recovery effect changed the original node: active=%v events=%v", n.active, n.events)
			}
		})
	}
}

func TestSeedHealthGatePreservesUnsetBaseKubectl(t *testing.T) {
	t.Parallel()
	fx := &restoreEffects{kubectlCallTimeout: time.Second}
	fx.seedKubectl = fx.localKubectl
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("missing baseKubectl panicked instead of returning the sentinel: %v", p)
		}
	}()
	err := fx.waitSeedHealthz(20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "baseKubectl") {
		t.Fatalf("health gate error = %v; want the missing baseKubectl effect", err)
	}
}

func TestWorkloadRestartsPreserveUnsetBaseKubectl(t *testing.T) {
	t.Parallel()
	fx := &restoreEffects{kubectlCallTimeout: time.Second}
	fx.seedKubectl = fx.localKubectl
	results := fx.restartClusterWorkloads()
	if len(results) == 0 {
		t.Fatal("workload restart returned no results")
	}
	for _, result := range results {
		if !errors.Is(result.err, errUnsetEffect) || !strings.Contains(result.err.Error(), "baseKubectl") {
			t.Errorf("%s: error = %v; want missing baseKubectl", result.name, result.err)
		}
	}
}

func TestRestoreRunsKeepTheirDataAndRollbackSeparate(t *testing.T) {
	first := newFakeNode(t)
	second := newFakeNode(t)
	second.fail("run", 0)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := first.restoreErr(); err != nil {
			t.Errorf("first restore: %v", err)
		}
	})
	wg.Go(func() {
		if err := second.restoreErr(); err == nil || !strings.Contains(err.Error(), "rolled back") {
			t.Errorf("second restore: %v; want rollback", err)
		}
	})
	wg.Wait()
	dataDir := func(n *fakeNode) string {
		t.Helper()
		if n.cmd == nil {
			t.Fatal("restore did not run etcdutl")
		}
		for i, arg := range n.cmd.Args {
			if arg == "--data-dir" {
				return n.cmd.Args[i+1]
			}
		}
		t.Fatal("restore command has no data dir")
		return ""
	}
	firstDir, secondDir := dataDir(first), dataDir(second)
	if firstDir == secondDir || first.cmd.Path == second.cmd.Path {
		t.Fatalf("restore runs share paths: data dirs %q, %q; executables %q, %q", firstDir, secondDir, first.cmd.Path, second.cmd.Path)
	}
	if !has(t, filepath.Join(firstDir, "member", "RESTORED")) || !has(t, firstDir+".pre-restore-"+testStamp) {
		t.Error("first restore lost its restored data or original copy")
	}
	if !has(t, filepath.Join(secondDir, "member", "ORIGINAL")) || !has(t, secondDir+".failed-restore-"+testStamp) {
		t.Error("second rollback lost its original data or failed copy")
	}
	copy, err := StatRestoreCopy(RestoreCopy{Path: firstDir + ".pre-restore-" + testStamp, Kind: "pre-restore"})
	if err != nil {
		t.Fatal(err)
	}
	first.fx.restoreRemoveAll = os.RemoveAll
	removed, err := removeRestoreCopies([]RestoreCopy{copy}, first.fx)
	if err != nil || len(removed) != 1 || has(t, copy.Path) {
		t.Fatalf("prune first run's copy = %v, %v; want just its shown copy removed", removed, err)
	}
	if !has(t, secondDir+".failed-restore-"+testStamp) {
		t.Error("pruning the first run removed the second run's failed copy")
	}
}

func TestRestoreMissingNodeLockEffectDoesNotCreateLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "node.lock")
	swap(t, &version.NodeUpgradeLockPath, lockPath)
	err := runRestore(io.Discard, RestoreOptions{Snapshot: "snap.db", PeerURL: testPeer, InitialCluster: testLedger}, &restoreEffects{})
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "restoreAcquireNodeLock") {
		t.Errorf("restore error = %v; want the missing node lock effect", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unset effects touched the node lock: %v", err)
	}
}

func TestSeedHealthGatePreservesUnsetEffect(t *testing.T) {
	t.Parallel()
	fx := &restoreEffects{kubectlCallTimeout: time.Second}
	err := fx.waitSeedHealthz(20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "seedKubectl") {
		t.Fatalf("health gate error = %v; want the missing seedKubectl effect", err)
	}
}

func TestWatcherRestartsPreserveUnsetComposedEffects(t *testing.T) {
	for _, field := range []string{"compsStatus", "compsRestart"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			fx := &restoreEffects{
				compsStatus: func() (map[string]supervisor.ProcStatus, error) {
					return map[string]supervisor.ProcStatus{"kubelet": {}, "operator": {}}, nil
				},
				compsRestart:           func(string) error { return nil },
				restoreStaticPodExists: func(string) bool { return false },
				restoreStaticPod:       func(string) error { return nil },
				restoreWorkloads:       func() []workloadResult { return nil },
			}
			fx.restoreKubelet = func() (bool, error) { return restartKubelet(fx) }
			fx.restoreOperator = func() (bool, error) { return restartOperator(fx) }
			if field == "compsStatus" {
				fx.compsStatus = nil
			} else {
				fx.compsRestart = nil
			}
			err := fx.restartWatchers(io.Discard)
			if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), field) {
				t.Fatalf("watcher restart error = %v; want the missing %s effect", err, field)
			}
			if !errors.Is(err, ErrWatchersNotRestarted) {
				t.Fatalf("watcher restart error = %v; want ErrWatchersNotRestarted", err)
			}
		})
	}
}
