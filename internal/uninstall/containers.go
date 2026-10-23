package uninstall

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// A node with running pods could never be uninstalled. The
// supervisor stopped containerd, but a runtime-v2 shim outlives containerd by
// design and keeps its containers running, so the shims survived and the
// host-process check refused every attempt. Killing a shim that still has
// containers is not the fix: a shim is the parent of its containers, which
// would be left running under init. So kubelet is stopped first, so nothing
// recreates a container, and then every container is stopped and its task
// deleted, and every pod sandbox removed, through this node's own containerd
// while it is still up. In containerd 2 a shim belongs to its pod sandbox and
// exits when the sandbox is shut down, not when its tasks are deleted:
// measured, deleting every task left every workload shim running. Removing the
// sandboxes ends most shims; the few left idle once containerd stops are
// stopped by stopIdleShims (shims.go).

// The wait durations here are read, never written, once the package is
// initialized, so two Runs in one process cannot race over them; tests shorten
// them, which is why they stay variables. Every probe the teardown consults
// (the process table, the node state) and every effect are fields on the run's
// hostEffects value, built in Run; there is no package-level one to fall back
// to.
var (
	kubeletStopWait = 60 * time.Second
	containerGrace  = 30 * time.Second
	ctrTimeout      = 2 * time.Minute

	containerdStartWait = 60 * time.Second
	hostExitWait        = 60 * time.Second
	shimExitWait        = 30 * time.Second
	pollInterval        = 500 * time.Millisecond
)

// stopKubelet stops kubelet through the supervisor and waits for its process
// to be gone, so it cannot recreate the containers stopped next. The stop, and
// the process-table probe that waits on it, are the run's effects: a zero
// hostEffects answers with the unset-effect sentinel instead of dialing the
// supervisor's control socket.
func stopKubelet(fx *hostEffects) error {
	if fx.stopSupervisedUnit == nil {
		return unsetEffect("stopSupervisedUnit")
	}
	if fx.hostProcessRunning == nil {
		return unsetEffect("hostProcessRunning")
	}
	if err := fx.stopSupervisedUnit("kubelet"); err != nil {
		return fmt.Errorf("stop kubelet: %w", err)
	}
	deadline := time.Now().Add(kubeletStopWait)
	for {
		running, err := fx.hostProcessRunning("kubelet")
		if err != nil {
			return fmt.Errorf("check kubelet stopped: %w", err)
		}
		if !running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kubelet still running %s after it was stopped", kubeletStopWait)
		}
		time.Sleep(pollInterval)
	}
}

// stopPods stops the pods' containers, which would otherwise keep their
// runtime shims alive after containerd stops, so the uninstall could never
// finish on a node running pods. kubelet goes first so
// nothing recreates them, and containerd must be up to stop them. Each step is
// recorded in undo before it is taken, so an abort knows what it has to put
// back and what it cannot. Its own effects are read from fx, the run's
// host-effects value, the same way the runner reads them.
func stopPods(w io.Writer, fx *hostEffects, undo *teardownUndo, status map[string]supervisor.ProcStatus) error {
	if fx.hostProcessRunning == nil {
		return unsetEffect("hostProcessRunning")
	}
	// Wanted as well as running: a kubelet waiting out a restart backoff is
	// not running, and would come back mid-teardown and recreate its pods.
	// Before containerd is started below, too, or kubelet could use it to
	// recreate them first. And a live kubelet process, whatever the
	// supervisor's status says.
	kubeletLive, err := fx.hostProcessRunning("kubelet")
	if err != nil {
		return fmt.Errorf("check for a running kubelet: %w", err)
	}
	if kubelet := status["kubelet"]; kubeletLive || kubelet.Running || kubelet.Wanted {
		fmt.Fprintln(w, "  stopping kubelet")
		undo.kubeletStopped = true
		if fx.stopKubeletFn == nil {
			return unsetEffect("stopKubeletFn")
		}
		if err := fx.stopKubeletFn(); err != nil {
			return err
		}
	}
	if !status["containerd"].Running {
		// A node a refused uninstall leaves with containerd stopped and the
		// shims still holding their containers: containerd is started again
		// to stop them, since nothing has been deleted and it reattaches to the
		// shims it left. With no shim left there is nothing to stop.
		shims, err := fx.hostProcessRunning("runtime-shim")
		if err != nil {
			return fmt.Errorf("check for runtime shims: %w", err)
		}
		if !shims {
			return nil
		}
		fmt.Fprintln(w, "  starting containerd to stop the containers still running under it")
		undo.containerdStarted = true
		if fx.startContainerdFn == nil {
			return unsetEffect("startContainerdFn")
		}
		if err := fx.startContainerdFn(); err != nil {
			return fmt.Errorf("start containerd to stop this node's containers: %w", err)
		}
	}
	undo.containersStopped = true
	if fx.stopContainersFn == nil {
		return unsetEffect("stopContainersFn")
	}
	if err := fx.stopContainersFn(); err != nil {
		return fmt.Errorf("stop this node's containers: %w", err)
	}
	return nil
}

