// Package uninstall removes a stratum node's footprint: it stops the supervised
// components (via the controller's control socket, when one is running) and
// deletes the on-disk layout created by `stratum up`.
// Everything lives under /var/lib/stratum plus a handful of /etc drop-ins, so
// the deletion list is short by construction. Destructive: this wipes etcd
// data and all local cluster state, so Run requires an explicit confirmation.
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

// units are stopped in reverse start order (kubelet first, etcd last), derived
// from the canonical comps.Units roster.
var units = func() []string {
	u := slices.Clone(comps.Units)
	slices.Reverse(u)
	return u
}()

// The wait durations around the top-level process stop. Production code only
// reads these, so two Runs in one process cannot race over them; tests shorten
// them, which is why they stay variables rather than constants. Every probe
// the runner consults is a field on hostEffects, built in Run; there is no
// package-level probe to fall back to.
var (
	topLevelStopWait = 30 * time.Second
	topLevelKillWait = 5 * time.Second
)

// startLockHeld is etcddata.StartLockHeld, shared with the restore path.
func startLockHeld() (bool, error) { return etcddata.StartLockHeld() }

// hostEffects is one teardown run's complete set of host effects, probes,
// discovered state and layout. The runner consults nothing else, so a run
// cannot reach the host through a path the value does not carry, and a test
// that builds a zero value fails closed on the unset-effect sentinel instead
// of reaching the node. The probes are the read-only seams the preflight and
// the teardown consult (the supervisor's state, the process table, the static
// pods); the discovery fields are what this run has learned about the node's
// network namespaces, seeded fresh at the runner's entry so a second Run never
// inherits a first one's answer; the layout fields are where the node keeps
// the state the teardown reads and removes. All of it is per-run: Run builds
// one value per call, so two Runs in one process share nothing.
type hostEffects struct {
	acquireNodeLock     func() (func(), error)
	acquireEtcdDataLock func() (func(), error)
	removeAll           func(path string) error
	// removeEmptyDir removes a directory only when it is empty (rmdir, not
	// os.Remove, which also unlinks a file): the local-path provisioner's
	// volume directory goes through it, so a volume that arrives after the
	// emptiness check, or a file that replaces the directory, is kept.
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
	// untrustPodCIDRs removes the firewalld bindings stratum added, before the
	// delete loop removes the record it reads. Run wires host.UntrustPodCIDRs
	// as its default; tests stub the field on their effects value.
	untrustPodCIDRs func() error
	// removeSELinuxContexts deletes the SELinux file contexts stratum
	// registered, before the delete loop: the record of what stratum
	// registered lives under DataDir. Run wires selinux.RemoveContexts as its
	// default.
	removeSELinuxContexts func() error
	unmountStateFn        func() error
	unmountFn             func(mp string) error
	putManifestFn         func(name string, manifest []byte) error
	stopStaticPodFn       func(name string) error
	startUnitFn           func(name string) error
	startContainerdFn     func() error
	stopContainerdFn      func() error
	stopKubeletFn         func() error
	stopContainersFn      func() error
	waitHostExitFn        func()
	startAndWaitActive    func(name string, wait time.Duration) error
	ctrRun                func(args ...string) (string, error)
	runCmd                func(stdin, name string, args ...string) ([]byte, error)
	nodeMarkerFn          func() (func(host string, at time.Time) error, error)
	teardownDatapathFn    func() error
	stopIdleShimsFn       func()
	signalPID             func(pid int, sig syscall.Signal) error
	// removeLegacyHostConfig heals a host an older stratum broke, self-logging
	// its outcome through the run's writer: Run wires the runtime function
	// itself, which reports both outcomes to the writer it is given.
	removeLegacyHostConfig func(w io.Writer)

	// The read-only probes the preflight gates and the teardown steps consult.
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

	// What this run has learned about the node's network namespaces
	// (datapath.go), and the filesystem layout it reads it from. The runner
	// seeds the discovery fresh at its entry; the layout is as installed.
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

	// assumeNoForeignRuntimes is --assume-no-foreign-runtimes: the operator has
	// checked the host, so a run that cannot see the host's processes proceeds.
	assumeNoForeignRuntimes bool

	// cniKept is the run's decision for each CNI directory, made once by
	// cniKeepReason so the unmount phase and the delete loop agree.
	cniKept map[string]error
}

// cniKeepReason is why the CNI directory t stays, nil when it goes:
// noOtherRuntime, asked once per run. The unmount phase asks first, so a
// mount under a directory that stays is left mounted, and the delete loop
// keeps the same directory.
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

