package uninstall

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// noKubelet makes a run's kubelet process absent: it re-stubs the run's
// hostProcessRunning probe, keeping every other answer the value already gives.
func noKubelet(fx *hostEffects) {
	running := fx.hostProcessRunning
	fx.hostProcessRunning = func(name string) (bool, error) {
		if name == "kubelet" {
			return false, nil
		}
		return running(name)
	}
}

// unitWanted is what the stubbed supervisor status reports as wanted, beside
// unitActive for running.
var unitWanted = func(string) bool { return true }

// healRuns counts the legacy-config heal's calls through its seam. stubTeardown
// resets it and swaps the seam with a counted no-op, so every Run test keeps
// the heal off the host's /etc/containerd/config.toml and can assert how often
// it ran; swap restores the previous fake, so the test that pins the heal's
// place in the teardown order layers its recorder on top and keeps counting.
var healRuns int

// swap sets *p to v for the rest of the test.
//
// Tests that call this run sequentially, and are the package's stated
// exception to t.Parallel: swap writes a package-level variable, which two
// parallel tests would race over. The per-run values carry the probes, the
// discovery state and the filesystem layout; what remains shared is only
// what two Runs cannot race over in production — the wait durations, the
// tool PATH, the version path redirects — and the counted healRuns.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// stubTeardown replaces the teardown steps for a test, recording
// each into events when it is non-nil. The fakes are fields on the run's
// fail-closed effects value: a step the test needs that stubTeardown does not
// set stays unset and stops the run on the unset-effect sentinel instead of
// reaching the host.
func stubTeardown(t *testing.T, fx *hostEffects, events *[]string) {
	t.Helper()
	// The legacy-config heal is a seam, so no test here reaches the host's
	// /etc/containerd/config.toml, whatever the host keeps there. The fake
	// counts rather than records into events: every runUninstall(io.Discard, true, fx) test reaches the
	// heal, and the full-order pins
	// (TestRunStopsContainersBeforeHostProcesses,
	// TestRunDisablesUnitBeforeTeardownAndStopsItOnlyAfter) name exactly the
	// events their teardowns produce. The tests that assert the heal re-swap
	// the seam; TestRunHealsTheHostConfigThroughTheSeamExactlyOnce pins its
	// place in the order.
	healRuns = 0
	fx.removeLegacyHostConfig = func(io.Writer) { healRuns++ }
	// A stubbed node has no static-pod manifests: the read probe stays unset,
	// so a test that wants a control plane to tear down sets both through
	// stubControlPlane and anything else stops on the unset-effect sentinel.
	fx.staticPodExists = func(string) bool { return false }
	record := func(e string) {
		if events != nil {
			*events = append(*events, e)
		}
	}
	fx.stopKubeletFn = func() error { record("stop kubelet"); return nil }
	fx.stopContainersFn = func() error { record("stop containers"); return nil }
	fx.startContainerdFn = func() error { record("start containerd"); return nil }
	fx.stopContainerdFn = func() error { record("stop containerd"); return nil }
	fx.startUnitFn = func(name string) error { record("start " + name); return nil }
	fx.waitHostExitFn = func() { record("wait exit") }
	fx.nodeMarkerFn = func() (func(string, time.Time) error, error) {
		return func(host string, _ time.Time) error { record("mark " + host); return nil }, nil
	}
	swap(t, &unitWanted, func(string) bool { return true }) // test-local probe stand-in
	fx.supervisorStatus = func() (map[string]supervisor.ProcStatus, error) {
		st := map[string]supervisor.ProcStatus{}
		for _, name := range comps.Units {
			st[name] = supervisor.ProcStatus{Name: name, Running: fx.unitActive(name), Wanted: unitWanted(name)}
		}
		return st, nil
	}
	fx.stopIdleShimsFn = func() { record("stop idle shims") }
	fx.loadNodeState = func() (*nodestate.NodeState, error) {
		return &nodestate.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}
	// Real no-ops on a non-systemd or non-root test host, but not on every
	// host: a CI runner with a real systemd PID 1 has SystemdAvailable()
	// return true, and a test process without permission to write under
	// /run/systemd/system then gets a REAL error from these instead of the
	// harmless no-op a sandboxed dev machine sees — the exact gap that broke
	// every test in this package on GitHub's ubuntu-latest runners (a CI failure, not a
	// flake). Stubbed here so this package's tests
	// never depend on the ambient host's init system or privileges; tests
	// that care about these calls specifically re-swap them afterward.
	fx.disableSystemdUnit = func() error { return nil }
	fx.stopSystemdUnit = func() error { return nil }
	fx.stopTopLevelFn = func() error { return nil }
	fx.teardownDatapathFn = func() error { record("teardown datapath"); return nil }
	fx.startInProgress = func() (bool, error) { return false, nil }
	fx.unitEnabledFn = func() (bool, error) { return false, nil }
	fx.enableSystemdUnit = func() error { return nil }
	fx.suppressRestartFn = func() error { return nil }
	fx.allowRestartFn = func() error { return nil }
	fx.reloadSystemd = func() error { return nil }
	// Recorded rather than silent: it must run before the delete loop, which
	// removes the record it reads. Stubbed so no test here reaches
	// a real firewalld on whatever host runs it.
	fx.untrustPodCIDRs = func() error { record("untrust pod cidrs"); return nil }
	fx.removeSELinuxContexts = func() error { return nil }
	fx.pidNamespaceFn = func() (string, error) { return initPIDNamespace, nil }
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return nil, nil, nil }
}

// runNode stubs a node where the supervisor, containerd and kubelet are up and
// every process is gone once asked to stop, and records the teardown order. It
// returns the run's effects value with it: the caller drives runUninstall with
// that value, so an effect no helper set fails closed instead of reaching the
// host. Sequential by swap's stated exception: it redirects the shared
// version.K8sConfDir and the unitWanted probe stand-in.
func runNode(t *testing.T, role string) (*[]string, *hostEffects) {
	t.Helper()
	events := &[]string{}
	fx := &hostEffects{}
	stubTeardown(t, fx, events)
	fx.loadNodeState = func() (*nodestate.NodeState, error) {
		return &nodestate.NodeState{Role: role, Hostname: "node-a"}, nil
	}
	info := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(info, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := false
	fx.mountInfoPath = info
	swap(t, &version.K8sConfDir, t.TempDir())
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) { return func() {}, nil }
	fx.supervisorRunning = func() bool { return true }
	fx.unitActive = func(string) bool { return true }
	fx.hostProcessRunning = func(string) (bool, error) { return !stopped, nil }
	fx.staticPodActive = func(string) bool { return false }
	fx.staticPodRunning = func(string) (bool, error) { return false, nil }
	fx.stopHostProcesses = func() error { *events = append(*events, "stop host processes"); stopped = true; return nil }
	fx.unmountStateFn = func() error { return nil }
	fx.removeAll = func(p string) error {
		if len(*events) == 0 || (*events)[len(*events)-1] != "remove state" {
			*events = append(*events, "remove state")
		}
		return nil
	}
	return events, fx
}