// startContainerd starts containerd through the supervisor and waits until it
// answers. Running is not serving: the supervisor reports it active once the
// process exists, before its socket does (measured: the first ctr call found
// no socket). Its start, and every ctr call, are effects of the run.
func startContainerd(fx *hostEffects) error {
	if fx.startAndWaitActive == nil {
		return unsetEffect("startAndWaitActive")
	}
	if fx.ctrRun == nil {
		return unsetEffect("ctrRun")
	}
	if err := fx.startAndWaitActive("containerd", containerdStartWait); err != nil {
		return err
	}
	deadline := time.Now().Add(containerdStartWait)
	for {
		_, err := fx.ctrRun("version")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("containerd did not answer within %s: %w", containerdStartWait, err)
		}
		time.Sleep(pollInterval)
	}
}

// stopContainerd stops containerd through the supervisor and waits for its
// process to be gone: the stop only signals it. A supervisor that cannot be
// asked is no failure once the process has gone anyway. The stop is the run's
// effect, read from fx like every other.
func stopContainerd(fx *hostEffects) error {
	if fx.stopSupervisedUnit == nil {
		return unsetEffect("stopSupervisedUnit")
	}
	if fx.hostProcessRunning == nil {
		return unsetEffect("hostProcessRunning")
	}
	stopErr := fx.stopSupervisedUnit("containerd")
	deadline := time.Now().Add(containerdStartWait)
	for {
		running, err := fx.hostProcessRunning("containerd")
		if err != nil {
			return fmt.Errorf("check containerd stopped: %w", err)
		}
		if !running {
			return nil
		}
		if time.Now().After(deadline) {
			if stopErr != nil {
				return fmt.Errorf("stop containerd: %w", stopErr)
			}
			return fmt.Errorf("containerd still running %s after it was stopped", containerdStartWait)
		}
		time.Sleep(pollInterval)
	}
}

// kubeletNodeMarker reads kubelet's credential, certificates included, into
// memory and returns what annotates this worker's own Node with it: the Node
// authorizer lets a node update Nodes, NodeRestriction limits that to its own
// and leaves annotations alone, and the stratum-node-uninstall-mark admission
// policy lets nobody else write the mark. In memory because the mark is
// written only after the state directory, which holds the kubeconfig, the
// certificates and kubectl, has been removed.
func kubeletNodeMarker() (func(host string, at time.Time) error, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"))
	if err != nil {
		return nil, err
	}
	if err := rest.LoadTLSFiles(cfg); err != nil {
		return nil, err
	}
	cfg.CAFile, cfg.CertFile, cfg.KeyFile = "", "", ""
	cfg.Timeout = 15 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return func(host string, at time.Time) error {
		patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, k8sutil.UninstalledAnnotation, k8sutil.UninstallMark(at))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := cs.CoreV1().Nodes().Patch(ctx, host, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	}, nil
}

