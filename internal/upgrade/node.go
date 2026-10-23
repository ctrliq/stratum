package upgrade

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/certinputs"
	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profilefiles"
	"github.com/ctrliq/stratum/internal/rtcheck"
	kuberuntime "github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// NodeOptions configures a local per-node upgrade.
type NodeOptions struct {
	Target       string // e.g. "v1.36.2"; defaults to version.K8sVersion
	From         string // optional path to this executing binary; "" => its own payload; CLI hands off to a different --from first
	SoakSec      int    // node-level soak before the final health gate (default 15)
	SkipSnapshot bool   // skip the pre-upgrade etcd snapshot on a controller
	// Check reports, and changes nothing, whether this node already runs the
	// upgrade binary's version and build, under a supervisor running the
	// installed stratum or one an earlier handoff found it cannot replace
	// (unreplaceableSupervisor): nil if it does, ErrUpgradeNeeded if not. The
	// cluster orchestrator asks it before draining a node.
	Check bool
	// ImportImages imports the upgrade binary's payload images into this
	// node's containerd and changes nothing else. The cluster orchestrator
	// runs it on every node before it upgrades the first one: that control
	// plane's add-on manifests roll out cluster-wide at once, and a node still
	// on the previous release must already hold their images.
	ImportImages bool
	// ExpectBuild and ExpectSHA256 pin a cluster upgrade's node to the upgrade
	// binary the orchestrator staged. Each node runs its own copy at
	// --from's path, so a leftover or different binary there would otherwise
	// answer --check, or upgrade the node, as itself. ExpectBuild is the build
	// ID, which holds across architectures; ExpectSHA256 is the exact bytes,
	// set only for nodes of the orchestrator's own architecture.
	ExpectBuild  string
	ExpectSHA256 string
	DryRun       bool
}

// RollbackOptions configures a local per-node rollback.
type RollbackOptions struct {
	SoakSec      int
	EtcdSnapshot string // if set, also restore etcd from this snapshot (single-CP only)
	DryRun       bool
}

// stratumInstallPath is where the installed stratum binary lives — the
// documented install location (quick-start) and the operator process's
// fallback path. A var so tests can redirect it.
var stratumInstallPath = version.StratumInstallPath

// The guard's probes, as vars so tests can redirect them (same pattern as
// stratumInstallPath).
var (
	supervisorRunning = comps.SupervisorRunning
	unitActive        = comps.IsActiveContext
	unitExists        = comps.UnitExists
	restartUnit       = comps.Restart
	reloadUnit        = comps.Reload
	// stampNode is how an upgraded controller re-asserts its version
	// annotations. The supervisor's own kubelet-start hook cannot be relied on
	// for it: the supervisor that survives `stratum upgrade` is the OLD
	// process, whose callbacks predate that hook.
	stampNode = k8sutil.StampNodeContext
)

// stampWindow bounds stampUpgraded's kubectl. It runs inside the per-node Job,
// ahead of the backstop uncordon, so a request the API server never answers
// must not hold the Job there. A var so tests can shorten it.
var stampWindow = 30 * time.Second

// applyHostConfigs' node-config writer and the legacy-config heal, indirected for
// tests: the order of reload and heal matters. writeNodeConfigs wraps the
// runtime render and drops its change flag: the render restarts nothing now,
// and the upgrade folds a changed config into its own reload below — the
// reload of containerd here is the one restart an upgrade gives it, so the
// reload runs whether or not the render changed anything (the binaries were
// swapped either way).
var (
	writeNodeConfigs = func(w io.Writer, st *nodestate.NodeState) error {
		_, err := kuberuntime.WriteNodeConfigsWithWriter(w, st)
		return err
	}
	removeLegacyConfig  = kuberuntime.RemoveLegacyHostConfig
	legacyConfigPresent = kuberuntime.LegacyHostConfig
)

// RollbackNode's etcd step, indirected for tests: its node IP, the member
// guard, and the restore.
var (
	rollbackNodeIP      = nodestate.NodeIP
	rollbackMemberIPs   = ha.MemberClientIPs
	rollbackEtcdRestore = etcdsnap.Restore
)

// waitHealthzCtx is k8sutil.WaitHealthzContext, indirected for tests. Every
// apiserver wait in this package runs under a deadline, so a kubectl that never
// answers is killed with it instead of holding a gate open.
var waitHealthzCtx = k8sutil.WaitHealthzContext

// The apiserver /healthz budgets of the upgrade gates. Vars so tests can
// shorten them. The component window keeps the 2s-apart attempt count it
// replaced (60 tries, 2m). The node gate's window is the larger because the
// node gate follows a containerd+kubelet restart: that bounce can take the
// local API server down entirely, so the gate must cover a restarted kubelet
// re-creating the apiserver static pod plus a cold apiserver start on a
// loaded node — at least as disruptive as the single component restart the
// 2m component window was sized for. It matches apiRetryWindow, which
// retryAPI sizes for the same disruption (its comment calls that window
// the per-node health gate's).
var (
	componentHealthzWindow = 2 * time.Minute
	nodeGateHealthzWindow  = apiRetryWindow
)

// UpgradeNode's other slow or host-bound steps, indirected so a test can run
// an upgrade to completion.
var (
	resolvePayload       = ResolveWithWriter
	importImages         = cluster.ImportImagesWithWriter
	waitStaticPodReady   = staticpod.WaitActive
	componentPIDs        = staticpod.PIDs
	restartStaticPod     = staticpod.Restart
	bootClock            = staticpod.Now
	waitComponentReplace = staticpod.WaitReplaced
	refreshCerts         = refreshControlPlaneCerts
	writeKubeVipConfig   = kubeconfig.WriteKubeVip
	writeSchedulerConfig = kubeconfig.RefreshScheduler
	soak                 = time.Sleep
)

// rollbackGateWindow bounds how long a rollback's node gate waits for the host
// processes it restarted to report active. A var so tests can shorten it.
var rollbackGateWindow = 60 * time.Second

// ensureSupervisor refuses unless a stratum supervisor answers. Bootstrap's
// staticpod.WriteAll re-renders the same manifests upgrade and rollback write;
// run concurrently, last write wins with no error either way (observed in CI:
// check-upgrade). RunController finishes WriteAll before it starts the
// supervisor, and the control socket exists only inside it, so a supervisor
// that answers has already done that write.
func ensureSupervisor(cmd string) error {
	if !supervisorRunning() {
		return fmt.Errorf("stratum %s: no stratum supervisor is running on this node — start it with `stratum up` first; nothing was changed", cmd)
	}
	return nil
}

// The command lines the handover restarts `stratum up` with, indirected for
// tests: the reboot-persistence unit's, and the running supervisor's with the
// directory it runs in.
var (
	unitArgv       = host.UnitArgv
	supervisorArgv = runningSupervisorArgv
)

// runningSupervisorArgv reads the supervisor's argv and working directory from
// /proc.
func runningSupervisorArgv() ([]string, string, error) {
	cur, err := runningSupervisor()
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cur.PID))
	if err != nil {
		return nil, "", err
	}
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", cur.PID))
	if err != nil {
		return nil, "", err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), cwd, nil
}

// restartConfigMissing refuses to upgrade a control plane whose handover would
// restart `stratum up` on a --config the upgraded build refuses: a file that
// no longer exists, an empty value, or a file that build refuses to load. The handover stops every component and
// then restarts `stratum up` — a supervisor re-execs itself with the --config
// it was started with (cluster.UpArgv), one that predates that is restarted
// through the unit, and systemd restarts the unit after either — so the
// upgrade would end with the node down and a unit that refuses on every
// restart, and with no supervisor left, `stratum rollback` refuses too.
//
// Both command lines are checked, because they can differ: every start
// rewrites the unit, but a start whose unit install failed only warns. A unit
// that exists but cannot be read is refused rather than skipped: an operator's
// drop-in that replaces ExecStart= is what host.UnitArgv cannot read, and a
// supervisor that predates re-exec is restarted through systemd onto that
// command, which nothing here has checked. The node state's record is the
// fallback when neither command line is there to read: it is best-effort (a
// start that cannot take the data lock does not save it), so it can be empty
// or name an earlier start's file, and builds before it was added never wrote
// it.
//
// The default path may be absent, so its absence is never refused, but a
// default file that is there is loaded like any other. Refused for --check
// and --dry-run as well: a --check that answered would have the orchestrator
// drain a node it then cannot upgrade.
//
// RollbackNode hands over the same way and is not guarded: in a supported
// rollout the build a rollback restores predates this one, and every build
// before it starts on a missing --config rather than refusing it. Once a
// rollback can restore a build that also refuses, it needs the same check.
func restartConfigMissing(st *nodestate.NodeState) error {
	if st.Role != "controller" {
		return nil
	}
	type source struct{ path, dir, what string }
	var sources []source
	argv, err := unitArgv()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No unit: nothing restarts this node through systemd.
	case err != nil:
		// A drop-in's ExecStart= outlives the unit `stratum up` rewrites, so
		// only removing it helps; a hand-edited ExecStart= in the unit itself
		// is rewritten by the next start.
		return fmt.Errorf("stratum upgrade: cannot tell which --config this control plane's unit restarts it with "+
			"(%v), and the upgraded build refuses to start on a missing one. `systemctl cat stratum` shows the unit "+
			"and its drop-ins: remove a drop-in's ExecStart= override and run `sudo systemctl daemon-reload`; a "+
			"hand-edited ExecStart= in the unit itself is rewritten by pointing the node at its config: %s",
			err, restartConfigHint)
	default:
		path, given := configFlag(argv)
		// The supervisor's re-exec maps an empty --config to the default
		// (cluster.RunController); a restart through the unit passes it on.
		// Refused even when the supervisor would re-exec: the unit is not
		// rewritten until the re-exec'd start reaches its unit install, and a
		// start that dies before then is restarted from this unit, refused,
		// with no supervisor left.
		if given && path == "" {
			return fmt.Errorf("stratum upgrade: this control plane's unit restarts it with an empty --config, which the "+
				"upgraded build refuses to start on. To point it at its config: %s", restartConfigHint)
		}
		// systemd starts the service in /.
		sources = append(sources, source{path, "/", "the unit's ExecStart"})
	}
	if argv, cwd, err := supervisorArgv(); err == nil {
		path, _ := configFlag(argv)
		sources = append(sources, source{path, cwd, "the running supervisor"})
	}
	if len(sources) == 0 {
		sources = append(sources, source{st.ConfigPath, "/", "the node state"})
	}
	for _, s := range sources {
		// No --config, or an empty one a re-exec maps to it, is the default.
		path := s.path
		if path == "" {
			path = cluster.DefaultConfigPath
		}
		// A relative path is one an older build was given and passed on.
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.dir, path)
		}
		path = filepath.Clean(path)
		if path != cluster.DefaultConfigPath {
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("stratum upgrade: this control plane restarts with --config %s (from %s), which no longer "+
					"exists, and the upgraded build refuses to start on a missing --config. Restore the file, or point the "+
					"node at the intended one: %s", path, s.what, restartConfigHint)
			}
		}
		// A file the earlier build started on can hold a value this build
		// refuses at load, and the default path is checked here too: most
		// control planes run on it.
		if err := restartConfigLoads(path); err != nil {
			return fmt.Errorf("stratum upgrade: this control plane restarts with --config %s (from %s), which the "+
				"upgraded build refuses to start on: %v. Correct the file", path, s.what, err)
		}
	}
	return nil
}

// restartConfigLoads is the upgraded build's load of a --config, the first
// thing `stratum up` refuses a config on; the profile checks `stratum up` makes
// after it are not repeated here. A var so a test can keep it off the host's
// default config.
var restartConfigLoads = func(path string) error {
	_, err := cluster.LoadConfig(path)
	return err
}

// restartConfigHint is how an operator points a control plane at a different
// --config. `stratum up` refuses over a running node, which may be one started
// by hand rather than by the unit; it runs in the foreground; and it must be
// given the unit's other flags, since a profile is not dropped by leaving its
// flag off.
var restartConfigHint = "stop the node (`sudo systemctl stop stratum`, or Ctrl-C a `stratum up` started by hand), then " +
	"run `sudo " + version.StratumInstallPath + " up --config <the intended file>` with the unit's other flags (`systemctl cat stratum`), which " +
	"records that path in the unit; it runs in the foreground, so once it is up stop it with Ctrl-C and run " +
	"`sudo systemctl start stratum`"

// configFlag is the --config value in a `stratum up` argv, and whether one was
// given. The last one wins, as it does for the flag parser.
func configFlag(argv []string) (path string, given bool) {
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			path, given = v, true
		} else if a == "--config" && i+1 < len(argv) {
			path, given = argv[i+1], true
		}
	}
	return path, given
}

// ensureNodeUp is the upgrade's guard: the supervisor, and every host process
// active. The upgrade restarts those processes and gates on them, so it has
// nothing to stand on while one is down. Rollback needs only the supervisor:
// a host process crash-looping on an interrupted upgrade's binaries is the
// state it exists to recover, so the refusal says so.
func ensureNodeUp(cmd string) error {
	if err := ensureSupervisor(cmd); err != nil {
		return err
	}
	for _, u := range hostProcs {
		if !unitActive(context.Background(), u) {
			return fmt.Errorf("stratum %s: %s is not active; if `stratum up` is still bootstrapping, wait for it to finish and retry%s; nothing was changed",
				cmd, u, rollbackHint())
		}
	}
	return nil
}

