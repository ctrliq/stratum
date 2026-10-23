// Package etcdsnap wraps the host-staged etcdctl for etcd snapshot save (the
// pre-upgrade data-safety floor), over the same mTLS endpoint/flags as
// `stratum etcdctl`, and the host-staged etcdutl for an offline single-member
// snapshot restore (the heavy rollback path).
package etcdsnap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/rollout"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// defaultSnapshotDir is where snapshots are written when nothing else is
// configured (matches the oss-etcd-backup-setup.sh / operator convention).
// Unexported: the effective snapshot directory comes from the resolver
// (cluster.LoadSnapshotConfig), which carries the same fallback, so a new
// caller cannot pick a directory of its own. It stays here for the
// pre-upgrade snapshot name and for Save's fallback when a caller passes no
// directory at all.
const defaultSnapshotDir = "/var/backups/etcd"

// scheduledPrefix begins every scheduled snapshot's name; pre-upgrade snapshots
// use "pre-upgrade-" instead so the two classes prune independently.
const scheduledPrefix = "etcd-snapshot-"

func etcdctl() string { return filepath.Join(version.BinDir, "etcdctl") }

// etcdutl is the offline etcd tool. Since etcd 3.6, snapshot restore lives here
// and not in etcdctl, whose `snapshot` has only `save`.
func etcdutl() string { return filepath.Join(version.BinDir, "etcdutl") }

// DefaultSnapshotPath returns a timestamped pre-upgrade snapshot path. ts must be
// the caller's UTC timestamp (callers pass one so the value is deterministic/testable).
func DefaultSnapshotPath(ts string) string {
	return filepath.Join(defaultSnapshotDir, "pre-upgrade-"+ts+".db")
}

// ScheduledName is the file name for a scheduled snapshot taken on host at ts.
// The host is in the name so 3 CPs uploading to one S3 bucket never clobber each
// other and each prunes only its own.
func ScheduledName(host, ts string) string {
	return fmt.Sprintf("%s%s-%s.db", scheduledPrefix, host, ts)
}

// scheduledHostPrefix is the name prefix for THIS host's scheduled snapshots
// (what Prune keys on so a node only prunes its own copies, locally and in S3).
func scheduledHostPrefix(host string) string { return scheduledPrefix + host + "-" }

// hostSnapshot matches host's scheduled snapshot names: the host prefix, then
// exactly one NowStamp and ".db". A bare prefix match would also take host
// "cp-2"'s snapshots for host "cp"'s, and prune them first in a shared bucket.
func hostSnapshot(host string) func(name string) bool {
	hp := scheduledHostPrefix(host)
	return func(name string) bool {
		ts, ok := strings.CutPrefix(name, hp)
		if !ok {
			return false
		}
		ts, ok = strings.CutSuffix(ts, ".db")
		if !ok || len(ts) != len(stampLayout) {
			return false
		}
		_, err := time.Parse(stampLayout, ts)
		return err == nil
	}
}

const stampLayout = "20060102-150405"

// NowStamp returns a UTC %Y%m%d-%H%M%S stamp for snapshot/.bak naming.
func NowStamp() string { return time.Now().UTC().Format(stampLayout) }

// snapshotContext saves an etcd snapshot of the local member to path, bounded
// by ctx: a context that has already ended launches no etcdctl run, and a run
// that is in flight when ctx ends is killed rather than waited out. The run group joins the snapshot loop at shutdown,
// so an unbounded save would hold that join open — and under systemd's
// KillMode=process a lingering etcdctl is exactly where a snapshot dies
// mid-write. etcdctl's progress goes to w, so SaveContext can supply its
// caller's stream and an MCP save cannot write progress into the JSON-RPC
// transport.
func snapshotContext(ctx context.Context, w io.Writer, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("etcd snapshot save: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, etcdctl(), "snapshot", "save", path)
	// A killed etcdctl must not leave Run waiting on a child that holds the
	// output pipes open (the same reason k8sutil's probes carry a WaitDelay).
	cmd.WaitDelay = 5 * time.Second
	// Injected values win here, which is the OPPOSITE of `stratum etcdctl`
	// (pkg/cli/multicall.go, where the operator is the authority). Deliberate:
	// this runs unattended from the snapshot timer and the pre-upgrade hook, and
	// it means "snapshot THIS node's member" — the filename encodes the host and
	// retention prunes on that prefix. Letting a stray ETCDCTL_ENDPOINTS in the
	// service environment point it elsewhere would file another member's
	// snapshot under this host's name, or stop backups with nobody paged.
	cmd.Env = append(os.Environ(), k8sutil.EtcdctlEnv()...)
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	if err := cmd.Run(); err != nil {
		// etcdctl writes path + ".part" and renames it only after a complete,
		// synced snapshot. A killed process cannot clean up that temporary file.
		// Leave the final path alone: it may contain an earlier valid backup.
		partial := path + ".part"
		if rmErr := os.Remove(partial); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("etcd snapshot save: %w (the partial snapshot it left at %s could not be removed: %v)", err, partial, rmErr)
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("etcd snapshot save: %w (etcdctl: %v)", cerr, err)
		}
		return fmt.Errorf("etcd snapshot save: %w", err)
	}
	return nil
}

// SaveOptions configures a scheduled/on-demand snapshot with optional S3 offload
// and retention.
type SaveOptions struct {
	Dir    string           // local dir (default defaultSnapshotDir)
	Host   string           // node name for the file name (default host.Hostname, short)
	Name   string           // explicit file name (default ScheduledName(host, now))
	S3     *objstore.Target // optional off-box copy (nil / !Enabled = local only)
	Retain int              // keep newest N of THIS host's scheduled snapshots (0 = keep all)
	// PruneWarn, when set, receives each failed prune as data while the save
	// still prints it: a failed prune is a warning beside the returned path,
	// never a save error, since the snapshot itself succeeded. A caller that
	// assembles a result from data (the snapshot_save tool) collects the
	// failures here instead of matching the printed text; the CLI and the
	// scheduled loop leave it nil and keep reading the printed line.
	PruneWarn func(error)
}

// Save takes a local snapshot, optionally uploads it to S3, then prunes both
// locations to Retain. Returns the local path. Its warnings go through the
// writer it is handed first: the CLI passes os.Stdout byte-identically, the
// scheduled loop the daemon's log stream, and the snapshot_save tool a buffer.
func Save(w io.Writer, o SaveOptions) (string, error) { return SaveContext(context.Background(), w, o) }