// runCtr runs the payload's ctr against this node's containerd. Only
// Stratum's containerd socket is ever addressed, never a host containerd.
// Every call is bounded: a wedged containerd or shim would otherwise hang the
// uninstall with the node half torn down and no abort to put it back.
func runCtr(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ctrTimeout)
	defer cancel()
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	cmd := exec.CommandContext(ctx, filepath.Join(version.BinDir, "ctr"), append([]string{"--address", sock}, args...)...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), fmt.Errorf("ctr %s: %w within %s", strings.Join(args, " "), errCtrTimeout, ctrTimeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("ctr %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// errCtrTimeout is runCtr's answer when containerd did not respond in time.
var errCtrTimeout = errors.New("no answer from containerd")

type task struct {
	ns, id string
	live   bool
}

// stopContainers stops every container this node's containerd runs, in every
// namespace: SIGTERM, a grace period, SIGKILL for anything still running, then
// the task is deleted. Then every pod sandbox is removed, which ends most
// shims. Every ctr call is an effect of the run, read from fx.
func stopContainers(w io.Writer, fx *hostEffects) error {
	if fx.ctrRun == nil {
		return unsetEffect("ctrRun")
	}
	out, err := fx.ctrRun("namespaces", "ls", "-q")
	if err != nil {
		return fmt.Errorf("list containerd namespaces: %w", err)
	}
	namespaces := strings.Fields(out)
	// Before anything is stopped or removed: the network namespaces are read
	// from the containers' specs and the pod sandboxes' metadata, which go with
	// them, and saved, so
	// that a record that cannot be saved stops the uninstall with nothing torn
	// down (datapath.go).
	if err := recordStratumNetns(fx, namespaces); err != nil {
		return err
	}
	if err := stopTasks(w, fx, namespaces); err != nil {
		return err
	}
	return removeSandboxes(w, fx, namespaces)
}

// stopTasks gives every task that may have a process SIGTERM and a grace
// period, SIGKILLs what is still not stopped, and deletes them all. A task
// already STOPPED has no process to signal, and signalling it is an error, so
// it is only deleted.
func stopTasks(w io.Writer, fx *hostEffects, namespaces []string) error {
	var tasks, live []task
	for _, ns := range namespaces {
		out, err := fx.ctrRun("-n", ns, "tasks", "ls")
		if err != nil {
			return fmt.Errorf("list tasks in containerd namespace %s: %w", ns, err)
		}
		for id, status := range taskStatuses(out) {
			t := task{ns, id, status != "STOPPED"}
			tasks = append(tasks, t)
			if t.live {
				live = append(live, t)
			}
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	slices.SortFunc(tasks, func(a, b task) int { return cmp.Compare(a.ns+"/"+a.id, b.ns+"/"+b.id) })
	slices.SortFunc(live, func(a, b task) int { return cmp.Compare(a.ns+"/"+a.id, b.ns+"/"+b.id) })
	fmt.Fprintf(w, "  stopping %d containers\n", len(tasks))
	// A task that exited on its own since the listing is not a failure, but
	// any other failure to signal one is, before any task is deleted: a forced
	// delete of a task whose process never got the signal can leave that
	// process running with nothing supervising it.
	if err := signalTasks(fx, live, "SIGTERM"); err != nil {
		return err
	}
	running := waitStopped(fx, live, containerGrace)
	if err := signalTasks(fx, running, "SIGKILL"); err != nil {
		return err
	}
	if len(running) > 0 {
		fmt.Fprintf(w, "  %d containers did not stop within %s of SIGTERM and were killed\n", len(running), containerGrace)
	}
	var failed []string
	for _, t := range tasks {
		// --force deletes a task whose SIGKILL has not been reaped yet.
		if _, err := fx.ctrRun("-n", t.ns, "tasks", "delete", "--force", t.id); err != nil && !taskGone(err) {
			failed = append(failed, fmt.Sprintf("%s/%s: %v", t.ns, t.id, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not delete %d container tasks: %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// signalTasks sends sig to every task and reports those it could not signal
// for a reason other than the task being gone.
func signalTasks(fx *hostEffects, tasks []task, sig string) error {
	var failed []string
	for _, t := range tasks {
		// Only NotFound: a shim that drops its connection mid-kill ("ttrpc:
		// closed") may leave its container running, and a forced delete
		// would then leave it unsupervised.
		if _, err := fx.ctrRun("-n", t.ns, "tasks", "kill", "--all", "--signal", sig, t.id); err != nil && !taskNotFound(err) {
			failed = append(failed, fmt.Sprintf("%s/%s: %v", t.ns, t.id, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not send %s to %d container tasks, so none was deleted: %s", sig, len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// removeSandboxes removes every pod sandbox, which usually shuts its shim down.
// `ctr sandboxes rm` only logs a sandbox it failed to load, stop or shut down
// and still exits 0, so each namespace is listed again afterwards and a
// sandbox still there is a failure.
func removeSandboxes(w io.Writer, fx *hostEffects, namespaces []string) error {
	var failed []string
	removed := 0
	for _, ns := range namespaces {
		out, err := fx.ctrRun("-n", ns, "sandboxes", "ls")
		if err != nil {
			return fmt.Errorf("list sandboxes in containerd namespace %s: %w", ns, err)
		}
		ids := sandboxIDs(out)
		if len(ids) == 0 {
			continue
		}
		rmErr := map[string]error{}
		for _, id := range ids {
			if _, err := fx.ctrRun("-n", ns, "sandboxes", "rm", "--force", id); err != nil && !taskGone(err) {
				rmErr[id] = err
			}
		}
		out, err = fx.ctrRun("-n", ns, "sandboxes", "ls")
		if err != nil {
			return fmt.Errorf("list sandboxes in containerd namespace %s after removing them: %w", ns, err)
		}
		left := sandboxIDs(out)
		for _, id := range ids {
			switch {
			case rmErr[id] != nil:
				failed = append(failed, fmt.Sprintf("%s/%s: %v", ns, id, rmErr[id]))
			case slices.Contains(left, id):
				failed = append(failed, fmt.Sprintf("%s/%s: still present after it was removed", ns, id))
			default:
				removed++
			}
		}
	}
	if removed > 0 {
		fmt.Fprintf(w, "  removed %d pod sandboxes\n", removed)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not remove %d pod sandboxes: %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// sandboxIDs is the ID column of `ctr sandboxes ls`, which has no quiet flag.
func sandboxIDs(out string) []string {
	var ids []string
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || (i == 0 && f[0] == "ID") {
			continue
		}
		ids = append(ids, f[0])
	}
	return ids
}

// waitStopped polls until none of tasks is running, or timeout, and returns
// those still running.
func waitStopped(fx *hostEffects, tasks []task, timeout time.Duration) []task {
	deadline := time.Now().Add(timeout)
	for {
		var running []task
		byNS := map[string]map[string]bool{}
		for _, t := range tasks {
			if _, ok := byNS[t.ns]; !ok {
				byNS[t.ns] = runningTasks(fx, t.ns)
			}
			if byNS[t.ns][t.id] || byNS[t.ns]["*"] {
				running = append(running, t)
			}
		}
		if len(running) == 0 || time.Now().After(deadline) {
			return running
		}
		time.Sleep(pollInterval)
	}
}

// runningTasks is the set of task IDs in ns that ctr does not report STOPPED:
// PAUSED, UNKNOWN and anything else may still own a process, so only the one
// terminal status counts as stopped. A listing that fails — or an unset ctr
// effect, which the caller reads the same way — is read as "all still
// running", so the caller escalates rather than assuming they stopped.
func runningTasks(fx *hostEffects, ns string) map[string]bool {
	if fx.ctrRun == nil {
		return map[string]bool{"*": true}
	}
	out, err := fx.ctrRun("-n", ns, "tasks", "ls")
	running := map[string]bool{}
	if err != nil {
		running["*"] = true
		return running
	}
	for id, status := range taskStatuses(out) {
		if status != "STOPPED" {
			running[id] = true
		}
	}
	return running
}

// taskStatuses is `ctr tasks ls` as task ID -> STATUS, header skipped.
func taskStatuses(out string) map[string]string {
	st := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] == "TASK" {
			continue
		}
		st[f[0]] = f[2]
	}
	return st
}

// taskGone reports whether a ctr error is containerd saying the task or
// sandbox no longer exists. A "ttrpc: closed" is the shim's connection going
// away mid-delete because the shim itself exited, which is the outcome being
// asked for (measured on a worker: the last task in a sandbox reported it).
// Not "no such": ctr failing to reach containerd at all says "no such file or
// directory", and is never "gone".
func taskGone(err error) bool {
	return taskNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "ttrpc: closed")
}

// taskNotFound reports whether a ctr error is containerd's NotFound for the
// task or sandbox itself.
func taskNotFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// waitHostExit waits for the host processes the supervisor was just asked to
// stop, then gives the runtime shims time to exit once their sandboxes are
// removed and containerd has stopped. The supervisor's stop only signals: a
// containerd 2.4 still shutting down failed the host-process check that
// follows. It only waits; stopIdleShims and then that check deal
// with anything that remains.
func waitHostExit(fx *hostEffects) {
	waitGone(fx, units, hostExitWait)
	waitGone(fx, []string{"runtime-shim"}, shimExitWait)
}

// waitGone polls until none of names is running, wait elapses, or the process
// table cannot be read. A nil probe ends the wait the same way an unreadable
// table does: it only ever shortens a best-effort wait, and the sweeps that
// follow it refuse the run if anything survived.
func waitGone(fx *hostEffects, names []string, wait time.Duration) {
	if fx.hostProcessRunning == nil {
		return
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		gone := true
		for _, name := range names {
			running, err := fx.hostProcessRunning(name)
			if err != nil {
				return
			}
			gone = gone && !running
		}
		if gone {
			return
		}
		time.Sleep(pollInterval)
	}
}
