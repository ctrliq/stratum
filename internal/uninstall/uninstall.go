// Package uninstall removes a stratum node's footprint.
package uninstall

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ctrliq/stratum/internal/apparmor"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// stopped in reverse start order: kubelet first, etcd last.
var units = func() []string {
	u := slices.Clone(comps.Units)
	slices.Reverse(u)
	return u
}()

// production only reads these: two Runs in one process cannot race. Tests shorten them.
var (
	topLevelStopWait = 30 * time.Second
	topLevelKillWait = 5 * time.Second
)

func startLockHeld() (bool, error) { return etcddata.StartLockHeld() }

// one teardown run's effects, probes, discovered state and layout; a zero value fails closed.
type hostEffects struct {
	acquireNodeLock     func() (func(), error)
	acquireEtcdDataLock func() (func(), error)
	removeAll           func(path string) error
	// rmdir, not os.Remove, which would also unlink a file that replaced the directory.
	removeEmptyDir     func(path string) error
	stopHostProcesses  func() error
	disableSystemdUnit func() error
	stopSystemdUnit    func() error
	stopSupervisedUnit func(name string) error
	stopTopLevelFn     func() error
	reloadSystemd      func() error
	enableSystemdUnit  func() error
	suppressRestartFn  func() error
	allowRestartFn     func() error
	// before the delete loop, which removes the ownership record this reads.
	untrustPodCIDRs func() error
	restoreAppArmor func() error
	// before the delete loop: its record of what stratum registered lives under DataDir.
	removeSELinuxContexts  func() error
	unmountStateFn         func() error
	unmountFn              func(mp string) error
	putManifestFn          func(name string, manifest []byte) error
	stopStaticPodFn        func(name string) error
	startUnitFn            func(name string) error
	startContainerdFn      func() error
	stopContainerdFn       func() error
	stopKubeletFn          func() error
	stopContainersFn       func() error
	waitHostExitFn         func()
	startAndWaitActive     func(name string, wait time.Duration) error
	ctrRun                 func(args ...string) (string, error)
	runCmd                 func(stdin, name string, args ...string) ([]byte, error)
	nodeMarkerFn           func() (func(host string, at time.Time) error, error)
	teardownDatapathFn     func() error
	stopIdleShimsFn        func()
	signalPID              func(pid int, sig syscall.Signal) error
	removeLegacyHostConfig func(w io.Writer)

	supervisorRunning  func() bool
	unitActive         func(name string) bool
	supervisorStatus   func() (map[string]supervisor.ProcStatus, error)
	hostProcessRunning func(name string) (bool, error)
	staticPodActive    func(name string) bool
	staticPodRunning   func(name string) (bool, error)
	unitEnabledFn      func() (bool, error)
	startInProgress    func() (bool, error)
	topLevelPIDs       func() ([]int, error)
	staticPodExists    func(name string) bool
	readManifestFn     func(name string) ([]byte, error)
	foreignRuntimesFn  func() (foreign, bystanders []string, err error)
	pidNamespaceFn     func() (string, error)
	loadNodeState      func() (*nodestate.NodeState, error)
	shimPIDs           func() ([]int, error)

	stratumNetns    map[string]bool
	stratumNetnsErr error
	netnsEnumerated bool

	netnsRecordPath    string
	containerdStateDir string
	netnsDir           string
	ciliumRunDir       string
	bpfRoot            string
	netClassDir        string
	cgroupRoot         string
	legacyKubeletDir   string
	localPathDir       string
	mountInfoPath      string
	procRoot           string

	// the operator has checked the host, so a run that cannot see its processes proceeds.
	assumeNoForeignRuntimes bool

	// the per-directory keep decision, made once so the unmount phase and the delete loop agree.
	cniKept map[string]error
}

func (fx *hostEffects) cniKeepReason(t string) error {
	if err, ok := fx.cniKept[t]; ok {
		return err
	}
	if fx.cniKept == nil {
		fx.cniKept = map[string]error{}
	}
	err := noOtherRuntime(fx, t)
	fx.cniKept[t] = err
	return err
}

// what a zero hostEffects answers with, so a test that forgets a stub fails closed.
var errUnsetEffect = errors.New("unset host effect")

func unsetEffect(name string) error {
	return fmt.Errorf("%w: hostEffects.%s is not set", errUnsetEffect, name)
}

// PID plus start time: a PID alone can be reused between SIGTERM and the SIGKILL that can follow.
type topLevelProc = shimProc

// systemctl stop only signals an ACTIVE unit; a hand-started node's process needs this /proc scan.
func stopTopLevelProcess(fx *hostEffects) error {
	if fx.signalPID == nil {
		return unsetEffect("signalPID")
	}
	procs, err := scanTopLevel(fx)
	if err != nil {
		return fmt.Errorf("find the stratum up process: %w", err)
	}
	if len(procs) == 0 {
		return nil
	}
	if err := signalTopLevel(fx, procs, syscall.SIGTERM); err != nil {
		return fmt.Errorf("check the stratum up process before signalling it: %w", err)
	}
	gone, err := waitTopLevelGone(fx, procs, topLevelStopWait)
	if err != nil {
		return fmt.Errorf("check the stratum up process stopped: %w", err)
	}
	if gone {
		return nil
	}
	if err := signalTopLevel(fx, procs, syscall.SIGKILL); err != nil {
		return fmt.Errorf("check the stratum up process before sending SIGKILL: %w", err)
	}
	gone, err = waitTopLevelGone(fx, procs, topLevelKillWait)
	if err != nil {
		return fmt.Errorf("check the stratum up process stopped after SIGKILL: %w", err)
	}
	if gone {
		return nil
	}
	return fmt.Errorf("stratum up still running after SIGTERM (%s) and SIGKILL (%s)", topLevelStopWait, topLevelKillWait)
}