// TestRunStopsContainersBeforeHostProcesses: with pods
// running, kubelet stops first so nothing recreates a container, then every
// container is stopped while containerd is still up, then the host processes,
// and only once the shims have gone is any state removed.
func TestRunStopsContainersBeforeHostProcesses(t *testing.T) {
	for _, tc := range []struct {
		role string
		want []string
	}{
		{"worker", []string{"stop kubelet", "stop containers", "stop host processes", "wait exit", "stop idle shims", "teardown datapath", "untrust pod cidrs", "remove state", "mark node-a"}},
		{"controller", []string{"stop kubelet", "stop containers", "stop host processes", "wait exit", "stop idle shims", "teardown datapath", "untrust pod cidrs", "remove state"}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			events, fx := runNode(t, tc.role)
			if err := runUninstall(io.Discard, true, fx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(*events, tc.want) {
				t.Errorf("teardown = %q, want %q", *events, tc.want)
			}
		})
	}
}

// TestRunDeletesNothingWhenContainersCannotBeStopped: a container that cannot
// be stopped keeps its shim, so the node's state stays where it is.
func TestRunDeletesNothingWhenContainersCannotBeStopped(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.stopContainersFn = func() error { return errors.New("containerd refused") }
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "containerd refused") || !strings.Contains(err.Error(), "Nothing was deleted") {
		t.Fatalf("Run = %v", err)
	}
	for _, e := range *events {
		if e == "remove state" || e == "stop host processes" {
			t.Errorf("events = %q; the node was torn down after its containers could not be stopped", *events)
		}
	}
}

// TestNodeNoticeSaysWhatIsLeftInTheCluster: a worker cannot delete
// its own Node (its node credential is not allowed to), and a control plane
// leaves its Node and etcd member, so uninstall says exactly what to run from a
// control plane.
func TestNodeNoticeSaysWhatIsLeftInTheCluster(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{}
	for _, tc := range []struct {
		role   string
		marked bool
		want   []string
	}{
		{"worker", true, []string{"removed by the control plane", "kubectl delete node node-a"}},
		{"worker", false, []string{"Node object stays", "kubectl delete node node-a"}},
		{"controller", false, []string{"etcdctl member remove", "kubectl delete node node-a"}},
	} {
		fx.loadNodeState = func() (*nodestate.NodeState, error) {
			return &nodestate.NodeState{Role: tc.role, Hostname: "node-a"}, nil
		}
		got := nodeNotice(fx, tc.marked)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s (marked=%v) notice = %q, want it to contain %q", tc.role, tc.marked, got, w)
			}
		}
	}
	fx.loadNodeState = func() (*nodestate.NodeState, error) { return nil, errors.New("no state") }
	if got := nodeNotice(fx, false); got != "" {
		t.Errorf("notice with no node state = %q, want none", got)
	}
}

// TestRunStartsContainerdForContainersAnEarlierUninstallLeft: a node can have
// been through an uninstall that stopped containerd and kubelet and then
// refused over the shims, which still held their containers. Re-running must
// stop those containers, so containerd is started again for it; a kubelet
// stopped on purpose is left stopped.
func TestRunStartsContainerdForContainersAnEarlierUninstallLeft(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(string) bool { return false }
	noKubelet(fx)
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	want := []string{"start containerd", "stop containers", "stop host processes", "wait exit", "stop idle shims", "teardown datapath", "untrust pod cidrs", "remove state", "mark node-a"}
	if !slices.Equal(*events, want) {
		t.Errorf("teardown = %q, want %q", *events, want)
	}
}

// TestRunLeavesContainerdStoppedWithNoShims: with containerd stopped and no
// shim left there is nothing to stop, so containerd is not started.
func TestRunLeavesContainerdStoppedWithNoShims(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(string) bool { return false }
	fx.hostProcessRunning = func(string) (bool, error) { return false, nil }
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*events, "start containerd") || slices.Contains(*events, "stop containers") {
		t.Errorf("events = %q; started containerd with nothing to stop", *events)
	}
}

// TestRunStopsAKubeletInRestartBackoff: a kubelet waiting out
// its restart backoff is not running, but the supervisor would bring it back
// mid-teardown to recreate the pods being stopped, so it is stopped too.
func TestRunStopsAKubeletInRestartBackoff(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(name string) bool { return name != "kubelet" }
	noKubelet(fx)
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*events, "stop kubelet") {
		t.Errorf("events = %q; a kubelet in backoff was not stopped", *events)
	}
}

// fakeCtr answers ctr calls for a node with two k8s.io tasks: "fast" exits on
// SIGTERM, "slow" only on SIGKILL.
type fakeCtr struct {
	noTasks   bool
	calls     []string
	running   map[string]bool
	failLs    bool
	failDel   map[string]error
	sandboxes []string
	failSbRm  map[string]error
	failKill  map[string]error
	status    map[string]string
	lsCalls   int
	stickySb  map[string]bool
}

func (f *fakeCtr) run(args ...string) (string, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "namespaces ls -q":
		return "k8s.io\n", nil
	case call == "-n k8s.io tasks ls":
		f.lsCalls++
		if f.failLs && f.lsCalls > 1 {
			return "", errors.New("containerd busy")
		}
		out := "TASK    PID    STATUS\n"
		if f.noTasks {
			return out, nil
		}
		for _, id := range []string{"fast", "slow"} {
			st := "STOPPED"
			if f.running[id] {
				st = "RUNNING"
			}
			if f.status[id] != "" {
				st = f.status[id]
			}
			out += fmt.Sprintf("%s    1    %s\n", id, st)
		}
		return out, nil
	case strings.HasPrefix(call, "-n k8s.io tasks kill --all --signal SIGTERM "):
		id := strings.TrimPrefix(call, "-n k8s.io tasks kill --all --signal SIGTERM ")
		if err := f.failKill[id]; err != nil {
			return "", err
		}
		if id == "fast" {
			f.running[id] = false
		}
		return "", nil
	case strings.HasPrefix(call, "-n k8s.io tasks kill --all --signal SIGKILL "):
		f.running[strings.TrimPrefix(call, "-n k8s.io tasks kill --all --signal SIGKILL ")] = false
		return "", nil
	case strings.HasPrefix(call, "-n k8s.io tasks delete --force "):
		return "", f.failDel[strings.TrimPrefix(call, "-n k8s.io tasks delete --force ")]
	case call == "-n k8s.io sandboxes ls":
		out := "ID  CREATED  RUNTIME\n"
		for _, id := range f.sandboxes {
			out += id + "  2026-09-26  io.containerd.runc.v2\n"
		}
		return out, nil
	case strings.HasPrefix(call, "-n k8s.io sandboxes rm --force "):
		id := strings.TrimPrefix(call, "-n k8s.io sandboxes rm --force ")
		err := f.failSbRm[id]
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return "", err
		}
		if !f.stickySb[id] {
			f.sandboxes = slices.DeleteFunc(f.sandboxes, func(s string) bool { return s == id })
		}
		return "", err
	}
	return "", fmt.Errorf("unexpected ctr %s", call)
}

// withFakeCtr hands back a run whose ctr calls the fake answers, with the
// container grace period and poll interval shortened. Sequential by swap's
// stated exception: those two are shared durations.
func withFakeCtr(t *testing.T, f *fakeCtr) *hostEffects {
	t.Helper()
	fx := &hostEffects{}
	fx.ctrRun = f.run
	// stopContainers saves the node's netns record through the value it is
	// given, so the fake's record path goes to a temp file, never the host's
	// /run.
	fx.netnsRecordPath = filepath.Join(t.TempDir(), "netns-record.json")
	swap(t, &containerGrace, 50*time.Millisecond)
	swap(t, &pollInterval, time.Millisecond)
	return fx
}

