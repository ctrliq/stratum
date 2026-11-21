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

	"github.com/ctrliq/stratum/internal/apparmor"
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

type NodeOptions struct {
	Target       string
	From         string
	SoakSec      int
	SkipSnapshot bool
	// Check: nil when the node already runs this binary's build, ErrUpgradeNeeded if not.
	Check bool
	// Imports the payload images only, so a node still on the old release holds the new add-on images.
	ImportImages bool
	// Pin a cluster upgrade's node to the staged upgrade binary; each node runs its own copy at --from.
	ExpectBuild  string
	ExpectSHA256 string
	DryRun       bool
}

type RollbackOptions struct {
	SoakSec      int
	EtcdSnapshot string // if set, also restore etcd from this snapshot (single-CP only)
	DryRun       bool
}

// A var so tests can redirect the install path.
var stratumInstallPath = version.StratumInstallPath

// Vars so tests can redirect them.
var (
	supervisorRunning = comps.SupervisorRunning
	unitActive        = comps.IsActiveContext
	unitExists        = comps.UnitExists
	restartUnit       = comps.Restart
	reloadUnit        = comps.Reload
	// The surviving supervisor is the old process, so its kubelet-start hook cannot re-stamp.
	stampNode = k8sutil.StampNodeContext
)

// A var so tests can shorten it.
var stampWindow = 30 * time.Second

// The render restarts nothing; the upgrade's own reload is containerd's one restart.
var (
	writeNodeConfigs = func(w io.Writer, st *nodestate.NodeState) error {
		_, err := kuberuntime.WriteNodeConfigsWithWriter(w, st)
		return err
	}
	removeLegacyConfig  = kuberuntime.RemoveLegacyHostConfig
	legacyConfigPresent = kuberuntime.LegacyHostConfig
)

var (
	rollbackNodeIP      = nodestate.NodeIP
	rollbackMemberIPs   = ha.MemberClientIPs
	rollbackEtcdRestore = etcdsnap.Restore
)

// Every wait runs under a deadline, so a stalled kubectl cannot hold a gate open.
var waitHealthzCtx = k8sutil.WaitHealthzContext

// The node gate window is the larger: it follows a containerd and kubelet restart.
var (
	componentHealthzWindow = 2 * time.Minute
	nodeGateHealthzWindow  = apiRetryWindow
)

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

// A var so tests can shorten it.
var rollbackGateWindow = 60 * time.Second

// RunController writes the manifests before starting the supervisor.
func ensureSupervisor(cmd string) error {
	if !supervisorRunning() {
		return fmt.Errorf("stratum %s: no stratum supervisor is running on this node — start it with `stratum up` first; nothing was changed", cmd)
	}
	return nil
}

var (
	unitArgv       = host.UnitArgv
	supervisorArgv = runningSupervisorArgv
)

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

// The handover restarts stratum up on this config; refuse before the upgraded build would.
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
		// A drop-in's ExecStart= outlives the unit; a hand-edited one is rewritten by the next start.
		return fmt.Errorf("stratum upgrade: cannot tell which --config this control plane's unit restarts it with "+
			"(%v), and the upgraded build refuses to start on a missing one. `systemctl cat stratum` shows the unit "+
			"and its drop-ins: remove a drop-in's ExecStart= override and run `sudo systemctl daemon-reload`; a "+
			"hand-edited ExecStart= in the unit itself is rewritten by pointing the node at its config: %s",
			err, restartConfigHint)
	default:
		path, given := configFlag(argv)
		// Refused even when re-exec would map it: the unit is not rewritten until after that.
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
		// A file the earlier build started on can hold a value this build refuses at load.
		if err := restartConfigLoads(path); err != nil {
			return fmt.Errorf("stratum upgrade: this control plane restarts with --config %s (from %s), which the "+
				"upgraded build refuses to start on: %v. Correct the file", path, s.what, err)
		}
	}
	return nil
}

// A var so a test can keep it off the host's default config.
var restartConfigLoads = func(path string) error {
	_, err := cluster.LoadConfig(path)
	return err
}

// `stratum up` refuses over a running node, runs in the foreground, and takes the unit's flags.
var restartConfigHint = "stop the node (`sudo systemctl stop stratum`, or Ctrl-C a `stratum up` started by hand), then " +
	"run `sudo " + version.StratumInstallPath + " up --config <the intended file>` with the unit's other flags (`systemctl cat stratum`), which " +
	"records that path in the unit; it runs in the foreground, so once it is up stop it with Ctrl-C and run " +
	"`sudo systemctl start stratum`"