func isProcGone(err error) bool { return os.IsNotExist(err) }

// a start-time read failure other than exit is an error: skipping it would read as nothing to stop.
func scanTopLevel(fx *hostEffects) ([]topLevelProc, error) {
	if fx.topLevelPIDs == nil {
		return nil, unsetEffect("topLevelPIDs")
	}
	pids, err := fx.topLevelPIDs()
	if err != nil {
		return nil, err
	}
	procs := make([]topLevelProc, 0, len(pids))
	for _, pid := range pids {
		start, err := startTime(fx, pid)
		if err != nil {
			if isProcGone(err) {
				continue // confirmed exit, between the scan above and this read
			}
			return nil, fmt.Errorf("read start time for pid %d: %w", pid, err)
		}
		procs = append(procs, topLevelProc{pid, start})
	}
	return procs, nil
}

func stillTopLevel(fx *hostEffects, procs []topLevelProc) ([]topLevelProc, error) {
	if fx.topLevelPIDs == nil {
		return nil, unsetEffect("topLevelPIDs")
	}
	return stillProcs(fx.topLevelPIDs, fx, procs)
}

// recheck identity immediately before each signal: a PID can be reused mid-sequence.
func signalTopLevel(fx *hostEffects, procs []topLevelProc, sig syscall.Signal) error {
	for _, p := range procs {
		still, err := stillTopLevel(fx, []topLevelProc{p})
		if err != nil {
			return err
		}
		if len(still) == 1 {
			_ = fx.signalPID(p.pid, sig)
		}
	}
	return nil
}