// errUnsetEffect marks an effect the caller never set. It is what a zero
// hostEffects answers with, so a test harness that forgets a stub sees the
// sentinel instead of the effect's real host behavior.
var errUnsetEffect = errors.New("unset host effect")

// unsetEffect is the fail-closed error for the named field: identifiable with
// errors.Is against errUnsetEffect, and naming the field so the harness can
// say which stub is missing.
func unsetEffect(name string) error {
	return fmt.Errorf("%w: hostEffects.%s is not set", errUnsetEffect, name)
}

// topLevelProc is a PID and its start time, which together identify one
// process — a PID alone can be reused between the SIGTERM below and the
// SIGKILL that can follow it.
type topLevelProc = shimProc

// stopTopLevelProcess finds the top-level `stratum up` process — the one
// holding the start lock, whether or not systemd ever started it — and
// stops it, waiting for it to actually exit. `systemctl stop` (stopSystemdUnit,
// above) only signals an ACTIVE unit; a unit that is loaded but was never
// started through systemd (a node started by hand, or one where systemd's
// bookkeeping and the real process have otherwise drifted apart) makes that
// call a no-op, leaving the process — and the start lock it holds — untouched.
// This finds it independently, through the same /proc scan already used to
// find containerd, kubelet, the operator and idle shims (internal/comps),
// rather than inventing a second way to identify a Stratum-managed process.
// There should be at most one match: the start lock enforces that only one
// `stratum up` runs at a time. Its signal is the run's signalPID effect.
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

// isProcGone reports whether err is startTime's answer to a pid that
// genuinely no longer exists (its /proc entry gone), as opposed to a read
// that simply failed for some other reason and proves nothing either way.
func isProcGone(err error) bool { return os.IsNotExist(err) }

// scanTopLevel snapshots the top-level `stratum up` process(es) currently
// running, pid and start time together, so a later PID reuse can be told
// apart from the same process still being there. A start-time read that
// fails for a reason other than the process having genuinely exited is
// returned as an error, never silently dropped: skipping a pid here on an
// unrelated failure (a permission error, an unreadable stat line) would read
// as "nothing to stop" and let uninstall delete state under a process this
// call simply could not check.
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

// stillTopLevel is the subset of procs that are still the same process (see
// stillProcs). It fails closed: a topLevelPIDs failure, or a start-time read
// failing for any reason other than the pid genuinely being gone, is returned
// as an error instead of an empty result, for the same reason as scanTopLevel
// above.
func stillTopLevel(fx *hostEffects, procs []topLevelProc) ([]topLevelProc, error) {
	if fx.topLevelPIDs == nil {
		return nil, unsetEffect("topLevelPIDs")
	}
	return stillProcs(fx.topLevelPIDs, fx, procs)
}

// signalTopLevel checks each process is still itself immediately before its
// own signal, the same as shims.go's signalShims: a signal to one can let a
// later PID be reused before that one is reached, and a PID a different
// process now holds must never receive a signal meant for the original one.
// A stillTopLevel failure aborts the whole call rather than signalling on an
// unconfirmed identity.
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

// waitTopLevelGone polls stillTopLevel(procs) until none of the originally
// found processes are still themselves, or wait elapses. A stillTopLevel
// failure returns immediately as an error: not being able to prove it is
// gone is not the same as it being gone, so this never resolves an
// unconfirmed case to "gone" by falling through the deadline instead.
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

// dirsAndFiles is the on-disk state to delete. version.DataDir covers the
// staged binaries, PKI, kubeconfigs, etcd data, manifests, logs — the whole
// point of the one-directory layout (the operator's helm cache among them,
// since it moved under the data dir as version.HelmCacheDir). The three roots
// are named through their version variables; every other entry is one of
// bring-up's outside-root paths (host.Footprint), named through the variable
// that declares it, so the delete list cannot drift from what bring-up
// writes. The coupling is pinned both ways: internal/host's
// TestUninstallTargetsCoverTheFootprint fails when a footprint path is missing
// here, and the footprint test fails when a path here is not bring-up's —
// with two named exceptions. host.KubeletDir is the one footprint path that is
// deliberately NOT a target: a plain RemoveAll here would delete the directory
// wholesale right after this run's own datapath teardown promised to keep
// whatever is not Stratum's kubelet's ("left /var/lib/kubelet in place"),
// destroying a co-hosted kubelet's state. removeLegacyKubeletDir
// (datapath.go) is the directory's only remover, stale sockets and
// Stratum-signed cert pairs included, and TestTargetsOutsideEveryRootAreListed
// carries the decision in its kept set.
// /var/lib/stratum-helm is the other exception: the operator helm cache's home
// on nodes brought up before it moved under the data dir. Nothing writes it
// anymore; version.LegacyHelmCacheDir declares it so the operator's migration
// and this delete target never drift apart. An uninstalled legacy node must
// not keep the operator's whole helm cache (the repository index cache,
// repositories.yaml and the written CA files).
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