// TestStopContainersTermsThenKillsThenDeletes: every task gets SIGTERM, only
// the one still running after the grace period gets SIGKILL, and every task is
// deleted before the sandboxes are removed.
func TestStopContainersTermsThenKillsThenDeletes(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{"fast": true, "slow": true}}
	fx := withFakeCtr(t, f)
	if err := stopContainers(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	var signals, deletes []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, "tasks kill"):
			signals = append(signals, strings.TrimPrefix(c, "-n k8s.io tasks kill --all --signal "))
		case strings.Contains(c, "tasks delete"):
			deletes = append(deletes, strings.TrimPrefix(c, "-n k8s.io tasks delete --force "))
		}
	}
	if want := []string{"SIGTERM fast", "SIGTERM slow", "SIGKILL slow"}; !slices.Equal(signals, want) {
		t.Errorf("signals = %q, want %q", signals, want)
	}
	if want := []string{"fast", "slow"}; !slices.Equal(deletes, want) {
		t.Errorf("deleted = %q, want %q", deletes, want)
	}
}

// TestStopContainersEscalatesWhenItCannotSeeTheTasks: a status listing that
// fails is not proof the containers stopped, so they get SIGKILL.
func TestStopContainersEscalatesWhenItCannotSeeTheTasks(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{"fast": true, "slow": true}, failLs: true}
	fx := withFakeCtr(t, f)
	if err := stopContainers(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "-n k8s.io tasks kill --all --signal SIGKILL fast") {
		t.Errorf("calls = %q; no SIGKILL after the task listing failed", f.calls)
	}
}

// TestStopContainersReportsATaskItCannotDelete: a task that cannot be deleted
// leaves a container behind, so it is an error; one already gone is not.
func TestStopContainersReportsATaskItCannotDelete(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{}, failDel: map[string]error{
		"fast": errors.New("ctr: task fast: not found"),
		"slow": errors.New("ctr: permission denied"),
	}}
	fx := withFakeCtr(t, f)
	err := stopContainers(io.Discard, fx)
	if err == nil || !strings.Contains(err.Error(), "k8s.io/slow") || strings.Contains(err.Error(), "k8s.io/fast") {
		t.Fatalf("stopContainers = %v; want only the undeletable task reported", err)
	}
}

// TestStopContainersRemovesEverySandbox: in containerd 2 a pod's shim
// exits when its sandbox is shut down, not when its tasks are deleted, so
// every sandbox is removed after the tasks, and the listing's header is not
// taken for one.
func TestStopContainersRemovesEverySandbox(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{}, sandboxes: []string{"sb1", "sb2"}}
	fx := withFakeCtr(t, f)
	if err := stopContainers(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	var removed []string
	lastTask, firstSandbox := -1, -1
	for i, c := range f.calls {
		if strings.Contains(c, "tasks delete") {
			lastTask = i
		}
		if strings.HasPrefix(c, "-n k8s.io sandboxes rm --force ") {
			removed = append(removed, strings.TrimPrefix(c, "-n k8s.io sandboxes rm --force "))
			if firstSandbox < 0 {
				firstSandbox = i
			}
		}
	}
	if want := []string{"sb1", "sb2"}; !slices.Equal(removed, want) {
		t.Errorf("sandboxes removed = %q, want %q", removed, want)
	}
	if firstSandbox < lastTask {
		t.Errorf("calls = %q; a sandbox was removed before its tasks were deleted", f.calls)
	}
}

// TestStopContainersReportsASandboxItCannotRemove: a sandbox left in place keeps
// its shim, so it is an error; one already gone is not.
func TestStopContainersReportsASandboxItCannotRemove(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{}, sandboxes: []string{"gone", "stuck"}, failSbRm: map[string]error{
		"gone":  errors.New("sandbox gone: not found"),
		"stuck": errors.New("shutdown: deadline exceeded"),
	}}
	fx := withFakeCtr(t, f)
	err := stopContainers(io.Discard, fx)
	if err == nil || !strings.Contains(err.Error(), "k8s.io/stuck") || strings.Contains(err.Error(), "k8s.io/gone") {
		t.Fatalf("stopContainers = %v; want only the stuck sandbox reported", err)
	}
}

// TestStateMountsFindsKubeletVolumesUnderTheStateDir: the
// pod volume mounts that made RemoveAll fail with EBUSY live under Stratum's
// state dir, not /var/lib/kubelet. Every mount strictly below a removed
// directory is returned deepest first; the directories themselves, and mounts
// elsewhere (including a host kubelet's), are not.
func TestStateMountsFindsKubeletVolumesUnderTheStateDir(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{}
	info := strings.Join([]string{
		"36 1 0:30 / / rw - ext4 /dev/sda1 rw",
		"40 36 0:31 / " + version.DataDir + " rw - ext4 /dev/sdb1 rw",
		"41 40 0:32 / " + version.DataDir + "/kubelet/pods/p1/volumes/kubernetes.io~projected/kube-api-access-x rw - tmpfs tmpfs rw",
		"42 40 0:33 / " + version.DataDir + "/kubelet/pods/p1/volumes/kubernetes.io~empty-dir/with\\040space rw - tmpfs tmpfs rw",
		"43 40 0:34 / " + version.DataDir + "/containerd/io.containerd.runtime.v2.task/k8s.io/abc/rootfs rw - overlay overlay rw",
		"44 36 0:35 / /var/lib/kubelet/pods/other/volumes/x rw - tmpfs tmpfs rw",
		"45 36 0:36 / " + version.RunDir + "/containerd/io.containerd.grpc.v1.cri/sandboxes/s1/shm rw - tmpfs shm rw",
		"46 41 0:37 / " + version.DataDir + "/kubelet/pods/p1/volumes/kubernetes.io~projected/kube-api-access-x rw - tmpfs tmpfs rw",
	}, "\n")
	f := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(f, []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.mountInfoPath = f

	got, err := stateMounts(fx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		version.DataDir + "/kubelet/pods/p1/volumes/kubernetes.io~projected/kube-api-access-x",
		version.DataDir + "/kubelet/pods/p1/volumes/kubernetes.io~empty-dir/with space",
		version.DataDir + "/containerd/io.containerd.runtime.v2.task/k8s.io/abc/rootfs",
		version.RunDir + "/containerd/io.containerd.grpc.v1.cri/sandboxes/s1/shm",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("stateMounts() = %q, missing %q", got, want)
		}
	}
	for _, never := range []string{version.DataDir, "/", "/var/lib/kubelet/pods/other/volumes/x"} {
		if slices.Contains(got, never) {
			t.Errorf("stateMounts() = %q, must not unmount %q", got, never)
		}
	}
	for i := 1; i < len(got); i++ {
		if strings.Count(got[i], "/") > strings.Count(got[i-1], "/") {
			t.Errorf("stateMounts() = %q is not deepest first", got)
		}
	}
	if len(got) != len(slices.Compact(slices.Sorted(slices.Values(got)))) {
		t.Errorf("stateMounts() = %q lists a stacked mount point twice", got)
	}
}