// The last --config wins, as for the flag parser.
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

// The upgrade restarts every host process, so it refuses while one is down; rollback needs less.
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

// Offers rollback only when an interrupted upgrade left a set that restores a host binary.
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

// Derived from comps.Units so a new component joins the swap without a second list.
var hostProcs = hostProcsFrom(comps.Units)

// The operator is restarted separately, last, after the node is proven healthy.
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

// Applies changed args to running host processes without a reboot.
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

// Refreshes configs from the new binary, reloads, then heals the legacy config — in that order.
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

// Any reload error but ErrReloadUnsupported may mean the new process is not running.
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

// Kubelet must notice the manifest change and recreate the pod from an already-imported image.
const componentGateTimeout = 3 * time.Minute

// Outside staticpod.Dir: kubelet parses every file there and would adopt a backup as a pod.
func manifestBakDir(ts string) string {
	return filepath.Join(version.DataDir, "upgrade", "manifests-bak-"+ts)
}

// staticpod.NamesFor verbatim, so the kube-vip and etcd rules live in one place.
func cpComponents(st *nodestate.NodeState) []string {
	return staticpod.NamesFor(st)
}

// Kubelet is the version marker, so it swaps last; an interrupted upgrade re-runs as a resume.
func hostSwapOrder(src string) ([]string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	var bins []string
	hasKubelet := false
	for _, e := range entries {
		n := e.Name()
		// Leftovers of an interrupted assets.extract, never installable binaries.
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

// Non-fatal; the surviving supervisor is the old process, so no automatic retry helps.
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

func parseVersion(s string) (string, error) { return k8sutil.ParseVersion(s) }

func currentNodeVersion() (string, error) { return k8sutil.KubeletVersion() }

// Stamps sort chronologically; one run per stamp, so mixing runs cannot pair mismatched binaries.
func newestSet(sets map[string][]backupItem) (string, []backupItem) {
	best := ""
	for ts := range sets {
		if ts > best {
			best = ts
		}
	}
	return best, sets[best]
}

// A resume restores exactly what this returns, so errors refuse rather than skip.
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

// Skipped, it would vanish from its set and read as an upgrade that changed nothing.
func notABackup(dir string, e os.DirEntry, want string) error {
	return fmt.Errorf("%s is named as a backup but is not %s; inspect or remove it", filepath.Join(dir, e.Name()), want)
}

// 2 keeps a spare, so a rollback of the rollback still has a set.
const keepBackupSets = 2

var upgradeDirBackups = []backupPattern{{prefix: "manifests-bak-", directory: true}, {prefix: "cisconfig-bak-", directory: true}}

// Pruning touches only artifacts this upgrade owns: binaries regular, only manifests recursive.
type backupPattern struct {
	prefix    string
	directory bool
}

// etcdsnap.NowStamp's format.
const stampLayout = "20060102-150405"

// A marker's stamp lands in paths that are removed and restored from, so it is checked.
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

// Whole sets removed together; only after success, or a rollback could lose the set it needs.
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

// Streams: the stratum binary carries a ~660MB payload, too large to hold in memory.
func copyFile(w io.Writer, src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyOpened(w, in, dst, mode)
}

func copyOpened(w io.Writer, in *os.File, dst string, mode os.FileMode) error {
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if unchanged(dst, mode, fi.Size()) {
		// Hash the descriptor the copy reads, not in.Name(): the path can be swapped.
		if same, err := sameContentFile(in, dst); err == nil && same {
			return nil
		}
	}
	return stageAndRename(w, dst, mode, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

func copyBytes(w io.Writer, data []byte, dst string, mode os.FileMode) error {
	if unchanged(dst, mode, int64(len(data))) {
		if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
			return nil
		}
	}
	return atomicfile.WriteFileWith(dst, data, mode, atomicfileSeams(w))
}

// The caller compares the bytes.
func unchanged(dst string, mode os.FileMode, size int64) bool {
	// Lstat and the whole mode: a symlink or a privileged bit must not match the fast path.
	fi, err := os.Lstat(dst)
	return err == nil && fi.Mode() == mode && fi.Size() == size
}

// Labelling diagnostics go to the operation's writer, never the process's stdout.
var labelLike = selinux.LabelLikeWithWriter

// The wrappers hand these seams to internal/atomicfile, so tests pin one write path.
var (
	syncFile = func(f *os.File) error { return f.Sync() }
	syncDir  = atomicfile.SyncDir
)

func atomicfileSeams(w io.Writer) atomicfile.Seams {
	return atomicfile.Seams{
		SyncFile:  syncFile,
		LabelLike: func(tmp, dst string) error { return labelLike(w, tmp, dst) },
		SyncDir:   syncDir,
	}
}

// atomicfile.WriteFile's streaming form; its failure reports match the shared writer's.
func stageAndRename(w io.Writer, dst string, mode os.FileMode, write func(io.Writer) error) (err error) {
	// Hidden: kubelet parses every non-hidden file in staticPodPath. Unique: two writers tear one temp.
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if renamed {
			return
		}
		// A failure before the rename must leave no staging copy behind.
		if rmErr := os.Remove(tmp.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = fmt.Errorf("%w; %s: %w (%v)", err, tmp.Name(), atomicfile.ErrStrayCopy, rmErr)
		}
	}()
	// Labelled under the temp name, before the bytes: a bad label stops the write before it starts.
	if err := labelLike(w, tmp.Name(), dst); err != nil {
		tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	// A mode set after the fsync is not covered by it; CreateTemp opens 0600, so the chmod is required.
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

// A backup kept from an earlier run under the same stamp is the original; a resume must keep it.
func backupAndInstall(w io.Writer, dst, src, ts string) (string, error) {
	bak := dst + ".bak-" + ts
	kept, err := keptBackup(bak, false)
	if err != nil {
		return "", fmt.Errorf("backup %s: %w", filepath.Base(dst), err)
	}
	hasBackup := kept
	if !kept {
		// Only the open may find dst absent; any other error would install over a file with no way back.
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

var writeProfileConfigs = profilefiles.Write

// The render derives the admission files; a rollback's restored manifests are never re-rendered.
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

// Named as the release before this one names it, so an older rollback recognises it.
func profileConfigBakDir(ts string) string {
	return filepath.Join(version.DataDir, "upgrade", "cisconfig-bak-"+ts)
}

// Names the paths the backup considered, so restore knows absent from never-known.
const profileConfigRecordName = ".cisconfig-paths"

// Staged under a name no scan recognises, and renamed in only once every copy succeeded.
func refreshProfileConfigs(w io.Writer, ts string, prof profile.Profile) ([]backupItem, error) {
	// The record's names are read back as path elements; refuse one that cannot round-trip.
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
		// A rebuilt resume may declare a config file the earlier run's backup cannot cover.
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
	// Written into the staging dir, so the record exists iff the backup does.
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

// A path the record does not cover is left alone: a pre-record backup never copied it.
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

// A pre-record backup covered exactly the legacy three; keep its absence semantics.
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

// Restored last: a restart during a pod replace can leave the old pod half torn down.
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

type replaced map[string]bool

// Gated before anything changes, so the old process cannot answer for the restored one.
func replacedComponents(manifestItems []backupItem) (replaced, error) {
	out := replaced{}
	for _, it := range manifestItems {
		entries, err := os.ReadDir(it.bak)
		if err != nil {
			return nil, fmt.Errorf("read the manifest backup %s: %w", it.bak, err)
		}
		for _, e := range entries {
			// Derived files are configs, not pods; a hidden staging file is an in-flight write.
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

// A node whose state is wrong still has every restored component gated.
func (r replaced) gateOrder(roleComponents []string) []string {
	out := slices.Clone(roleComponents)
	for _, name := range staticpod.Names {
		if r[name] && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Mark taken before the restore, so what answers next is the restored component.
func (r replaced) wait(name string, mark staticpod.Mark) error {
	if !r[name] {
		return nil
	}
	return waitComponentReplace(name, mark, componentGateTimeout)
}

// Appended as each mutation lands, so a rollback restores exactly what changed.
type backupItem struct {
	name    string // named in the INCOMPLETE error if the restore fails
	bak     string
	dst     string
	dir     bool // bak is a manifest-dir snapshot
	profile bool // profile config backup dir; dst unused
}

func restoresStratum(items []backupItem) bool {
	for _, it := range items {
		if it.dst == stratumInstallPath {
			return true
		}
	}
	return false
}

var disableSELinux = kuberuntime.DisableSELinux

// Restored manifests predating spc_t would run as container_t, denied their data.
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

const manifestsItem = "manifests"

func manifestBackup(items []backupItem) string {
	for _, it := range items {
		if it.dir {
			return it.bak
		}
	}
	return ""
}

// Best-effort: stopping at the first failure would widen the damage INCOMPLETE reports.
func restoreAll(w io.Writer, items []backupItem) (failed []string) {
	for _, it := range restoreOrder(items) {
		var err error
		switch {
		case it.dir:
			// The package that renders the set owns what belongs in it.
			err = staticpod.RestoreSnapshot(w, it.bak)
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

// A backup kept from the interrupted run predates its re-render, so it is the original.
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

// Never claim a rollback that did not happen; the error names what is still on the new version.
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

// No per-component soak: the gate proves liveness; soak is a node-level property.
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

// Kubelet registers under NodeState.Hostname via --hostname-override, so the checks read that Node.
func nodeName(st *nodestate.NodeState) string {
	if st != nil && st.Hostname != "" {
		return st.Hostname
	}
	h, _ := host.Hostname()
	return h
}

// A var so tests can shorten it.
var runtimeProbeWindow = 30 * time.Second

func withinWindow(window time.Duration, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	return probe(ctx)
}

// The running-version probes.
var (
	installedVersion  = rtcheck.Installed
	servingContainerd = rtcheck.Serving
	kubeletReports    = func(ctx context.Context, node string) (string, error) {
		// The Node authorizer lets a kubelet read its own Node; a worker has no admin.conf.
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

// Unit active does not prove the installed containerd holds the CRI socket.
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

// nodeInfo refreshes on kubelet's status sync, so a stale value is retried first.
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

func hostUnitsActive(ctx context.Context) error {
	for _, u := range hostProcs {
		if !unitActive(ctx, u) {
			return fmt.Errorf("%s is not active", u)
		}
	}
	return nil
}

// One deadline covers every step: retrying nodeGate would run a full healthz wait per try.
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
	// A step that finds none left fails rather than getting a fresh allowance.
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

// Both upgrade paths refuse a suffixed target through it, so they give the same advice.
func suffixedTarget(target string, isBare func(base string) bool) error {
	base, _, ok := strings.Cut(target, "-")
	if !ok || !isBare(base) {
		return nil
	}
	return fmt.Errorf("--target takes the bare Kubernetes version: use --target %s, not %q (the build suffix is not part of it)",
		base, target)
}

// Says what to do about a --target this binary cannot install.
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

// Refuses an upgrade binary that is not the one the cluster upgrade is rolling out.
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
	// The build ID cannot say a payload is present, and another architecture has no sha256 pin.
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

// Uncordons only a cordon the cluster upgrade made, via the installed binary's path.
func uncordonWarning(node string, err error) string {
	return fmt.Sprintf("  WARN: could not check whether %s is still cordoned: %v; "+
		"if the cluster upgrade cordoned it (you did not cordon it yourself before the upgrade), run: %s kubectl uncordon %s\n",
		node, err, shellQuote(stratumInstallPath), node)
}

// A resuming node has already been changed by the interrupted run, so it warns instead of refusing.
var checkNodePrereqs = func(w io.Writer, resuming bool) error {
	if err := checkSELinuxPrereqs(w, resuming); err != nil {
		return err
	}
	if err := nodeAppArmorPrereq(); err != nil {
		if resuming {
			fmt.Fprintf(w, "  WARN: %v; resuming the interrupted upgrade anyway\n", err)
			return nil
		}
		// A cluster upgrade relays only a node's WARN lines.
		fmt.Fprintf(w, "  WARN: refusing the upgrade: %v\n", err)
		return fmt.Errorf("%w: %w", ErrPrereqMissing, err)
	}
	return nil
}

// checkSELinuxPrereqs is checkNodePrereqs's SELinux half.
func checkSELinuxPrereqs(w io.Writer, resuming bool) error {
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
		// The check's tools failed, not the policy went missing.
		fmt.Fprintf(w, "  WARN: %v; not checking the SELinux prerequisites\n", perr)
		return nil
	}
	if resuming {
		fmt.Fprintf(w, "  WARN: %v; resuming the interrupted upgrade anyway\n", perr)
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPrereqMissing, perr)
}

// Vars so tests can answer for the host's policy checks.
var nodePrereq = selinux.Prerequisite

var nodeAppArmorPrereq = apparmor.Prerequisite

var nodeSELinuxOptions = cluster.NodeSELinuxOptions

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

// Failures before the config/argv refresh roll the node back; a resume re-runs as a resume.
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
	// The upgrade binary is the artifact: a disagreeing --target means the wrong artifact.
	if o.Target != version.K8sVersion {
		return targetMismatch(o.Target, o.From)
	}
	if err := checkPinnedBuild(o); err != nil {
		return err
	}
	if o.ImportImages {
		return importUpgradeImages(w, o.From)
	}
	// Dry-run and --check write nothing, so they skip the guard.
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
	// Held, the lock makes the check fail rather than answer a racing node.
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
	// A binary that does not carry this node's profile cannot render it, so refuse for every role.
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
	// Read after the marker and backups, before the marker is cleared or anything written.
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
			return fmt.Errorf("%w. Then finish the interrupted upgrade to %s, left as it stopped, with that upgrade "+
				"binary, or undo it with `stratum rollback`, and retry", err, marker.Target)
		}
	}
	// An upgrade to this target that recorded nothing cannot vouch for "already on this build".
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
	// At target is done only with no marker and on this build; --check must not answer on doubt.
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
				// The handoff would only warn again, and "needs the upgrade" would drain this node every rerun.
				if knownUnreplaceable(sup) {
					fmt.Fprintf(w, "  WARN: the supervisor (pid %d) predates restarting itself in place, and systemd does "+
						"not run it, so no upgrade can hand it over; %s\n", sup.PID, restartByHand(sup))
					return nil
				}
				// The handoff restarts every component, so the node must pass the same prerequisites.
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
			fmt.Fprintf(w, "stratum upgrade: %s is at %s on another stratum build; it needs the upgrade\n", hostname, o.Target)
		default:
			fmt.Fprintf(w, "stratum upgrade: %s is at %s on another stratum build; installing this one\n", hostname, o.Target)
		}
	}
	// The version check said it needs this build and the prerequisites say it may not take it.
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

	// A resume starts from the interrupted run's backups, so a failure restores what it changed.
	ts, backups, hostSwapped, err := startBackupSet(w, marker, o.Target, sets)
	if err != nil {
		return err
	}
	fail := func(cause error) error { return rollbackNode(w, cause, backups, hostSwapped, ts) }

	// A crash anywhere after this leaves on-disk binary and manifests converging on the same version.
	k8sutil.Step(w, "Install stratum "+version.StratumVersion)
	// Before this, a fresh run has changed nothing and returns plainly.
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
		// Old images stay imported and pinned: they are what a rollback boots from on an airgapped node.
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

		// Only a leaf lacking a required SAN is reissued; a superset is never touched.
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

		// Before the apiserver is re-rendered, so the recreated pod reads this release's Pod Security pin.
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
			// Start preserves the restarted-at annotation, so an already-upgraded pod is left alone.
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
		// Restart onto the reissued cert only when kubelet did not already recreate the pod.
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

	// Host binaries after the CP pods: kubelet must never be newer than its apiserver.
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
	// A plain restart reuses the old supervisor's stored spec, keeping this step fully reversible.
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

	// What follows rewrites configs and argv, which no rollback restores; the marker must go first.
	if err := removeUpgradeMarker(); err != nil {
		return fmt.Errorf("stratum upgrade: %s passed its health gate on the new binaries, but %w; "+
			"nothing past the gate was applied (host configs and argv unchanged) — fix that, then re-run `stratum upgrade` to finish",
			hostname, err)
	}

	// Destructive and hard to reverse, so it runs post-gate without automatic rollback.
	if err := applyHostConfigs(w, st); err != nil {
		return err
	}

	// Runs on the way out, after the handoff, which restarts the operator with the rest.
	operatorPending := true
	defer func() {
		if operatorPending && unitExists("operator") {
			k8sutil.Step(w, "Restart operator")
			if err := restartUnit("operator"); err != nil {
				fmt.Fprintf(w, "  WARN: operator restart failed: %v\n", err)
			}
		}
	}()

	// Config/argv have already changed, so the old binaries cannot safely be restored here.
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

	// The orchestrator may not survive the apiserver restart on a one-node cluster.
	if isCP {
		node := nodeName(st)
		if done, err := uncordonUpgraded(node, o.Target); err != nil {
			fmt.Fprint(w, uncordonWarning(node, err))
		} else if done {
			fmt.Fprintf(w, "stratum upgrade: uncordoned %s (the cluster upgrade that cordoned it may have halted)\n", node)
		}
	}

	// Only after success, so a rollback keeps its sets; best-effort, as the upgrade already landed.
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

func saveUpgradeSnapshot(w io.Writer, path string) error {
	_, err := etcdsnap.Save(w, etcdsnap.SaveOptions{Dir: filepath.Dir(path), Name: filepath.Base(path)})
	return err
}

// Reports whether the supervisor was replaced; every component it runs was then restarted with it.
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

// Manifests re-render from the installed binary on `stratum up`, so the new stratum binary must go.
func RollbackNode(w io.Writer, o RollbackOptions) error {
	if o.SoakSec == 0 {
		o.SoakSec = 15
	}
	hostname, _ := host.Hostname()
	st, _ := nodestate.LoadState()
	isCP := st != nil && st.Role == "controller"

	// A kubelet crash-looping on an interrupted upgrade's binaries is what this restores.
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
	// A nothing-changed interrupted upgrade only removes its marker, leaving the earlier set alone.
	var ts string
	var items []backupItem
	switch {
	case marker.changedNothing(sets) && o.EtcdSnapshot != "":
		// The marker goes only once the etcd restore succeeds, or a retry would roll back first.
		if o.DryRun {
			fmt.Fprintf(w, "would remove the marker of the interrupted upgrade to %s, which changed nothing\n", marker.Target)
		}
	case marker.changedNothing(sets):
		if o.DryRun {
			fmt.Fprintf(w, "would remove the marker of the interrupted upgrade to %s, which changed nothing; nothing to roll back for it\n", marker.Target)
			return nil
		}
		// "Run it again" is only true once the marker is gone, or the next run lands here again.
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
	// No host backup cannot recover a down unit, and an etcd restore needs kubelet: full guard.
	if !hasHostBackup(items) && !o.DryRun {
		if err := ensureNodeUp("rollback"); err != nil {
			return err
		}
	}

	// Every backup is checked before the first restore, as a resume checks the ones it keeps.
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
		// Restarts every managed unit, not only running ones: crash-looping ones are what this recovers.
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
		// The pods are being recreated while the host was just bounced: use the upgrade's patient gate.
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
		// Restart last, on the way out, after the handoff; best-effort, said not dropped.
		operatorPending = true
		ver, _ := currentNodeVersion()
		fmt.Fprintf(w, "stratum rollback: %s restored backup set %s; now at %s\n", hostname, ts, ver)
	}

	if o.EtcdSnapshot != "" {
		nodeIP, err := rollbackNodeIP()
		if err != nil {
			return err
		}
		// Restoring resets this member to a single-member cluster; an error must not bypass the guard.
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
		// RollbackNode holds the node lock, so the nested restore must not refuse on its own holder.
		err = rollbackEtcdRestore(w, etcdsnap.RestoreOptions{
			Snapshot:       o.EtcdSnapshot,
			Name:           version.EtcdName,
			PeerURL:        peer,
			InitialCluster: version.EtcdName + "=" + peer,
			NodeLockHeld:   true,
		})
		if errors.Is(err, etcdsnap.ErrWatchersNotRestarted) {
			// The data is restored; failing here would leave the marker for a retry to restore again.
			fmt.Fprintf(w, "  WARN: %v\n         restart the ones named above by hand\n", err)
			err = nil
		}
		if err != nil {
			return err
		}
		// The other nodes' kubelets and what the operator installed are left to the operator.
		fmt.Fprint(w, "\n"+etcdsnap.StillToDo(false))
	}

	// The set restored is the interrupted upgrade's, so the marker goes; a retry restores the same set.
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

var generateCerts = pki.GenerateAllWithWriter

// A dual-stack node whose state lost the recorded v4,v6 pair is not reduced to its primary.
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

var startStaticPod = staticpod.Start

// Reports whether its manifest changed on disk, which is when kubelet recreates the pod.
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

// Restarts the apiserver only when its cert was reissued and nothing else recreated the pod.
func restartForReissuedCert(reissued, recreated bool) bool { return reissued && !recreated }