// SaveContext is Save bounded by ctx: the etcdctl run and the S3 upload it
// starts are killed when ctx ends, so a shutdown joins it promptly instead of
// waiting a snapshot out.
func SaveContext(ctx context.Context, w io.Writer, o SaveOptions) (string, error) {
	dir := o.Dir
	if dir == "" {
		dir = defaultSnapshotDir
	}
	hn := o.Host
	if hn == "" {
		// Short name (host.Hostname strips the domain) to match the naming used
		// by `snapshot prune`, rejoin, and the rest of the codebase — an FQDN here
		// would make manual prune (which keys on the short name) never match.
		hn, _ = host.Hostname()
	}
	name := o.Name
	if name == "" {
		name = ScheduledName(hn, NowStamp())
	}
	path := filepath.Join(dir, name)
	if err := snapshotContext(ctx, w, path); err != nil {
		return "", err
	}
	if o.S3.Enabled() {
		if err := o.S3.Upload(ctx, path, name); err != nil {
			return path, fmt.Errorf("snapshot saved locally (%s) but S3 upload failed: %w", path, err)
		}
	}
	if o.Retain > 0 {
		// A failed prune is a warning, not an error: the snapshot itself
		// succeeded. Each failure is printed through the writer the save was
		// handed for the scheduled loop and `stratum snapshot save`, whose
		// output must not change, and handed to PruneWarn as data for callers
		// that report it beside the path.
		for _, perr := range PruneScheduledContext(ctx, dir, hn, o.Retain, o.S3) {
			fmt.Fprintf(w, "  WARN: %v\n", perr)
			if o.PruneWarn != nil {
				o.PruneWarn(perr)
			}
		}
	}
	return path, nil
}

// PruneScheduled keeps the newest `keep` of `host`'s scheduled snapshots, locally
// and (if configured) in S3, and returns one error per failed target. Prune
// failures are warnings, not errors (a snapshot already succeeded), but they
// travel as returned data so a caller reports them from the return value
// instead of matching printed text. keep <= 0 is a no-op; a nil result means
// every target was pruned.
func PruneScheduled(dir, host string, keep int, s3 *objstore.Target) []error {
	return PruneScheduledContext(context.Background(), dir, host, keep, s3)
}

// PruneScheduledContext bounds remote retention by the save's context, so
// shutdown can also interrupt an S3 list or delete after upload completes.
// Each returned error names the target that failed; nothing is printed.
func PruneScheduledContext(ctx context.Context, dir, host string, keep int, s3 *objstore.Target) []error {
	if ctx.Err() != nil || keep <= 0 {
		return nil
	}
	owned := hostSnapshot(host)
	var errs []error
	if err := Prune(dir, owned, keep); err != nil {
		errs = append(errs, fmt.Errorf("local snapshot prune: %w", err))
	}
	if s3.Enabled() {
		if err := s3.Prune(ctx, owned, keep); err != nil {
			errs = append(errs, fmt.Errorf("S3 snapshot prune: %w", err))
		}
	}
	return errs
}

// Prune keeps the newest `keep` local snapshots whose name owned accepts
// (timestamped names sort chronologically). keep <= 0 is a no-op.
func Prune(dir string, owned func(name string) bool, keep int) error {
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
	var names []string
	for _, e := range entries {
		if !e.IsDir() && owned(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) <= keep {
		return nil
	}
	for _, n := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}

// SnapItem is one snapshot in a listing (local or S3).
type SnapItem struct {
	Name     string
	Location string // "local" or "s3"
	Size     int64
	Modified time.Time
}

// List merges the local snapshot dir and (if configured) the S3 target.
func List(dir string, s3 *objstore.Target) ([]SnapItem, error) {
	var items []SnapItem
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, SnapItem{Name: e.Name(), Location: "local", Size: info.Size(), Modified: info.ModTime()})
	}
	if s3.Enabled() {
		objs, err := s3.List(context.Background())
		if err != nil {
			return items, fmt.Errorf("list S3 (local shown): %w", err)
		}
		for _, o := range objs {
			items = append(items, SnapItem{Name: o.Name, Location: "s3", Size: o.Size, Modified: o.Modified})
		}
	}
	return items, nil
}

// RestoreLocalSeed restores snapshotPath as a NEW single-member cluster on THIS
// node — the seed for a full-cluster restore. The
// other control planes then rejoin via ha.RejoinEtcd. Unlike the upgrade rollback
// path it does NOT guard against a prior multi-member cluster: resetting one is
// the whole point. The caller must confirm (--yes) — it stops apiserver+etcd and
// rewrites cluster state. The writer leads, so every "etcd restore:" step and
// warning reaches the caller's stream: the CLI passes os.Stdout byte-identically,
// the MCP handler the buffer its result is built from.
func RestoreLocalSeed(w io.Writer, snapshotPath string) error {
	nodeIP, err := nodestate.NodeIP()
	if err != nil {
		return err
	}
	peer := nodestate.EtcdPeerURL(nodeIP)
	return Restore(w, RestoreOptions{
		Snapshot:       snapshotPath,
		Name:           version.EtcdName,
		PeerURL:        peer,
		InitialCluster: version.EtcdName + "=" + peer,
	})
}

// RestoreOptions describes a single-member etcd restore.
type RestoreOptions struct {
	Snapshot       string // snapshot .db file
	Name           string // etcd member name (default version.EtcdName)
	PeerURL        string // https://<nodeIP>:2380
	InitialCluster string // "<name>=<peerURL>"
	Stamp          string // UTC stamp for the moved-aside data dir
	// NodeLockHeld says the caller already holds the node maintenance lock
	// (internal/nodelock) — the way RollbackNode does for the restore nested
	// in a rollback — so Restore skips that acquire instead of refusing
	// against its own holder. It lifts only the maintenance lock, never the
	// data lock, which Restore always takes itself.
	NodeLockHeld bool
}

// restoreEffects is one restore run's complete set of host effects. The runner
// consults nothing else for its effects, so a run cannot perform a host effect
// through a path the value does not carry, and a harness that builds a zero
// value — or forgets one stub — stops on the unset-effect sentinel instead of
// performing the effect. Paths and filesystem probes belong to the run too,
// so independent fake runs need no shared version-path redirects. Restore is the only place
// that builds the production value, one per call; the copies removal builds the
// subset it needs.
type restoreEffects struct {
	etcdDataDir              string
	etcdutlPath              string
	restoreAcquireNodeLock   func() (func(), error)
	restoreStat              func(string) (os.FileInfo, error)
	restoreCopies            func() ([]RestoreCopy, error)
	restoreAcquireDataLock   func() (func(), error)
	restoreSupervisorRunning func() bool
	restoreStartLockHeld     func() (bool, error)
	restoreStop              func(name string) error
	restoreReadManifest      func(name string) ([]byte, error)
	restoreStart             func(name string, manifest []byte) error
	restoreWaitActive        func(name string) error
	restoreWaitHealthz       func() error
	restorePathExists        func(path string) (bool, error)
	restoreRun               func(cmd *exec.Cmd) error
	restoreNonce             func() string
	// restoreKubelet and restoreOperator are the supervised watchers' per-name
	// restarts, the grain the kubelet and operator restarts were injected at
	// before the restart set. A run value carries the roster effects below, and
	// restartSupervisedWatcher consults these only when it finds no roster on it.
	restoreKubelet         func() (bool, error)
	restoreOperator        func() (bool, error)
	compsStatus            func() (map[string]supervisor.ProcStatus, error)
	compsRestart           func(name string) error
	restoreWorkloads       func() []workloadResult
	seedKubectl            func(ctx context.Context, args ...string) (*exec.Cmd, error)
	baseKubectl            func(ctx context.Context, args ...string) *exec.Cmd
	restoreStaticPod       func(name string) error
	restoreStaticPodExists func(name string) bool
	restoreRemoveAll       func(path string) error
	// restoreMoveDir renames a directory (os.Rename in production). Moving the
	// live data dir aside, and putting it back, are the restore's most
	// destructive steps, so they go through the value like every other effect.
	restoreMoveDir func(from, to string) error

	// The timing knobs the kubectl work reads. Fields, not package variables,
	// so parallel tests shorten them per run instead of swapping shared state.
	kubectlCallTimeout time.Duration
	healthzWindow      time.Duration
	rolloutWait        time.Duration
}