// TestRemoveTargetEmptiesAMountPoint: a removed directory that is itself a
// mount point is emptied, and the directory (and so the mount) is left; one
// that is not is removed outright.
func TestRemoveTargetEmptiesAMountPoint(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	root := t.TempDir()
	mounted, plain := filepath.Join(root, "mounted"), filepath.Join(root, "plain")
	for _, d := range []string{mounted, plain} {
		if err := os.MkdirAll(filepath.Join(d, "kubelet", "pods"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "state"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info := filepath.Join(root, "mountinfo")
	if err := os.WriteFile(info, []byte("40 36 0:31 / "+mounted+" rw - ext4 /dev/sdb1 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The real os.RemoveAll does the removing, so the run's effects value
	// carries it — the same value Run builds for the delete loop — and the
	// mount table it reads is the value's own.
	fx := &hostEffects{removeAll: os.RemoveAll, mountInfoPath: info}
	for _, d := range []string{mounted, plain} {
		if err := removeTarget(io.Discard, fx, d); err != nil {
			t.Fatalf("removeTarget(%s) = %v", d, err)
		}
	}
	if entries, err := os.ReadDir(mounted); err != nil || len(entries) != 0 {
		t.Errorf("mount point after removeTarget: %v entries, err %v; want it present and empty", len(entries), err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Errorf("a directory that is not a mount point was left: %v", err)
	}
}

// TestRunPrintsTheNodeNoticeFromStateItThenRemoves: the notice comes from the
// node state under the state dir, which Run removes; it must be read first
// (measured: reading it afterwards printed nothing on a real worker).
func TestRunPrintsTheNodeNoticeFromStateItThenRemoves(t *testing.T) {
	_, fx := runNode(t, "worker")
	removed := false
	fx.removeAll = func(string) error { removed = true; return nil }
	fx.loadNodeState = func() (*nodestate.NodeState, error) {
		if removed {
			return nil, errors.New("state dir removed")
		}
		return &nodestate.NodeState{Role: "worker", Hostname: "node-a"}, nil
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kubectl delete node node-a") {
		t.Errorf("uninstall output does not say how to remove the Node:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	// Sequential by the same exception as swap: it redirects os.Stdout, which
	// two parallel tests would race over.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		old := os.Stdout
		os.Stdout = w
		defer func() { os.Stdout = old; _ = w.Close() }()
		fn()
	}()
	return <-done
}

// captureWriter runs fn, handing it a fresh buffer as the run's writer, and
// returns what the run wrote to it: the writer seam's replacement for
// capturing os.Stdout around a run, whose prints go to the writer now. Tests
// of helpers that still print to the process's stdout directly (the
// legacy-config heal's own seam in internal/runtime) keep captureStdout.
func captureWriter(fn func(w io.Writer)) string {
	var buf bytes.Buffer
	fn(&buf)
	return buf.String()
}

// TestStopContainersRemovesSandboxesWithNoTasks: an idle pod
// sandbox has no task, and removing it is what ends its shim, so it is removed
// even when there is no task to stop.
func TestStopContainersRemovesSandboxesWithNoTasks(t *testing.T) {
	f := &fakeCtr{noTasks: true, running: map[string]bool{}, sandboxes: []string{"idle"}}
	fx := withFakeCtr(t, f)
	if err := stopContainers(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "-n k8s.io sandboxes rm --force idle") {
		t.Errorf("calls = %q; an idle sandbox with no tasks was not removed", f.calls)
	}
}

// stubControlPlane gives runNode a control plane: etcd and kube-apiserver
// manifests that Run reads and stops, and records what an abort puts back.
func stubControlPlane(t *testing.T, fx *hostEffects) (puts *[]string, kubeletStarts *int) {
	t.Helper()
	puts, kubeletStarts = &[]string{}, new(int)
	fx.staticPodExists = func(name string) bool { return name == "etcd" || name == "kube-apiserver" }
	fx.readManifestFn = func(name string) ([]byte, error) { return []byte(name + " manifest"), nil }
	fx.stopStaticPodFn = func(string) error { return nil }
	fx.putManifestFn = func(name string, b []byte) error {
		*puts = append(*puts, name+"="+string(b))
		return nil
	}
	fx.startUnitFn = func(name string) error {
		if name == "kubelet" {
			*kubeletStarts++
		}
		return nil
	}
	return puts, kubeletStarts
}

// TestAbortPutsTheControlPlaneBack: the static pods are stopped
// (their manifests removed) and kubelet is stopped before containers are, so a
// refusal there must not leave the control plane down behind "Nothing was
// deleted": the abort writes back the exact manifests and restarts kubelet.
func TestAbortPutsTheControlPlaneBack(t *testing.T) {
	events, fx := runNode(t, "controller")
	puts, starts := stubControlPlane(t, fx)
	fx.stopContainersFn = func() error { return errors.New("containerd refused") }
	err := runUninstall(io.Discard, true, fx)
	for _, want := range []string{"Nothing was deleted", "containers may have been stopped, and kubelet recreates its pods",
		"the control-plane manifests were put back", "kubelet was started again"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Run = %v, want it to say %q", err, want)
		}
	}
	slices.Sort(*puts)
	if want := []string{"etcd=etcd manifest", "kube-apiserver=kube-apiserver manifest"}; !slices.Equal(*puts, want) {
		t.Errorf("manifests put back = %q, want %q", *puts, want)
	}
	if *starts != 1 {
		t.Errorf("kubelet restarted %d times, want 1", *starts)
	}
	if slices.Contains(*events, "remove state") {
		t.Errorf("events = %q; state was removed after an abort", *events)
	}
}

// TestAbortSaysWhenItCannotPutTheControlPlaneBack: a restore that fails is
// reported as such, with the way back.
func TestAbortSaysWhenItCannotPutTheControlPlaneBack(t *testing.T) {
	_, fx := runNode(t, "controller")
	stubControlPlane(t, fx)
	fx.putManifestFn = func(string, []byte) error { return errors.New("read-only filesystem") }
	fx.stopKubeletFn = func() error { return errors.New("kubelet will not stop") }
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "could not be put back") || !strings.Contains(err.Error(), "restart `stratum up`") ||
		strings.Contains(err.Error(), "manifests were put back") {
		t.Fatalf("Run = %v", err)
	}
}

// TestAbortSaysOnlyWhatItDid: the account of the node comes
// from what the abort actually did. A worker has no control plane to put back;
// a failed etcd stop never touched kubelet; and a failed container stop on a
// node whose kubelet was not running changed something, so never "Nothing was
// changed".
func TestAbortSaysOnlyWhatItDid(t *testing.T) {
	t.Run("worker", func(t *testing.T) {
		_, fx := runNode(t, "worker")
		fx.stopContainersFn = func() error { return errors.New("containerd refused") }
		err := runUninstall(io.Discard, true, fx)
		if err == nil || strings.Contains(err.Error(), "control-plane") || !strings.Contains(err.Error(), "kubelet was started again") {
			t.Fatalf("Run = %v", err)
		}
	})
	t.Run("etcd will not stop", func(t *testing.T) {
		events, fx := runNode(t, "controller")
		puts, starts := stubControlPlane(t, fx)
		fx.stopStaticPodFn = func(name string) error {
			if name == "etcd" {
				return errors.New("etcd wedged")
			}
			return nil
		}
		err := runUninstall(io.Discard, true, fx)
		if err == nil || !strings.Contains(err.Error(), "etcd wedged") || strings.Contains(err.Error(), "kubelet") {
			t.Fatalf("Run = %v", err)
		}
		if !slices.Contains(*puts, "etcd=etcd manifest") || *starts != 0 || len(*events) != 0 {
			t.Errorf("puts = %q, kubelet starts = %d, events = %q", *puts, *starts, *events)
		}
	})
	t.Run("kubelet not running", func(t *testing.T) {
		_, fx := runNode(t, "worker")
		fx.unitActive = func(name string) bool { return name != "kubelet" }
		unitWanted = func(name string) bool { return name != "kubelet" }
		noKubelet(fx)
		fx.stopContainersFn = func() error { return errors.New("containerd refused") }
		err := runUninstall(io.Discard, true, fx)
		if err == nil || strings.Contains(err.Error(), "Nothing was changed") || !strings.Contains(err.Error(), "nothing recreates them") {
			t.Fatalf("Run = %v", err)
		}
	})
	t.Run("containerd started for it", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		fx.unitActive = func(string) bool { return false }
		unitWanted = func(string) bool { return false }
		noKubelet(fx)
		fx.stopContainersFn = func() error { return errors.New("containerd refused") }
		err := runUninstall(io.Discard, true, fx)
		if err == nil || !strings.Contains(err.Error(), "containerd, started to stop them, was stopped again") || !slices.Contains(*events, "stop containerd") {
			t.Fatalf("Run = %v, events = %q", err, *events)
		}
	})
}

// TestRunRemovesNoManifestItCannotRead: a manifest that could
// not be read could not be put back, so it is not removed.
func TestRunRemovesNoManifestItCannotRead(t *testing.T) {
	events, fx := runNode(t, "controller")
	stubControlPlane(t, fx)
	var stopped []string
	fx.stopStaticPodFn = func(name string) error { stopped = append(stopped, name); return nil }
	fx.readManifestFn = func(name string) ([]byte, error) {
		if name == "kube-apiserver" {
			return nil, errors.New("permission denied")
		}
		return []byte(name + " manifest"), nil
	}
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "read the kube-apiserver manifest") {
		t.Fatalf("Run = %v", err)
	}
	if slices.Contains(stopped, "kube-apiserver") || slices.Contains(*events, "remove state") {
		t.Errorf("stopped = %q, events = %q", stopped, *events)
	}
}

// TestUnmountStateRefusesWhileAMountRemains: RemoveAll does
// not stop at a mount boundary, so a mount below the state directories that
// could not be detached, or a mount table that cannot be read, stops the
// uninstall before anything is removed.
func TestUnmountStateRefusesWhileAMountRemains(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	vol := version.DataDir + "/kubelet/pods/p1/volume-subpaths/data/c/0"
	if err := os.WriteFile(info, []byte("41 40 0:32 / "+vol+" rw - ext4 /dev/sdc1 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := &hostEffects{mountInfoPath: info}
	fx.unmountFn = func(string) error { return errors.New("target is busy") }
	if err := unmountState(fx); err == nil || !strings.Contains(err.Error(), vol) || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("unmountState = %v; want the mount that stayed, and why", err)
	}
	fx.mountInfoPath = filepath.Join(t.TempDir(), "missing")
	if err := unmountState(fx); err == nil {
		t.Fatal("unmountState succeeded with no mount table to read")
	}

	events, fx := runNode(t, "worker")
	fx.unmountStateFn = func() error { return errors.New("1 mounts could not be unmounted") }
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "Nothing was deleted") || slices.Contains(*events, "remove state") {
		t.Fatalf("Run = %v, events = %q", err, *events)
	}
}

// TestRunCtrGivesUpOnAWedgedContainerd: a ctr call that never
// returns would hang the uninstall with the node half torn down, so every call
// is bounded and a timeout is an error the abort handles.
func TestRunCtrGivesUpOnAWedgedContainerd(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ctr"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	swap(t, &version.BinDir, bin)
	swap(t, &ctrTimeout, 200*time.Millisecond)
	start := time.Now()
	_, err := runCtr("tasks", "ls")
	if err == nil || !strings.Contains(err.Error(), "no answer from containerd") {
		t.Fatalf("runCtr = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("runCtr took %s against a wedged containerd", d)
	}
}

// TestRunStopsKubeletBeforeStartingContainerd: kubelet could
// use a containerd started for the teardown to recreate its pods, so it is
// stopped first.
func TestRunStopsKubeletBeforeStartingContainerd(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(name string) bool { return name == "kubelet" }
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop kubelet", "start containerd", "stop containers"}; !slices.Equal((*events)[:3], want) {
		t.Errorf("teardown = %q, want it to begin %q", *events, want)
	}
}

// TestAbortAfterTheHostProcessesStopStartsThemAgain: a
// failure after the supervisor was asked to stop everything, before anything
// is removed, still goes through the abort, which starts again what was up
// and puts the control plane back.
func TestAbortAfterTheHostProcessesStopStartsThemAgain(t *testing.T) {
	for name, fail := range map[string]func(fx *hostEffects){
		"stop fails": func(fx *hostEffects) {
			fx.stopHostProcesses = func() error { return errors.New("lost stop acknowledgement") }
		},
		"a shim stays": func(fx *hostEffects) {
			fx.hostProcessRunning = func(n string) (bool, error) { return n == "runtime-shim", nil }
		},
		"a mount stays": func(fx *hostEffects) {
			fx.unmountStateFn = func() error { return errors.New("1 mounts could not be unmounted") }
		},
		"static pod survives": func(fx *hostEffects) { fx.staticPodRunning = func(n string) (bool, error) { return n == "etcd", nil } },
	} {
		t.Run(name, func(t *testing.T) {
			events, fx := runNode(t, "controller")
			puts, starts := stubControlPlane(t, fx)
			fail(fx)
			err := runUninstall(io.Discard, true, fx)
			if err == nil || !strings.Contains(err.Error(), "services were started again (containerd, kubelet, operator)") ||
				!strings.Contains(err.Error(), "manifests were put back") || strings.Contains(err.Error(), "uninstall: uninstall") {
				t.Fatalf("Run = %v", err)
			}
			if len(*puts) != 2 || *starts != 1 || slices.Contains(*events, "remove state") {
				t.Errorf("puts = %q, kubelet starts = %d, events = %q", *puts, *starts, *events)
			}
		})
	}
}

// TestStartContainerdWaitsUntilItAnswers: the supervisor reports containerd
// active before its socket exists (measured: the first ctr call found no
// socket), so the start waits for ctr to get an answer.
func TestStartContainerdWaitsUntilItAnswers(t *testing.T) {
	started, calls := false, 0
	fx := &hostEffects{}
	fx.startAndWaitActive = func(string, time.Duration) error { started = true; return nil }
	fx.ctrRun = func(...string) (string, error) {
		if !started {
			t.Fatal("ctr was called before containerd was started")
		}
		calls++
		if calls < 3 {
			return "", errors.New("cannot access socket")
		}
		return "", nil
	}
	swap(t, &pollInterval, time.Millisecond)
	if err := startContainerd(fx); err != nil || calls != 3 {
		t.Fatalf("startContainerd = %v after %d ctr calls, want success on the third", err, calls)
	}
	swap(t, &containerdStartWait, 10*time.Millisecond)
	fx.ctrRun = func(...string) (string, error) { return "", errors.New("cannot access socket") }
	if err := startContainerd(fx); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("startContainerd = %v, want a timeout when containerd never answers", err)
	}
}

// TestAbortStopsTheTeardownsContainerdBeforeKubelet: kubelet
// started again beside the containerd started only for the teardown could
// recreate the pods just stopped, so that containerd is stopped first, and
// kubelet is left stopped if it cannot be.
func TestAbortStopsTheTeardownsContainerdBeforeKubelet(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(name string) bool { return name == "kubelet" }
	fx.stopContainersFn = func() error { return errors.New("containerd refused") }
	if err := runUninstall(io.Discard, true, fx); err == nil {
		t.Fatal("Run succeeded")
	}
	if i, j := slices.Index(*events, "stop containerd"), slices.Index(*events, "start kubelet"); i < 0 || j < 0 || i > j {
		t.Errorf("events = %q; kubelet was started again before the teardown's containerd was stopped", *events)
	}

	events, fx = runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(name string) bool { return name == "kubelet" }
	fx.stopContainersFn = func() error { return errors.New("containerd refused") }
	fx.stopContainerdFn = func() error { return errors.New("supervisor gone") }
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "kubelet left stopped") || slices.Contains(*events, "start kubelet") {
		t.Fatalf("Run = %v, events = %q", err, *events)
	}
}

// TestStopContainersDeletesNoTaskItCouldNotSignal: a forced
// delete of a task whose process never got the signal can leave that process
// running unsupervised, so a signal that fails for any reason but the task
// being gone stops the teardown before any delete.
func TestStopContainersDeletesNoTaskItCouldNotSignal(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{"fast": true, "slow": true}, failKill: map[string]error{
		"fast": errors.New("cannot kill: process already finished: not found"),
		"slow": errors.New("ttrpc: connection refused"),
	}}
	fx := withFakeCtr(t, f)
	err := stopContainers(io.Discard, fx)
	if err == nil || !strings.Contains(err.Error(), "k8s.io/slow") || strings.Contains(err.Error(), "k8s.io/fast") {
		t.Fatalf("stopContainers = %v; want only the task that could not be signalled reported", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "tasks delete") || strings.Contains(c, "sandboxes rm") {
			t.Errorf("calls = %q; deleted after a signal failed", f.calls)
		}
	}
}