// Targets returns every path uninstall will delete (used for the dry-run preview
// and unit-tested to guard against drift from the installer's layout): the
// three roots plus host.Footprint's outside-root paths, and the upgrade's
// binary backup sets beside version.StratumInstallPath, which are enumerated
// from the node as it stands.
func Targets() []string {
	return append(dirsAndFiles(), stratumBinaryBackups()...)
}

// stratumBinaryBackups returns the backup sets an upgrade wrote beside the
// installed stratum binary: each <install>.bak-<ts> file it saved before
// replacing the live binary, which stays there through every rollback
// (internal/upgrade keeps the newest sets beside version.StratumInstallPath).
// They sit outside the three roots, so they are targets of their own; the live
// binary and every neighbour that is not one of its .bak- sets are not
// uninstall's to remove. Enumerated at call time, so only sets that exist are
// named and the list follows version.StratumInstallPath wherever a test or a
// future release moves it. A directory that cannot be read contributes none:
// a leftover backup binary is not worth failing an uninstall over, and the
// three roots and the footprint — the spine of the delete list — never depend
// on this read.
func stratumBinaryBackups() []string {
	if !filepath.IsAbs(version.StratumInstallPath) {
		// A relative install path would put relative paths in the destructive
		// delete loop (TestTargetsAreAbsolute); treat it as no backups, as for
		// an unreadable directory.
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
		// The upgrade writes regular files with this exact UTC stamp. A
		// similarly named directory or malformed suffix is not its backup.
		const layout = "20060102-150405"
		tm, err := time.Parse(layout, stamp)
		if err == nil && tm.Format(layout) == stamp {
			baks = append(baks, filepath.Join(filepath.Dir(version.StratumInstallPath), e.Name()))
		}
	}
	return baks
}

// orphanRefusal returns a non-nil error when uninstall cannot rely on kubelet
// to reap live control-plane static pods. A serving supervisor socket alone is
// insufficient: kubelet may be deliberately stopped or restarting beneath it.
// Force-terminating orphaned processes is deliberately separate work.
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
		// Listener reachability alone is not liveness: Stop also waits for the
		// process to disappear, and kube-vip deliberately has no listener.
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
	// Do not issue a second control request after a failed supervisor probe.
	// A false kubelet status then only strengthens the same fail-closed refusal.
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

// activeHostProcesses returns every Stratum host process still present. The
// process table is authoritative when the supervisor has disappeared or after
// it has been asked to stop children; a control-socket status is only a
// snapshot and must not authorize state deletion.
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

// refuseActiveHostProcesses is an error, for Run's abort to wrap, while any
// Stratum host process is still present after the supervisor was asked to
// stop them.
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
// Without force it only prints what would be removed. The CLI passes os.Stdout,
// so its output is what it always printed; the MCP server passes the buffer its
// result is built from, so the same text reaches the result as data and never
// the process's stdout.
func Run(w io.Writer, force bool) error {
	return RunWithOptions(w, Options{Force: force})
}

// Options are uninstall's flags.
type Options struct {
	Force bool
	// AssumeNoForeignRuntimes skips the refusal of a run that cannot see the
	// host's processes; the operator has checked the host. Force does not
	// imply it.
	AssumeNoForeignRuntimes bool
}

// RunWithOptions is Run with every flag. It is the only place
// that builds a production hostEffects value: one per call, with every
// effect's real implementation and every probe's real answer wired inline, so
// a second Run never inherits the first run's value and no effect or probe
// stays behind as a package variable a forgotten stub could fall back to. The
// teardown sequence itself lives in the unexported runner, which consults the
// value and nothing else.
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

		// The layout paths the datapath teardown reads come from the same
		// declarations the preview names as fixed items (preview.go), so the
		// two cannot name different paths.
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
	// The steps that are themselves built from other effects close over fx: by
	// the time any of them runs, the value above is complete.
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