// a probe failure is an error, never resolved to gone at the deadline.
func waitTopLevelGone(fx *hostEffects, procs []topLevelProc, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	for {
		left, err := stillTopLevel(fx, procs)
		if err != nil {
			return false, err
		}
		if len(left) == 0 {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(pollInterval)
	}
}

// pinned against bring-up's footprint by tests; the kubelet dir's exception is theirs.
func dirsAndFiles() []string {
	return append([]string{
		version.DataDir,
		version.ConfDir,
		version.RunDir,
		host.CNIDir,
		version.CertsDir,
		version.ContainerdConfD,
		host.SysctlConf,
		host.SysctlCISConf,
		host.ModulesLoadFile,
		version.AuditLogDir,
		version.SystemdUnitPath,
		version.LegacyHelmCacheDir,
	}, host.ProfileSysctlFiles()...)
}

// Targets returns every path uninstall will delete.
func Targets() []string {
	return append(dirsAndFiles(), stratumBinaryBackups()...)
}

// only the existing .bak- sets beside the install path; a live binary is not uninstall's to remove.
func stratumBinaryBackups() []string {
	if !filepath.IsAbs(version.StratumInstallPath) {
		// a relative path would put relative paths in the destructive delete loop
		return nil
	}
	entries, err := os.ReadDir(filepath.Dir(version.StratumInstallPath))
	if err != nil {
		return nil
	}
	prefix := filepath.Base(version.StratumInstallPath) + ".bak-"
	var baks []string
	for _, e := range entries {
		stamp, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		// the upgrade writes regular files with this exact UTC stamp
		const layout = "20060102-150405"
		tm, err := time.Parse(layout, stamp)
		if err == nil && tm.Format(layout) == stamp {
			baks = append(baks, filepath.Join(filepath.Dir(version.StratumInstallPath), e.Name()))
		}
	}
	return baks
}

// a supervisor socket alone is not enough: kubelet may be stopped beneath it.
func orphanRefusal(supervisorUp, kubeletUp bool, activeControlPlane, activeHostProcesses []string) error {
	if supervisorUp {
		if kubeletUp || len(activeControlPlane) == 0 {
			return nil
		}
	} else if len(activeControlPlane) == 0 && len(activeHostProcesses) == 0 {
		return nil
	}
	if !supervisorUp {
		if len(activeHostProcesses) > 0 && len(activeControlPlane) > 0 {
			return fmt.Errorf("uninstall: supervisor unavailable with surviving control-plane processes (%s) and host/runtime processes (%s). Do not start another `stratum up` while host processes survive; recover or stop those processes first, then restore the node before retrying uninstall. Nothing was changed", processList(activeControlPlane), processList(activeHostProcesses))
		}
		if len(activeControlPlane) > 0 {
			return fmt.Errorf("uninstall: the stratum supervisor is not running, but control-plane processes are still up (%s). "+
				"start `stratum up`, wait for the node to become Ready so kubelet can reap the static pods, then re-run `stratum uninstall --yes`. Nothing was changed",
				strings.Join(activeControlPlane, ", "))
		}
		return fmt.Errorf("uninstall: the stratum supervisor is not running, but Stratum processes are still up (control plane: %s; host: %s). "+
			"uninstall cannot stop or safely delete state beneath orphaned processes — repair or stop the remaining host processes, then re-run `stratum uninstall --yes`. Nothing was changed",
			processList(activeControlPlane), processList(activeHostProcesses))
	}
	return fmt.Errorf("uninstall: the stratum supervisor or kubelet from `stratum up` is not running, but the control plane is still up (%s). "+
		"do not start a second `stratum up`: restore the kubelet through the existing supervisor, wait for the node to become Ready, then re-run `stratum uninstall --yes`. Nothing was changed",
		strings.Join(activeControlPlane, ", "))
}

func processList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func activeControlPlane(fx *hostEffects) ([]string, error) {
	if fx.staticPodRunning == nil {
		return nil, unsetEffect("staticPodRunning")
	}
	if fx.staticPodActive == nil {
		return nil, unsetEffect("staticPodActive")
	}
	var active []string
	for _, name := range staticpod.Names {
		// a closed port is only a shutdown in progress, and kube-vip has no port at all.
		running, err := fx.staticPodRunning(name)
		if err != nil {
			return nil, fmt.Errorf("uninstall: cannot determine whether static pod %q is still running: %w. Refusing to delete state. Nothing was changed", name, err)
		}
		if fx.staticPodActive(name) || running {
			active = append(active, name)
		}
	}
	return active, nil
}

func orphanPreflight(fx *hostEffects) error {
	if fx.supervisorRunning == nil {
		return unsetEffect("supervisorRunning")
	}
	if fx.unitActive == nil {
		return unsetEffect("unitActive")
	}
	supervisorUp := fx.supervisorRunning()
	activeControlPlane, err := activeControlPlane(fx)
	if err != nil {
		return err
	}
	// a failed supervisor probe already refuses; a second control request adds nothing.
	kubeletUp := supervisorUp && fx.unitActive("kubelet")
	var activeHost []string
	if !supervisorUp {
		activeHost, err = activeHostProcesses(fx)
		if err != nil {
			return fmt.Errorf("uninstall: %w. Refusing to delete state. Nothing was changed", err)
		}
	}
	return orphanRefusal(supervisorUp, kubeletUp, activeControlPlane, activeHost)
}

// the process table is authoritative: a socket status is only a snapshot, not deletion authority.
func activeHostProcesses(fx *hostEffects) ([]string, error) {
	if fx.hostProcessRunning == nil {
		return nil, unsetEffect("hostProcessRunning")
	}
	var active []string
	for _, name := range append(slices.Clone(units), "runtime-shim") {
		running, err := fx.hostProcessRunning(name)
		if err != nil {
			return nil, fmt.Errorf("cannot determine whether Stratum host process %q is still running: %w", name, err)
		}
		if running {
			active = append(active, name)
		}
	}
	return active, nil
}

func refuseActiveHostProcesses(fx *hostEffects) error {
	activeHost, err := activeHostProcesses(fx)
	if err != nil {
		return err
	}
	if len(activeHost) > 0 {
		return fmt.Errorf("Stratum host processes are still running after the supervisor was asked to stop them (%s); "+
			"re-run `stratum uninstall --yes` once they exit. A runtime-shim that never exits belongs to a container "+
			"this node's containerd could not stop, and rebooting the node clears it", strings.Join(activeHost, ", "))
	}
	return nil
}

// Run tears the node down, writing its preview, progress and warnings to w.
func Run(w io.Writer, force bool) error {
	return RunWithOptions(w, Options{Force: force})
}

// Options are uninstall's flags.
type Options struct {
	Force bool
	// the operator has checked the host; Force does not imply it.
	AssumeNoForeignRuntimes bool
}

// the only place a production hostEffects is built: one per call, nothing shared between runs.
func RunWithOptions(w io.Writer, o Options) error {
	force := o.Force
	fx := &hostEffects{
		acquireNodeLock:        func() (func(), error) { return nodelock.Acquire("uninstall") },
		acquireEtcdDataLock:    func() (func(), error) { return etcddata.Lock(version.EtcdDataDir) },
		removeAll:              os.RemoveAll,
		removeEmptyDir:         syscall.Rmdir,
		stopHostProcesses:      comps.StopAllIfPresent,
		disableSystemdUnit:     host.DisableUnit,
		stopSystemdUnit:        host.StopUnit,
		stopSupervisedUnit:     comps.Stop,
		reloadSystemd:          host.ReloadSystemd,
		enableSystemdUnit:      host.EnableUnit,
		suppressRestartFn:      host.SuppressRestart,
		allowRestartFn:         host.AllowRestart,
		untrustPodCIDRs:        func() error { return host.UntrustPodCIDRsWithWriter(w) },
		removeSELinuxContexts:  func() error { return selinux.RemoveContextsWithWriter(w) },
		restoreAppArmor:        func() error { return apparmor.RestoreWithWriter(w) },
		unmountFn:              umountMount,
		putManifestFn:          staticpod.Put,
		stopStaticPodFn:        func(name string) error { return staticpod.Stop(name, 90*time.Second) },
		startUnitFn:            comps.EnableNow,
		startAndWaitActive:     comps.StartAndWaitActive,
		ctrRun:                 runCtr,
		runCmd:                 execCmd,
		nodeMarkerFn:           kubeletNodeMarker,
		signalPID:              func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		removeLegacyHostConfig: runtime.RemoveLegacyHostConfig,

		supervisorRunning:  comps.SupervisorRunning,
		unitActive:         comps.IsActive,
		supervisorStatus:   comps.Status,
		hostProcessRunning: comps.ProcessRunning,
		staticPodActive:    staticpod.Active,
		staticPodRunning:   staticpod.Running,
		unitEnabledFn:      host.UnitEnabled,
		startInProgress:    startLockHeld,
		topLevelPIDs:       func() ([]int, error) { return comps.ProcessPIDs("stratum") },
		staticPodExists:    staticpod.Exists,
		readManifestFn:     staticpod.Read,
		loadNodeState:      nodestate.LoadState,
		shimPIDs:           func() ([]int, error) { return comps.ProcessPIDs("runtime-shim") },

		// the same declarations the preview names as fixed items, so the two cannot name different paths.
		netnsDir:         netnsDir,
		ciliumRunDir:     ciliumRunDir,
		bpfRoot:          "/sys/fs/bpf",
		netClassDir:      "/sys/class/net",
		cgroupRoot:       cgroupRoot,
		legacyKubeletDir: legacyKubeletDir,
		localPathDir:     selinux.LocalPathDir,
		mountInfoPath:    "/proc/self/mountinfo",
		procRoot:         "/proc",

		assumeNoForeignRuntimes: o.AssumeNoForeignRuntimes,

		containerdStateDir: version.ContainerdRootDir,
		netnsRecordPath:    "/run/stratum-uninstall-netns.json",
	}
	// close over fx only after the literal above is complete.
	fx.stopTopLevelFn = func() error { return stopTopLevelProcess(fx) }
	fx.startContainerdFn = func() error { return startContainerd(fx) }
	fx.stopContainersFn = func() error { return stopContainers(w, fx) }
	fx.stopIdleShimsFn = func() { stopIdleShims(w, fx) }
	fx.unmountStateFn = func() error { return unmountState(fx) }
	fx.teardownDatapathFn = func() error { return teardownDatapath(w, fx) }
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return foreignRuntimes(fx) }
	fx.pidNamespaceFn = func() (string, error) { return pidNamespace(fx) }
	fx.stopKubeletFn = func() error { return stopKubelet(fx) }
	fx.stopContainerdFn = func() error { return stopContainerd(fx) }
	fx.waitHostExitFn = func() { waitHostExit(fx) }
	return runUninstall(w, force, fx)
}