// The production values for the timing knobs. Constants, so no run inherits
// another's shortening and the defaults cannot drift per call site.
const (
	defaultKubectlCallTimeout = 30 * time.Second
	defaultHealthzWindow      = 2 * time.Minute
	defaultRolloutWait        = 60 * time.Second
	// healthzPollInterval is how long the health gate waits between probes.
	healthzPollInterval = 2 * time.Second
)

// errUnsetEffect marks an effect the caller never set. It is what a zero
// restoreEffects answers with, so a test harness that forgets a stub sees the
// sentinel instead of the effect's real host behavior.
var errUnsetEffect = errors.New("unset restore effect")

// unsetEffect is the fail-closed error for the named field: identifiable with
// errors.Is against errUnsetEffect, and naming the field so the harness can
// say which stub is missing.
func unsetEffect(name string) error {
	return fmt.Errorf("%w: restoreEffects.%s is not set", errUnsetEffect, name)
}

// The accessors below are the only way the runner and the rollback touch the
// effects: each fails closed on an unset field, so no code path — the restore
// itself or the rollback it runs — can panic through a nil function or fall
// back to a real host call.

func (fx *restoreEffects) acquireDataLock() (func(), error) {
	if fx.restoreAcquireDataLock == nil {
		return nil, unsetEffect("restoreAcquireDataLock")
	}
	return fx.restoreAcquireDataLock()
}

func (fx *restoreEffects) readManifest(name string) ([]byte, error) {
	if fx.restoreReadManifest == nil {
		return nil, unsetEffect("restoreReadManifest")
	}
	return fx.restoreReadManifest(name)
}

func (fx *restoreEffects) stopPod(name string) error {
	if fx.restoreStop == nil {
		return unsetEffect("restoreStop")
	}
	return fx.restoreStop(name)
}

func (fx *restoreEffects) startPod(name string, manifest []byte) error {
	if fx.restoreStart == nil {
		return unsetEffect("restoreStart")
	}
	return fx.restoreStart(name, manifest)
}

func (fx *restoreEffects) waitActive(name string) error {
	if fx.restoreWaitActive == nil {
		return unsetEffect("restoreWaitActive")
	}
	return fx.restoreWaitActive(name)
}

func (fx *restoreEffects) waitHealthz() error {
	if fx.restoreWaitHealthz == nil {
		return unsetEffect("restoreWaitHealthz")
	}
	return fx.restoreWaitHealthz()
}

func (fx *restoreEffects) pathExists(path string) (bool, error) {
	if fx.restorePathExists == nil {
		return false, unsetEffect("restorePathExists")
	}
	return fx.restorePathExists(path)
}

func (fx *restoreEffects) run(cmd *exec.Cmd) error {
	if fx.restoreRun == nil {
		return unsetEffect("restoreRun")
	}
	return fx.restoreRun(cmd)
}

func (fx *restoreEffects) removeAll(path string) error {
	if fx.restoreRemoveAll == nil {
		return unsetEffect("restoreRemoveAll")
	}
	return fx.restoreRemoveAll(path)
}

func (fx *restoreEffects) move(from, to string) error {
	if fx.restoreMoveDir == nil {
		return unsetEffect("restoreMoveDir")
	}
	return fx.restoreMoveDir(from, to)
}

// ErrWatchersNotRestarted marks a restore that put the data back and left the
// apiserver healthy, but could not restart every component holding a watch
// from before it. Those components keep serving the pre-restore view of the
// cluster until they are restarted, so the restore is not finished.
var ErrWatchersNotRestarted = errors.New("etcd restored, but not every component holding a pre-restore watch or start-up state was restarted")

// restartSupervised restarts a component the supervisor runs as a host process;
// false when the supervisor's roster, successfully read, has none. A roster that
// cannot be read is an error, not an absent component: skipping it would report
// a restore with a stale watch as complete. (comps.UnitExists answers false for
// both, so it is not used.)
func restartSupervised(fx *restoreEffects, name string) (bool, error) {
	if fx.compsStatus == nil {
		return true, unsetEffect("compsStatus")
	}
	roster, err := fx.compsStatus()
	if err != nil {
		return true, fmt.Errorf("ask the supervisor whether this node runs %s: %w", name, err)
	}
	if _, ok := roster[name]; !ok {
		return false, nil
	}
	if fx.compsRestart == nil {
		return true, unsetEffect("compsRestart")
	}
	return true, fx.compsRestart(name)
}

// supervisedRestart is the value's per-name restart for the supervised watcher
// name — the grain the kubelet and operator restarts were injected at before
// the restart set — nil when the value carries none.
func (fx *restoreEffects) supervisedRestart(name string) func() (bool, error) {
	switch name {
	case "kubelet":
		return fx.restoreKubelet
	case "operator":
		return fx.restoreOperator
	}
	return nil
}

// restartSupervisedWatcher restarts the supervised watcher name: through the
// value's per-name restart when it carries no roster effect, and through the
// supervisor's roster (restartSupervised) otherwise.
func (fx *restoreEffects) restartSupervisedWatcher(name string) (bool, error) {
	if fx.compsStatus == nil {
		if restart := fx.supervisedRestart(name); restart != nil {
			return restart()
		}
	}
	return restartSupervised(fx, name)
}

// restartKubelet restarts the supervised kubelet.
func restartKubelet(fx *restoreEffects) (bool, error) { return restartSupervised(fx, "kubelet") }

// restartOperator restarts the supervised stratum operator. It polls rather than
// watching, but it loads the shared requester-signing key and registers its
// webhook endpoint when it starts, and works under a leader lease, so it carries
// state from before the restore until it restarts. Present only on control
// planes.
func restartOperator(fx *restoreEffects) (bool, error) { return restartSupervised(fx, "operator") }

// seedServer is this node's own apiserver. The restore runs on the seed, and
// its kubectl calls must reach it, not whatever admin.conf names: `stratum
// ha-repoint` points admin.conf at the VIP, and before the other control planes
// have rejoined the VIP can be down, or lead to a member that still holds the
// old datastore.
func seedServer() string { return fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort) }