// TestAbortAfterTheHostStopAwaitsTheTeardownsContainerd: the
// supervisor's stop only signals, so a containerd started only for the
// teardown may still be serving when a later failure aborts; it is stopped and
// waited on before kubelet is started again.
func TestAbortAfterTheHostStopAwaitsTheTeardownsContainerd(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(name string) bool { return name == "kubelet" }
	fx.stopHostProcesses = func() error { return errors.New("lost stop acknowledgement") }
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "lost stop acknowledgement") {
		t.Fatalf("Run = %v", err)
	}
	if i, j := slices.Index(*events, "stop containerd"), slices.Index(*events, "start kubelet"); i < 0 || j < 0 || i > j {
		t.Errorf("events = %q; kubelet was started before the teardown's containerd was stopped", *events)
	}
}

// TestStopContainerdWaitsForItsProcess: the supervisor's stop only signals, so
// stopping containerd waits for the process to be gone. The stop goes through
// the run's stopSupervisedUnit effect, so the test never dials the real
// control socket.
func TestStopContainerdWaitsForItsProcess(t *testing.T) {
	fx := &hostEffects{}
	polls := 0
	stops := 0
	fx.stopSupervisedUnit = func(string) error { stops++; return nil }
	swap(t, &pollInterval, time.Millisecond)
	fx.hostProcessRunning = func(string) (bool, error) { polls++; return polls < 3, nil }
	if err := stopContainerd(fx); err != nil || polls != 3 {
		t.Fatalf("stopContainerd = %v after %d polls, want success once the process is gone", err, polls)
	}
	if stops != 1 {
		t.Errorf("the supervisor was asked to stop containerd %d times, want once", stops)
	}
	swap(t, &containerdStartWait, 10*time.Millisecond)
	fx.hostProcessRunning = func(string) (bool, error) { return true, nil }
	if err := stopContainerd(fx); err == nil {
		t.Fatal("stopContainerd succeeded with containerd still running")
	}
}

