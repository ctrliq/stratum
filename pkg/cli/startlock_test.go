package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/uninstall"
	"github.com/ctrliq/stratum/internal/version"
)

func TestStartLockSurvivesUninstall(t *testing.T) {
	for _, target := range uninstall.Targets() {
		rel, err := filepath.Rel(target, startLockPath)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			t.Fatalf("start lock %s is inside uninstall target %s", startLockPath, target)
		}
	}
	runDir := filepath.Join(t.TempDir(), "stratum")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := runDir + "-start.lock"
	release, err := etcddata.AcquireStartLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.RemoveAll(runDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if release2, err := etcddata.AcquireStartLock(path); err == nil {
		release2()
		t.Fatal("runtime directory recreation bypassed the start lock")
	}
}

// TestTheStartLockIsTheOneTheProbesRead pins the agreement the lock's safety
// rests on: `stratum up` acquires startLockPath here (pkg/cli), while the
// uninstall, restore and upgrade probes open version.StartLockPath
// (internal/etcddata/lock.go). Each side is tested on its own; nothing else
// proves they are the same file, and a path that drifted apart would let
// uninstall, restore or an upgrade replace etcd data while a node start runs.
func TestTheStartLockIsTheOneTheProbesRead(t *testing.T) {
	if startLockPath != version.StartLockPath {
		t.Fatalf("the package default startLockPath = %q, want version.StartLockPath %q: the probes read the latter",
			startLockPath, version.StartLockPath)
	}

	path := filepath.Join(t.TempDir(), "start.lock")
	oldVersion, oldCLI := version.StartLockPath, startLockPath
	version.StartLockPath = path
	startLockPath = path
	t.Cleanup(func() { version.StartLockPath, startLockPath = oldVersion, oldCLI })

	release, err := etcddata.AcquireStartLock(startLockPath)
	if err != nil {
		t.Fatalf("AcquireStartLock: %v", err)
	}

	held, err := etcddata.StartLockHeld()
	if err != nil {
		t.Fatalf("StartLockHeld while the start lock is held: %v", err)
	}
	if !held {
		t.Error("StartLockHeld reports the node free while `stratum up` holds the lock: the probes read a different file")
	}
	// No supervisor runs in a unit test, which is exactly the window the
	// probe exists for: preparation is under way, nothing is visible yet.
	// The real lock probe is passed so the whole path — acquisition to
	// probe — agrees, not just the two functions in isolation.
	if err := etcddata.RefuseStartInProgress("test op", "replacing its data",
		func() bool { return false }, etcddata.StartLockHeld); err == nil {
		t.Error("RefuseStartInProgress allowed the operation while `stratum up` holds the lock")
	}

	release()

	held, err = etcddata.StartLockHeld()
	if err != nil {
		t.Fatalf("StartLockHeld after release: %v", err)
	}
	if held {
		t.Error("StartLockHeld still reports the node held after the start lock was released")
	}
	if err := etcddata.RefuseStartInProgress("test op", "replacing its data",
		func() bool { return false }, etcddata.StartLockHeld); err != nil {
		t.Errorf("RefuseStartInProgress refused a free node: %v", err)
	}
}

// TestRunUpTakesTheStartLockBeforeStaging: a `stratum up` that finds another
// start holding the node-start lock must refuse before it stages the payload,
// takes the etcd data lock or prepares the node, so the start already under way
// keeps the host to itself.
func TestRunUpTakesTheStartLockBeforeStaging(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}

	// Stub every step past staging so that a runUp which skipped the lock
	// still never touches the test machine.
	oldInstall, oldRunController, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRunController, oldPrepare
	})
	prepared := false
	installUnit = func([]string) error { return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		prepared = true
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}

	release, err := etcddata.AcquireStartLock(startLockPath)
	if err != nil {
		t.Fatalf("hold the start lock for the other start: %v", err)
	}
	defer release()

	err = runUp(context.Background(), runUpOptions{configPath: writeEmptyConfig(t, t.TempDir())})
	if err == nil || !strings.Contains(err.Error(), "already starting or running") {
		t.Fatalf("runUp while another start holds the lock = %v, want the concurrent-start refusal", err)
	}
	if prepared {
		t.Error("runUp prepared the node while another start held the start lock")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "start.lock" {
			t.Errorf("runUp created %s although another start held the start lock: the refusal must come before staging and the data lock", e.Name())
		}
	}
}

// TestRunUpPreparesUnderTheHeldDataLock: runUp hands preparation the release
// of the etcd data lock it holds, and the lock is held while preparation runs.
// Preparation saves node.yaml under that lock and refuses a nil release, so a
// runUp that passed nil would fail every controller start at its last step.
// node.yaml's directory and the control socket are redirected too: runUp reads
// the recorded state and probes for a running node before preparation, and the
// host's own record or node must not decide the test.
func TestRunUpPreparesUnderTheHeldDataLock(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&version.K8sConfDir, filepath.Join(tmp, "k8s")},
		{&version.ControlSocket, filepath.Join(tmp, "control.sock")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}

	oldInstall, oldRunController, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRunController, oldPrepare
	})
	prepared := false
	installUnit = func([]string) error { return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
	prepareControllerFn = func(_ context.Context, _ *cluster.ClusterConfig, _ string, _ *k8sutil.MaintenanceLease, releaseDataLock func()) (*cluster.NodeState, error) {
		prepared = true
		if releaseDataLock == nil {
			t.Error("runUp prepared the node without the release of the etcd data lock it holds")
		}
		if release, err := etcddata.Lock(version.EtcdDataDir); err == nil {
			release()
			t.Error("the etcd data lock was free while the node was being prepared")
		}
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}

	configPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runUp(context.Background(), runUpOptions{configPath: configPath}); err != nil {
		t.Fatalf("runUp = %v", err)
	}
	if !prepared {
		t.Fatal("runUp never reached preparation")
	}
}

// TestRunUpRechecksThePreflightUnderTheStartLock pins the part of runUp's
// start-lock wiring a unit test cannot drive: the read-only preflight runs
// again once the lock is held, so a node that came up between the first
// preflight and the lock is still refused. A source pin because
// preflightNoRunningNode probes the host's sockets and has no seam that could
// pass the first call and fail the second.
func TestRunUpRechecksThePreflightUnderTheStartLock(t *testing.T) {
	src, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(src, []byte("func runUp("))
	if start < 0 {
		t.Fatal("up.go no longer declares runUp — re-express this pin")
	}
	body := src[start:]
	if end := bytes.Index(body[1:], []byte("\nfunc ")); end >= 0 {
		body = body[:end+1]
	}
	if !bytes.Contains(body, []byte("etcddata.AcquireStartLockChecked(startLockPath, preflightNoRunningNode)")) {
		t.Fatal("runUp does not take the start lock with etcddata.AcquireStartLockChecked(startLockPath, preflightNoRunningNode): " +
			"the preflight is not re-run under the lock, so a node that came up after the first check is started over")
	}
}