// localKubectl is kubectl against the seed's own apiserver, through the run's
// baseKubectl effect.
func (fx *restoreEffects) localKubectl(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if fx.baseKubectl == nil {
		return nil, unsetEffect("baseKubectl")
	}
	cmd := fx.baseKubectl(ctx, append([]string{"--server", seedServer()}, args...)...)
	// A kubectl killed with ctx must not leave its output pipes held open.
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// The recovery's kubectl calls run under deadlines. A stalled apiserver request
// blocks the first request of a `kubectl` before its own --timeout applies, so
// without one a single hung call would hold the whole recovery, and the recovery
// guidance that follows it, open. Per-run fields on the effects value, so tests
// can shorten them without swapping shared state.

// seedOutput runs one kubectl call against the seed under its own deadline and
// returns its output (combined with stderr when combined is set). A call that
// gets no answer in time is killed and reported as such, never waited on.
func (fx *restoreEffects) seedOutput(ctx context.Context, timeout time.Duration, combined bool, args ...string) ([]byte, error) {
	if fx.seedKubectl == nil {
		return nil, unsetEffect("seedKubectl")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := fx.seedKubectl(ctx, args...)
	if err != nil {
		return nil, err
	}
	var out []byte
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("no answer within %s: %w", timeout, err)
	}
	return out, err
}

// waitSeedHealthz polls the seed's /healthz until it answers "ok" or window
// ends, each probe under the run's kubectlCallTimeout. It is
// k8sutil.WaitHealthzContext against the seed rather than admin.conf's
// server, for the restore and its rollback only: the gate that the restore
// worked, or that the rollback put the original back, has to be about THIS
// apiserver. A VIP that is down would fail it with a healthy seed, and an old
// control plane answering at the VIP would pass it for the seed.
func (fx *restoreEffects) waitSeedHealthz(window, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	for {
		out, err := fx.seedOutput(ctx, fx.kubectlCallTimeout, false, "get", "--raw", "/healthz")
		if errors.Is(err, errUnsetEffect) {
			return err
		}
		if strings.TrimSpace(string(out)) == "ok" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the apiserver at %s did not become healthy within %s", seedServer(), window)
		case <-time.After(interval):
		}
	}
}

// SeedKubectlHint is the command line for an operator to run kubectl against
// the seed by hand: on the seed, and not through admin.conf's server.
func SeedKubectlHint(args string) string {
	return "stratum kubectl --server " + seedServer() + " " + args
}

// restoredStaticPods are the control-plane static pods, besides etcd and the
// apiserver that the restore itself restarts and health-gates, that hold
// watches. The roster is staticpod.Names's full candidate set — kube-vip in
// it whether or not this node renders one, because the repeat tells an
// operator to restart these on every control plane and each control plane
// renders its own. They are restarted from the manifests on disk
// (staticpod.RestartAsIs); a component with no manifest on this node is
// skipped and named, never dropped from the set, so the restore's own account
// contradicts neither the guidance nor what the node printed. A fresh slice
// per call: no run shares a mutable record with another.
func restoredStaticPods() []string {
	var out []string
	for _, n := range staticpod.Names {
		if n == "etcd" || n == "kube-apiserver" {
			continue
		}
		out = append(out, n)
	}
	return out
}

// How the restore restarts a component of its restart set: one value per kind
// of component. The supervisor's watchers are restarted by the supervisor, the
// static pods from the manifests on disk, and the in-cluster workloads with
// kubectl.
const (
	howSupervisor = "supervisor"
	howManifest   = "manifest"
	howRollout    = "rollout"
)

// RestartEntry is one component of the restore's restart set: what the restore
// restarts because it holds a watch, or start-up state, from before the
// restore, and how an operator restarts it by hand.
type RestartEntry struct {
	// Component is the name the restore reports the component under.
	Component string
	// How is how the restore restarts it: one of the how* constants above.
	How string
	// Command is how an operator restarts it by hand. For a workload it is
	// the arguments after the kubectl binary; for everything else a whole
	// command line.
	Command string
	// Repeat says whether the restart belongs in the per-control-plane repeat:
	// the host-side watchers are restarted on every control plane once each
	// has rejoined, the in-cluster workloads once for the cluster.
	Repeat bool

	// display is how prose names the component, when that differs from the
	// name the restore reports it under ("CoreDNS", reported as "coredns").
	// Empty renders Component.
	display string
	// namespace and targets are a workload entry's rollout restart, so the
	// set renders one command per namespace, exactly as the roster's owner
	// does. An entry built by hand carries neither and renders its command
	// whole.
	namespace string
	targets   []string
}

// RestartSet is the restore's restart set, in the order the restore restarts
// them.
type RestartSet []RestartEntry

// RestoreRestartSet is the restore's restart set, built from the rosters that
// own the components: the supervised watchers filtered out of comps.Units —
// every roster unit but containerd, which holds no long watch on the cluster —
// each with the command its roster name has in supervisedWatcherCommands, and
// refused when it has none; the control-plane static pods of staticpod.Names
// besides etcd and the apiserver, which the restore restarts and health-gates
// itself; and rollout.Workloads. A component added to a roster joins the set,
// and through it every surface rendered from the set, without a second list to
// forget.
//
// The set is the candidate roster, as restoredStaticPods is: kube-vip is in
// it whether or not this node renders one, because the
// repeat tells an operator to restart these on every control plane and each
// control plane renders its own; the restore itself skips a component that is
// not on the node. kube-vip restarts from its manifest exactly like the
// controller manager and scheduler, so its pkill belongs in the repeat
// alongside theirs.
//
// The order is dependency order: the roster's first supervised watcher —
// kubelet — goes first, since the static pods are containers it has to
// recreate; the remaining supervised watchers follow them in roster order, and
// the workloads are rolled last, once for the cluster.
func RestoreRestartSet() RestartSet {
	watchers := supervisedWatchers()
	if len(watchers) == 0 {
		panic("etcdsnap: comps.Units has no supervised watcher; the restart set would have nothing to restart on the node")
	}
	set := RestartSet{watchers[0]}
	for _, name := range restoredStaticPods() {
		set = append(set, RestartEntry{
			Component: name,
			How:       howManifest,
			Command:   pkillCommand(name),
			Repeat:    true,
		})
	}
	set = append(set, watchers[1:]...)
	for _, w := range rollout.Workloads {
		set = append(set, RestartEntry{
			Component: w.Name,
			How:       howRollout,
			Command:   "-n " + w.Namespace + " rollout restart " + strings.Join(w.Targets, " "),
			Repeat:    false,
			display:   workloadDisplay[w.Name],
			namespace: w.Namespace,
			targets:   w.Targets,
		})
	}
	return set
}

// supervisedWatcherCommands is the command that restarts a supervised watcher
// of comps.Units by hand, per roster name: the process is killed so the
// supervisor starts it again. The kubelet is matched by executable name; the
// operator by its subcommand and Stratum's flags, which is what makes it the
// operator and more than `pkill -x` can match (comps.OperatorCommandLinePattern).
// A roster watcher with no command here refuses RestoreRestartSet — the
// pattern of comps' predicate table — because a watcher with no command would
// render into every surface with nothing for the operator to run.
var supervisedWatcherCommands = map[string]string{
	"kubelet":  "sudo pkill -x kubelet",
	"operator": "sudo pkill -TERM -f '" + comps.OperatorCommandLinePattern() + "'",
}

// supervisedWatcherDisplay is how prose names a supervised watcher, when that
// differs from the roster name it is restarted under. A watcher the map does
// not name renders under its roster name.
var supervisedWatcherDisplay = map[string]string{
	"operator": "stratum operator",
}

// supervisedWatchers are comps.Units's supervised watchers — every roster unit
// but containerd, which holds no long watch on the cluster — in roster order,
// each with the command its roster name has in supervisedWatcherCommands.
func supervisedWatchers() []RestartEntry {
	var out []RestartEntry
	for _, name := range comps.Units {
		if name == "containerd" {
			continue // containerd holds no long watch on the cluster
		}
		command, ok := supervisedWatcherCommands[name]
		if !ok {
			panic(fmt.Sprintf("etcdsnap: supervised watcher %q has no command to restart it by hand; add one to supervisedWatcherCommands", name))
		}
		out = append(out, RestartEntry{
			Component: name,
			How:       howSupervisor,
			Command:   command,
			Repeat:    true,
			display:   supervisedWatcherDisplay[name],
		})
	}
	return out
}