// TestStopHelpersFailClosedOnUnsetEffects: the supervisor-stop helpers are the
// run's effects like every other, so a zero hostEffects answers with the
// unset-effect sentinel before dialing the supervisor's control socket — a
// const path no test can redirect, and a live supervisor on a developer node
// would take the command for real. A stop read from a package call would
// let a zero fx reach the socket dial (and then panic through the nil
// process probe) instead of returning the sentinel.
func TestStopHelpersFailClosedOnUnsetEffects(t *testing.T) {
	t.Parallel() // host-free: the sentinel fires before any host call
	for _, fn := range []struct {
		name string
		stop func(fx *hostEffects) error
	}{
		{"stopKubelet", stopKubelet},
		{"stopContainerd", stopContainerd},
	} {
		err := fn.stop(&hostEffects{})
		if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "stopSupervisedUnit") {
			t.Errorf("%s with every effect unset = %v; want the sentinel naming stopSupervisedUnit", fn.name, err)
		}
	}
}

// TestRunStopsAKubeletTheSupervisorCannotReport: a supervisor
// status that cannot be read says "not running", so a live kubelet process is
// stopped whatever the status says.
func TestRunStopsAKubeletTheSupervisorCannotReport(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(name string) bool { return name != "kubelet" }
	unitWanted = func(name string) bool { return name != "kubelet" }
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*events, "stop kubelet") {
		t.Errorf("events = %q; a live kubelet was left running under the teardown", *events)
	}
}

// TestStopContainersSignalsOnlyTasksThatMayHaveAProcess: a
// STOPPED task has no process, and signalling one is an error, so it is only
// deleted; PAUSED and UNKNOWN may still own one, so they are signalled and,
// not stopping, killed.
func TestStopContainersSignalsOnlyTasksThatMayHaveAProcess(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{"fast": false, "slow": true}, status: map[string]string{"slow": "PAUSED"}}
	fx := withFakeCtr(t, f)
	if err := stopContainers(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	var signals, deletes []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, "tasks kill"):
			signals = append(signals, strings.TrimPrefix(c, "-n k8s.io tasks kill --all --signal "))
		case strings.Contains(c, "tasks delete"):
			deletes = append(deletes, strings.TrimPrefix(c, "-n k8s.io tasks delete --force "))
		}
	}
	if want := []string{"SIGTERM slow", "SIGKILL slow"}; !slices.Equal(signals, want) {
		t.Errorf("signals = %q, want %q", signals, want)
	}
	if want := []string{"fast", "slow"}; !slices.Equal(deletes, want) {
		t.Errorf("deleted = %q, want %q", deletes, want)
	}
}

// TestTaskGoneIsOnlyContainerdSayingSo: ctr failing to reach
// containerd can say "no such file or directory", and is never a task gone.
func TestTaskGoneIsOnlyContainerdSayingSo(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for msg, want := range map[string]bool{
		"ctr: task fast: not found":                        true,
		"cannot kill: process already finished: not found": true,
		`unable to delete x" error="ttrpc: closed`:         true,
		"ctr: cannot access socket /run/stratum/containerd/containerd.sock: stat: no such file or directory": false,
		"connection error: desc = transport: dial unix /run/x.sock: connect: no such file or directory":      false,
		"ctr: permission denied": false,
	} {
		if got := taskGone(errors.New(msg)); got != want {
			t.Errorf("taskGone(%q) = %v, want %v", msg, got, want)
		}
	}
}