// an unset effect stops the run before its host call; a zero value can only drive the preview.
func runUninstall(w io.Writer, force bool, fx *hostEffects) error {
	if !force {
		fmt.Fprintln(w, "stratum uninstall will STOP all stratum services and irreversibly DELETE:")
		report := Preview()
		for _, t := range report.Targets {
			fmt.Fprintln(w, "  - "+t)
		}
		for _, it := range report.Items {
			fmt.Fprintln(w, renderPreviewItem(it))
		}
		fmt.Fprintln(w, "\nIt also stops every container Stratum's containerd runs on this node (SIGTERM, then SIGKILL")
		fmt.Fprintln(w, "after 30s), and then that containerd.")
		fmt.Fprintln(w, "Drain the node from a control plane first so its pods move gracefully:")
		fmt.Fprintln(w, "  kubectl drain <node> --ignore-daemonsets --delete-emptydir-data")
		fmt.Fprintln(w, "A worker's Node object is then removed by the operator on the control plane about 2 minutes")
		fmt.Fprintln(w, "after the worker is gone; without an operator, remove it from a control plane. A control")
		fmt.Fprintln(w, "plane's Node is never removed for you: remove it, and its etcd member, from a remaining one.")
		fmt.Fprintln(w, "\netcd data and all local cluster state will be lost.")
		fmt.Fprintln(w, "Re-run with --yes to proceed. (A host containerd or Docker, if installed, is not touched.)")
		return nil
	}

	// Run can be called again in a persistent process, so discovery is seeded fresh per run.
	fx.stratumNetns, fx.stratumNetnsErr, fx.netnsEnumerated = map[string]bool{}, nil, false

	release, unitWasEnabled, err := refuseUninstall(fx)
	if release != nil {
		// the locks are held for the whole teardown, not just the refusal phase.
		defer release()
	}
	if err != nil {
		return err
	}
	if err := prepareTeardown(w, fx, unitWasEnabled); err != nil {
		return err
	}
	if err := commitTeardown(w, fx); err != nil {
		return err
	}
	return removeState(w, fx)
}

// returns the locks' release for the runner to defer over the whole teardown.
func refuseUninstall(fx *hostEffects) (release func(), unitWasEnabled bool, err error) {
	// the node lock first: an upgrade or rotation holds only it, and a parallel uninstall races it.
	if fx.acquireNodeLock == nil {
		return nil, false, unsetEffect("acquireNodeLock")
	}
	releaseNode, err := fx.acquireNodeLock()
	if err != nil {
		return nil, false, err
	}
	// join, restore and rejoin hold this lock while changing the etcd directory.
	if fx.acquireEtcdDataLock == nil {
		releaseNode()
		return nil, false, unsetEffect("acquireEtcdDataLock")
	}
	releaseData, err := fx.acquireEtcdDataLock()
	if err != nil {
		releaseNode()
		return nil, false, fmt.Errorf("uninstall: %w", err)
	}
	release = func() { releaseData(); releaseNode() }
	if fx.supervisorRunning == nil {
		return release, false, unsetEffect("supervisorRunning")
	}
	if fx.startInProgress == nil {
		return release, false, unsetEffect("startInProgress")
	}
	if err := etcddata.RefuseStartInProgress("uninstall", "deleting its data", fx.supervisorRunning, fx.startInProgress); err != nil {
		return release, false, err
	}

	if err := hostPIDNamespaceGate(fx); err != nil {
		return release, false, err
	}

	// fail closed before any stop or delete.
	if err := orphanPreflight(fx); err != nil {
		return release, false, err
	}

	// captured before anything changes, so an abort restores enablement exactly as found.
	if fx.unitEnabledFn == nil {
		return release, false, unsetEffect("unitEnabledFn")
	}
	unitWasEnabled, err = fx.unitEnabledFn()
	if err != nil {
		return release, false, fmt.Errorf("uninstall: determine whether stratum.service is enabled: %w", err)
	}
	return release, unitWasEnabled, nil
}