// workloadDisplay is how prose names a workload, when that differs from the
// name the restore reports it under (rollout.Workload.Name). A workload the
// map does not name is rendered under its roster name.
var workloadDisplay = map[string]string{
	"cilium agent + operator": "Cilium agents and operator",
	"coredns":                 "CoreDNS",
}

// pkillCommand is the command that restarts a static pod's controller by hand:
// kubelet recreates the container once its process exits. The pattern matches
// the executable at the start of the command line, including an absolute path:
// `pkill -x` matches only the first 15 characters of a process name, which
// kube-controller-manager exceeds, and a bare `-f` pattern would match the sudo
// and shell wrappers. The brackets around the pattern's first character keep
// pkill from matching its own invocation.
func pkillCommand(name string) string {
	return `sudo pkill -TERM -f '^([^[:space:]]*/)?[` + name[:1] + `]` + name[1:] + `([[:space:]]|$)'`
}

// RepeatCommands is the set's per-control-plane block: the command each repeat
// entry is restarted by hand with, in roster order.
func (set RestartSet) RepeatCommands() []string {
	var commands []string
	for _, e := range set {
		if e.Repeat {
			commands = append(commands, e.Command)
		}
	}
	return commands
}

// RolloutRestartCommands is the set's once-for-the-cluster block: one
// `kubectl -n <namespace> rollout restart` command per namespace the set's
// workloads live in, in roster order — the same commands their roster owner
// renders (rollout.RestartCommands), so the texts keep the commands the
// operator-facing docs carry.
func (set RestartSet) RolloutRestartCommands(kubectl string) []string {
	var commands []string
	var namespaces []string
	targets := map[string][]string{}
	for _, e := range set {
		if e.How != howRollout {
			continue
		}
		if e.namespace == "" {
			// An entry built by hand carries its command whole, without the
			// kubectl prefix.
			commands = append(commands, kubectl+" "+e.Command)
			continue
		}
		if _, ok := targets[e.namespace]; !ok {
			namespaces = append(namespaces, e.namespace)
		}
		targets[e.namespace] = append(targets[e.namespace], e.targets...)
	}
	for _, ns := range namespaces {
		commands = append(commands, kubectl+" -n "+ns+" rollout restart "+strings.Join(targets[ns], " "))
	}
	return commands
}

// split divides the set into its host-side watchers and its in-cluster
// workloads, in roster order.
func (set RestartSet) split() (watchers, workloads []RestartEntry) {
	for _, e := range set {
		if e.How == howRollout {
			workloads = append(workloads, e)
		} else {
			watchers = append(watchers, e)
		}
	}
	return watchers, workloads
}

// label is how prose names the entry: its display form when the set carries
// one, its component name otherwise.
func (e RestartEntry) label() string {
	if e.display != "" {
		return e.display
	}
	return e.Component
}

// labels is entries as prose names them.
func labels(entries []RestartEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.label()
	}
	return names
}

// englishList is names as prose lists them: comma-separated, the last two
// joined by "and".
func englishList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// RestartedProse is the sentence that says what the restore restarted: the
// set's host-side watchers as this node's, then its workloads as the cluster's,
// in roster order. The CLI success text and the MCP next-step text are built
// from it, so a component added to the set is named without the sentence being
// edited by hand.
func (set RestartSet) RestartedProse() string {
	watchers, workloads := set.split()
	return "This node's " + englishList(labels(watchers)) + ", and the cluster's " +
		englishList(labels(workloads)) +
		", were restarted where the cluster runs them: they held watches, or start-up state, from before the restore."
}

// RolloutRestartCommands is the restart set's once-for-the-cluster block: the
// guidance's commands to restart by hand what the restore rolls.
func RolloutRestartCommands(kubectl string) []string {
	return RestoreRestartSet().RolloutRestartCommands(kubectl)
}

// SeedRolloutRestartCommands is RolloutRestartCommands run against the seed's own
// apiserver, as the guidance before the other control planes rejoin needs.
func SeedRolloutRestartCommands() []string {
	return RestoreRestartSet().RolloutRestartCommands("stratum kubectl --server " + seedServer())
}

// RepeatRestartCommands are the commands to restart a control plane's host-side
// watchers by hand, for the guidance that tells an operator to repeat the
// restore's restarts once every control plane has rejoined: the restart set's
// per-control-plane block. Kubelet recreates the controller containers once
// they exit, and the supervisor restarts kubelet and the operator.
func RepeatRestartCommands() []string { return RestoreRestartSet().RepeatCommands() }

// workloadResult is rollout.Result, for restartClusterWorkloads' callers here.
type workloadResult struct {
	name      string
	present   bool // false when the cluster does not run it
	warn, err error
}

// restartClusterWorkloads rolls every Stratum-owned workload the cluster runs,
// through the seed's own apiserver, under the run's timing knobs.
func (fx *restoreEffects) restartClusterWorkloads() []workloadResult {
	rs := rollout.Roller{Kubectl: fx.seedOutput, Hint: SeedKubectlHint, CallTimeout: fx.kubectlCallTimeout, Wait: fx.rolloutWait}.Restart()
	results := make([]workloadResult, len(rs))
	for i, r := range rs {
		results[i] = workloadResult{r.Name, r.Present, r.Warn, r.Err}
	}
	return results
}