// TestAbortStopsATeardownContainerdEvenIfItWasWanted: a
// containerd in restart backoff was wanted but not running, and the one
// started for the teardown is stopped before kubelet starts; it is started
// again after.
func TestAbortStopsATeardownContainerdEvenIfItWasWanted(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unitActive = func(string) bool { return false }
	fx.stopContainersFn = func() error { return errors.New("containerd refused") }
	if err := runUninstall(io.Discard, true, fx); err == nil {
		t.Fatal("Run succeeded")
	}
	stop, restart, kubelet := slices.Index(*events, "stop containerd"), slices.Index(*events, "start containerd"), slices.Index(*events, "start kubelet")
	if stop < 0 || kubelet < 0 || stop > kubelet || slices.Index((*events)[stop:], "start containerd") < 0 {
		t.Errorf("events = %q; want the teardown's containerd stopped before kubelet, then started again (%d %d %d)", *events, stop, restart, kubelet)
	}
}

// TestRunAbortsWhenTheSupervisorStatusCannotBeRead: a status
// that cannot be read would read as nothing running and nothing wanted, which
// could skip a kubelet in restart backoff and leave an abort nothing to
// start again, so it stops the teardown before any container is touched.
func TestRunAbortsWhenTheSupervisorStatusCannotBeRead(t *testing.T) {
	events, fx := runNode(t, "worker")
	noKubelet(fx)
	fx.supervisorStatus = func() (map[string]supervisor.ProcStatus, error) { return nil, errors.New("control socket timeout") }
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "read the supervisor's status") {
		t.Fatalf("Run = %v", err)
	}
	for _, e := range *events {
		if e == "stop containers" || e == "stop host processes" || e == "remove state" {
			t.Errorf("events = %q; the teardown went on without the supervisor's status", *events)
		}
	}
}

// TestSignalTasksDoesNotTakeADroppedShimForAStoppedTask: a
// "ttrpc: closed" is measured only for a delete; mid-kill it may leave the
// container running, so it fails the teardown before any delete.
func TestSignalTasksDoesNotTakeADroppedShimForAStoppedTask(t *testing.T) {
	f := &fakeCtr{running: map[string]bool{"fast": true, "slow": true}, failKill: map[string]error{
		"slow": errors.New(`unable to kill slow" error="ttrpc: closed`),
	}}
	fx := withFakeCtr(t, f)
	if err := stopContainers(io.Discard, fx); err == nil || !strings.Contains(err.Error(), "k8s.io/slow") {
		t.Fatalf("stopContainers = %v; a kill the shim dropped was taken for a stopped task", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "tasks delete") {
			t.Errorf("calls = %q; deleted after a kill was dropped", f.calls)
		}
	}
}

// TestRemoveSandboxesChecksTheyAreGone: ctr sandboxes rm logs
// a sandbox it could not stop or shut down and still exits 0, so one still
// listed afterwards is a failure.
func TestRemoveSandboxesChecksTheyAreGone(t *testing.T) {
	f := &fakeCtr{noTasks: true, running: map[string]bool{}, sandboxes: []string{"sb1", "stuck"}, stickySb: map[string]bool{"stuck": true}}
	fx := withFakeCtr(t, f)
	err := stopContainers(io.Discard, fx)
	if err == nil || !strings.Contains(err.Error(), "k8s.io/stuck: still present") || strings.Contains(err.Error(), "sb1") {
		t.Fatalf("stopContainers = %v; want only the sandbox still listed reported", err)
	}
}

// TestRunMarksAWorkersNodeOnlyPastEveryAbort: a worker marks its own
// Node only once nothing can abort or fail any more, after its state is removed,
// so the control plane removes it once the worker is gone, and says so; an
// uninstall that aborts never marks it, so the operator cannot delete the Node
// of a worker that stays. A control plane is never marked.
func TestRunMarksAWorkersNodeOnlyPastEveryAbort(t *testing.T) {
	t.Run("worker", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		var err error
		out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.Index(*events, "mark node-a"); i != len(*events)-1 || slices.Index(*events, "remove state") > i {
			t.Errorf("events = %q; want the Node marked last, once its state is removed", *events)
		}
		if !strings.Contains(out, "removed by the control plane") {
			t.Errorf("output does not say the control plane removes the Node:\n%s", out)
		}
	})
	t.Run("abort", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		fx.stopContainersFn = func() error { return errors.New("containerd refused") }
		if err := runUninstall(io.Discard, true, fx); err == nil {
			t.Fatal("Run succeeded")
		}
		if slices.Contains(*events, "mark node-a") {
			t.Errorf("events = %q; an aborted uninstall marked its Node", *events)
		}
	})
	t.Run("mark fails", func(t *testing.T) {
		_, fx := runNode(t, "worker")
		fx.nodeMarkerFn = func() (func(string, time.Time) error, error) {
			return func(string, time.Time) error { return errors.New("forbidden") }, nil
		}
		var err error
		out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "could not mark Node node-a") || !strings.Contains(out, "Node object stays") {
			t.Errorf("output does not say the mark failed and how to remove the Node:\n%s", out)
		}
	})
	t.Run("controller", func(t *testing.T) {
		events, fx := runNode(t, "controller")
		if err := runUninstall(io.Discard, true, fx); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(*events, "mark node-a") {
			t.Errorf("events = %q; a control plane's Node was marked", *events)
		}
	})
}

// TestRunDoesNotMarkWhenTheLastSweepRefuses: the host-process
// sweep just before removal is the last abort, and a refusal there must leave
// the Node unmarked.
func TestRunDoesNotMarkWhenTheLastSweepRefuses(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.unmountStateFn = func() error {
		fx.hostProcessRunning = func(n string) (bool, error) { return n == "kubelet", nil }
		return nil
	}
	if err := runUninstall(io.Discard, true, fx); err == nil {
		t.Fatal("Run succeeded with kubelet running again")
	}
	if slices.Contains(*events, "mark node-a") {
		t.Errorf("events = %q; the Node was marked although the uninstall refused", *events)
	}
}

// TestRunDoesNotMarkWhenItsStateCannotBeRemoved: a removal
// that fails is an uninstall that did not finish, so the Node is not marked.
func TestRunDoesNotMarkWhenItsStateCannotBeRemoved(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.removeAll = func(string) error { return errors.New("device or resource busy") }
	if err := runUninstall(io.Discard, true, fx); err == nil {
		t.Fatal("Run succeeded with its state still there")
	}
	if slices.Contains(*events, "mark node-a") {
		t.Errorf("events = %q; the Node was marked although its state could not be removed", *events)
	}
}