// the early systemd disable is preparation, not preflight: its failure refuses, nothing to undo.
func prepareTeardown(w io.Writer, fx *hostEffects, unitWasEnabled bool) error {
	if fx.allowRestartFn == nil {
		return unsetEffect("allowRestartFn")
	}
	if unitWasEnabled && fx.enableSystemdUnit == nil {
		return unsetEffect("enableSystemdUnit")
	}
	if fx.startUnitFn == nil {
		return unsetEffect("startUnitFn")
	}
	if fx.stopContainerdFn == nil {
		return unsetEffect("stopContainerdFn")
	}
	// disable first: a reboot mid-uninstall must not bring the node back over deleted state.
	if fx.disableSystemdUnit == nil {
		return unsetEffect("disableSystemdUnit")
	}
	if err := fx.disableSystemdUnit(); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}

	// no timers: cert renewal and etcd snapshots are loops inside the controller process.

	// static pods first, while kubelet is still up to reap them; then the host processes.

	// each manifest is read before it is removed, so an abort can put it back.
	undo := &teardownUndo{manifests: map[string][]byte{}, unitWasEnabled: unitWasEnabled}

	// DisableUnit only stops the next boot; without this systemd relaunches the unit mid-teardown.
	if fx.suppressRestartFn == nil {
		return undo.abort(fx, unsetEffect("suppressRestartFn"))
	}
	if err := fx.suppressRestartFn(); err != nil {
		return undo.abort(fx, fmt.Errorf("suppress the unit's Restart= during teardown: %w", err))
	}
	if fx.staticPodExists == nil {
		return undo.abort(fx, unsetEffect("staticPodExists"))
	}
	for _, name := range staticpod.Names {
		if !fx.staticPodExists(name) {
			continue
		}
		// a manifest that cannot be read could not be put back, so it is not removed.
		if fx.readManifestFn == nil {
			return undo.abort(fx, unsetEffect("readManifestFn"))
		}
		m, err := fx.readManifestFn(name)
		if err != nil {
			return undo.abort(fx, fmt.Errorf("read the %s manifest: %w", name, err))
		}
		if m != nil {
			if fx.putManifestFn == nil {
				return undo.abort(fx, unsetEffect("putManifestFn"))
			}
			undo.manifests[name] = m
		}
		if fx.stopStaticPodFn == nil {
			return undo.abort(fx, unsetEffect("stopStaticPodFn"))
		}
		if err := fx.stopStaticPodFn(name); err != nil {
			if name == "etcd" {
				return undo.abort(fx, fmt.Errorf("stop %s: %w", name, err))
			}
			fmt.Fprintf(w, "  WARN: stop %s: %v\n", name, err)
		}
	}
	if fx.supervisorRunning() {
		// a status that cannot be read is no reason to believe kubelet stopped.
		if fx.supervisorStatus == nil {
			return undo.abort(fx, unsetEffect("supervisorStatus"))
		}
		status, err := fx.supervisorStatus()
		if err != nil {
			return undo.abort(fx, fmt.Errorf("read the supervisor's status: %w", err))
		}
		for _, name := range comps.Units {
			if status[name].Wanted {
				undo.wanted = append(undo.wanted, name)
			}
		}
		if err := stopPods(w, fx, undo, status); err != nil {
			return undo.abort(fx, err)
		}
	}
	// failures from here still abort: the manifests are gone and the services stopped.
	if fx.stopHostProcesses == nil {
		return undo.abort(fx, unsetEffect("stopHostProcesses"))
	}
	undo.hostStopped = true
	if err := fx.stopHostProcesses(); err != nil {
		return undo.abort(fx, fmt.Errorf("disable host processes: %w", err))
	}
	if fx.waitHostExitFn == nil {
		return undo.abort(fx, unsetEffect("waitHostExitFn"))
	}
	fx.waitHostExitFn()
	if fx.stopIdleShimsFn == nil {
		return undo.abort(fx, unsetEffect("stopIdleShimsFn"))
	}
	fx.stopIdleShimsFn()
	// RemoveAll under a live process silently corrupts it: unlinking open files "succeeds".
	if fx.staticPodRunning == nil {
		return undo.abort(fx, unsetEffect("staticPodRunning"))
	}
	for _, name := range staticpod.Names {
		running, err := fx.staticPodRunning(name)
		if err != nil {
			return undo.abort(fx, fmt.Errorf("cannot determine whether static pod %s is still running: %w", name, err))
		}
		if running {
			return undo.abort(fx, fmt.Errorf("static pod %s is still running after its manifest was removed; stop `stratum up` on this node, then re-run", name))
		}
	}
	// the stop only signals; do not unmount kubelet volumes while it may still run.
	if err := refuseActiveHostProcesses(fx); err != nil {
		return undo.abort(fx, err)
	}
	if fx.unmountStateFn == nil {
		return undo.abort(fx, unsetEffect("unmountStateFn"))
	}
	if err := fx.unmountStateFn(); err != nil {
		return undo.abort(fx, fmt.Errorf("%w; re-run `stratum uninstall --yes`, after a reboot if the mount stays", err))
	}
	// sweep again right before RemoveAll to close a process-restart race.
	if err := refuseActiveHostProcesses(fx); err != nil {
		return undo.abort(fx, err)
	}

	// probe the query's askability while undo is still possible; the answer is checked at commit.
	if _, err := fx.startInProgress(); err != nil {
		return undo.abort(fx, fmt.Errorf("the start lock cannot be queried: %w", err))
	}
	// static pods down, host processes stopped: the unit's SIGTERM has nothing to keep alive.
	if fx.stopSystemdUnit == nil {
		return undo.abort(fx, unsetEffect("stopSystemdUnit"))
	}
	// check the stop's wiring while rollback is still reachable.
	if fx.stopTopLevelFn == nil {
		return undo.abort(fx, unsetEffect("stopTopLevelFn"))
	}
	if err := fx.stopSystemdUnit(); err != nil {
		return undo.abort(fx, fmt.Errorf("stop stratum.service: %w", err))
	}
	if err := fx.stopTopLevelFn(); err != nil {
		return undo.abort(fx, fmt.Errorf("stop the stratum up process: %w", err))
	}
	return nil
}