// restartWatchers restarts what holds a watch on the cluster from before the
// restore. etcd's resourceVersions went back to the snapshot's, so a watch or
// informer that had already seen a later one is never told that its view is
// stale: kubelet keeps running the post-snapshot pods and never starts the
// restored ones, Cilium keeps no backend for the kubernetes Service, CoreDNS
// answers headless-Service names from the post-snapshot endpoints, and the
// local-path provisioner never provisions a restored claim. The set is
// what SKE ships and runs a watch in; see rollout.Workloads for what is left out.
//
// It executes the restore's restart set (RestoreRestartSet), dispatching each
// entry by its How: a supervised watcher through restartSupervisedWatcher under
// its roster name, a static pod through the manifest gate, and the set's
// rollout entries in one workload pass. The order is the set's — dependency
// order, so kubelet goes first, since the rest are pods it has to recreate, and
// the controller manager and scheduler come before the in-cluster workloads: a
// rolling DaemonSet or Deployment needs both to act on the new pods, so
// waiting on those rollouts with their stale watches still running could time
// out on the very thing it is waiting for. The static pods are restarted from
// the manifests on disk (RestartAsIs), not re-rendered: an upgrade rollback
// restores older manifests and runs the newer binary. Everything is attempted,
// and the ones that failed are named.
func (fx *restoreEffects) restartWatchers(w io.Writer) error {
	if fx.restoreStaticPodExists == nil {
		return unsetEffect("restoreStaticPodExists")
	}
	if fx.restoreStaticPod == nil {
		return unsetEffect("restoreStaticPod")
	}
	if fx.restoreWorkloads == nil {
		return unsetEffect("restoreWorkloads")
	}
	k8sutil.Step(w, "etcd restore: restarting components that hold pre-restore watches or start-up state")
	var failed []string
	var causes []error
	report := func(name string, present bool, err error) {
		switch {
		case err != nil:
			fmt.Fprintf(w, "  %s: NOT restarted: %v\n", name, err)
			failed = append(failed, name)
			causes = append(causes, err)
		case !present:
			fmt.Fprintf(w, "  %s: not on this node, skipped\n", name)
		default:
			fmt.Fprintf(w, "  restarted %s\n", name)
		}
	}
	rolled := false
	for _, e := range RestoreRestartSet() {
		switch e.How {
		case howSupervisor:
			present, err := fx.restartSupervisedWatcher(e.Component)
			report(e.label(), present, err)
		case howManifest:
			if !fx.restoreStaticPodExists(e.Component) {
				report(e.Component, false, nil)
				continue
			}
			report(e.Component, true, fx.restoreStaticPod(e.Component))
		case howRollout:
			// The set names one entry per workload, but the roller restarts
			// them in one pass; the first rollout entry rolls them all.
			if rolled {
				continue
			}
			rolled = true
			for _, r := range fx.restoreWorkloads() {
				report(r.name, r.present, r.err)
				if r.warn != nil {
					fmt.Fprintf(w, "  WARN: %s: %v\n        it was restarted and may yet finish; the steps below do not depend on it\n", r.name, r.warn)
				}
			}
		}
	}
	if len(failed) > 0 {
		return errors.Join(fmt.Errorf("%w: %s", ErrWatchersNotRestarted, strings.Join(failed, ", ")), errors.Join(causes...))
	}
	return nil
}

// startManifest starts a static pod from the manifest it had before the restore
// stopped it, and renders one only when it had none. Rendering takes
// the images from this binary, and an upgrade rollback runs the newer binary
// against the older manifests it has just restored.
func startManifest(name string, manifest []byte) error {
	if manifest == nil {
		return staticpod.Start(name)
	}
	return staticpod.Put(name, manifest)
}

// newNonce is 8 random hex characters, unique to one restore attempt.
func newNonce() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

// restoreCmd is the etcdutl invocation for a restore, with its progress passed
// through to the operation's writer.
func (fx *restoreEffects) restoreCmd(w io.Writer, o RestoreOptions, token string) *exec.Cmd {
	cmd := exec.Command(fx.etcdutlPath, restoreArgs(o, token, fx.etcdDataDir)...)
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	return cmd
}

// restoreArgs is the etcdutl command line for a single-member restore into the
// etcd data dir.
//
// It uses a UNIQUE cluster token, not the shared version.EtcdClusterToken: etcd
// derives the cluster-id from initial-cluster + token, so reusing the bootstrap
// token reproduces the OLD cluster-id, and any still-running old member (a peer
// not reset yet) would then reach this fresh single-member etcd and crash its
// raft log ("tocommit out of range"). A distinct token gives a distinct
// cluster-id, so stray old peers are rejected. Rejoining members use
// --initial-cluster-state=existing and adopt this cluster-id via member-add, so
// they don't need the token to match.
//
// The token is unique per attempt, not per Stamp: Stamp has second precision,
// so two restores in the same second would otherwise share a cluster-id.
func restoreArgs(o RestoreOptions, token, dataDir string) []string {
	return []string{"snapshot", "restore", o.Snapshot,
		"--data-dir", dataDir,
		"--name", o.Name,
		"--initial-advertise-peer-urls", o.PeerURL,
		"--initial-cluster", o.InitialCluster,
		"--initial-cluster-token", token,
	}
}

// Restore is the disruptive single-CP recovery path: stop apiserver + etcd, move
// the data dir aside, snapshot-restore into a fresh data dir, restart, health-gate.
// The caller MUST guard against running this in a live multi-member cluster
// (restoring one member's snapshot corrupts quorum).
//
// A failure before etcd is running on the restored data is rolled back:
// the original data dir goes back in place, a failed restore is kept
// aside for inspection, and what was stopped is started again. etcd is started
// again only on the original data, and kube-apiserver only once etcd is serving,
// so a node that had no data dir before the restore is left stopped. Once etcd
// is up on the restored data the restore has worked, and a later failure (the
// apiserver not coming back) does not throw it away; it is reported as exactly
// that. Once the apiserver is healthy, the components on this node and in the
// cluster that hold watches from before the restore are restarted
// (restartWatchers); one that cannot be is ErrWatchersNotRestarted.
//
// The moved-aside data dir, and any failed restore, are full copies of etcd's
// data on the same filesystem. They are kept for the operator to inspect and
// are never removed here; delete them once the cluster is verified.
//
// The writer leads: every "etcd restore:" step, warning and rollback note is
// written through it, never to the process's own stdout.
func Restore(w io.Writer, o RestoreOptions) error {
	// The one place a production effects value is built: one per call, with
	// every effect's real implementation wired inline, so a second Restore never
	// inherits the first run's value and no effect stays behind as a package
	// variable a forgotten stub could fall back to.
	fx := &restoreEffects{
		etcdDataDir:              version.EtcdDataDir,
		etcdutlPath:              etcdutl(),
		restoreAcquireNodeLock:   func() (func(), error) { return nodelock.Acquire("etcd restore") },
		restoreStat:              os.Stat,
		restoreSupervisorRunning: comps.SupervisorRunning,
		restoreStartLockHeld:     etcddata.StartLockHeld,
		restoreStop:              func(name string) error { return staticpod.Stop(name, 90*time.Second) },
		restoreReadManifest:      staticpod.Read,
		restoreStart:             startManifest,
		restoreWaitActive:        func(name string) error { return staticpod.WaitActive(name, 90*time.Second) },
		restorePathExists:        pathExists,
		restoreRun:               func(cmd *exec.Cmd) error { return cmd.Run() },
		restoreNonce:             newNonce,
		compsStatus:              comps.Status,
		compsRestart:             comps.Restart,
		baseKubectl:              k8sutil.KubectlContext,
		restoreStaticPod:         staticpod.RestartAsIs,
		restoreStaticPodExists:   staticpod.Exists,
		restoreMoveDir:           os.Rename,
		kubectlCallTimeout:       defaultKubectlCallTimeout,
		healthzWindow:            defaultHealthzWindow,
		rolloutWait:              defaultRolloutWait,
	}
	fx.restoreAcquireDataLock = func() (func(), error) { return etcddata.Lock(fx.etcdDataDir) }
	// The steps that are themselves built from other effects close over fx: by
	// the time any of them runs, the value above is complete.
	fx.seedKubectl = fx.localKubectl
	fx.restoreWaitHealthz = func() error { return fx.waitSeedHealthz(fx.healthzWindow, healthzPollInterval) }
	fx.restoreWorkloads = func() []workloadResult { return fx.restartClusterWorkloads() }
	return runRestore(w, o, fx)
}