// runUninstall is the teardown sequence Run hands its effects value to, with
// the writer every preview, progress and warning print below it writes to.
// Every host effect it reaches is a field on fx: one that is not set stops the
// run with the unset-effect sentinel before that effect's host call is made, so
// a zero value can drive the preview, which consults no effects, but never the
// destructive path. The preview's own host reads — the legacy containerd
// config, the firewalld and SELinux probes — are the read-only PreviewProbes
// the shared Preview() consults (preview.go), so the dry run and the MCP
// uninstall_preview enumerate the same effects from one function's value.
// They are direct read-only calls on the ambient host, not effects, and
// report what they cannot read instead of failing.
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

	// What this invocation learned about the node's network namespaces is its
	// own: Run can be called again in a persistent process (the MCP server), and
	// a flag, ownership or error left by an earlier call — on this value or
	// another — would stand in for what this one has not read. Seeding the
	// per-run fields fresh here takes the place of the shared reset a
	// package-level discovery state would need. The saved record on disk is
	// kept: it is the part that is meant to outlive a call (datapath.go).
	fx.stratumNetns, fx.stratumNetnsErr, fx.netnsEnumerated = map[string]bool{}, nil, false

	// The teardown runs in four declared phases, each carrying its steps'
	// error class and rollback behavior: refuseUninstall's failures change
	// nothing, prepareTeardown's roll back through undo.abort, commitTeardown
	// makes the commit decision, and removeState's errors accumulate instead
	// of rolling back.
	release, unitWasEnabled, err := refuseUninstall(fx)
	if release != nil {
		// The locks are held for the whole teardown, not just the refusal phase.
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

// refuseUninstall is the refusal phase: the fail-closed gates the run must
// pass before it touches the node — the node maintenance lock, the etcd data
// lock, the start-in-progress probe, the orphan preflight and the enablement
// query. Its declared error class is refusal: a failure here stops the run
// with nothing stopped, disabled or deleted, so there is nothing to roll back.
// It returns the locks' release for the runner to defer over the whole
// teardown, since the locks outlive this phase, and the unit's enablement as
// found, which the preparation phase's undo record restores exactly. That
// query fails closed, before anything is touched, the same as orphanPreflight
// below: proceeding on a question this could not actually answer risks an
// abort later restoring the wrong enablement.
func refuseUninstall(fx *hostEffects) (release func(), unitWasEnabled bool, err error) {
	// The node maintenance lock first, then the data lock, the order rejoin and
	// restore take them in. An upgrade, rollback or certificate rotation holds
	// only the node lock, and an uninstall beside one would remove the PKI and
	// state it is changing while it carries on, holding its cluster leases.
	if fx.acquireNodeLock == nil {
		return nil, false, unsetEffect("acquireNodeLock")
	}
	releaseNode, err := fx.acquireNodeLock()
	if err != nil {
		return nil, false, err
	}
	// Join, restore, and rejoin hold this same lock while changing the etcd
	// directory. Take it before stopping services or removing version.DataDir.
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

	// Fail closed before any stop or delete. A supervisor socket is not enough:
	// the supervisor may be running while kubelet is stopped or in backoff, and
	// without kubelet there is no process to reap a removed static-pod manifest.
	if err := orphanPreflight(fx); err != nil {
		return release, false, err
	}

	// Captured before anything changes, so a later failure's abort can put
	// enablement back exactly where it found it — never force it either way.
	if fx.unitEnabledFn == nil {
		return release, false, unsetEffect("unitEnabledFn")
	}
	unitWasEnabled, err = fx.unitEnabledFn()
	if err != nil {
		return release, false, fmt.Errorf("uninstall: determine whether stratum.service is enabled: %w", err)
	}
	return release, unitWasEnabled, nil
}

// prepareTeardown is the reversible-preparation phase: the early systemd
// disable, the restart suppression, the static-pod and container stops, the
// host-process stop, the sweeps, the unmount, the pre-stop start-lock probe,
// the unit stop and the top-level process stop. Its declared rollback is
// undo.abort: everything it changes is still put-back-able, so every failure
// after the first change goes through the abort, which restores what was
// changed and deletes nothing. The early systemd disable is the phase's first
// step — preparation, not preflight: it is the run's first host change (the
// unit's boot enablement) — but when it fails nothing has changed that undo
// could put back, so it alone refuses outright instead of aborting.
func prepareTeardown(w io.Writer, fx *hostEffects, unitWasEnabled bool) error {
	// The abort must be able to restore the unit and stopped host processes
	// before preparation makes its first change. Manifest restoration is
	// checked below before removing each manifest that would need it.
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
	// Disable the reboot-persistence unit from starting at the NEXT boot —
	// the first step, before touching any running process or on-disk state.
	// A reboot that interrupts uninstall (or a failed rollback below) must
	// not bring the whole node back over state this function has already
	// deleted. This alone does nothing about the unit's Restart= while it
	// stays ACTIVE, which is what suppressRestartFn (right below) is for; and
	// stopping the unit outright, rather than just disabling it, would
	// SIGTERM the top-level `stratum up` process now, taking kubelet down
	// before the static-pod teardown below gets a chance to use it — see
	// stopSystemdUnit in the commit phase.
	if fx.disableSystemdUnit == nil {
		return unsetEffect("disableSystemdUnit")
	}
	if err := fx.disableSystemdUnit(); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}

	// No timers to stop: cert renewal and etcd snapshots are loops inside the
	// controller process, so stopping the components stops them too.
	//
	// Control-plane static pods first, while kubelet is still up to tear them
	// down (removing the manifest is the stop verb); then the host processes.
	//
	// Each manifest is read before it is removed, so an abort below can put the
	// control plane back rather than leave it stopped behind "nothing was
	// deleted".
	undo := &teardownUndo{manifests: map[string][]byte{}, unitWasEnabled: unitWasEnabled}

	// Disarm Restart= on the ACTIVE unit too: DisableUnit only
	// stops the unit from starting at the NEXT boot, so without this an
	// unrelated exit of `stratum up` during the teardown below would have
	// systemd relaunch it mid-teardown, racing the static-pod and
	// host-process removal this function is doing by hand.
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
		// A manifest that cannot be read could not be put back, so it is not
		// removed.
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
		// One snapshot, read with its error: a status that cannot be read is
		// no reason to believe kubelet stopped or that nothing needs starting
		// again after an abort.
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
	// Every failure from here until the first removal still goes through the
	// abort: the manifests are gone and the services stopped, and "Nothing was
	// deleted" alone would leave a control plane down.
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
	// Refuse to delete data underneath any static pod that is still running:
	// RemoveAll of open files "succeeds" on Linux, so wiping state below a live
	// process silently corrupts it. Running, not Active: a closed port is only a
	// shutdown in progress, and kube-vip has no port at all.
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
	// Stopping the host processes only signals children and a supervisor can disappear after the
	// preflight. Do not unmount kubelet volumes while it may still be running.
	if err := refuseActiveHostProcesses(fx); err != nil {
		return undo.abort(fx, err)
	}
	if fx.unmountStateFn == nil {
		return undo.abort(fx, unsetEffect("unmountStateFn"))
	}
	if err := fx.unmountStateFn(); err != nil {
		return undo.abort(fx, fmt.Errorf("%w; re-run `stratum uninstall --yes`, after a reboot if the mount stays", err))
	}
	// The unmount itself takes time, so make the authoritative sweep again
	// immediately before RemoveAll to close a process-restart race.
	if err := refuseActiveHostProcesses(fx); err != nil {
		return undo.abort(fx, err)
	}

	// One last chance to fail closed before anything below actually stops:
	// this probes the SAME start-lock query the gate below will rely on
	// after the top-level process is gone, while there is still a node here
	// for undo.abort to put back if the query itself cannot even be made
	// (permissions, a path that vanished). Its answer (held or not) is not
	// the point yet — `stratum up` is presumably still running at this point
	// in the normal case, so "held" here is expected, not a finding — only
	// whether the question can be asked at all.
	if _, err := fx.startInProgress(); err != nil {
		return undo.abort(fx, fmt.Errorf("the start lock cannot be queried: %w", err))
	}
	// Now safe to stop the reboot-persistence unit itself: the static pods are
	// down and every host process this uninstall manages is confirmed stopped,
	// so the top-level process's SIGTERM (host.StopUnit) has nothing left it
	// needs to keep alive for. Goes through the same abort as everything else
	// in this window: a stop failure here must still offer to
	// put the node back, not just say "nothing was deleted" over a control
	// plane it already stopped.
	if fx.stopSystemdUnit == nil {
		return undo.abort(fx, unsetEffect("stopSystemdUnit"))
	}
	// Stopping the unit also stops the supervisor that startUnitFn needs
	// for rollback. Check the independent stop's wiring while that rollback
	// is still reachable.
	if fx.stopTopLevelFn == nil {
		return undo.abort(fx, unsetEffect("stopTopLevelFn"))
	}
	if err := fx.stopSystemdUnit(); err != nil {
		return undo.abort(fx, fmt.Errorf("stop stratum.service: %w", err))
	}
	// systemctl stop only signals an ACTIVE unit, so a unit that is loaded but
	// was never started through systemd — a manually launched node, or one
	// whose systemd bookkeeping otherwise drifted from the real process —
	// leaves the top-level `stratum up` process (and the start lock it holds)
	// untouched above. This finds and stops it independently, whether or not
	// stopSystemdUnit already did the same job through systemd.
	if err := fx.stopTopLevelFn(); err != nil {
		return undo.abort(fx, fmt.Errorf("stop the stratum up process: %w", err))
	}
	return nil
}