// nothing is left running, so the refusal returns directly instead of through undo.abort.
func commitTeardown(w io.Writer, fx *hostEffects) error {
	// the exe check can miss a stratum up running from a different copy of the binary; the lock cannot.
	if held, err := fx.startInProgress(); err != nil {
		return fmt.Errorf("uninstall: determine whether the start lock is still held: %w. Node state was not "+
			"deleted; the stratum up process this uninstall could find is already stopped, so there is nothing "+
			"to restore. Stop any stratum up still running on this node, then re-run `stratum uninstall --yes`", err)
	} else if held {
		return fmt.Errorf("uninstall: the start lock is still held even though stopping stratum.service and " +
			"every stratum up process this uninstall could identify both succeeded — a running `stratum up` may " +
			"be a different copy of the binary than this `stratum uninstall`, which the process check cannot " +
			"recognize. Node state was not deleted; the process this uninstall found is already stopped, so " +
			"there is nothing to restore. Stop the running `stratum up` on this node, then re-run " +
			"`stratum uninstall --yes`")
	}
	// a same-boot `stratum up` would inherit the Restart=no drop-in; the next reboot clears /run.

	// a failure here is a warning: the unit is already down, nothing is left to restore.
	if fx.allowRestartFn == nil {
		return unsetEffect("allowRestartFn")
	}
	if err := fx.allowRestartFn(); err != nil {
		fmt.Fprintf(w, "  WARN: restore stratum.service's Restart=: %v (a later `stratum up` on this node clears it)\n", err)
	}
	return nil
}

// past the commit boundary nothing rolls back: every error is kept and the teardown continues.
func removeState(w io.Writer, fx *hostEffects) error {
	// check all wiring before the first mutation, so no step is skipped halfway through deletion.
	if fx.teardownDatapathFn == nil {
		return unsetEffect("teardownDatapathFn")
	}
	if fx.untrustPodCIDRs == nil {
		return unsetEffect("untrustPodCIDRs")
	}
	if fx.removeSELinuxContexts == nil {
		return unsetEffect("removeSELinuxContexts")
	}
	if fx.restoreAppArmor == nil {
		return unsetEffect("restoreAppArmor")
	}
	if fx.removeAll == nil {
		return unsetEffect("removeAll")
	}
	if fx.localPathDir != "" && fx.removeEmptyDir == nil {
		return unsetEffect("removeEmptyDir")
	}
	if fx.removeLegacyHostConfig == nil {
		return unsetEffect("removeLegacyHostConfig")
	}
	if fx.reloadSystemd == nil {
		return unsetEffect("reloadSystemd")
	}
	// removal destroys the credential, so read it first; mark after it: an early mark loses the Node.
	var mark func() error
	var host string
	if fx.loadNodeState == nil {
		return unsetEffect("loadNodeState")
	}
	if st, err := fx.loadNodeState(); err == nil && st.Role == "worker" && st.Hostname != "" {
		host = st.Hostname
		if fx.nodeMarkerFn == nil {
			return unsetEffect("nodeMarkerFn")
		}
		if m, err := fx.nodeMarkerFn(); err != nil {
			fmt.Fprintf(w, "  WARN: could not read kubelet's credential to mark Node %s for removal: %v\n", host, err)
		} else {
			mark = func() error { return m(host, time.Now()) }
		}
	}
	// Both read before the state they come from is removed.
	markedNotice, unmarkedNotice := nodeNotice(fx, true), nodeNotice(fx, false)

	// kept apart from firstErr so a worker whose state was removed still marks its Node.
	var datapathErr error
	if err := fx.teardownDatapathFn(); err != nil {
		datapathErr = fmt.Errorf("the node's network datapath and cgroups were not fully removed: %w", err)
	}
	var firstErr error
	// the ownership record lives under DataDir, which the loop removes.
	untrustErr := fx.untrustPodCIDRs()
	if untrustErr != nil {
		firstErr = errors.Join(firstErr, fmt.Errorf("%w; kept %s for the firewalld ownership record so a later uninstall can retry", untrustErr, version.DataDir))
	}
	var selinuxErr error
	if err := fx.removeSELinuxContexts(); err != nil {
		selinuxErr = fmt.Errorf("the SELinux file contexts stratum registered were not all removed: %w", err)
	}
	var apparmorErr error
	if err := fx.restoreAppArmor(); err != nil {
		apparmorErr = fmt.Errorf("the host's runc AppArmor profile was not restored: %w", err)
	}
	stateRemoved := false
	keepState := errors.Is(datapathErr, errKeepState)
	for _, t := range Targets() {
		if t == version.DataDir && keepState {
			// the netns record is unreadable, so what is left to detach is unknown: keep DataDir.
			continue
		}
		if isCNIDir(t) {
			if err := fx.cniKeepReason(t); err != nil {
				fmt.Fprintf(w, "  %v\n", err)
				firstErr = errors.Join(firstErr, err)
				continue
			}
		}
		if t == version.DataDir && untrustErr != nil {
			// deleting the record after a failed untrust would hide the leftover binding from a later run.
			continue
		}
		// os.RemoveAll is a no-op on missing paths, so this is idempotent.
		err := removeTarget(w, fx, t)
		if t == version.DataDir && err == nil {
			stateRemoved = true
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("some paths could not be removed: %w", err)
		}
	}
	// users' local-path volume data: the directory goes only when it is empty.
	if err := removeLocalPathDir(w, fx); err != nil {
		firstErr = errors.Join(firstErr, fmt.Errorf("some paths could not be removed: %w", err))
	}
	// left in place, the old config keeps the distro's containerd crash-looping after a reboot.

	// an independent step: a target-removal failure must not skip it.
	fx.removeLegacyHostConfig(w)
	// left un-reloaded, systemd keeps serving the just-deleted unit's definition.
	if err := fx.reloadSystemd(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("reload systemd: %w", err)
	}
	notice := unmarkedNotice
	if mark != nil && stateRemoved {
		if err := mark(); err != nil {
			fmt.Fprintf(w, "  WARN: could not mark Node %s for removal by the control plane: %v\n", host, err)
		} else {
			fmt.Fprintf(w, "  marked Node %s for removal by the control plane once this worker is gone\n", host)
			notice = markedNotice
		}
	}
	if err := errors.Join(firstErr, datapathErr, selinuxErr, apparmorErr); err != nil {
		if !stateRemoved {
			return fmt.Errorf("uninstall: %w", err)
		}
		if notice != "" {
			fmt.Fprintln(w, notice)
		}
		return fmt.Errorf("uninstall: %w. The node's state was removed; fix the reported cleanup failures and re-run `stratum uninstall --yes` to finish", err)
	}
	fmt.Fprintln(w, "stratum uninstalled.")
	if notice != "" {
		fmt.Fprintln(w, notice)
	}
	return nil
}