// runRestore is the restore sequence Restore hands its effects value to. The
// writer leads, before the options: the run's progress goes through it. Every
// host effect it reaches is a field on fx: one that is not set stops the run
// with the unset-effect sentinel before that effect's host call is made, so a
// zero value can never drive a restore against the node. Filesystem probes
// use the same effects value and captured paths as the destructive steps.
func runRestore(w io.Writer, o RestoreOptions, fx *restoreEffects) error {
	if o.Snapshot == "" || o.PeerURL == "" || o.InitialCluster == "" {
		return fmt.Errorf("etcd restore: snapshot, peer URL and initial cluster are required")
	}
	// The node maintenance lock first, then the data lock: the data lock
	// fences only other writers of the etcd data dir, and an upgrade, rollback
	// or CA rotation takes none, so a restore that took it alone would run
	// beside them. While another maintenance operation holds the lock, the
	// restore refuses before anything is stopped. A caller that already holds
	// it (the restore nested in a rollback) passes NodeLockHeld; the data lock
	// below is always taken.
	if !o.NodeLockHeld {
		if fx.restoreAcquireNodeLock == nil {
			return unsetEffect("restoreAcquireNodeLock")
		}
		releaseNode, err := fx.restoreAcquireNodeLock()
		if err != nil {
			return err
		}
		defer releaseNode()
	}
	release, err := fx.acquireDataLock()
	if err != nil {
		return err
	}
	defer release()
	// The data lock alone leaves a gap: `stratum up` releases it after preparing
	// node state and before its supervisor is visible, and a restore landing there
	// would replace the data dir under a start that then runs with state prepared
	// against the old one. Refused before anything is stopped. The two probes are
	// host reads, so a missing stub is caught before they are consulted.
	if fx.restoreSupervisorRunning == nil {
		return unsetEffect("restoreSupervisorRunning")
	}
	if fx.restoreStartLockHeld == nil {
		return unsetEffect("restoreStartLockHeld")
	}
	if err := etcddata.RefuseStartInProgress("etcd restore", "replacing its etcd data",
		fx.restoreSupervisorRunning, fx.restoreStartLockHeld); err != nil {
		return err
	}
	if o.Name == "" {
		o.Name = version.EtcdName
	}
	if o.Stamp == "" {
		o.Stamp = NowStamp()
	}
	if fx.etcdDataDir == "" {
		return unsetEffect("etcdDataDir")
	}
	if fx.etcdutlPath == "" {
		return unsetEffect("etcdutlPath")
	}
	if fx.restoreStat == nil {
		return unsetEffect("restoreStat")
	}
	if fi, err := fx.restoreStat(o.Snapshot); err != nil {
		return fmt.Errorf("etcd restore: snapshot %s: %w", o.Snapshot, err)
	} else if fi.IsDir() {
		return fmt.Errorf("etcd restore: snapshot %s is a directory (nothing was stopped)", o.Snapshot)
	}
	// A node staged by an older release has no etcdutl. Finding that out
	// once the control plane is down would cost an outage and a rollback.
	if ok, err := fx.pathExists(fx.etcdutlPath); err != nil || !ok {
		if err == nil {
			err = os.ErrNotExist
		}
		return fmt.Errorf("etcd restore: %s: %w; run `stratum stage` to install it (nothing was stopped)", fx.etcdutlPath, err)
	}

	// Both paths are chosen before anything is stopped: NowStamp has second
	// precision, and a retry within the same second would otherwise find the
	// previous attempt's directory in the way of a rename mid-rollback.
	asideCandidate, err := freePath(fx.etcdDataDir+".pre-restore-"+o.Stamp, fx.pathExists)
	if err != nil {
		return fmt.Errorf("etcd restore: %w", err)
	}
	failed, err := freePath(fx.etcdDataDir+".failed-restore-"+o.Stamp, fx.pathExists)
	if err != nil {
		return fmt.Errorf("etcd restore: %w", err)
	}
	r := &restoreRollback{failed: failed, manifests: map[string][]byte{}}
	// Read before anything is stopped: Stop removes the manifest, and starting
	// the component again puts back exactly these bytes.
	if fx.restoreReadManifest == nil {
		return unsetEffect("restoreReadManifest")
	}
	for _, name := range []string{"kube-apiserver", "etcd"} {
		m, err := fx.readManifest(name)
		if err != nil {
			return fmt.Errorf("etcd restore: read the %s manifest: %w (nothing was stopped)", name, err)
		}
		r.manifests[name] = m
	}

	k8sutil.Step(w, "etcd restore: stopping apiserver + etcd")
	// The rename below is destructive: if either component is still running when
	// the data dir moves, the OLD process keeps serving pre-restore data and the
	// post-restore health checks pass against it — reporting success falsely.
	// staticpod.Stop removes the manifest and returns only once the listener has
	// closed and the process is gone. A component is recorded as stopped before
	// the call: a Stop that fails part-way has still removed its manifest, and
	// the rollback must put it back.
	if fx.restoreStop == nil {
		return unsetEffect("restoreStop")
	}
	// The restore nonce is checked here, beside the stop gate and before
	// anything is stopped: past this point a bare return would leave the
	// control plane stopped and the data dir moved aside with no rollback,
	// which is why every failure below goes through r.undo instead.
	if fx.restoreNonce == nil {
		return unsetEffect("restoreNonce")
	}
	// Recovery needs the same start, health gates and directory moves as
	// the forward path. Missing wiring cannot be repaired by undo, so reject
	// it while both pods and the original data are still untouched.
	if fx.restoreStart == nil {
		return unsetEffect("restoreStart")
	}
	if fx.restoreWaitActive == nil {
		return unsetEffect("restoreWaitActive")
	}
	if fx.restoreWaitHealthz == nil {
		return unsetEffect("restoreWaitHealthz")
	}
	if fx.restoreMoveDir == nil {
		return unsetEffect("restoreMoveDir")
	}
	if fx.restoreRun == nil {
		return unsetEffect("restoreRun")
	}
	for _, name := range []string{"kube-apiserver", "etcd"} {
		r.stopped = append(r.stopped, name)
		if err := fx.stopPod(name); err != nil {
			return r.undo(w, fx, fmt.Errorf("etcd restore: could not stop %s: %w (aborted before touching the data dir)", name, err))
		}
	}

	// A missing data dir is a node whose etcd data is gone, which is what a
	// restore is for; anything else that stops the check is not that.
	present, err := fx.pathExists(fx.etcdDataDir)
	if err != nil {
		return r.undo(w, fx, fmt.Errorf("etcd restore: %w (aborted before touching the data dir)", err))
	}
	if present {
		if err := fx.move(fx.etcdDataDir, asideCandidate); err != nil {
			return r.undo(w, fx, fmt.Errorf("etcd restore: move data dir aside: %w", err))
		}
		r.aside = asideCandidate
		fmt.Fprintf(w, "  moved %s -> %s\n", fx.etcdDataDir, asideCandidate)
	}

	k8sutil.Step(w, "etcd restore: restoring snapshot")
	r.ran = true
	token := version.EtcdClusterToken + "-restore-" + o.Stamp + "-" + fx.restoreNonce()
	if err := fx.run(fx.restoreCmd(w, o, token)); err != nil {
		return r.undo(w, fx, fmt.Errorf("etcd snapshot restore: %w", err))
	}

	k8sutil.Step(w, "etcd restore: restarting etcd + apiserver")
	if err := r.start(fx, "etcd"); err != nil {
		return r.undo(w, fx, fmt.Errorf("etcd restore: start etcd on the restored data: %w", err))
	}
	if err := fx.waitActive("etcd"); err != nil {
		return r.undo(w, fx, fmt.Errorf("etcd restore: etcd did not come up on the restored data: %w", err))
	}
	if err := r.start(fx, "kube-apiserver"); err != nil {
		return fmt.Errorf("etcd restore: etcd is running on the restored data, but kube-apiserver did not start: %w (%s)",
			err, r.original())
	}
	if err := fx.waitHealthz(); err != nil {
		return fmt.Errorf("etcd restore: etcd is running on the restored data, but the apiserver is not healthy: %w (%s)",
			err, r.original())
	}
	fmt.Fprintf(w, "  etcd restored from %s; %s\n", o.Snapshot, r.original())
	if r.aside != "" {
		fmt.Fprintln(w, "  once the cluster is verified, `stratum snapshot prune-restores` lists it with every other "+
			"copy restore has kept, and `stratum snapshot prune-restores --yes` removes all of them")
	}
	return fx.restartWatchers(w)
}

