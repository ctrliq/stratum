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

// Read-only after init, so two Runs cannot race; tests shorten them, hence variables.
var (
	kubeletStopWait = 60 * time.Second
	containerGrace  = 30 * time.Second
	ctrTimeout      = 2 * time.Minute

	containerdStartWait = 60 * time.Second
	hostExitWait        = 60 * time.Second
	shimExitWait        = 30 * time.Second
	pollInterval        = 500 * time.Millisecond
)

// Stops kubelet so it cannot recreate the containers stopped next.
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

// A running container keeps its shim alive past containerd, so an uninstall could never finish.
func stopPods(w io.Writer, fx *hostEffects, undo *teardownUndo, status map[string]supervisor.ProcStatus) error {
	if fx.hostProcessRunning == nil {
		return unsetEffect("hostProcessRunning")
	}
	// A backoff kubelet returns mid-teardown; stop it before containerd starts, or it recreates pods.
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
		// A refused uninstall leaves shims holding containers; a restarted containerd reattaches to them.
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

// The supervisor reports containerd active before its socket answers, so the start waits for ctr.
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

// The stop only signals, so the process is waited out; a failed ask is no failure once it is gone.
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

// Kubelet's credential may annotate its own Node; held in memory because the state dir goes first.
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

// Bounded: a wedge would hang a half-torn-down node; only Stratum's containerd, never a host one.
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

var errCtrTimeout = errors.New("no answer from containerd")

type task struct {
	ns, id string
	live   bool
}

// Removing the pod sandboxes ends most shims; deleting tasks alone does not.
func stopContainers(w io.Writer, fx *hostEffects) error {
	if fx.ctrRun == nil {
		return unsetEffect("ctrRun")
	}
	out, err := fx.ctrRun("namespaces", "ls", "-q")
	if err != nil {
		return fmt.Errorf("list containerd namespaces: %w", err)
	}
	namespaces := strings.Fields(out)
	// Recorded before anything is removed: the netns specs go with the containers they describe.
	if err := recordStratumNetns(fx, namespaces); err != nil {
		return err
	}
	if err := stopTasks(w, fx, namespaces); err != nil {
		return err
	}
	return removeSandboxes(w, fx, namespaces)
}

// A STOPPED task has no process to signal, and signalling it is an error, so it is only deleted.
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
	// A forced delete of a task whose process never got the signal leaves it running unsupervised.
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

// Sends sig to every task; reports those it could not signal for any reason but the task gone.
func signalTasks(fx *hostEffects, tasks []task, sig string) error {
	var failed []string
	for _, t := range tasks {
		// Only NotFound: a shim dropping its connection mid-kill may leave the container running.
		if _, err := fx.ctrRun("-n", t.ns, "tasks", "kill", "--all", "--signal", sig, t.id); err != nil && !taskNotFound(err) {
			failed = append(failed, fmt.Sprintf("%s/%s: %v", t.ns, t.id, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not send %s to %d container tasks, so none was deleted: %s", sig, len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// `ctr sandboxes rm` exits 0 even when it fails to remove a sandbox, so each list is rechecked.
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

// `ctr sandboxes ls` has no quiet flag.
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

// Only STOPPED counts as stopped; PAUSED and UNKNOWN may still own a process.
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

// "ttrpc: closed" is the shim exiting, the asked outcome; "no such file or directory" is not gone.
func taskGone(err error) bool {
	return taskNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "ttrpc: closed")
}

func taskNotFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// The stop only signals, so containerd is waited out; shims exit once their sandbox is removed.
func waitHostExit(fx *hostEffects) {
	waitGone(fx, units, hostExitWait)
	waitGone(fx, []string{"runtime-shim"}, shimExitWait)
}

// Only a best-effort wait; the sweeps that follow refuse the run if anything survived.
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