// rollbackHint offers rollback only when it can recover this node: an
// interrupted upgrade whose set restores a host binary, the one condition
// under which RollbackNode relaxes its guard. Without an interrupted upgrade, a
// unit that is down is most likely a node still starting, and a rollback there
// would restore the upgrade before the last good one; with one whose set
// restores no host binary, the rollback would refuse on the same down unit.
func rollbackHint() string {
	m, err := readUpgradeMarker()
	if err != nil || m == nil {
		return ""
	}
	sets, err := scanBackupSets()
	if err != nil || !hasHostBackup(sets[m.TS]) {
		return ""
	}
	return fmt.Sprintf("; if it is crash-looping after the interrupted upgrade to %s, `stratum rollback` restores the version before it", m.Target)
}

// hostProcs are the supervised host processes whose binaries the upgrade
// swaps and then restarts, in restart order — the derivation of comps.Units
// beside this line, so a component added to the roster joins the swap, the
// restarts and the guards here without a second list to remember.
var hostProcs = hostProcsFrom(comps.Units)

// hostProcsFrom filters a component roster down to the upgrade's host
// processes: every unit but the operator, in roster order. The operator is
// restarted separately and LAST (spec: the disruptive re-exec of the stratum
// binary happens only on a proven-healthy node).
func hostProcsFrom(units []string) []string {
	var out []string
	for _, u := range units {
		if u == "operator" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// reloadHostProcs pushes each active host process's NEW argv/env to the running
// supervisor and restarts it, so an in-place upgrade applies changed args (and
// config paths) without a reboot. It derives the fresh definitions the
// way `stratum up` does — cluster.Processes — with the operator switch off,
// because the reload reads only containerd and kubelet, whose args do not
// depend on that switch (RunWorker builds them the same way). A component not
// present in that set falls back to a plain restart (which still picks up a
// swapped same-path binary).
//
// It reports which components now run on their new definition. One it skipped
// (not active) or only restarted keeps the argv the supervisor was started
// with, which is what applyHostConfigs needs to know before deleting a config
// path that argv may still name.
func reloadHostProcs(st *nodestate.NodeState) (map[string]bool, error) {
	specs := map[string]supervisor.ProcSpec{}
	for _, p := range cluster.Processes(st, false) {
		args := p.Args
		if p.ArgsFn != nil {
			args = p.ArgsFn()
		}
		specs[p.Name] = supervisor.ProcSpec{Path: p.Path, Args: args, Env: p.Env}
	}
	reloaded := map[string]bool{}
	for _, u := range hostProcs {
		if !unitActive(context.Background(), u) {
			continue
		}
		if spec, ok := specs[u]; ok {
			if err := reloadUnit(u, spec); err != nil {
				return reloaded, fmt.Errorf("reload %s: %w", u, err)
			}
			reloaded[u] = true
			continue
		}
		if err := restartUnit(u); err != nil {
			return reloaded, fmt.Errorf("restart %s: %w", u, err)
		}
	}
	return reloaded, nil
}

// applyHostConfigs is the upgrade's post-gate step: refresh the
// on-disk node configs from the NEW binary, reload each host process with the
// NEW binary's argv, and only then reap the legacy /etc/containerd/config.toml.
//
// The node-config render writes files only — it never restarts containerd — so
// the reload of containerd here is the upgrade's single restart of it: a render
// that restarted containerd itself would restart it twice in one upgrade.
//
// The order of the last two matters. A node upgraded from a
// build that wrote the legacy file has a supervisor whose stored containerd
// argv is `--config /etc/containerd/config.toml`, and that argv changes only
// when a reload lands. When the heal ran first and the supervisor predated
// reload, the file was gone while the supervisor still passed it: every later
// containerd start failed with "no such file", and the node was one containerd
// exit away from having no runtime at all. So the file is deleted only once
// containerd's reload succeeded; otherwise it is kept and `stratum up`, which
// starts a supervisor on the new argv, removes it.
func applyHostConfigs(w io.Writer, st *nodestate.NodeState) error {
	if err := writeNodeConfigs(w, st); err != nil {
		fmt.Fprintf(w, "  WARN: refresh node configs after upgrade: %v\n         new host-process config applies on the next `stratum up` or reboot\n", err)
		return nil
	}
	reloaded, rerr := reloadHostProcs(st)
	if err := hostReloadResult(w, rerr); err != nil {
		return err
	}
	if reloaded["containerd"] {
		removeLegacyConfig(w)
	} else if p, present := legacyConfigPresent(); present {
		fmt.Fprintf(w, "  kept %s: the running supervisor still starts containerd with it; the next `stratum up` removes it\n", p)
	}
	return nil
}

// Only a supervisor that predates reload can defer applying the new argv.
// A timeout or rejected/failed request cannot prove the new process is running,
// even if a subsequent liveness probe still sees the old process.
func hostReloadResult(w io.Writer, err error) error {
	if errors.Is(err, supervisor.ErrReloadUnsupported) {
		fmt.Fprintf(w, "  WARN: reload host-process args after upgrade: %v\n         new host-process args apply on the next `stratum up` or reboot\n", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reload host-process args after upgrade (new version remains installed; no rollback attempted): %w", err)
	}
	return nil
}

// componentGateTimeout bounds each static pod's post-re-render gate: kubelet
// has to notice the manifest change and recreate the pod from an image that
// is already imported, so minutes — not tens of minutes — is the budget.
const componentGateTimeout = 3 * time.Minute

// manifestBakDir is where the pre-upgrade manifests are backed up. It must
// live OUTSIDE the kubelet staticPodPath (staticpod.Dir): kubelet parses
// every file in that directory regardless of name or extension and would
// adopt a backup as a second static pod.
func manifestBakDir(ts string) string {
	return filepath.Join(version.DataDir, "upgrade", "manifests-bak-"+ts)
}

// cpComponents is this node's re-render order — staticpod.NamesFor verbatim,
// the kube-vip-only-with-VIP rule read from its owner (WriteAll renders no
// manifest for kube-vip on a node with no VIP, and staticpod.Start would
// fail). etcd is always included: exclusion cannot survive a re-render
// (manifests re-render from the installed binary's constants on every start),
// so there is no knob for it.
func cpComponents(st *nodestate.NodeState) []string {
	return staticpod.NamesFor(st)
}

// hostSwapOrder returns the payload binaries in src, kubelet LAST: `kubelet
// --version` is the node version marker, so it must not report the
// target version until everything else is already in place — that is what
// makes an interrupted upgrade re-run as a resume.
func hostSwapOrder(src string) ([]string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	var bins []string
	hasKubelet := false
	for _, e := range entries {
		n := e.Name()
		// assets.extract stages under hidden .<name>.tmp-* files; an interrupted
		// extraction can leave one behind, but it is never an installable binary.
		if !e.Type().IsRegular() || strings.HasPrefix(n, ".") || strings.Contains(n, ".bak-") || strings.HasSuffix(n, ".tmp") {
			continue
		}
		if n == "kubelet" {
			hasKubelet = true
			continue
		}
		bins = append(bins, n)
	}
	if hasKubelet {
		bins = append(bins, "kubelet")
	}
	return bins, nil
}

// stampUpgraded re-asserts a controller's version annotations once its upgrade
// has landed. Non-fatal for the same reason pruning is: the upgrade succeeded.
// There is no automatic retry here: the supervisor that survives an upgrade is
// the old process, whose kubelet-start callback predates the restamp, so
// re-running `stratum upgrade` is no help (it returns early at target).
// Only a supervisor started from a build with that callback re-stamps on the
// next kubelet start. A worker has no admin.conf to stamp with;
// `stratum upgrade --cluster` stamps it (stampVersion).
// The node is st.Hostname, the name it registered under, not the OS hostname.
func stampUpgraded(w io.Writer, st *nodestate.NodeState) {
	if st.Role != "controller" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stampWindow)
	defer cancel()
	if err := stampNode(ctx, st.Hostname); err != nil {
		fmt.Fprintf(w, "  WARN: stamp the version annotations on %s: %v\n", st.Hostname, err)
	}
}

// parseVersion is k8sutil.ParseVersion: the suffix trim and its reasoning
// live there, beside the stamp that shares it.
func parseVersion(s string) (string, error) { return k8sutil.ParseVersion(s) }

// currentNodeVersion reports the node version marker: `kubelet --version`.
func currentNodeVersion() (string, error) { return k8sutil.KubeletVersion() }

// newestSet picks the newest set: stamps are sortable %Y%m%d-%H%M%S, so the
// lexically greatest. Single-stamp on purpose: each upgrade run writes one
// stamp, and mixing runs could pair a restored old kubelet with a newer
// stratum binary, which would re-do that upgrade at the next `stratum up`.
func newestSet(sets map[string][]backupItem) (string, []backupItem) {
	best := ""
	for ts := range sets {
		if ts > best {
			best = ts
		}
	}
	return best, sets[best]
}

// scanBackupSets groups every backup on the node by its stamp. Only a name
// whose stamp NowStamp could have written counts, as pruning already requires:
// any other suffix, "stratum.bak-zzz" say, would sort as the newest set and be
// restored by a rollback. One with a real stamp that is the
// wrong kind of entry is an error rather than skipped (notABackup). A directory it
// cannot read is an error, not "no backups there": a resume restores exactly
// the set this returns, and a partial one would restore part of the node and
// report the whole of it rolled back. A backup in flight is
// copyBytes' ".<name>.bak-<ts>.tmp-*" file: the stamp check is what keeps it
// out, since its stamp carries the ".tmp-*" tail.
func scanBackupSets() (map[string][]backupItem, error) {
	byTS := map[string][]backupItem{}
	add := func(ts string, it backupItem) { byTS[ts] = append(byTS[ts], it) }
	readDir := func(dir string) ([]os.DirEntry, error) {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("scan backups in %s: %w", dir, err)
		}
		return entries, nil
	}

	stratumDir := filepath.Dir(stratumInstallPath)
	stratumPrefix := filepath.Base(stratumInstallPath) + ".bak-"
	entries, err := readDir(stratumDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if ts, ok := strings.CutPrefix(e.Name(), stratumPrefix); ok && validStamp(ts) {
			if !e.Type().IsRegular() {
				return nil, notABackup(stratumDir, e, "a regular file")
			}
			add(ts, backupItem{name: "stratum", bak: filepath.Join(stratumDir, e.Name()), dst: stratumInstallPath})
		}
	}

	upgradeDir := filepath.Join(version.DataDir, "upgrade")
	if entries, err = readDir(upgradeDir); err != nil {
		return nil, err
	}
	for _, e := range entries {
		for _, kind := range []struct {
			prefix string
			item   func(bak string) backupItem
		}{
			{"manifests-bak-", func(bak string) backupItem {
				return backupItem{name: manifestsItem, bak: bak, dst: staticpod.Dir(), dir: true}
			}},
			{"cisconfig-bak-", func(bak string) backupItem { return backupItem{name: "profile configs", bak: bak, profile: true} }},
		} {
			ts, ok := strings.CutPrefix(e.Name(), kind.prefix)
			if !ok || !validStamp(ts) {
				continue
			}
			if !e.IsDir() {
				return nil, notABackup(upgradeDir, e, "a directory")
			}
			add(ts, kind.item(filepath.Join(upgradeDir, e.Name())))
		}
	}

	if entries, err = readDir(version.BinDir); err != nil {
		return nil, err
	}
	for _, e := range entries {
		base, ts, ok := strings.Cut(e.Name(), ".bak-")
		if !ok || !validStamp(ts) {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, notABackup(version.BinDir, e, "a regular file")
		}
		dst := filepath.Join(version.BinDir, base)
		if dst == stratumInstallPath {
			continue // already picked up by the stratum scan above
		}
		add(ts, backupItem{name: base, bak: filepath.Join(version.BinDir, e.Name()), dst: dst})
	}
	return byTS, nil
}

// notABackup is the scan's refusal of an entry named as a backup, with a real
// stamp, that is not one. Skipped, it would vanish from its set — and a set
// with nothing left reads as an upgrade that changed nothing, whose marker and
// baseline are then discarded.
func notABackup(dir string, e os.DirEntry, want string) error {
	return fmt.Errorf("%s is named as a backup but is not %s; inspect or remove it", filepath.Join(dir, e.Name()), want)
}

// keepBackupSets is how many of the newest .bak-<ts> sets pruneOldBackups
// leaves behind after a successful upgrade. It must be >= 1 so RollbackNode
// (which restores the single newest stamp when no upgrade is in progress)
// always has its set;
// 2 keeps one spare so a rollback of the rollback still has something to land
// on, without letting the sets grow without bound.
const keepBackupSets = 2

// upgradeDirBackups are the backup sets kept under DataDir/upgrade.
var upgradeDirBackups = []backupPattern{{prefix: "manifests-bak-", directory: true}, {prefix: "cisconfig-bak-", directory: true}}

// backupPattern limits pruning to artifacts this upgrade owns. Binary backups
// must be regular files; only manifest backups may be recursively removed.
type backupPattern struct {
	prefix    string
	directory bool
}

// stampLayout is etcdsnap.NowStamp's format, which every backup stamp has.
const stampLayout = "20060102-150405"

// validStamp reports whether ts is exactly a stamp NowStamp could have made.
// A marker's stamp is joined into paths that are removed and restored from, so
// it is checked rather than trusted.
func validStamp(ts string) bool {
	t, err := time.Parse(stampLayout, ts)
	return err == nil && t.Format(stampLayout) == ts
}

func backupStamp(entry os.DirEntry, patterns []backupPattern) (string, bool) {
	for _, pattern := range patterns {
		if entry.IsDir() != pattern.directory || (!pattern.directory && !entry.Type().IsRegular()) {
			continue
		}
		ts, ok := strings.CutPrefix(entry.Name(), pattern.prefix)
		if !ok {
			continue
		}
		if validStamp(ts) {
			return ts, true
		}
	}
	return "", false
}

// pruneOldBackups removes every .bak-<ts> backup set in dir except the newest
// keep, so the timestamped sets each upgrade writes cannot accumulate until
// they fill the filesystem (an ENOSPC that then trips the very swap the backups
// exist to protect). One <ts> can span several entries in a dir (e.g.
// kubelet.bak-<ts> and containerd.bak-<ts> from one run), so entries are grouped
// by stamp and whole sets are removed together; the stamp is lexically
// chronological, so the newest keep stamps survive intact. MUST be called only
// AFTER an upgrade has succeeded — never around a failure, or a rollback could
// lose the set it needs to restore.
func pruneOldBackups(dir string, keep int, patterns ...backupPattern) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	byTS := map[string][]string{}
	var stamps []string
	for _, e := range entries {
		ts, ok := backupStamp(e, patterns)
		if !ok {
			continue
		}
		if _, seen := byTS[ts]; !seen {
			stamps = append(stamps, ts)
		}
		byTS[ts] = append(byTS[ts], e.Name())
	}
	if len(stamps) <= keep {
		return nil
	}
	sort.Strings(stamps) // %Y%m%d-%H%M%S sorts oldest-first
	var firstErr error
	for _, ts := range stamps[:len(stamps)-keep] {
		for _, name := range byTS[ts] {
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// copyFile copies src to dst through a rename, so a crash mid-write cannot
// leave a half-written file at dst (kubelet parses every manifest it sees;
// the supervisor execs binaries as they are).
//
// It streams. The stratum binary carries the ~660MB payload, and holding it —
// or dst's copy of it — in memory is what OOM-killed a failed upgrade's
// rollback on a 3GiB worker.
func copyFile(w io.Writer, src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyOpened(w, in, dst, mode)
}

// copyOpened is copyFile for a source the caller already opened, so a caller
// can tell a missing source (at open) from a failure to stage the copy.
func copyOpened(w io.Writer, in *os.File, dst string, mode os.FileMode) error {
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if unchanged(dst, mode, fi.Size()) {
		// Hash the descriptor the copy reads, not in.Name(): the path can be
		// swapped between the two. A hash error falls through to the copy.
		if same, err := sameContentFile(in, dst); err == nil && same {
			return nil
		}
	}
	return stageAndRename(w, dst, mode, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// copyBytes is copyFile for data already in memory: the bytes go through the
// shared durable writer, this package's seams handed to it, so the write is
// fsynced at its final mode before the rename and the rename is made durable
// by the directory fsync.
func copyBytes(w io.Writer, data []byte, dst string, mode os.FileMode) error {
	if unchanged(dst, mode, int64(len(data))) {
		if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
			return nil
		}
	}
	return atomicfile.WriteFileWith(dst, data, mode, atomicfileSeams(w))
}

// unchanged reports whether dst could already hold the content: a regular
// file of exactly this mode and size. The caller compares the bytes.
func unchanged(dst string, mode os.FileMode, size int64) bool {
	// Nothing to copy: dst already holds data. Restoring into a directory
	// kubelet watches, and rewriting a manifest whose bytes never moved
	// (etcd's, when its pin did not change) buys nothing but an inotify
	// wakeup and a re-parse — the same no-op property staticpod.write has.
	// Stat first so an unchanged 100MB binary is not read back on every
	// rollback just to discover it did not change.
	// Lstat and the WHOLE mode, which is one comparison covering two ways the
	// fast path could diverge from doing the copy. os.Stat would follow a
	// symlink at dst and leave it a symlink, where every caller here promises
	// a renamed-over regular file — a symlink's mode carries ModeSymlink, so
	// it cannot match a plain 0o600/0o755. And Perm() alone would ignore
	// setuid, setgid and sticky, which the staged file's chmod to exactly
	// mode would have cleared; comparing the full mode means an unexpected
	// privileged bit forces the replacement instead of surviving it.
	fi, err := os.Lstat(dst)
	return err == nil && fi.Mode() == mode && fi.Size() == size
}

// labelLike is selinux.LabelLikeWithWriter; a var so a test can see which files are
// labelled. The writer is the operation's: labelling diagnostics belong to the
// progress the operation reports, never to the process's real stdout.
var labelLike = selinux.LabelLikeWithWriter

// The durable single-file writer's own test seams. The writer for the bytes
// the callers hold in memory is internal/atomicfile's, and the wrappers below
// hand these to it on every call; stageAndRename, the streamed form, calls
// them in the same order around its own staging — the pattern
// internal/host/systemd.go established for the same reason — so the tests
// that pin this package's write behavior pin both.
var (
	// syncFile is (*os.File).Sync behind a seam, so a test can see what the
	// file looks like at the moment it is made durable.
	syncFile = func(f *os.File) error { return f.Sync() }
	// syncDir fsyncs a directory behind a seam, so a test can see it is
	// called, and with which directory.
	syncDir = atomicfile.SyncDir
)

// atomicfileSeams returns this package's write seams, for handing to the
// atomicfile writer.
func atomicfileSeams(w io.Writer) atomicfile.Seams {
	return atomicfile.Seams{
		SyncFile:  syncFile,
		LabelLike: func(tmp, dst string) error { return labelLike(w, tmp, dst) },
		SyncDir:   syncDir,
	}
}

// stageAndRename is atomicfile.WriteFile for content that streams: write
// fills the staged temp file, so a copy never holds its source whole in
// memory (copyFile's comment has the incident), and every step around it is
// the shared writer's own — the temp file labelled for its destination,
// fsynced at its final mode, renamed into place, and the directory fsynced so
// the rename survives a power loss. Its failure reports are the shared
// writer's too: a directory sync that failed after the rename matches
// atomicfile.ErrNotDurable, and a temp file that could not be removed matches
// atomicfile.ErrStrayCopy.
func stageAndRename(w io.Writer, dst string, mode os.FileMode, write func(io.Writer) error) (err error) {
	// Staged under a HIDDEN, UNIQUE name, which is load-bearing twice over.
	// Hidden: kubelet's file source globs `[^.]*` over its
	// staticPodPath, so it parses every non-hidden file there whatever the
	// name or extension — a visible "etcd.yaml.tmp" landing in that directory
	// is a pod under etcd's own namespace/name key with a different UID (the
	// source hashes the file path into it), which displaces the running etcd.
	// Unique: every writer staging beside its destination in a watched
	// directory — staticpod's manifest writes among them, through
	// atomicfile — creates its own temp, so a fixed hidden name would be one
	// path two writers could tear a file over.
	// Same shape as assets.extract, for the same reasons.
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if renamed {
			return
		}
		// A failure before the rename must leave no staging copy behind: the
		// temp name is removed, and a removal that fails leaves a file
		// holding all or part of the contents, reported as the shared writer
		// reports one.
		if rmErr := os.Remove(tmp.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = fmt.Errorf("%w; %s: %w (%v)", err, tmp.Name(), atomicfile.ErrStrayCopy, rmErr)
		}
	}()
	// Labelled for its destination while still under the temp name, before
	// the bytes: a label that cannot be set stops the write before it starts.
	if err := labelLike(w, tmp.Name(), dst); err != nil {
		tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	// The mode is set before the fsync: a mode changed after it is not
	// covered by it, so a crash could leave a "successful" write at the temp
	// file's 0600 instead of mode. The chmod is also what makes unchanged's
	// perm comparison able to match at all — CreateTemp opens 0600.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := syncFile(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	renamed = true
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("%w: %w", atomicfile.ErrNotDurable, err)
	}
	return nil
}

func atomicInstall(w io.Writer, src, dst string) error { return copyFile(w, src, dst, 0o755) }

// backupAndInstall copies the live file at dst to dst.bak-<ts>, then
// atomically installs src over it. A dst that does not exist yet gets no
// backup — there is nothing to restore to.
//
// A backup that already exists under ts is kept: on a resume it was taken by
// the interrupted run, before that run changed dst, so it is the original and
// dst may not be. That is also why the backup is written through a
// rename — an interrupted backup must be absent, never a truncated original.
//
// It returns the backup's path, or "" when dst did not exist and there is
// nothing to restore to. The caller records exactly that, rather than asking
// the filesystem again, where any error but "not there" would silently drop a
// backup from the rollback list.
func backupAndInstall(w io.Writer, dst, src, ts string) (string, error) {
	bak := dst + ".bak-" + ts
	kept, err := keptBackup(bak, false)
	if err != nil {
		return "", fmt.Errorf("backup %s: %w", filepath.Base(dst), err)
	}
	hasBackup := kept
	if !kept {
		// Only an absent dst has nothing to back up. Any other read error
		// would install over a file with no way back.
		// Only the open may find dst absent. An ENOENT from staging the
		// backup is a failed backup, and installing over dst then would leave
		// no way back.
		cur, err := os.Open(dst)
		switch {
		case err == nil:
			err = copyOpened(w, cur, bak, 0o755)
			cur.Close()
			if err != nil {
				return "", fmt.Errorf("backup %s: %w", filepath.Base(dst), err)
			}
			hasBackup = true
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("backup %s: %w", filepath.Base(dst), err)
		}
	}
	if !hasBackup {
		bak = ""
	}
	return bak, atomicInstall(w, src, dst)
}

// writeProfileConfigs is profilefiles.Write, indirected for
// tests.
var writeProfileConfigs = profilefiles.Write

// profileConfigPaths are the configs a control plane of prof reads, which
// refreshProfileConfigs backs up and restoreProfileConfigs puts back: each of
// the profile's ConfigFiles where the renderers read it
// (version.ProfileConfigFile), and, for a profile with an
// AdmissionConfiguration, the two files the render derives from it: the
// admission file the apiserver's argv names (version.AdmissionWebhookConfig)
// and the webhook client kubeconfig that file's MutatingAdmissionWebhook
// plugin names (version.WebhookClientKubeconfig). The render derives both
// whenever the apiserver manifest is written, which a rollback's restored
// manifests are not, so they must be restored themselves or the rolled-back
// apiserver keeps the new release's. The manifest set's snapshot carries
// both as well (staticpod.Snapshot); they stay on this list so the profile
// restore alone still reassembles the admission chain. The list follows the
// profile, so a config the apiserver starts reading joins the backup the day
// it joins the profile.
func profileConfigPaths(prof profile.Profile) []string {
	var out []string
	admission := false
	for _, f := range prof.ConfigFiles {
		out = append(out, version.ProfileConfigFile(f.Name))
		admission = admission || f.Kind == profile.AdmissionConfiguration
	}
	if admission {
		out = append(out, version.AdmissionWebhookConfig(), version.WebhookClientKubeconfig())
	}
	return out
}

// profileConfigBakDir is where one upgrade run backs them up: beside the
// manifest backups, where scanBackupSets finds a set for `stratum rollback`
// and the success path prunes it. The name predates profiles and is kept: a
// `stratum rollback` run by the release before this one recognises only it.
func profileConfigBakDir(ts string) string {
	return filepath.Join(version.DataDir, "upgrade", "cisconfig-bak-"+ts)
}

// profileConfigRecordName is the file inside a profile config backup that
// records the names of the paths its writer considered —
// profileConfigPaths as that run held it, whichever of them existed.
// restoreProfileConfigs reads it to tell a config the backup found absent
// (its writer looked, and it was not there) from a path the backup never
// knew, and to know which configs the backup covers at all. Dot-prefixed like
// the staging dir, so no config can collide with it, and named as the
// release before this one names it, for its rollback.
const profileConfigRecordName = ".cisconfig-paths"

// refreshProfileConfigs backs up the configs a control plane of prof runs
// with, each with its own mode, and writes this release's renders of the
// profile's config files; re-rendering the apiserver derives the rest. A
// config that did not exist is absent from the backup and named in its
// record, which is how restoreProfileConfigs knows to remove the one written
// since rather than leave it — and how it tells that from a path a pre-record
// backup never considered, whose absence says nothing about whether it
// existed.
//
// The backup is complete or it does not exist: it is staged under a name
// neither scanBackupSets nor pruning recognises and renamed into place only
// once every copy has succeeded. A partial one would read as configs that did
// not exist before, and restoring it would delete live ones the upgrade never
// touched.
func refreshProfileConfigs(w io.Writer, ts string, prof profile.Profile) ([]backupItem, error) {
	// Before any write, on a fresh run and on a resume alike: the record
	// below holds one name per line and the restore reads each back as a
	// path element, so a name the record cannot round-trip would have the
	// rollback restore or remove the wrong file. Registration refuses such a
	// name already; this is the upgrade refusing to trust that.
	for _, p := range profileConfigPaths(prof) {
		if err := profile.PlainFileName(filepath.Base(p)); err != nil {
			return nil, fmt.Errorf("back up the %s profile's configs: %w; nothing was changed", prof.Name, err)
		}
	}
	bak := profileConfigBakDir(ts)
	item := []backupItem{{name: "profile configs", bak: bak, profile: true}}
	// Kept on a resume, like every backup.
	if kept, err := keptBackup(bak, true); err != nil {
		return nil, fmt.Errorf("back up the %s profile's configs: %w", prof.Name, err)
	} else if kept {
		// A resume writes only what the kept backup can put back. The
		// marker checks the target version alone, so a resumed run may be a
		// rebuilt binary whose profile declares a config file the earlier run
		// never backed up; writing it would leave a rollback unable to
		// restore it, and the upgrade would report success over a lost file.
		covered, err := profileConfigRecord(bak)
		if err != nil {
			return nil, fmt.Errorf("back up the %s profile's configs: read the backup kept from the earlier run: %w", prof.Name, err)
		}
		var uncovered []string
		for _, f := range prof.ConfigFiles {
			if !covered[f.Name] {
				uncovered = append(uncovered, f.Name)
			}
		}
		if len(uncovered) > 0 {
			return nil, fmt.Errorf("the %s profile's configs were not written: this binary's profile declares %s, which the backup kept from the earlier run (%s) does not cover, so a rollback could not restore it. "+
				"Run `stratum rollback` to return to the configs that backup holds, then start the upgrade again with this binary, which backs up every file it writes; nothing was changed",
				prof.Name, strings.Join(uncovered, ", "), bak)
		}
		return item, writeProfileConfigs(prof)
	}
	staging := filepath.Join(filepath.Dir(bak), ".cisconfig-staging-"+ts)
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("back up the %s profile's configs: %w", prof.Name, err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, fmt.Errorf("back up the %s profile's configs: %w", prof.Name, err)
	}
	paths := profileConfigPaths(prof)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = copyFile(w, p, filepath.Join(staging, filepath.Base(p)), fi.Mode().Perm())
		}
		if err != nil {
			_ = os.RemoveAll(staging)
			return nil, fmt.Errorf("back up %s: %w", filepath.Base(p), err)
		}
	}
	// The record rides the same complete-or-absent guarantee: written into
	// the staging dir before the rename, a backup either names every path its
	// writer considered or does not exist.
	var record strings.Builder
	for _, p := range paths {
		record.WriteString(filepath.Base(p))
		record.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(staging, profileConfigRecordName), []byte(record.String()), 0o600); err != nil {
		_ = os.RemoveAll(staging)
		return nil, fmt.Errorf("back up the %s profile's configs: %w", prof.Name, err)
	}
	if err := os.Rename(staging, bak); err != nil {
		_ = os.RemoveAll(staging)
		return nil, fmt.Errorf("back up the %s profile's configs: %w", prof.Name, err)
	}
	return item, writeProfileConfigs(prof)
}

// restoreProfileConfigs puts each config the backup covers back as bak holds
// it, at version.ProfileConfigFile of its name: every profile config, and the
// two derived admission files, live in that one config directory. It needs no
// profile: the backup names what it covers, so a rollback restores whatever
// the run that wrote it backed up, whatever the node state says now. One bak
// does not hold is removed only when the backup's own record covers it: then
// its writer looked for the file and did not find it, so it did not exist
// before the upgrade that wrote it. A path no record covers is left alone: a
// backup that predates the path never copied it though the file was on the
// node — the render writes webhook-client.kubeconfig on every apiserver
// write — and removing it would leave the restored admission-webhook.yaml
// naming a kubeconfig no rollback put back, an admission chain the
// rolled-back apiserver cannot load.
func restoreProfileConfigs(w io.Writer, bak string) error {
	entries, err := os.ReadDir(bak)
	if err != nil {
		return err
	}
	covered, err := profileConfigRecord(bak)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for name := range covered {
		names[name] = true
	}
	for _, e := range entries {
		if e.Name() != profileConfigRecordName && !e.IsDir() {
			names[e.Name()] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		p := version.ProfileConfigFile(name)
		saved := filepath.Join(bak, name)
		fi, err := os.Stat(saved)
		if errors.Is(err, fs.ErrNotExist) {
			if covered[name] {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := copyFile(w, saved, p, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// profileConfigRecord reads a backup's record of the paths its writer
// considered. A backup with no record predates records and covered exactly
// the legacy three basenames of the cis profile, the only one there was. Keep
// that format's absence semantics without treating the later webhook client
// kubeconfig as previously absent. A record entry that is not a bare file
// name is refused before anything is restored or removed.
func profileConfigRecord(bak string) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(bak, profileConfigRecordName))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{
			"admission-config.yaml":  true,
			"audit-policy.yaml":      true,
			"admission-webhook.yaml": true,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, name := range strings.Split(string(raw), "\n") {
		if name == "" {
			continue
		}
		if err := profile.PlainFileName(name); err != nil {
			return nil, fmt.Errorf("%s names %q, which is not a config file name", filepath.Join(bak, profileConfigRecordName), name)
		}
		covered[name] = true
	}
	return covered, nil
}

// restoreOrder is items with the profile configs first, otherwise as given.
// Pure.
func restoreOrder(items []backupItem) []backupItem {
	out := slices.Clone(items)
	slices.SortStableFunc(out, func(a, b backupItem) int {
		switch {
		case a.profile && !b.profile:
			return -1
		case b.profile && !a.profile:
			return 1
		}
		return 0
	})
	return out
}

// splitManifests separates the static pod manifests from the rest of a backup
// set. A rollback restores the manifests last, after containerd and kubelet
// are back: kubelet and containerd restarted while kubelet replaces a static
// pod can leave the old pod half torn down, and kubelet then never starts the
// new one.
func splitManifests(items []backupItem) (host, manifests []backupItem) {
	for _, it := range items {
		if it.dir {
			manifests = append(manifests, it)
		} else {
			host = append(host, it)
		}
	}
	return host, manifests
}

// replaced is the static pods a restore is about to change.
type replaced map[string]bool

// replacedComponents names every static pod whose manifest the restore of
// manifestItems changes. A component's gate passed while a process from the
// manifest kubelet is replacing still answers proves nothing about the
// restored one. Each is looked for here, before anything changes, so a node
// whose processes cannot be listed is refused while nothing has.
func replacedComponents(manifestItems []backupItem) (replaced, error) {
	out := replaced{}
	for _, it := range manifestItems {
		entries, err := os.ReadDir(it.bak)
		if err != nil {
			return nil, fmt.Errorf("read the manifest backup %s: %w", it.bak, err)
		}
		for _, e := range entries {
			// Only rendered manifests are pods a restore replaces; the
			// render-derived files the snapshot also holds are configs, and a
			// hidden staging file is an in-flight write.
			component, ok := staticpod.ManifestName(e.Name())
			if !ok {
				continue
			}
			bak, err := os.ReadFile(filepath.Join(it.bak, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("read the backed-up manifest: %w", err)
			}
			live, err := os.ReadFile(filepath.Join(it.dst, e.Name()))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("read the live manifest: %w", err)
			}
			if err == nil && bytes.Equal(live, bak) {
				continue
			}
			if _, err := componentPIDs(component); err != nil {
				return nil, fmt.Errorf("find the %s process the restore replaces: %w", component, err)
			}
			out[component] = true
		}
	}
	return out, nil
}

// gateOrder is the components to gate after the restore: those the node's
// role names, then any other whose manifest the restore changed, so a node
// whose state is missing or wrong still has what it restored gated.
func (r replaced) gateOrder(roleComponents []string) []string {
	out := slices.Clone(roleComponents)
	for _, name := range staticpod.Names {
		if r[name] && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// wait waits until every process running name started after mark, taken just
// before the restore, so that what answers next is the restored component.
func (r replaced) wait(name string, mark staticpod.Mark) error {
	if !r[name] {
		return nil
	}
	return waitComponentReplace(name, mark, componentGateTimeout)
}

// backupItem is one restorable artifact of an in-flight upgrade. Items are
// appended as each mutation lands, so a rollback restores exactly what was
// changed — no more, no less.
type backupItem struct {
	name    string // what the INCOMPLETE error names if this restore fails
	bak     string
	dst     string
	dir     bool // bak is a snapshot of the manifest dir, restored through staticpod.RestoreSnapshot
	profile bool // bak is a profile config backup dir (restoreProfileConfigs); dst unused
}

// restoresStratum reports whether items put back the stratum binary.
func restoresStratum(items []backupItem) bool {
	for _, it := range items {
		if it.dst == stratumInstallPath {
			return true
		}
	}
	return false
}

// disableSELinux is runtime.DisableSELinux; a var so a test can observe it.
var disableSELinux = kuberuntime.DisableSELinux

// unconfineForOldManifests turns containerd's enable_selinux off when the
// manifests a rollback restores predate the control plane's spc_t: with it
// on, the restored etcd and apiserver would run as container_t and be denied
// their own data, and the older build they came from never turns it off.
// containerd picks the change up when the rollback restarts it, before the
// manifests are restored. Running containers keep their labels.
func unconfineForOldManifests(w io.Writer, bak string) error {
	if bak == "" {
		return nil
	}
	entries, err := os.ReadDir(bak)
	if err != nil {
		return fmt.Errorf("read the manifest backup %s: %w", bak, err)
	}
	for _, e := range entries {
		if _, ok := staticpod.ManifestName(e.Name()); !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(bak, e.Name()))
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if strings.Contains(string(b), "spc_t") {
			return nil
		}
	}
	changed, err := disableSELinux()
	if err != nil {
		return fmt.Errorf("turn off containerd's SELinux labelling for manifests that predate it: %w", err)
	}
	if changed {
		fmt.Fprintln(w, "  containerd's SELinux labelling is off: the restored control plane predates running as spc_t")
	}
	return nil
}

// manifestsItem names the manifest directory backup in a backup set.
const manifestsItem = "manifests"

// manifestBackup is the manifest directory backup among items, "" when there
// is none.
func manifestBackup(items []backupItem) string {
	for _, it := range items {
		if it.dir {
			return it.bak
		}
	}
	return ""
}

// restoreAll puts every backed-up item back and returns the names of the
// items whose restore failed. Best-effort by design: stopping at the first
// failure would widen the damage the INCOMPLETE error exists to report.
//
// CIS configs go first: restored manifests are what kubelet acts on, and an
// older apiserver started from them must find its own admission configs, not
// the newer release's Pod Security pin.
func restoreAll(w io.Writer, items []backupItem) (failed []string) {
	for _, it := range restoreOrder(items) {
		var err error
		switch {
		case it.dir:
			// The manifest set is staticpod's to put back: the package that
			// renders it owns what belongs in it, where its derived files
			// land, and what kubelet must never find lying there.
			err = staticpod.RestoreSnapshot(it.bak)
		case it.profile:
			err = restoreProfileConfigs(w, it.bak)
		default:
			err = atomicInstall(w, it.bak, it.dst)
		}
		if err != nil {
			failed = append(failed, it.name)
			fmt.Fprintf(w, "  WARN: restore %s from %s failed: %v\n", it.name, it.bak, err)
		}
	}
	return failed
}

// backupManifests copies the live manifest set to manifestBakDir(ts) and
// returns the backup path. The set itself — the manifests together with the
// render-derived files the apiserver render writes beside them, staged
// complete-or-absent — is staticpod.Snapshot's: the package that renders the
// set owns what belongs in it. A backup that already exists under ts is kept:
// on a resume it was taken by the interrupted run, before that run
// re-rendered anything, so it is the original and the live dir may not be.
func backupManifests(ts string) (string, error) {
	bak := manifestBakDir(ts)
	if kept, err := keptBackup(bak, true); err != nil {
		return "", err
	} else if kept {
		return bak, nil
	}
	if err := staticpod.Snapshot(bak); err != nil {
		return "", err
	}
	return bak, nil
}

// rollbackNode restores every tracked backup, bounces the host processes when
// their binaries were already swapped (restored manifests need no help —
// kubelet picks them up on its own), and wraps cause in the rollback outcome.
//
// The honesty contract: never claim a rollback that did not happen. A restore
// can fail for the same reason the upgrade did (e.g. disk full), leaving the
// node on the NEW version — the returned error names exactly what is still
// new.
func rollbackNode(w io.Writer, cause error, backups []backupItem, hostSwapped bool, ts string) error {
	k8sutil.Step(w, "Upgrade FAILED — rolling back the node")
	if err := prepareEtcdForRollback(w, manifestBackup(backups), false); err != nil {
		return fmt.Errorf("upgrade failed, and its rollback did not start: %v — the node is still on the new version "+
			"(backups: *.bak-%s): %w", err, ts, cause)
	}
	hostItems, manifestItems := splitManifests(backups)
	failed := restoreAll(w, hostItems)
	var notBack []string
	if hostSwapped {
		for _, u := range hostProcs {
			if !unitExists(u) {
				continue
			}
			if err := restartUnit(u); err != nil {
				notBack = append(notBack, fmt.Sprintf("%s (%v)", u, err))
			}
		}
	}
	failed = append(failed, restoreAll(w, manifestItems)...)
	if err := settleStoppedEtcdManifest(); err != nil {
		fmt.Fprintf(w, "  WARN: %v\n", err)
	}
	if len(failed) > 0 {
		// The marker stays: a retry must keep this baseline.
		return fmt.Errorf("upgrade failed and rollback was INCOMPLETE — %s still on the new version (backups: *.bak-%s)%s: %w",
			strings.Join(failed, ", "), ts, stoppedEtcdNote(), cause)
	}
	if len(notBack) > 0 {
		return fmt.Errorf("upgrade failed and rollback was INCOMPLETE — every backup was restored, but %s did not "+
			"come back on its restored binary (backups: *.bak-%s)%s: %w", strings.Join(notBack, ", "), ts,
			stoppedEtcdNote(), cause)
	}
	clearUpgradeMarker(w)
	return fmt.Errorf("upgrade failed and the node was rolled back: %w", cause)
}

// gateComponent is one static pod's health gate after its re-render: the
// local listener back up for every component that owns one — the set
// staticpod.HasListener declares, kube-vip the one listenerless component and
// so the one with no gate — plus /healthz for the apiserver. No per-component
// soak on purpose — the gate proves liveness; delayed instability is a
// node-level property (spec).
func gateComponent(name string) error {
	if !staticpod.HasListener(name) {
		return nil
	}
	if err := waitStaticPodReady(name, componentGateTimeout); err != nil {
		return err
	}
	if name == "kube-apiserver" {
		return withinWindow(componentHealthzWindow, waitHealthzCtx)
	}
	return nil
}

// nodeGate is the single node-level health gate: every supervised host
// process active, plus apiserver /healthz on a controller.
func nodeGate(isCP bool, node string) error {
	if err := hostUnitsActive(context.Background()); err != nil {
		return err
	}
	if err := withinWindow(runtimeProbeWindow, containerdServesInstalled); err != nil {
		return err
	}
	if isCP {
		if err := withinWindow(nodeGateHealthzWindow, waitHealthzCtx); err != nil {
			return err
		}
	}
	return withinWindow(runtimeProbeWindow, func(ctx context.Context) error { return kubeletServesInstalled(ctx, node) })
}

// nodeName is the name this node's kubelet registers its Node under, which is
// what the version check must read. Kubelet runs with --hostname-override= the
// persisted NodeState.Hostname (internal/cluster/processes.go), so that is not
// the OS hostname once the two differ, and stampUpgraded uses it for the same
// reason. Only with no node state at all (a rollback on a node that lost it)
// does it fall back to the OS hostname, the name a kubelet with no override
// registers under.
func nodeName(st *nodestate.NodeState) string {
	if st != nil && st.Hostname != "" {
		return st.Hostname
	}
	h, _ := host.Hostname()
	return h
}

// runtimeProbeWindow bounds each running-version probe of nodeGate. A var so
// tests can shorten it.
var runtimeProbeWindow = 30 * time.Second

// withinWindow runs a probe under one deadline, which covers everything it
// does: the installed-version subprocess, the requests and the retry sleeps.
func withinWindow(window time.Duration, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	return probe(ctx)
}

// The running-version probes, as vars so tests can redirect them.
var (
	installedVersion  = rtcheck.Installed
	servingContainerd = rtcheck.Serving
	kubeletReports    = func(ctx context.Context, node string) (string, error) {
		// This node's own kubelet credential, on controllers and workers alike:
		// the Node authorizer lets a kubelet read its own Node, and a worker has
		// no admin.conf.
		cfg, err := clientcmd.BuildConfigFromFlags("", filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"))
		if err != nil {
			return "", err
		}
		cfg.Timeout = 5 * time.Second
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return "", err
		}
		n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return n.Status.NodeInfo.KubeletVersion, nil
	}
)

// containerdServesInstalled fails unless the runtime answering the CRI socket
// reports the containerd version on disk. Active only says a supervised child
// exists; an orphan from an earlier run can hold the socket while the new
// containerd crash-loops, and the node then runs a different runtime than the installed one while the
// upgrade reports success.
func containerdServesInstalled(ctx context.Context) error {
	want, err := installedVersion(ctx, filepath.Join(version.BinDir, "containerd"))
	if err != nil {
		return err
	}
	got, err := servingContainerd(ctx, version.ContainerdSock)
	if err != nil {
		return fmt.Errorf("containerd %s is installed but the runtime on %s did not answer: %w", want, version.ContainerdSock, err)
	}
	if !rtcheck.Same(got, want) {
		return fmt.Errorf("containerd %s is installed but the runtime serving %s reports %s — another containerd, an orphan from an earlier run, holds the socket; stop it so the supervisor can start the installed one", want, version.ContainerdSock, got)
	}
	return nil
}

// kubeletServesInstalled is the same check for kubelet, through the version it
// reports in its Node's nodeInfo, on every node. nodeInfo is refreshed on
// kubelet's status sync, so a stale value is retried until ctx ends before it
// is called a mismatch.
func kubeletServesInstalled(ctx context.Context, node string) error {
	want, err := installedVersion(ctx, filepath.Join(version.BinDir, "kubelet"))
	if err != nil {
		return err
	}
	for {
		got, err := kubeletReports(ctx, node)
		if err == nil && rtcheck.Same(got, want) {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("kubelet %s is installed but node %s's nodeInfo could not be read: %w", want, node, err)
			}
			return fmt.Errorf("kubelet %s is installed but node %s reports kubelet %s", want, node, got)
		case <-time.After(2 * time.Second):
		}
	}
}

// hostUnitsActive is nodeGate's host-process half; ctx bounds the supervisor
// requests.
func hostUnitsActive(ctx context.Context) error {
	for _, u := range hostProcs {
		if !unitActive(ctx, u) {
			return fmt.Errorf("%s is not active", u)
		}
	}
	return nil
}

// waitNodeGate is nodeGate within window: the host processes are polled
// until active, then each check gets what is left of window. A rollback starts from a node whose host processes
// may have been down, and one sample straight after the soak can land while a
// just-restarted unit is still coming up. Bounding the whole gate, healthz
// included, keeps it to the window it advertises: retrying nodeGate itself
// would run a full nodeGateHealthzWindow healthz wait on every try.
func waitNodeGate(isCP bool, node string, window time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	for {
		err := hostUnitsActive(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * time.Second):
		}
	}
	// One deadline covers every step, down to the requests and sleeps inside
	// them; a step that finds none left fails rather than getting a fresh
	// allowance.
	used := func(next string) error {
		if ctx.Err() != nil {
			return fmt.Errorf("the %v gate window was used up before the %s check", window, next)
		}
		return nil
	}
	if err := used("containerd version"); err != nil {
		return err
	}
	if err := containerdServesInstalled(ctx); err != nil {
		return err
	}
	if isCP {
		if err := used("API server health"); err != nil {
			return err
		}
		if err := waitHealthzCtx(ctx); err != nil {
			return err
		}
	}
	if err := used("kubelet version"); err != nil {
		return err
	}
	return kubeletServesInstalled(ctx, node)
}

// suffixedTarget names the bare version for a build-suffixed --target such as
// "vMAJOR.MINOR.PATCH-stratum.0-rc.3", or returns nil when target has no suffix
// or isBare does not accept what precedes it. Both upgrade paths refuse a suffixed target
// through it, so they give the same advice. isBare is the caller's idea of an
// acceptable version: base is only echoed once it has passed it, while target
// is user input and is quoted.
func suffixedTarget(target string, isBare func(base string) bool) error {
	base, _, ok := strings.Cut(target, "-")
	if !ok || !isBare(base) {
		return nil
	}
	return fmt.Errorf("--target takes the bare Kubernetes version: use --target %s, not %q (the build suffix is not part of it)",
		base, target)
}

// targetMismatch says what to do about a --target the executing binary cannot
// install. After HandOff, a non-empty From is always this binary, so "wrong
// artifact staged" is the accurate reading only then; without --from, the
// operator ran the installed binary and needs to be pointed at the new one.
func targetMismatch(target, from string) error {
	if err := suffixedTarget(target, func(base string) bool { return base == version.K8sVersion }); err != nil {
		return err
	}
	if from != "" {
		return fmt.Errorf("--target %s does not match the upgrade binary %s, which installs Kubernetes %s — wrong artifact staged?",
			target, from, version.K8sVersion)
	}
	return fmt.Errorf("--target %s does not match this binary, which installs Kubernetes %s — stage the %s binary and pass it with --from, or run it directly",
		target, version.K8sVersion, target)
}

// checkPinnedBuild refuses an upgrade binary that is not the one the cluster
// upgrade is rolling out, before anything on the node changes.
// stagedPayload reports whether the binary at path carries a payload; a var
// so a test can answer for a fake binary.
var stagedPayload = func(path string) error {
	_, err := assets.Names(path)
	return err
}

func checkPinnedBuild(o NodeOptions) error {
	stage := func(what string) error {
		return fmt.Errorf("stratum upgrade: the upgrade binary at %s on this node is not the one the cluster upgrade is rolling out (%s); stage the same binary at that path on every node. Nothing was changed", o.From, what)
	}
	if o.ExpectBuild != "" && o.ExpectBuild != version.BuildID {
		return stage("its build is " + version.BuildID + ", the rollout's is " + o.ExpectBuild)
	}
	// A rollout's binary must carry the payload it installs. The build ID
	// cannot say so -- the payload is appended after linking, and an
	// EMBEDDED_BINS=none build of the same commit stamps the same ID -- and a
	// node of another architecture has no sha256 pin, so it is checked here,
	// before the drain, rather than found missing by the upgrade after it.
	if o.ExpectBuild != "" {
		self, err := selfExecutable()
		if err != nil {
			return fmt.Errorf("stratum upgrade: identify the executing binary to check its payload: %w", err)
		}
		if err := stagedPayload(self); err != nil {
			if errors.Is(err, assets.ErrNoPayload) {
				return stage("it carries no embedded payload")
			}
			return fmt.Errorf("stratum upgrade: read the payload of %s: %w", self, err)
		}
	}
	if o.ExpectSHA256 != "" {
		self, err := selfExecutable()
		if err != nil {
			return fmt.Errorf("stratum upgrade: identify the executing binary to check it against the one being rolled out: %w", err)
		}
		sum, err := fileSHA256(self)
		if err != nil {
			return fmt.Errorf("stratum upgrade: hash %s to check it against the one being rolled out: %w", self, err)
		}
		if got := hex.EncodeToString(sum); got != o.ExpectSHA256 {
			return stage("its sha256 is " + got + ", the rollout's is " + o.ExpectSHA256)
		}
	}
	return nil
}

// uncordonWarning is what the Job's backstop prints when it could not read or
// write its own node. It runs when ownership could not be read, so it says to
// uncordon only a cordon the cluster upgrade made: an admin's is not to be
// undone. The command is the installed binary's absolute path: the node has no
// kubectl on its PATH, and under sudo (Rocky's secure_path) a bare `stratum`
// in /usr/local/bin is not found either. `stratum kubectl` is the bundled one
// and supplies the admin kubeconfig.
func uncordonWarning(node string, err error) string {
	return fmt.Sprintf("  WARN: could not check whether %s is still cordoned: %v; "+
		"if the cluster upgrade cordoned it (you did not cordon it yourself before the upgrade), run: %s kubectl uncordon %s\n",
		node, err, shellQuote(stratumInstallPath), node)
}

// checkNodePrereqs reports whether this node may take the upgrade at all,
// with Ensure's rules for a new node: an enforcing or CIS node without
// container-selinux would run unconfined, so it refuses. It reads the node's
// SELinux settings itself; one it cannot read is a question the upgrade
// cannot ask, so it warns and lets the node through — and so is a policy
// check whose tools failed rather than went missing, which Prerequisite
// answers with ErrCannotTell. A resuming node has
// already been changed by the interrupted run, so it warns instead of
// refusing. A var so a test can answer for it.
var checkNodePrereqs = func(w io.Writer, resuming bool) error {
	opts, err := nodeSELinuxOptions()
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not read this node's SELinux settings: %v; not checking the SELinux prerequisites\n", err)
		return nil
	}
	perr := nodePrereq(opts)
	if perr == nil {
		return nil
	}
	if errors.Is(perr, selinux.ErrCannotTell) {
		// The policy check could not be asked, so the node is let through
		// with the failure named, like one whose settings could not be read.
		fmt.Fprintf(w, "  WARN: %v; not checking the SELinux prerequisites\n", perr)
		return nil
	}
	if resuming {
		fmt.Fprintf(w, "  WARN: %v; resuming the interrupted upgrade anyway\n", perr)
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPrereqMissing, perr)
}

// nodePrereq is selinux.Prerequisite; a var so a test can answer for the
// policy check, whose host state it cannot fake from this package.
var nodePrereq = selinux.Prerequisite

// nodeSELinuxOptions is cluster.NodeSELinuxOptions; a var so a test can make
// the node's settings unreadable.
var nodeSELinuxOptions = cluster.NodeSELinuxOptions

// importUpgradeImages imports the payload images of the upgrade binary at from
// into this node's containerd, pinned as every payload image is, and changes
// nothing else: no install, no backup set, no marker. It takes the node lock,
// so it refuses beside a running upgrade, rollback or uninstall.
func importUpgradeImages(w io.Writer, from string) error {
	const cmd = "upgrade --import-images"
	if err := ensureNodeUp(cmd); err != nil {
		return err
	}
	release, err := lockNode(cmd)
	if err != nil {
		return err
	}
	defer release()
	src, err := resolvePayload(w, from, hostProcs)
	if err != nil {
		return err
	}
	if src.Images == "" {
		fmt.Fprintln(w, "stratum upgrade: the upgrade binary carries no images; nothing to import")
		return nil
	}
	k8sutil.Step(w, "Import payload images")
	return importImages(context.Background(), w, src.Images)
}

// UpgradeNode upgrades this node to the executing binary's version:
// controllers get image import + per-component manifest re-render with gates,
// every node gets the host binary swap. Failures before config/argv refresh
// roll the node back; failures after that irreversible step are reported without
// rollback.
// Idempotent — a node already at Target with no upgrade in progress is a
// no-op, and an interrupted run re-runs as a resume (kubelet, the version
// marker, swaps last), even at Target.
func UpgradeNode(w io.Writer, o NodeOptions) error {
	if err := requireRunningFrom(o.From); err != nil {
		return err
	}
	if o.Target == "" {
		o.Target = version.K8sVersion
	}
	if o.SoakSec == 0 {
		o.SoakSec = 15
	}
	// The upgrade binary IS the artifact: its pinned constants are what get
	// installed, so a --target that disagrees with them means the wrong
	// artifact was staged. Refuse before touching anything.
	if o.Target != version.K8sVersion {
		return targetMismatch(o.Target, o.From)
	}
	if err := checkPinnedBuild(o); err != nil {
		return err
	}
	if o.ImportImages {
		return importUpgradeImages(w, o.From)
	}
	// Dry-run and --check write nothing, and previewing from a half-up node
	// is useful, so they skip the guard.
	if !o.DryRun && !o.Check {
		if err := ensureNodeUp("upgrade"); err != nil {
			return err
		}
		release, err := lockNode("upgrade")
		if err != nil {
			return err
		}
		defer release()
	}
	// --check takes the lock too, without waiting. Its "already on this
	// build" is only true while nothing else is changing the node: a
	// concurrent upgrade installs stratum first, so a check that read no
	// marker and then compared binaries could answer yes, and the upgrade
	// could still roll back after the orchestrator had skipped the node.
	// Held, the lock makes the check fail -- exit 1, which
	// the orchestrator refuses without draining -- rather than answer.
	if o.Check {
		release, err := lockNode("upgrade --check")
		if err != nil {
			return err
		}
		defer release()
	}
	st, err := nodestate.LoadState()
	if err != nil {
		return fmt.Errorf("node state (is this a stratum node?): %w", err)
	}
	// Before the marker, the backups and the self-install: a binary that does
	// not carry this node's profile cannot render it, and a worker is never
	// resolved below, so an unregistered profile is refused here for every
	// role, --check and --dry-run included.
	if _, err := profile.Select(st.ProfileName()); err != nil {
		return fmt.Errorf("stratum upgrade: this node runs profile %q, which this binary does not register; nothing was changed: %w", st.ProfileName(), err)
	}
	isCP := st.Role == "controller"
	hostname, _ := host.Hostname()

	marker, err := readUpgradeMarker()
	if err != nil {
		return fmt.Errorf("stratum upgrade: %w", err)
	}
	sets, err := scanBackupSets()
	if err != nil {
		return fmt.Errorf("stratum upgrade: %w; nothing was changed", err)
	}
	// After the marker and the backups are read, so the refusal can say what an
	// interrupted run left; before the marker is cleared and anything else is
	// written.
	if err := restartConfigMissing(st); err != nil {
		switch {
		case marker == nil || marker.changedNothing(sets):
			next := "upgrade"
			if o.Check {
				next = "check"
			}
			return fmt.Errorf("%w. Then %s again; nothing was changed", err, next)
		case marker.Target == o.Target:
			return fmt.Errorf("%w. Then upgrade again, which finishes the interrupted upgrade to %s, left as it "+
				"stopped", err, marker.Target)
		default:
			// refuseOtherInterruptedUpgrade, below, refuses the retry: that
			// upgrade is finished with its own binary or rolled back first.
			return fmt.Errorf("%w. Then finish the interrupted upgrade to %s, left as it stopped, with that upgrade "+
				"binary, or undo it with `stratum rollback`, and retry", err, marker.Target)
		}
	}
	// An upgrade to this target that recorded nothing is not proof it changed
	// nothing: installing stratum over no binary at all backs nothing up, and a
	// run killed after that install matches the build check below with its
	// host binaries never swapped. So it cannot vouch for
	// "already on this build"; the upgrade runs again.
	abandonedSameTarget := false
	if marker.changedNothing(sets) {
		fmt.Fprintf(w, "stratum upgrade: the interrupted upgrade to %s changed nothing (no backups under %s); starting afresh\n", marker.Target, marker.TS)
		abandonedSameTarget = marker.Target == o.Target
		if !o.DryRun && !o.Check {
			clearUpgradeMarker(w)
		}
		marker = nil
	}
	if err := refuseOtherInterruptedUpgrade(marker, o.Target); err != nil {
		return err
	}
	// At the target is "done" only when no upgrade is in progress. With a
	// marker it stopped after the kubelet swap and before its health gate:
	// finish it, or the marker outlives it and the NEXT upgrade's rollback
	// lands on this one's baseline, two versions back.
	//
	// And only on this build: a new build of the same Kubernetes release
	// leaves the kubelet version where it was, so that alone would skip every
	// build update. The build match needs the marker check beside
	// it: stratum is installed first, so a killed run leaves them matching.
	//
	// An upgrade that cannot tell goes ahead. --check must
	// not answer then: "needs the upgrade" is what makes the orchestrator
	// drain the node, and a question that could not be asked is no reason to drain it.
	// An error exits 1, which the orchestrator refuses.
	cur, verr := currentNodeVersion()
	if verr != nil && o.Check {
		return fmt.Errorf("stratum upgrade --check: read this node's version: %w", verr)
	}
	if verr == nil && cur == o.Target && marker == nil && !abandonedSameTarget {
		same, berr := installedIsThisBuild()
		switch {
		case berr == nil && same:
			sup, serr := runningSupervisor()
			if serr != nil && o.Check {
				return fmt.Errorf("stratum upgrade --check: tell which stratum the supervisor runs: %w", serr)
			}
			if serr == nil && sup.Stale() {
				fmt.Fprintf(w, "stratum upgrade: %s already at %s on this build, but its supervisor (pid %d) runs %s, not "+
					"the installed stratum\n", hostname, o.Target, sup.PID, sup.Exe)
				// One an earlier run found it cannot replace stays as it is:
				// the handoff would only warn again, and "needs the upgrade"
				// would have the cluster upgrade drain this node on every
				// rerun for that. The warning is all there is to do, so
				// --check, --dry-run and the run all end with it, and nothing
				// restarts that needs the prerequisites.
				if knownUnreplaceable(sup) {
					fmt.Fprintf(w, "  WARN: the supervisor (pid %d) predates restarting itself in place, and systemd does "+
						"not run it, so no upgrade can hand it over; %s\n", sup.PID, restartByHand(sup))
					return nil
				}
				// The handoff restarts every component on the installed
				// stratum, as an upgrade does, so the node must pass the same
				// prerequisites first; --check answers for it too, or the
				// cluster preflight would pass a node its upgrade restarts
				// unconfined.
				if err := checkNodePrereqs(w, false); err != nil {
					return err
				}
				if o.Check {
					return ErrUpgradeNeeded
				}
			} else {
				fmt.Fprintf(w, "stratum upgrade: %s already at %s on this build; nothing to do\n", hostname, o.Target)
				if o.Check {
					return nil
				}
			}
			if o.DryRun {
				previewSupervisorHandoff(w, false)
				return nil
			}
			_, err := handOverAndGate(w, "upgrade", func() error { return nodeGate(isCP, nodeName(st)) })
			return err
		case berr != nil && o.Check:
			return fmt.Errorf("stratum upgrade --check: compare the installed stratum with this build: %w", berr)
		case berr != nil:
			fmt.Fprintf(w, "  WARN: could not compare %s's stratum build with this one: %v; upgrading anyway\n", hostname, berr)
		case o.Check:
			// --check changes nothing; say what it found, not what an upgrade
			// would do.
			fmt.Fprintf(w, "stratum upgrade: %s is at %s on another stratum build; it needs the upgrade\n", hostname, o.Target)
		default:
			fmt.Fprintf(w, "stratum upgrade: %s is at %s on another stratum build; installing this one\n", hostname, o.Target)
		}
	}
	// The node refuses its own upgrade before anything on it changes: the
	// version check above said it needs this build, and the prerequisites
	// say it may not take it. A node already on this build never reaches
	// here: one whose supervisor is current passes whatever its policy looks
	// like, and one whose supervisor is stale was checked above.
	if err := checkNodePrereqs(w, marker != nil); err != nil {
		return err
	}
	if o.Check {
		return ErrUpgradeNeeded
	}
	if marker != nil {
		verb := "resuming"
		if o.DryRun {
			verb = "would resume"
		}
		fmt.Fprintf(w, "stratum upgrade: %s %s the interrupted upgrade to %s (backup set %s)\n", hostname, verb, marker.Target, marker.TS)
	}

	if o.DryRun {
		if isCP {
			fmt.Fprintf(w, "would upgrade %s (controller) -> %s: re-render %v; swap host binaries; restart %v; "+
				"then restart the supervisor on the installed stratum, or the operator alone if it is not restarted\n", hostname, o.Target, cpComponents(st), hostProcs)
		} else {
			fmt.Fprintf(w, "would upgrade %s (worker) -> %s: swap host binaries; restart %v; then restart the supervisor on "+
				"the installed stratum\n", hostname, o.Target, hostProcs)
		}
		return nil
	}

	src, err := resolvePayload(w, o.From, hostProcs)
	if err != nil {
		return err
	}
	swapBins, err := hostSwapOrder(src.Dir)
	if err != nil {
		return err
	}

	if isCP && !o.SkipSnapshot {
		k8sutil.Step(w, "Pre-upgrade etcd snapshot")
		if err := saveUpgradeSnapshot(w, etcdsnap.DefaultSnapshotPath(etcdsnap.NowStamp())); err != nil {
			fmt.Fprintf(w, "  WARN: etcd snapshot failed: %v\n", err)
		}
	}

	// A resume keeps the interrupted run's stamp and starts from every backup
	// it took, so a failure restores what that run changed even when this run
	// fails before revisiting it. A fresh upgrade records itself
	// before its first change.
	ts, backups, hostSwapped, err := startBackupSet(w, marker, o.Target, sets)
	if err != nil {
		return err
	}
	fail := func(cause error) error { return rollbackNode(w, cause, backups, hostSwapped, ts) }

	// Self-install FIRST (spec): a crash anywhere after this point leaves the
	// on-disk binary and the manifests converging on the same version at the
	// next start. The operator is NOT restarted until the node gate passes.
	k8sutil.Step(w, "Install stratum "+version.StratumVersion)
	// Before this, a fresh run has changed nothing and returns plainly. A
	// resume already holds what the killed run changed, so it rolls that back
	// rather than leave the node half-upgraded without a word.
	installFailed := func(err error) error {
		err = fmt.Errorf("install stratum: %w", err)
		if len(backups) > 0 {
			return fail(err)
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return installFailed(err)
	}
	bak, err := backupAndInstall(w, stratumInstallPath, exe, ts)
	if err != nil {
		return installFailed(err)
	}
	if bak != "" {
		backups = addBackup(backups, backupItem{name: "stratum", bak: bak, dst: stratumInstallPath})
	}

	if src.Images != "" {
		k8sutil.Step(w, "Import payload images")
		// Old images stay imported and pinned on purpose — they are what a
		// rollback boots from on an airgapped node.
		if err := importImages(context.Background(), w, src.Images); err != nil {
			return fail(err)
		}
	}

	if isCP {
		k8sutil.Step(w, "Back up static pod manifests")
		bak, err := backupManifests(ts)
		if err != nil {
			return fail(fmt.Errorf("back up manifests: %w", err))
		}
		backups = addBackup(backups, backupItem{name: manifestsItem, bak: bak, dst: staticpod.Dir(), dir: true})

		// The same idempotent PKI pass every `stratum up` runs.
		// Without it an upgraded control plane kept serving a certificate the
		// new release's spec says is wrong: a custom service CIDR's Service IP
		// missing from apiserver.crt. Only a leaf that lacks a required SAN is
		// reissued, and a superset is never touched, so a rollback to the old
		// binaries stays safe.
		k8sutil.Step(w, "Refresh control-plane certificates")
		apiserverReissued, err := refreshCerts(w, st, hostname)
		if err != nil {
			return fail(fmt.Errorf("refresh control-plane certificates: %w", err))
		}

		// Before kube-scheduler is re-rendered: its new manifest names this file.
		if err := writeSchedulerConfig(); err != nil {
			return fail(fmt.Errorf("write kube-scheduler's kubeconfig: %w", err))
		}

		// Before kube-vip is re-rendered: its new manifest names this file.
		if st.VIP.Address != "" {
			if err := writeKubeVipConfig(); err != nil {
				return fail(fmt.Errorf("write kube-vip's kubeconfig: %w", err))
			}
		}

		// Before the apiserver is re-rendered, so the pod the new image
		// recreates reads this release's Pod Security pin: otherwise the pod keeps
		// the old one until some later restart happens to recreate it.
		prof, err := profile.ForNode(st.ProfileName())
		if err != nil {
			return fail(fmt.Errorf("resolve this node's profile to refresh its configs: %w", err))
		}
		if len(prof.ConfigFiles) > 0 {
			k8sutil.Step(w, "Refresh the "+prof.Name+" profile's configs")
			items, err := refreshProfileConfigs(w, ts, prof)
			for _, it := range items {
				backups = addBackup(backups, it)
			}
			if err != nil {
				return fail(fmt.Errorf("refresh the %s profile's configs: %w", prof.Name, err))
			}
		}

		apiserverRecreated := false
		for _, name := range cpComponents(st) {
			k8sutil.Step(w, "Upgrade static pod "+name)
			// staticpod.Start, not Restart: Start preserves the restarted-at
			// annotation, so a component already at the target re-renders
			// byte-identically and kubelet leaves its pod alone — the resume
			// property. The image-tag change is the content change
			// that makes kubelet recreate a not-yet-upgraded pod.
			mark, merr := bootClock()
			if merr != nil {
				return fail(fmt.Errorf("%s: %w", name, merr))
			}
			changed, err := startReportingChange(name)
			if err != nil {
				return fail(err)
			}
			if name == "kube-apiserver" {
				apiserverRecreated = changed
			}
			if changed {
				if err := waitComponentReplace(name, mark, componentGateTimeout); err != nil {
					return fail(fmt.Errorf("%s gate: %w", name, err))
				}
			}
			if err := gateComponent(name); err != nil {
				return fail(fmt.Errorf("%s gate: %w", name, err))
			}
		}
		// A resumed upgrade re-renders an already-upgraded manifest byte-for-byte
		// and kubelet leaves that pod alone, so a reissued cert would sit unused.
		// Restart the apiserver onto it, but only then: when Start changed the
		// manifest, kubelet has already recreated the pod on the new cert, and a
		// second restart would be a second apiserver outage.
		if restartForReissuedCert(apiserverReissued, apiserverRecreated) {
			k8sutil.Step(w, "Restart kube-apiserver onto its reissued certificate")
			mark, merr := bootClock()
			if merr != nil {
				return fail(fmt.Errorf("kube-apiserver: %w", merr))
			}
			if err := restartStaticPod("kube-apiserver"); err != nil {
				return fail(err)
			}
			if err := waitComponentReplace("kube-apiserver", mark, componentGateTimeout); err != nil {
				return fail(fmt.Errorf("kube-apiserver gate: %w", err))
			}
			if err := gateComponent("kube-apiserver"); err != nil {
				return fail(fmt.Errorf("kube-apiserver gate: %w", err))
			}
		}
	}

	// Host binaries AFTER the CP pods (k8s skew policy: kubelet must never be
	// newer than its apiserver; a kubelet restart does not kill running
	// containers, so the upgraded CP pods ride through it).
	k8sutil.Step(w, "Swap host binaries")
	hostSwapped = true
	for _, b := range swapBins {
		dst := filepath.Join(version.BinDir, b)
		bak, err := backupAndInstall(w, dst, filepath.Join(src.Dir, b), ts)
		if err != nil {
			return fail(fmt.Errorf("swap %s: %w", b, err))
		}
		if bak != "" {
			backups = addBackup(backups, backupItem{name: b, bak: bak, dst: dst})
		}
	}
	// Bounce each host process onto its swapped (same-path) binary. Args and
	// configs are NOT touched yet: a plain restart reuses the running (old-binary)
	// supervisor's stored spec, so nothing here changes the node's on-disk config
	// or argv — keeping this a fully reversible step ahead of the health gate.
	for _, u := range hostProcs {
		if unitActive(context.Background(), u) {
			if err := restartUnit(u); err != nil {
				return fail(fmt.Errorf("restart %s: %w", u, err))
			}
		}
	}

	k8sutil.Step(w, fmt.Sprintf("Soak %ds, then node health gate", o.SoakSec))
	soak(time.Duration(o.SoakSec) * time.Second)
	if err := nodeGate(isCP, nodeName(st)); err != nil {
		return fail(err)
	}

	// Everything a rollback can undo is done and gated, so the upgrade ends
	// here as far as a resume is concerned: what follows rewrites configs and
	// argv, which no rollback restores, and a resume that failed after it
	// would roll old binaries back under new configs. A run killed
	// past this point re-runs as "already at target".
	//
	// So a marker that will not go is fatal HERE, before anything
	// irreversible: left in place past this point it would do exactly that.
	// Stopped here, a re-run resumes safely.
	if err := removeUpgradeMarker(); err != nil {
		return fmt.Errorf("stratum upgrade: %s passed its health gate on the new binaries, but %w; "+
			"nothing past the gate was applied (host configs and argv unchanged) — fix that, then re-run `stratum upgrade` to finish",
			hostname, err)
	}

	// The node is proven healthy on the new binaries. ONLY NOW apply the new
	// host-process args/config: refresh the on-disk node configs from
	// the NEW binary (containerd config, kubelet.conf), reload each host
	// process with the NEW binary's argv via the supervisor's reload op, and
	// only then the legacy-config heal (applyHostConfigs). This runs post-gate
	// without automatic rollback:
	//   - It is destructive and hard to reverse (it rewrites on-disk config and
	//     swaps stored argv), so it must never run on a path a rollback undoes —
	//     rollback restores the old binaries but not these config/arg changes.
	//   - An old-binary supervisor without the reload op (a cross-version upgrade)
	//     answers "unknown op reload". Only that compatibility case defers argv
	//     changes to the next `stratum up`/reboot. Other reload errors are fatal,
	//     even if the old process still passes a liveness check.
	if err := applyHostConfigs(w, st); err != nil {
		return err
	}

	// Operator restart LAST, only after the gate passed — the disruptive
	// re-exec of the (now new) stratum binary happens on a proven-healthy
	// node. Best-effort: the node is upgraded either way. It runs on the way
	// out, so the supervisor handoff goes first: a handoff that replaces the
	// supervisor restarts the operator with every other component, and a
	// second restart would only bounce it twice. Any other way out restarts
	// it here.
	operatorPending := true
	defer func() {
		if operatorPending && unitExists("operator") {
			k8sutil.Step(w, "Restart operator")
			if err := restartUnit("operator"); err != nil {
				fmt.Fprintf(w, "  WARN: operator restart failed: %v\n", err)
			}
		}
	}()

	// Reload changes the running node after the earlier health gate. Never report
	// success with a host process still down. Config/argv have already changed,
	// so the old binaries cannot safely
	// be restored here; leave the new version installed and report the failure.
	if err := nodeGate(isCP, nodeName(st)); err != nil {
		return fmt.Errorf("post-upgrade host reload health gate (new version remains installed; no rollback attempted): %w", err)
	}

	newVer, err := currentNodeVersion()
	if err != nil {
		return fmt.Errorf("verify node version after upgrade: %w", err)
	}
	if newVer != o.Target {
		return fmt.Errorf("post-upgrade kubelet reports %s, expected %s — payload/binary mismatch", newVer, o.Target)
	}
	fmt.Fprintf(w, "stratum upgrade: %s now at %s (rollback with: stratum rollback)\n", hostname, newVer)
	stampUpgraded(w, st)

	// A controller's upgrade restarts the API server the orchestrator uses; on
	// a one-node cluster the orchestrator may not survive it, and the node
	// would stay cordoned for good. Best-effort: the upgrade landed.
	//
	// The Node's registered name (nodeName), as the health gates and the stamp
	// use, not the OS hostname: a kubelet started with --hostname-override
	// registers under another name, and a lookup by the OS name would find no
	// node and leave the registered one cordoned.
	if isCP {
		node := nodeName(st)
		if done, err := uncordonUpgraded(node, o.Target); err != nil {
			fmt.Fprint(w, uncordonWarning(node, err))
		} else if done {
			fmt.Fprintf(w, "stratum upgrade: uncordoned %s (the cluster upgrade that cordoned it may have halted)\n", node)
		}
	}

	// Prune stale backup sets now that the upgrade has SUCCEEDED — never before
	// or around a failure, where a rollback still needs them. Best-effort: an
	// upgrade that already landed must not be reported as failed because old
	// backups could not be removed. keepBackupSets leaves the newest sets
	// (including the one just written) so `stratum rollback` still works.
	// Derive host-binary ownership from the payload we installed, not arbitrary
	// filenames discovered in the destination directory.
	var hostPatterns []backupPattern
	for _, name := range swapBins {
		hostPatterns = append(hostPatterns, backupPattern{prefix: name + ".bak-"})
	}
	for _, scope := range []struct {
		dir      string
		patterns []backupPattern
	}{
		{filepath.Dir(stratumInstallPath), []backupPattern{{prefix: filepath.Base(stratumInstallPath) + ".bak-"}}},
		{version.BinDir, hostPatterns},
		{filepath.Join(version.DataDir, "upgrade"), upgradeDirBackups},
	} {
		if err := pruneOldBackups(scope.dir, keepBackupSets, scope.patterns...); err != nil {
			fmt.Fprintf(w, "  WARN: prune old backups in %s: %v\n", scope.dir, err)
		}
	}
	handedOver, err := handOverAndGate(w, "upgrade", func() error { return nodeGate(isCP, nodeName(st)) })
	operatorPending = !handedOver
	return err
}

// saveUpgradeSnapshot keeps the pre-upgrade snapshot's exact path while routing
// etcdctl's progress through the upgrade's writer. Save adds no upload or pruning
// when those options are left unset.
func saveUpgradeSnapshot(w io.Writer, path string) error {
	_, err := etcdsnap.Save(w, etcdsnap.SaveOptions{Dir: filepath.Dir(path), Name: filepath.Base(path)})
	return err
}

// handOverAndGate hands the node to a supervisor running the installed
// stratum, the last step of an upgrade or a rollback, and gates the node again
// when that restarted every component. The installed build stays either way:
// it already passed its gates, and what failed is only the restart onto it.
//
// It reports whether the supervisor was replaced, a failed gate after it
// included: every component it runs was then restarted with it, so the
// caller restarts none of them again.
func handOverAndGate(w io.Writer, what string, gate func() error) (bool, error) {
	replaced, err := handOverSupervisor(w)
	if err != nil {
		return replaced, fmt.Errorf("%s done, but handing the node to its supervisor failed (the installed build stays; "+
			"nothing was rolled back): %w", what, err)
	}
	if !replaced {
		return false, nil
	}
	if err := gate(); err != nil {
		return true, fmt.Errorf("%s done, but the node health gate after restarting its supervisor failed (the installed "+
			"build stays; nothing was rolled back): %w", what, err)
	}
	return true, nil
}

// RollbackNode restores a coherent .bak-<ts> set written by UpgradeNode — the
// interrupted upgrade's when a marker names one, otherwise the newest; a
// marker whose upgrade changed nothing is removed and nothing is restored —
// manifests, host binaries, and the stratum binary itself —
// then restarts every supervised host unit — including one that was down,
// which is the node it exists to recover — soaks once, health-gates, and
// restarts the operator, unless handing the node to a supervisor on the
// restored build already restarted it. Restoring stratum is what makes the rollback stick: manifests
// re-render from the installed binary's constants on every `stratum up`, so
// leaving the new binary in place would silently re-do the upgrade.
// With EtcdSnapshot set it additionally restores etcd (single-CP only).
func RollbackNode(w io.Writer, o RollbackOptions) error {
	if o.SoakSec == 0 {
		o.SoakSec = 15
	}
	hostname, _ := host.Hostname()
	st, _ := nodestate.LoadState()
	isCP := st != nil && st.Role == "controller"

	// The supervisor only, not every host process (ensureNodeUp): a kubelet
	// crash-looping on an interrupted upgrade's binaries is what this restores.
	// Dry-run writes nothing and skips the guard.
	if !o.DryRun {
		if err := ensureSupervisor("rollback"); err != nil {
			return err
		}
		release, err := lockNode("rollback")
		if err != nil {
			return err
		}
		defer release()
	}
	marker, err := readUpgradeMarker()
	if err != nil {
		return fmt.Errorf("stratum rollback: %w", err)
	}
	sets, err := scanBackupSets()
	if err != nil {
		return fmt.Errorf("stratum rollback: %w; nothing was changed", err)
	}
	// An interrupted upgrade is what a rollback undoes, and its set is the one
	// to restore. One that changed nothing has nothing to undo, and the newest
	// set is then the upgrade before it: rolling that back as well is a second
	// decision, so this one only removes the marker and says so.
	var ts string
	var items []backupItem
	switch {
	case marker.changedNothing(sets) && o.EtcdSnapshot != "":
		// Nothing of the upgrade's to restore, and the etcd restore below is
		// what was asked for. The marker goes once that succeeds (below): gone
		// before a restore that then failed, a retry would roll back the
		// upgrade before this one.
		if o.DryRun {
			fmt.Fprintf(w, "would remove the marker of the interrupted upgrade to %s, which changed nothing\n", marker.Target)
		}
	case marker.changedNothing(sets):
		if o.DryRun {
			fmt.Fprintf(w, "would remove the marker of the interrupted upgrade to %s, which changed nothing; nothing to roll back for it\n", marker.Target)
			return nil
		}
		// Not best-effort: "run it again" is only true once the marker is gone,
		// or the next run lands here again.
		if err := removeUpgradeMarker(); err != nil {
			return fmt.Errorf("stratum rollback: %w; nothing was changed", err)
		}
		fmt.Fprintf(w, "stratum rollback: the interrupted upgrade to %s changed nothing (no backups under %s); removed its marker and restored nothing — "+
			"run `stratum rollback` again to roll back the upgrade before it\n", marker.Target, marker.TS)
		return nil
	case marker != nil:
		ts, items = marker.TS, sets[marker.TS]
	default:
		ts, items = newestSet(sets)
	}
	if len(items) == 0 && o.EtcdSnapshot == "" {
		return fmt.Errorf("no .bak-<ts> backup set found (looked in %s, %s, and %s) — nothing to roll back",
			filepath.Dir(stratumInstallPath), version.BinDir, filepath.Join(version.DataDir, "upgrade"))
	}
	// The supervisor-only guard is for a rollback that restores host
	// binaries: that is what brings a unit crash-looping on the upgrade's
	// binaries back. One whose set restores none — snapshot-only, or an
	// upgrade interrupted before its host swap — has nothing to recover a
	// down unit with, and an etcd restore needs kubelet to cycle the etcd pod.
	// Those keep the upgrade's full guard and refuse up front, rather than
	// failing part-way.
	if !hasHostBackup(items) && !o.DryRun {
		if err := ensureNodeUp("rollback"); err != nil {
			return err
		}
	}

	// Every backup is checked before the first restore, as a resume checks
	// the ones it keeps: one that cannot be read would otherwise be found
	// part-way through, after the others were restored.
	for _, it := range items {
		if _, err := keptBackup(it.bak, it.dir || it.profile); err != nil {
			return fmt.Errorf("stratum rollback: the %s backup of set %s is unusable: %w; nothing was changed", it.name, ts, err)
		}
	}
	hostItems, manifestItems := splitManifests(items)
	var replacing replaced
	if !o.DryRun {
		var err error
		if replacing, err = replacedComponents(manifestItems); err != nil {
			return fmt.Errorf("stratum rollback: %w; nothing was changed", err)
		}
		if len(manifestItems) > 0 {
			if _, err := bootClock(); err != nil {
				return fmt.Errorf("stratum rollback: %w; nothing was changed", err)
			}
		}
	}
	if err := prepareEtcdForRollback(w, manifestBackup(items), o.DryRun); err != nil {
		return fmt.Errorf("stratum rollback: %w", err)
	}
	// Set once the restored node passes its gate; see the restart's comment.
	operatorPending := false
	defer func() {
		if operatorPending && unitExists("operator") {
			if err := restartUnit("operator"); err != nil {
				fmt.Fprintf(w, "  WARN: operator restart failed: %v\n"+
					"         the node is rolled back; the operator's log is in %s\n", err, version.LogDir)
			}
		}
	}()
	if o.DryRun {
		for _, it := range items {
			fmt.Fprintf(w, "would restore %s from %s\n", it.name, it.bak)
		}
	} else if len(items) > 0 {
		k8sutil.Step(w, "Restore backup set "+ts)
		failed := restoreAll(w, hostItems)
		unconfineErr := unconfineForOldManifests(w, manifestBackup(items))
		if unconfineErr != nil {
			fmt.Fprintf(w, "  WARN: the manifests were not restored: %v; under containerd's SELinux labelling the "+
				"restored control plane would run as container_t, without access to its certificates or etcd's data\n", unconfineErr)
		}
		// Restored manifests need no help — kubelet re-reads its staticPodPath
		// on its own. The host binaries and the operator (the stratum binary
		// just changed under it) need a supervisor restart.
		//
		// Every host process the supervisor manages, not only the running ones:
		// one crash-looping on the upgrade's binaries, or stopped, is what this
		// rollback is here to bring back. Restart starts a unit that is down
		// and waits for it, and one that does not come back is an error, not a
		// rollback that "succeeded" around it.
		var restartErrs []error
		for _, u := range hostProcs {
			if !unitExists(u) {
				continue
			}
			if err := restartUnit(u); err != nil {
				restartErrs = append(restartErrs, fmt.Errorf("%s: %w", u, err))
			}
		}
		var mark staticpod.Mark
		if len(manifestItems) > 0 && unconfineErr != nil {
			failed = append(failed, manifestsItem)
		} else if len(manifestItems) > 0 {
			var merr error
			if mark, merr = bootClock(); merr != nil {
				fmt.Fprintf(w, "  WARN: the manifests were not restored: without the boot clock (%v), a restored "+
					"component cannot be told from the one it replaces\n", merr)
				failed = append(failed, manifestsItem)
			} else {
				failed = append(failed, restoreAll(w, manifestItems)...)
			}
		}
		if err := settleStoppedEtcdManifest(); err != nil {
			fmt.Fprintf(w, "  WARN: %v\n", err)
		}
		if len(failed) > 0 {
			notBack := ""
			if err := errors.Join(restartErrs...); err != nil {
				notBack = fmt.Sprintf("; a host process also did not come back on its restored binary (%v)", err)
			}
			return fmt.Errorf("rollback was INCOMPLETE — %s still on the new version (backups: *.bak-%s)%s%s",
				strings.Join(failed, ", "), ts, notBack, stoppedEtcdNote())
		}
		if err := errors.Join(restartErrs...); err != nil {
			return fmt.Errorf("rollback restored backup set %s, but a host process did not come back on it%s: %w", ts,
				stoppedEtcdNote(), err)
		}
		// The restored manifests just changed content under kubelet (new →
		// old image refs), so the CP pods are being recreated while
		// containerd and kubelet were themselves just bounced. Give each
		// component the same patient gate the upgrade path uses BEFORE the
		// node-level gate — a bare 30s healthz check here fails on a loaded
		// node that would have been fine a minute later.
		var gated []string
		if isCP && st != nil {
			gated = cpComponents(st)
		}
		for _, name := range replacing.gateOrder(gated) {
			if err := replacing.wait(name, mark); err != nil {
				return fmt.Errorf("rollback %s gate: %w", name, err)
			}
			if err := gateComponent(name); err != nil {
				return fmt.Errorf("rollback %s gate: %w", name, err)
			}
		}
		k8sutil.Step(w, fmt.Sprintf("Soak %ds, then node health gate", o.SoakSec))
		time.Sleep(time.Duration(o.SoakSec) * time.Second)
		if err := waitNodeGate(isCP, nodeName(st), rollbackGateWindow); err != nil {
			return fmt.Errorf("rollback health gate failed: %w", err)
		}
		// Operator restart last, matching UpgradeNode: the disruptive re-exec
		// of the (just restored) stratum binary happens on a proven-healthy
		// node, on the way out, after the supervisor handoff, and not at all
		// when that handoff replaced the supervisor, which restarts the
		// operator with every other component. Best-effort — the rollback has
		// already stuck either way.
		//
		// Not an error, and not a reason to keep the marker: the node's own
		// processes are restored and gated, and the operator is the cluster's
		// add-on deployer, which a second rollback would not bring back
		// either. Said, though, not dropped.
		operatorPending = true
		ver, _ := currentNodeVersion()
		fmt.Fprintf(w, "stratum rollback: %s restored backup set %s; now at %s\n", hostname, ts, ver)
	}

	if o.EtcdSnapshot != "" {
		nodeIP, err := rollbackNodeIP()
		if err != nil {
			return err
		}
		// Single-CP safety: a snapshot restore resets this member to a fresh
		// single-member cluster, which corrupts a live multi-member quorum. Refuse
		// unless we can PROVE the cluster is single-member — an error (e.g. local
		// etcd down, a plausible rollback state) must not silently bypass the guard.
		ips, err := rollbackMemberIPs(nodeIP)
		if err != nil {
			return fmt.Errorf("refusing etcd restore: cannot verify etcd is single-member (%w) — "+
				"restoring one member of a live multi-member cluster corrupts quorum; "+
				"use `stratum snapshot restore` + `rejoin` for an HA full-cluster restore", err)
		}
		if len(ips) > 1 {
			return fmt.Errorf("refusing etcd restore: %d-member etcd detected — restoring one member's "+
				"snapshot corrupts quorum; use `stratum snapshot restore` + `rejoin` for HA", len(ips))
		}
		if o.DryRun {
			fmt.Fprintf(w, "would restore etcd from %s\n", o.EtcdSnapshot)
			previewSupervisorHandoff(w, restoresStratum(items))
			return nil
		}
		peer := nodestate.EtcdPeerURL(nodeIP)
		// RollbackNode already holds the node maintenance lock (above) for the
		// whole rollback, so the nested restore must not refuse against its
		// own holder; NodeLockHeld lifts only that acquire, and the data lock
		// stays the restore's to take.
		err = rollbackEtcdRestore(w, etcdsnap.RestoreOptions{
			Snapshot:       o.EtcdSnapshot,
			Name:           version.EtcdName,
			PeerURL:        peer,
			InitialCluster: version.EtcdName + "=" + peer,
			NodeLockHeld:   true,
		})
		if errors.Is(err, etcdsnap.ErrWatchersNotRestarted) {
			// The data is restored; failing the rollback here would leave the
			// upgrade marker in place and a retry would restore it again.
			fmt.Fprintf(w, "  WARN: %v\n         restart the ones named above by hand\n", err)
			err = nil
		}
		if err != nil {
			return err
		}
		// The data is restored, and the restore restarted what it could. The other
		// nodes' kubelets and what the operator installed are left to the operator,
		// as after `stratum snapshot restore`; a rollback is single-member, so
		// there are no control planes to rejoin.
		fmt.Fprint(w, "\n"+etcdsnap.StillToDo(false))
	}

	// Only now is the rollback done. The set restored is the interrupted
	// upgrade's when there is one, so the node is back where that upgrade
	// started and the marker goes; any earlier failure above returned with it
	// in place, so a retry restores the same set.
	if marker != nil && !o.DryRun {
		if marker.changedNothing(sets) {
			if err := removeUpgradeMarker(); err != nil {
				return fmt.Errorf("stratum rollback: etcd restored, but %w", err)
			}
			fmt.Fprintf(w, "stratum rollback: the interrupted upgrade to %s changed nothing; removed its marker\n", marker.Target)
		} else {
			clearUpgradeMarker(w)
		}
	}
	if o.DryRun {
		previewSupervisorHandoff(w, restoresStratum(items))
		return nil
	}
	handedOver, err := handOverAndGate(w, "rollback", func() error { return waitNodeGate(isCP, nodeName(st), rollbackGateWindow) })
	operatorPending = operatorPending && !handedOver
	return err
}

// generateCerts is pki.GenerateAllWithWriter, indirected for tests.
var generateCerts = pki.GenerateAllWithWriter

// refreshControlPlaneCerts runs the PKI pass for this control plane with the
// certificate inputs the one constructor (internal/certinputs) builds from this
// node's state and the cluster's network config — the same node address and
// SAN purposes renewal issues under, and renewal's refusal where renewal
// refuses: a dual-stack node whose state lost the recorded v4,v6 pair is
// refused instead of falling back to the primary, which would reissue an
// apiserver certificate without the node's IPv6 address. It reports whether
// apiserver.crt changed, which is the one leaf whose SANs follow the service
// range and the VIP.
func refreshControlPlaneCerts(w io.Writer, st *nodestate.NodeState, hostname string) (bool, error) {
	netc, err := netcfg.Load()
	if err != nil {
		return false, err
	}
	in, err := certinputs.ForNode(st, netc, hostname)
	if err != nil {
		return false, err
	}
	crt := filepath.Join(version.PKIDir, "apiserver.crt")
	before, _ := os.ReadFile(crt)
	if err := generateCerts(w, in.Host, in.NodeIP, in.SANs); err != nil {
		return false, err
	}
	after, err := os.ReadFile(crt)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(before, after), nil
}

// startStaticPod is staticpod.Start, indirected for tests.
var startStaticPod = staticpod.Start

// startReportingChange starts a static pod and reports whether its manifest
// changed on disk, which is when kubelet recreates the pod.
func startReportingChange(name string) (bool, error) {
	before, _ := os.ReadFile(staticpod.Path(name))
	if err := startStaticPod(name); err != nil {
		return false, err
	}
	after, err := os.ReadFile(staticpod.Path(name))
	if err != nil {
		return false, err
	}
	return !bytes.Equal(before, after), nil
}

// restartForReissuedCert: an explicit apiserver restart is needed only when its
// certificate was reissued and nothing else recreated the pod. Pure.
func restartForReissuedCert(reissued, recreated bool) bool { return reissued && !recreated }