// restoreRollback records what Restore has changed, so a failure can put it back.
type restoreRollback struct {
	failed  string   // where a failed restore is kept, chosen up front
	stopped []string // components whose manifests were removed, in stop order
	aside   string   // where the original data dir was moved; "" if there was none
	ran     bool     // etcdutl has run, so the data dir may hold a failed restore
	// manifests holds each component's manifest from before the restore, nil
	// when it had none.
	manifests map[string][]byte
}

// start starts name from the manifest it had before the restore.
func (r *restoreRollback) start(fx *restoreEffects, name string) error {
	return fx.startPod(name, r.manifests[name])
}

// original says where the pre-restore data is.
func (r *restoreRollback) original() string {
	if r.aside == "" {
		return "there was no data dir before the restore"
	}
	return "the original data is kept at " + r.aside
}

// undo puts the node back as it was before the restore began, and wraps cause
// with what it did. A rollback that cannot finish says what is left and where
// the original data is, instead of leaving an operator to discover it.
//
// etcd is started again only on the original data: never beside an etcd that
// would not stop, and never on a failed restore or on nothing, which would
// bootstrap an empty cluster. kube-apiserver is started again only once etcd is
// serving; without it the apiserver cannot come up, and with an etcd that would
// not stop it could serve the failed restore.
func (r *restoreRollback) undo(w io.Writer, fx *restoreEffects, cause error) error {
	k8sutil.Step(w, "etcd restore FAILED — rolling back")
	var problems []string
	note := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	etcdStopFailed, back, kept := false, false, ""
	if r.ran {
		// Stopped unconditionally, not only when its port answers: an etcd that
		// is starting or shutting down holds the data dir without listening, and
		// the data dir must not move under a live process. Stop is a no-op when
		// there is no manifest and no process.
		if err := fx.stopPod("etcd"); err != nil {
			etcdStopFailed = true
			note("stop etcd on the failed restore, so the data dir was left as it is: %v", err)
		} else {
			back, kept = r.putBack(w, fx, note)
		}
	}

	// Before etcdutl runs the data dir is untouched, so etcd restarts on it.
	etcdServing := !slices.Contains(r.stopped, "etcd")
	if slices.Contains(r.stopped, "etcd") && (!r.ran || back) {
		if err := r.start(fx, "etcd"); err != nil {
			note("restart etcd: %v", err)
		} else if err := fx.waitActive("etcd"); err != nil {
			note("etcd did not come back: %v", err)
		} else {
			etcdServing = true
		}
	}
	noOriginal := r.ran && r.aside == "" && !etcdStopFailed
	if slices.Contains(r.stopped, "kube-apiserver") {
		switch {
		case etcdServing:
			if err := r.start(fx, "kube-apiserver"); err != nil {
				note("restart kube-apiserver: %v", err)
			} else if err := fx.waitHealthz(); err != nil {
				note("the apiserver did not come back: %v", err)
			}
		case !noOriginal:
			note("kube-apiserver not restarted, since etcd is not running on the original data")
		}
	}

	if len(problems) > 0 {
		where := "its data dir is unchanged"
		switch {
		case back:
			where = "the original data is back in " + fx.etcdDataDir
		case r.aside != "":
			where = "the original data is at " + r.aside
		case r.ran:
			where = "there was no etcd data dir before the restore"
		}
		if kept != "" {
			where += "; the failed restore is kept at " + kept
		}
		if r.ran && !back && kept == "" {
			// Nothing moved it out, so it is where etcd would start.
			if present, err := fx.pathExists(fx.etcdDataDir); err == nil && present {
				where += "; the failed restore is still in " + fx.etcdDataDir
			}
		}
		state := "the control plane may be stopped"
		if etcdStopFailed {
			state = "etcd may still be running on the failed restore, and kube-apiserver was left stopped"
		}
		return fmt.Errorf("%w; the ROLLBACK DID NOT FINISH (%s), so %s: %s",
			cause, strings.Join(problems, "; "), state, where)
	}
	var msg string
	if noOriginal {
		msg = "nothing to roll back to: there was no etcd data dir before the restore, so etcd and kube-apiserver were left stopped"
	} else {
		msg = "rolled back: " + strings.Join(r.stopped, " and ") + " restarted"
		if r.aside != "" {
			msg += " on the original data, which is back in place"
		}
	}
	if kept != "" {
		msg += "; the failed restore is kept at " + kept
	}
	return fmt.Errorf("%w; %s", cause, msg)
}

// putBack moves a failed restore out of the data dir and the original data
// back in. It reports whether the original is back, and where the failed
// restore went.
func (r *restoreRollback) putBack(w io.Writer, fx *restoreEffects, note func(string, ...any)) (back bool, kept string) {
	vacated := false
	present, err := fx.pathExists(fx.etcdDataDir)
	switch {
	case err != nil:
		note("look for the failed restore: %v", err)
	case !present:
		vacated = true
	default:
		if err := fx.move(fx.etcdDataDir, r.failed); err != nil {
			note("move the failed restore aside: %v", err)
		} else {
			fmt.Fprintf(w, "  moved %s -> %s\n", fx.etcdDataDir, r.failed)
			kept, vacated = r.failed, true
		}
	}
	if vacated && r.aside != "" {
		if err := fx.move(r.aside, fx.etcdDataDir); err != nil {
			note("put the original data back from %s: %v", r.aside, err)
		} else {
			fmt.Fprintf(w, "  moved %s -> %s\n", r.aside, fx.etcdDataDir)
			back = true
		}
	}
	return back, kept
}

// pathExists reports whether path exists. Only not-existing is "no"; any other
// error is returned, not read as either answer.
func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("check %s: %w", path, err)
}

// freePath returns base, or base with the first free numeric suffix when
// something already exists there. Any error other than not-existing is
// returned rather than read as "taken", so an unreadable parent cannot turn
// this into an endless search.
func freePath(base string, exists func(string) (bool, error)) (string, error) {
	p := base
	for i := 1; i <= 100; i++ {
		taken, err := exists(p)
		if err != nil {
			return "", err
		}
		if !taken {
			return p, nil
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fmt.Errorf("no free path for %s after 100 attempts; remove old restore directories", base)
}