// TestRunReadsKubeletsCredentialBeforeRemovingIt: the credential that writes
// the mark lives in the state that is removed first, so it is read before;
// one that cannot be read is said so, with the manual command.
func TestRunReadsKubeletsCredentialBeforeRemovingIt(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.nodeMarkerFn = func() (func(string, time.Time) error, error) {
		if slices.Contains(*events, "remove state") {
			t.Error("kubelet's credential was read after the state that holds it was removed")
		}
		return nil, errors.New("open kubelet.kubeconfig: no such file or directory")
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "could not read kubelet's credential") || !strings.Contains(out, "Node object stays") {
		t.Errorf("output does not say the credential could not be read and how to remove the Node:\n%s", out)
	}
}

// A failed firewalld removal must leave the ownership record under DataDir for
// retry, while the other teardown targets are still removed.
func TestRunReportsAFirewalldSourceItCouldNotRemove(t *testing.T) {
	events, fx := runNode(t, "controller")
	var removed []string
	fx.removeAll = func(path string) error {
		removed = append(removed, path)
		return nil
	}
	fx.untrustPodCIDRs = func() error {
		*events = append(*events, "untrust pod cidrs")
		return errors.New("firewalld still trusts pod CIDRs stratum added")
	}
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "firewalld still trusts") {
		t.Fatalf("Run = %v, want the firewalld failure", err)
	}
	if slices.Contains(removed, version.DataDir) || !slices.Contains(removed, version.ConfDir) {
		t.Errorf("removed paths = %q; want the ownership record preserved but other targets removed", removed)
	}
	if !slices.Contains(*events, "untrust pod cidrs") {
		t.Errorf("untrust was skipped: %q", *events)
	}
}

// TestRunRemovesEveryTargetAfterReadingTheFirewalldRecord pins the delete
// loop: it removes exactly Targets(), in order, and only after the firewalld
// record is read — the untrust of the pod CIDRs goes first because the
// record it reads lives under DataDir, which the loop removes. A loop
// deleting another set than Targets(), or removing the record before the
// cleanup that reads it, fails here.
func TestRunRemovesEveryTargetAfterReadingTheFirewalldRecord(t *testing.T) {
	events, fx := runNode(t, "worker")
	var removed []string
	fx.removeAll = func(p string) error {
		removed = append(removed, p)
		*events = append(*events, "remove "+p)
		return nil
	}
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	if want := Targets(); !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want Targets() in order: %q", removed, want)
	}
	untrust, first := slices.Index(*events, "untrust pod cidrs"), slices.Index(*events, "remove "+Targets()[0])
	if untrust < 0 || first < 0 || untrust > first {
		t.Errorf("events = %q; want the firewalld record read (\"untrust pod cidrs\") before the first removal", *events)
	}
}

// TestWaitHostExitWaitsForContainerd: the supervisor's stop only
// signals, and a containerd still shutting down when the host-process check
// ran made uninstall refuse. The wait lasts until containerd is gone, then
// until the shims are.
func TestWaitHostExitWaitsForContainerd(t *testing.T) {
	fx := &hostEffects{}
	polls := map[string]int{}
	swap(t, &pollInterval, time.Millisecond)
	swap(t, &fx.hostProcessRunning, func(name string) (bool, error) {
		polls[name]++
		switch name {
		case "containerd":
			return polls[name] <= 3, nil
		case "runtime-shim":
			if polls["containerd"] <= 3 {
				t.Error("waited for the shims before containerd was gone")
			}
			return polls[name] <= 2, nil
		}
		return false, nil
	})
	waitHostExit(fx)
	if polls["containerd"] != 4 || polls["runtime-shim"] != 3 {
		t.Errorf("polls = %v, want containerd until its 4th poll and the shims until their 3rd", polls)
	}
}

// The SELinux contexts stratum registered are removed before the delete loop,
// which removes the record of them with DataDir, and a failure is reported
// without keeping the rest of the node.
func TestRunRemovesTheSELinuxContextsBeforeTheData(t *testing.T) {
	events, fx := runNode(t, "controller")
	fx.removeSELinuxContexts = func() error {
		*events = append(*events, "remove selinux contexts")
		return nil
	}
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	ctx, state := slices.Index(*events, "remove selinux contexts"), slices.Index(*events, "remove state")
	if ctx < 0 || state < 0 || ctx > state {
		t.Errorf("events = %q, want the SELinux contexts removed before the state", *events)
	}

	events, fx = runNode(t, "controller")
	var removed []string
	fx.removeAll = func(path string) error {
		removed = append(removed, path)
		return nil
	}
	fx.removeSELinuxContexts = func() error { return errors.New("semanage: policy store locked") }
	fx.removeAll = func(path string) error {
		removed = append(removed, path)
		if path == version.ConfDir {
			return errors.New("device busy")
		}
		return nil
	}
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "SELinux file contexts") || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("Run = %v, want the SELinux failure and the path failure both reported", err)
	}
	if !slices.Contains(removed, version.DataDir) {
		t.Errorf("removed = %q; a SELinux failure must not keep the node's data", removed)
	}
}

// TestRunHealsTheHostConfigThroughTheSeamExactlyOnce: the legacy-config heal
// deletes /etc/containerd/config.toml when an older stratum wrote it over the
// host's own config — the one teardown effect outside stratum's own layout —
// so it goes through an injectable seam like every other effect, invoked
// exactly once on the forced path. Its place in the order is after the state's
// removal (the production call sits before the firstErr return on purpose: an
// independent healing step a removal failure elsewhere must not skip) and
// before the Node is marked.
func TestRunHealsTheHostConfigThroughTheSeamExactlyOnce(t *testing.T) {
	t.Run("forced", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		fx.removeLegacyHostConfig = func(io.Writer) {
			healRuns++
			*events = append(*events, "remove legacy host config")
		}
		if err := runUninstall(io.Discard, true, fx); err != nil {
			t.Fatal(err)
		}
		if healRuns != 1 {
			t.Errorf("the legacy-config heal ran %d times on the forced path, want exactly once", healRuns)
		}
		want := []string{"stop kubelet", "stop containers", "stop host processes", "wait exit",
			"stop idle shims", "teardown datapath", "untrust pod cidrs", "remove state",
			"remove legacy host config", "mark node-a"}
		if !slices.Equal(*events, want) {
			t.Errorf("teardown = %q, want %q", *events, want)
		}
	})
	t.Run("once even when a removal fails", func(t *testing.T) {
		_, fx := runNode(t, "worker")
		heals := 0
		fx.removeLegacyHostConfig = func(io.Writer) { heals++ }
		fx.removeAll = func(string) error { return errors.New("device or resource busy") }
		err := runUninstall(io.Discard, true, fx)
		if err == nil || !strings.Contains(err.Error(), "device or resource busy") {
			t.Fatalf("Run = %v, want the removal failure", err)
		}
		if heals != 1 {
			t.Errorf("the heal ran %d times although the forced path reached it, want exactly once", heals)
		}
	})
}

// TestDryRunNeverHealsTheHostConfig: the heal is a forced-path step, so a dry
// run never invokes the seam — it only prints what a forced run would do, and
// its disclosure of a legacy config goes through the unseamed probe
// runtime.LegacyHostConfig like the other dry-run probes.
func TestDryRunNeverHealsTheHostConfig(t *testing.T) {
	fx := &hostEffects{}
	stubTeardown(t, fx, nil)
	err := runUninstall(io.Discard, false, fx)
	if err != nil {
		t.Fatal(err)
	}
	if healRuns != 0 {
		t.Errorf("the dry run invoked the legacy-config heal %d times, want never", healRuns)
	}
}