// commitTeardown is the commit phase: the post-stop start-lock gate and the
// Restart= restore. Everything the run could find is already stopped by the
// preparation phase, so its declared class is refusal, returned directly
// rather than through undo.abort: there is nothing left running for the
// abort to restore, and the node is left stopped, its state undeleted. The
// Restart= restore is warning-only: the unit is already down, so a drop-in
// that could not be removed only matters to a same-boot `stratum up`, which
// strips it itself. Once this phase returns nil the teardown is committed:
// removeState accumulates its errors and nothing is put back.
func commitTeardown(w io.Writer, fx *hostEffects) error {
	// stopTopLevelFn's exe check can miss a `stratum up` running from a
	// different copy of the binary than this `stratum uninstall` — same
	// process, different file, so /proc/<pid>/exe never matches
	// os.Executable(). Widening that matcher risks matching an unrelated
	// program instead, so this checks the one fact only the real holder can
	// produce, rather than trying to recognize it by name: is the start lock
	// still held? A reboot leaves no orphans, but a still-running `stratum
	// up` this uninstall could not identify is not a reboot, and deleting
	// state under it would corrupt a live node, not a stopped one.
	//
	// The gate is the commit decision, and its refusal does NOT go through
	// undo.abort: the top-level process this uninstall could find is already
	// stopped, along with every static pod and host process it manages, so
	// there is nothing left running for abort to restore — it would only
	// tear back into re-enabling the unit and restarting kubelet/containerd
	// for a node this call cannot actually put back into the state it was in
	// before. Unlike the preparation phase, though, this still refuses to
	// proceed: it returns the refusal directly, leaving node state exactly
	// as it is (stopped, not deleted) instead of trying to reconstruct a
	// "running" account it cannot make true.
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
	// The unit is stopped for good now, so the Restart=no override
	// suppressRestartFn wrote no longer protects anything — remove it here,
	// on the success path, rather than leaving it for the next real reboot
	// (which clears /run on its own): without this, a `stratum up` run
	// before that reboot would install and start a fresh unit that inherits
	// Restart=no from a drop-in this uninstall never intended to outlive it.
	//
	// A failure here is a warning, not an abort: the unit
	// is already stopped and every static pod and host process this
	// uninstall manages is already confirmed down, so there is nothing left
	// running to restore — undo.abort would tear back into re-enabling the
	// unit and restarting kubelet/containerd for a node that was never
	// actually left in a bad state, just to work around a drop-in file that
	// could not be removed. The leftover file only matters to a same-boot
	// `stratum up` before the next real reboot clears /run on its own, and
	// InstallUnit always strips it there (see internal/host.InstallUnit),
	// so uninstall itself can just proceed to delete state.
	if fx.allowRestartFn == nil {
		return unsetEffect("allowRestartFn")
	}
	if err := fx.allowRestartFn(); err != nil {
		fmt.Fprintf(w, "  WARN: restore stratum.service's Restart=: %v (a later `stratum up` on this node clears it)\n", err)
	}
	return nil
}