func isCNIDir(t string) bool { return t == host.CNIDir }

// what Run has changed before it could fail: nothing is deleted yet, so abort restores what it can.
type teardownUndo struct {
	manifests         map[string][]byte
	wanted            []string
	kubeletStopped    bool
	containerdStarted bool
	containersStopped bool
	hostStopped       bool
	// as found; abort restores exactly this, never forcing enablement either way.
	unitWasEnabled bool
}

// restores through the same effect fields the runner reads.
func (u *teardownUndo) abort(fx *hostEffects, cause error) error {
	var failed, notes []string

	// a recovered node is worth nothing if it cannot come back after a reboot.

	// AllowRestart ran unconditionally, so undo it unconditionally; re-enable only as found.
	if fx.allowRestartFn == nil {
		return unsetEffect("allowRestartFn")
	}
	if err := fx.allowRestartFn(); err != nil {
		failed = append(failed, fmt.Sprintf("restore stratum.service's Restart=: %v", err))
	}
	if u.unitWasEnabled {
		if fx.enableSystemdUnit == nil {
			return unsetEffect("enableSystemdUnit")
		}
		if err := fx.enableSystemdUnit(); err != nil {
			failed = append(failed, fmt.Sprintf("re-enable stratum.service: %v", err))
		} else {
			notes = append(notes, "stratum.service was re-enabled for the next reboot")
		}
	}

	put := 0
	for _, name := range staticpod.Names {
		m, ok := u.manifests[name]
		if !ok {
			continue
		}
		if fx.putManifestFn == nil {
			return unsetEffect("putManifestFn")
		}
		if err := fx.putManifestFn(name, m); err != nil {
			failed = append(failed, fmt.Sprintf("put back the %s manifest: %v", name, err))
			continue
		}
		put++
	}
	// kubelet started beside a live containerd would recreate the pods just stopped.
	kubeletBack, containerdBack, kubeletSafe := false, false, true
	if u.containerdStarted {
		if fx.stopContainerdFn == nil {
			return unsetEffect("stopContainerdFn")
		}
		if err := fx.stopContainerdFn(); err != nil {
			failed = append(failed, fmt.Sprintf("stop containerd again: %v", err))
			kubeletSafe = false
		} else {
			containerdBack = true
		}
	}
	// containerd kept up before is restarted only after the teardown's own is gone.
	var restart []string
	for _, name := range comps.Units {
		wanted := slices.Contains(u.wanted, name)
		if (u.hostStopped && wanted) || (name == "kubelet" && u.kubeletStopped) || (name == "containerd" && u.containerdStarted && wanted) {
			restart = append(restart, name)
		}
	}
	var started []string
	for _, name := range restart {
		if name == "kubelet" && !kubeletSafe {
			failed = append(failed, "kubelet left stopped, since containerd started for the teardown could not be stopped")
			continue
		}
		if fx.startUnitFn == nil {
			return unsetEffect("startUnitFn")
		}
		if err := fx.startUnitFn(name); err != nil {
			failed = append(failed, fmt.Sprintf("start %s again: %v", name, err))
			continue
		}
		started = append(started, name)
	}
	kubeletBack = slices.Contains(started, "kubelet")
	if !u.hostStopped {
		started = nil
	}
	if u.containersStopped {
		if kubeletBack {
			notes = append(notes, "this node's containers may have been stopped, and kubelet recreates its pods")
		} else {
			notes = append(notes, "this node's containers may have been stopped, and nothing recreates them until kubelet runs again")
		}
	}
	if put > 0 {
		notes = append(notes, "the control-plane manifests were put back")
	}
	if len(started) > 0 {
		notes = append(notes, "this node's services were started again ("+strings.Join(started, ", ")+")")
	} else if kubeletBack {
		notes = append(notes, "kubelet was started again")
	}
	if containerdBack {
		notes = append(notes, "containerd, started to stop them, was stopped again")
	}
	if len(notes) == 0 && len(failed) == 0 {
		return fmt.Errorf("uninstall: %w. Nothing was changed", cause)
	}
	outcome := "Nothing was deleted"
	if len(notes) > 0 {
		outcome += "; " + strings.Join(notes, "; ")
	}
	if len(failed) > 0 {
		// `stratum up` itself refuses while the supervisor runs, as it does on every path here.
		return fmt.Errorf("uninstall: %w. %s. The node could not be put back as it was (%s): "+
			"restart `stratum up` on this node (stop it, then start it again) to restore it", cause, outcome, strings.Join(failed, "; "))
	}
	return fmt.Errorf("uninstall: %w. %s", cause, outcome)
}

// a worker's credential cannot delete Nodes; the removal falls to the operator or a control plane.
func nodeNotice(fx *hostEffects, marked bool) string {
	st, err := fx.loadNodeState()
	if err != nil || st.Hostname == "" {
		return ""
	}
	if st.Role == "worker" && marked {
		return fmt.Sprintf("This worker's Node object is removed by the control plane once the worker has been gone for\n"+
			"2 minutes. On a cluster that runs no operator, remove it from a control plane:\n"+
			"  kubectl delete node %s", st.Hostname)
	}
	if st.Role == "worker" {
		return fmt.Sprintf("This worker's Node object stays in the cluster. Remove it from a control plane:\n"+
			"  kubectl delete node %s", st.Hostname)
	}
	return fmt.Sprintf("If this control plane was part of an HA cluster, remove its etcd member and its Node from a\n"+
		"remaining control plane:\n  stratum etcdctl member remove <member-id>   (see: stratum etcdctl member list)\n"+
		"  kubectl delete node %s", st.Hostname)
}

// lingering pod and container mounts: RemoveAll would delete through them and fail with EBUSY.

// the removed directories are never unmounted, so removeTarget can empty a dedicated disk there.
func unmountState(fx *hostEffects) error {
	if fx.unmountFn == nil {
		return unsetEffect("unmountFn")
	}
	return detachMounts(fx.unmountFn, func() ([]string, error) { return stateMounts(fx) }, "below the state directories")
}

func detachMounts(unmount func(string) error, list func() ([]string, error), where string) error {
	mounts, err := list()
	if err != nil {
		return err
	}
	failed := map[string]error{}
	for _, mp := range mounts {
		if err := unmount(mp); err != nil {
			failed[mp] = err
		}
	}
	left, err := list()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		reason := ""
		if err := failed[left[0]]; err != nil {
			reason = ": " + err.Error()
		}
		return fmt.Errorf("%d mounts %s could not be unmounted, the first %s%s", len(left), where, left[0], reason)
	}
	return nil
}

func umountMount(mp string) error {
	ctx, cancel := context.WithTimeout(context.Background(), unmountWait)
	defer cancel()
	if out, err := toolCmd(ctx, "umount", "-l", mp).CombinedOutput(); err != nil {
		err = fmt.Errorf("umount -l: %w", err)
		if o := strings.TrimSpace(string(out)); o != "" {
			err = fmt.Errorf("%w: %s", err, o)
		}
		return err
	}
	return nil
}

var unmountWait = 30 * time.Second

// a mount point cannot be removed: empty it and leave the directory, or the data stays on the disk.
func removeTarget(w io.Writer, fx *hostEffects, t string) error {
	if fx.removeAll == nil {
		return unsetEffect("removeAll")
	}
	mounts, err := mountPoints(fx)
	if err != nil {
		return err
	}
	if !slices.Contains(mounts, t) {
		return fx.removeAll(t)
	}
	entries, err := os.ReadDir(t)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := fx.removeAll(filepath.Join(t, e.Name())); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "  emptied %s; it is a mount point, so the directory itself was left in place\n", t)
	return nil
}

// an unreadable mountinfo is an error: "no mounts" would let RemoveAll into a mounted filesystem.
func mountPoints(fx *hostEffects) ([]string, error) {
	b, err := os.ReadFile(fx.mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read the mount table: %w", err)
	}
	var mounts []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 5 {
			mounts = append(mounts, unescapeMount(f[4]))
		}
	}
	return mounts, nil
}

// a CNI directory that stays contributes none: another runtime's config may be mounted there.
func stateMounts(fx *hostEffects) ([]string, error) {
	all, err := mountPoints(fx)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, t := range Targets() {
		if isCNIDir(t) && fx.cniKeepReason(t) != nil {
			continue
		}
		targets = append(targets, t)
	}
	var mounts []string
	for _, mp := range all {
		for _, t := range targets {
			if strings.HasPrefix(mp, strings.TrimSuffix(t, "/")+"/") {
				mounts = append(mounts, mp)
				break
			}
		}
	}
	slices.SortFunc(mounts, func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/")), strings.Compare(a, b))
	})
	return slices.Compact(mounts), nil
}

// mountinfo escapes spaces and friends as octal (\040).
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