// removeState is the commit-and-removal phase: the datapath teardown, the
// firewalld untrust, the SELinux removal, the delete loop, the legacy-config
// heal and the systemd reload, then the worker's Node mark. Its declared
// error class is accumulated: past the commit boundary nothing is rolled
// back, so each failure is kept apart, the rest of the teardown still runs,
// and the joined errors are reported at the end; only the heal, the
// credential read and the Node mark are warning-only, being best-effort or
// self-logging steps.
func removeState(w io.Writer, fx *hostEffects) error {
	// Once cleanup starts, it must attempt every independent removal and
	// retain their errors. Catch missing wiring before the first cleanup
	// mutation instead of returning halfway through deletion and skipping
	// the systemd reload or a worker's Node mark.
	if fx.teardownDatapathFn == nil {
		return unsetEffect("teardownDatapathFn")
	}
	if fx.untrustPodCIDRs == nil {
		return unsetEffect("untrustPodCIDRs")
	}
	if fx.removeSELinuxContexts == nil {
		return unsetEffect("removeSELinuxContexts")
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
	// A worker marks its own Node so the control plane's operator removes it
	// once the worker is gone. Only once its state is removed, and
	// whether that removal succeeded decides it, not the other cleanup: a mark
	// taken earlier could have the Node deleted by the operator while this
	// uninstall aborted or failed, and kubelet would come back to a new Node
	// without its labels, taints or cordon. The removal takes kubelet's kubeconfig and its
	// certificates with it, so the credential is read into memory first.
	// Best-effort: without the mark the notice says how.
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

	// Every container is stopped and kubelet is gone, so the datapath and
	// cgroup state they held can go. Before the delete loop. A failure
	// is reported at the end and the rest of the teardown still runs. It is kept
	// apart from firstErr (a state-removal failure) so that a worker whose state
	// was removed still marks its Node: a retry could not, as the credential the
	// mark needs is gone with the state.
	var datapathErr error
	if err := fx.teardownDatapathFn(); err != nil {
		datapathErr = fmt.Errorf("the node's network datapath and cgroups were not fully removed: %w", err)
	}
	var firstErr error
	// Before the delete loop: the record of what stratum added lives under
	// DataDir, which the loop removes.
	untrustErr := fx.untrustPodCIDRs()
	if untrustErr != nil {
		firstErr = errors.Join(firstErr, fmt.Errorf("%w; kept %s for the firewalld ownership record so a later uninstall can retry", untrustErr, version.DataDir))
	}
	var selinuxErr error
	if err := fx.removeSELinuxContexts(); err != nil {
		selinuxErr = fmt.Errorf("the SELinux file contexts stratum registered were not all removed: %w", err)
	}
	// Whether the node's state, DataDir (the node state file and kubelet's
	// credential), was deleted is what decides the Node mark below, apart from
	// whether the other cleanup succeeded.
	stateRemoved := false
	keepState := errors.Is(datapathErr, errKeepState)
	for _, t := range Targets() {
		if t == version.DataDir && keepState {
			// The saved record of the node's network namespaces could not be
			// read, so what is left to detach is not known (datapath.go). Keep
			// DataDir, and carry on with every other target.
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
			// The record lives under DataDir. Deleting it after a failed
			// untrust would make a later uninstall forget the leftover binding.
			// Continue tearing down every other target.
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
	// The local-path provisioner's volume directory goes only when it is empty:
	// what it can hold is users' local-path volume data, which an uninstall must
	// not delete. A missing directory is silent; a removal failure here is a
	// reported failure, joined to any earlier one: removeLocalPathDir prints
	// nothing for it, so the error is the only place the directory is named.
	if err := removeLocalPathDir(w, fx); err != nil {
		firstErr = errors.Join(firstErr, fmt.Errorf("some paths could not be removed: %w", err))
	}
	// Heal a host an older stratum broke by writing its own config over
	// /etc/containerd/config.toml: left in place it keeps the distro's containerd
	// (and Docker) crash-looping after a reboot. Only removes a file
	// stratum provably wrote; a host's own config is untouched. Self-logging.
	// Runs BEFORE the joined-error return below: it is an independent healing
	// step, so a failure to remove some other target must not skip it.
	fx.removeLegacyHostConfig(w)
	// The unit file above is now gone; drop it from systemd's view too. Folded
	// into firstErr, rather than a silent best-effort call:
	// left un-reloaded, systemd keeps serving the just-deleted unit's
	// last-loaded definition.
	if err := fx.reloadSystemd(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("reload systemd: %w", err)
	}
	// A worker marks its Node once its state is gone, whatever else failed:
	// the credential the mark needs went with the state, so a retry could not.
	// With the state kept, or its removal failed, it does not: a retry still
	// can, and a mark now could have the Node deleted under a node still there.
	notice := unmarkedNotice
	if mark != nil && stateRemoved {
		if err := mark(); err != nil {
			fmt.Fprintf(w, "  WARN: could not mark Node %s for removal by the control plane: %v\n", host, err)
		} else {
			fmt.Fprintf(w, "  marked Node %s for removal by the control plane once this worker is gone\n", host)
			notice = markedNotice
		}
	}
	if err := errors.Join(firstErr, datapathErr, selinuxErr); err != nil {
		if !stateRemoved {
			return fmt.Errorf("uninstall: %w", err)
		}
		// Not "uninstalled", but what is left to do in the cluster is still said.
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

// isCNIDir is whether t is the CNI config directory, which another runtime on
// the host may use too.
func isCNIDir(t string) bool { return t == host.CNIDir }

// teardownUndo is what Run has changed before it could fail: the control-plane
// manifests it removed, whether it stopped kubelet or started containerd,
// whether it began stopping containers, and whether it asked the supervisor to
// stop every component, with the ones it kept up beforehand. Nothing has been
// deleted at that point, so an abort puts back what it can and says exactly
// what it did.
type teardownUndo struct {
	manifests         map[string][]byte
	wanted            []string
	kubeletStopped    bool
	containerdStarted bool
	containersStopped bool
	hostStopped       bool
	// unitWasEnabled is the reboot-persistence unit's enablement as found
	// BEFORE disableSystemdUnit touched it — abort restores exactly this,
	// never forcing the unit enabled or disabled from what an operator
	// already had.
	unitWasEnabled bool
}

// The undo path's restoration is read from fx the same way the runner reads
// it: its effects (putManifestFn, stopStaticPodFn, startUnitFn,
// startContainerdFn, stopContainerdFn) are fields on hostEffects, wired in
// Run, and no probe it consults is a package variable.

// abort restores what Run changed and wraps cause with an account of the node
// as it is left, built from what was actually done. Its own restoration is a
// set of effects too, read from fx the same way the runner reads them.
func (u *teardownUndo) abort(fx *hostEffects, cause error) error {
	var failed, notes []string

	// Restore the unit exactly as this uninstall found it, first: a node left
	// running (which every path below is trying to arrange) is worth nothing
	// if it can no longer come back after a reboot.
	// AllowRestart undoes suppressRestartFn unconditionally — it ran
	// regardless of unitWasEnabled — while re-enabling only when the unit
	// was actually enabled to begin with.
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
	// containerd started only for the teardown is stopped, and waited on,
	// before anything is started again, on every path: kubelet started beside
	// it could recreate the pods just stopped, and the supervisor's own stop,
	// when that was asked for, only signals. kubelet is left
	// stopped if it cannot be.
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
	// After the supervisor was asked to stop everything, start again what it
	// kept up before, containerd first; before that, only kubelet was stopped,
	// and containerd if it was started for the teardown. A containerd it kept
	// up before (one in restart backoff) is started again only now, after the
	// one started for the teardown is gone.
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
		// `stratum up` itself refuses while the supervisor runs, which it does
		// on every path that reaches here.
		return fmt.Errorf("uninstall: %w. %s. The node could not be put back as it was (%s): "+
			"restart `stratum up` on this node (stop it, then start it again) to restore it", cause, outcome, strings.Join(failed, "; "))
	}
	return fmt.Errorf("uninstall: %w. %s", cause, outcome)
}

// nodeNotice is what is left to do in the cluster once this node is gone.
// A worker cannot delete its own Node object: its credential is
// system:node:<name>, which the Node authorizer does not allow to delete
// nodes (measured: "cannot delete resource nodes"). A marked worker's Node is
// removed by the operator, and the command is still printed for a
// cluster that runs none. A control plane's Node, and its etcd member, are
// removed from a remaining control plane.
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

// unmountState lazy-unmounts every mount point below the directories uninstall
// is about to remove, deepest first: kubelet's pod volumes (projected tokens,
// secrets, emptyDirs) and any container rootfs or shm mount left behind. After
// kubelet stops they linger, and RemoveAll fails on them with EBUSY.
// Stratum's kubelet never uses /var/lib/kubelet (its
// --root-dir is under version.DataDir), so matching that path unmounts
// nothing and a node that has run pods cannot have its state removed. The
// removed directories themselves are never unmounted: a dedicated disk at
// version.DataDir stays mounted so removeTarget empties it, rather than
// RemoveAll deleting an empty mount point and leaving the data on the disk.
//
// Every mount must be gone before anything is removed, so mountinfo is read
// again afterwards: RemoveAll does not stop at a mount boundary, and deleting
// through a mount it could not detach would delete what is mounted there, a
// persistent volume or the host directory a subPath bind-mounts.
func unmountState(fx *hostEffects) error {
	if fx.unmountFn == nil {
		return unsetEffect("unmountFn")
	}
	return detachMounts(fx.unmountFn, func() ([]string, error) { return stateMounts(fx) }, "below the state directories")
}

// detachMounts lazy-unmounts every mount list returns, then lists again: any
// left is an error, naming the first and where (a phrase after "mounts"). The
// unmount itself is an effect of the run, passed in from its effects value.
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

// umountMount lazy-unmounts one mount point, bounded like every tool call. It
// is Run's production default for the effects value's unmount effect.
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

// removeTarget removes t, or, when t is itself a mount point (a dedicated disk
// at version.DataDir, or a volume in a container), removes everything in it
// and leaves the directory: rmdir on a mount point is EBUSY, and unmounting it
// first would let RemoveAll delete an empty mount point while the data stayed
// on the disk.
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

// mountPoints is every mount point in the run's mountinfo, escapes decoded. Not
// being able to read it is an error, never "no mounts": that answer is what lets
// RemoveAll into a mounted filesystem.
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

// stateMounts is every mount point strictly below one of Targets(), deepest
// first, each once. A CNI directory that stays (cniKeepReason) contributes
// none: another runtime's configuration may be mounted there.
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

// unescapeMount decodes mountinfo's octal escapes (\040 is a space).
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
