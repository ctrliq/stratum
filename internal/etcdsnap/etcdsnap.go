// Package etcdsnap wraps the host-staged etcdctl/etcdutl for etcd snapshot save and restore.
package etcdsnap

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/boundedfile"
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
	"sigs.k8s.io/yaml"
)

// Unexported: the effective snapshot dir comes from cluster.LoadSnapshotConfig.
const defaultSnapshotDir = "/var/backups/etcd"

// Pre-upgrade snapshots use "pre-upgrade-" instead, so the two classes prune independently.
const scheduledPrefix = "etcd-snapshot-"

func etcdctl() string { return filepath.Join(version.BinDir, "etcdctl") }

// etcdutl holds snapshot restore since etcd 3.6; etcdctl snapshot has only save.
func etcdutl() string { return filepath.Join(version.BinDir, "etcdutl") }

// DefaultSnapshotPath returns a timestamped pre-upgrade snapshot path; ts must be a UTC stamp.
func DefaultSnapshotPath(ts string) string {
	return filepath.Join(defaultSnapshotDir, "pre-upgrade-"+ts+".db")
}

// ScheduledName names a scheduled snapshot; the host in it keeps shared-bucket writes apart.
func ScheduledName(host, ts string) string {
	return fmt.Sprintf("%s%s-%s.db", scheduledPrefix, host, ts)
}

func scheduledHostPrefix(host string) string { return scheduledPrefix + host + "-" }

// hostSnapshot matches exactly one stamp and .db; a bare prefix would prune "cp-2" for "cp".
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

// snapshotContext saves a snapshot of the local member via etcdctl, bounded by ctx.
func snapshotContext(ctx context.Context, w io.Writer, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("etcd snapshot save: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, etcdctl(), "snapshot", "save", path)
	// A killed etcdctl must not leave Run waiting on a child holding the output pipes.
	cmd.WaitDelay = 5 * time.Second
	// Injected env wins, the opposite of `stratum etcdctl`: unattended runs must reach this node.
	cmd.Env = append(os.Environ(), k8sutil.EtcdctlEnv()...)
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	if err := cmd.Run(); err != nil {
		// etcdctl renames .part only after a complete snapshot; the final path may hold a backup.
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

// SaveOptions configures a snapshot save with optional S3 offload and retention.
type SaveOptions struct {
	Dir    string           // local dir (default defaultSnapshotDir)
	Host   string           // node name for the file name (default host.Hostname, short)
	Name   string           // explicit file name (default ScheduledName(host, now))
	S3     *objstore.Target // optional off-box copy (nil / !Enabled = local only)
	Retain int              // keep newest N of THIS host's scheduled snapshots (0 = keep all)
	// PruneWarn receives each failed prune as data; a failed prune is a warning, never a save error.
	PruneWarn func(error)
	// Evidence durably records this save's attempts; the scheduled loop turns it on.
	Evidence bool
	// EvidenceWarnings carries the tick's scheduling warnings, recorded beside the attempts.
	EvidenceWarnings []string
}

// Save takes a local snapshot, optionally uploads it to S3, prunes, returns the local path.
func Save(w io.Writer, o SaveOptions) (string, error) { return SaveContext(context.Background(), w, o) }

// SaveContext is Save bounded by ctx: the etcdctl run and the S3 upload are killed when ctx ends.
func SaveContext(ctx context.Context, w io.Writer, o SaveOptions) (string, error) {
	dir := o.Dir
	if dir == "" {
		dir = defaultSnapshotDir
	}
	hn := o.Host
	if hn == "" {
		// Short name, to match the name `snapshot prune` keys on; an FQDN would never match.
		hn, _ = host.Hostname()
	}
	name := o.Name
	if name == "" {
		name = ScheduledName(hn, NowStamp())
	}
	path := filepath.Join(dir, name)
	var evidence *evidenceRecorder
	if o.Evidence {
		evidence = &evidenceRecorder{warnings: o.EvidenceWarnings}
		defer evidence.report(w)
	}
	if err := snapshotContext(ctx, w, path); err != nil {
		if evidence != nil {
			evidence.recordSave(err)
		}
		return "", err
	}
	if evidence != nil {
		evidence.recordSave(nil)
	}
	remote := o.S3
	var uploadErr error
	if remote.Enabled() {
		upload := remote.Upload(ctx, path, name)
		if evidence != nil {
			evidence.recordUpload(upload)
		}
		if upload != nil {
			uploadErr = fmt.Errorf("snapshot saved locally (%s) but S3 upload failed: %w", path, upload)
			remote = nil
		}
	}
	if o.Retain > 0 && ctx.Err() == nil {
		var pruneErrs []error
		for _, perr := range PruneScheduledContext(ctx, dir, hn, o.Retain, remote) {
			fmt.Fprintf(w, "  WARN: %v\n", perr)
			if o.PruneWarn != nil {
				o.PruneWarn(perr)
			}
			pruneErrs = append(pruneErrs, perr)
		}
		if evidence != nil {
			evidence.recordPrune(pruneErrs)
		}
	}
	return path, uploadErr
}

// PruneScheduled keeps `host`'s newest `keep` snapshots, local+S3; one error per failed target.
func PruneScheduled(dir, host string, keep int, s3 *objstore.Target) []error {
	return PruneScheduledContext(context.Background(), dir, host, keep, s3)
}

// PruneScheduledContext bounds remote retention by ctx; each error names its target.
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

// snapshot-evidence.yaml is the contract with internal/backupevidence; etcdsnap cannot import it.

// evidenceSchemaVersion is the record's schema version, pinned in lockstep with the reader.
const evidenceSchemaVersion = 1

func evidencePath() string {
	return filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml")
}

// snapshotAttempt is one recorded attempt; Result is empty when it succeeded.
type snapshotAttempt struct {
	AttemptedAt time.Time `json:"attemptedAt"`
	Result      string    `json:"result,omitempty"`
}

// IsZero reports no attempt recorded, so a never-run slot is absent from the file.
func (a snapshotAttempt) IsZero() bool {
	return a.AttemptedAt.IsZero() && a.Result == ""
}

// Save and Upload carry separate last-success stamps: a save can succeed locally, fail its copy.
type snapshotEvidence struct {
	SchemaVersion     int             `json:"schemaVersion"`
	Save              snapshotAttempt `json:"save,omitzero"`
	Upload            snapshotAttempt `json:"upload,omitzero"`
	Prune             snapshotAttempt `json:"prune,omitzero"`
	LastSaveSuccess   time.Time       `json:"lastSaveSuccess,omitzero"`
	LastUploadSuccess time.Time       `json:"lastUploadSuccess,omitzero"`
	Warnings          []string        `json:"warnings,omitempty"`
}

// recordSave records a save attempt; the last-success stamp moves only on success.
func (e *snapshotEvidence) recordSave(at time.Time, err error) {
	e.Save = snapshotAttemptOf(at, err)
	if err == nil {
		e.LastSaveSuccess = at
	}
}

func (e *snapshotEvidence) recordUpload(at time.Time, err error) {
	e.Upload = snapshotAttemptOf(at, err)
	if err == nil {
		e.LastUploadSuccess = at
	}
}

// recordPrune records the attempt; retention has no last-success stamp.
func (e *snapshotEvidence) recordPrune(at time.Time, err error) {
	e.Prune = snapshotAttemptOf(at, err)
}

// snapshotAttemptOf shapes one attempt from its outcome.
func snapshotAttemptOf(at time.Time, err error) snapshotAttempt {
	a := snapshotAttempt{AttemptedAt: at}
	if err != nil {
		a.Result = err.Error()
	}
	return a
}

// mutateEvidence runs one mutation under the etcd data lock; a newer schema is refused.
func mutateEvidence(mutate func(*snapshotEvidence)) error {
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return err
	}
	defer release()
	rec, err := loadEvidence()
	if err != nil {
		return err
	}
	mutate(rec)
	return saveEvidence(rec)
}

// loadEvidence reads under the etcd data lock, so the bounded reader must refuse a FIFO promptly.
func loadEvidence() (*snapshotEvidence, error) {
	b, err := boundedfile.Read(context.Background(), evidencePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &snapshotEvidence{}, nil
		}
		return nil, err
	}
	var rec snapshotEvidence
	if err := yaml.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", evidencePath(), err)
	}
	if rec.SchemaVersion > evidenceSchemaVersion {
		return nil, fmt.Errorf("%s: schema version %d is newer than this binary's %d", evidencePath(), rec.SchemaVersion, evidenceSchemaVersion)
	}
	return &rec, nil
}

// saveEvidence persists atomically at 0600: the record names failures; no torn writes.
func saveEvidence(rec *snapshotEvidence) error {
	rec.SchemaVersion = evidenceSchemaVersion
	b, err := yaml.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(evidencePath()), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(evidencePath(), b, 0o600)
}

// evidenceRecorder records a save's attempts as they finish, best-effort; it never fails the save.
type evidenceRecorder struct {
	warnings []string
	err      error
}

// recordSave records the local snapshot attempt's outcome.
func (r *evidenceRecorder) recordSave(err error) {
	r.mutate(func(e *snapshotEvidence) {
		e.recordSave(time.Now(), err)
		e.Warnings = r.warnings
	})
}

func (r *evidenceRecorder) recordUpload(err error) {
	r.mutate(func(e *snapshotEvidence) {
		e.recordUpload(time.Now(), err)
		e.Warnings = r.warnings
	})
}

// recordPrune also appends each failed target to the tick's warnings.
func (r *evidenceRecorder) recordPrune(errs []error) {
	var joined error
	if len(errs) > 0 {
		joined = errors.Join(errs...)
		for _, perr := range errs {
			r.warnings = append(r.warnings, fmt.Sprintf("WARN: %v", perr))
		}
	}
	r.mutate(func(e *snapshotEvidence) {
		e.recordPrune(time.Now(), joined)
		e.Warnings = r.warnings
	})
}

// report warns once when a recording failed; silent evidence loss is what the record prevents.
func (r *evidenceRecorder) report(w io.Writer) {
	if r.err != nil {
		fmt.Fprintf(w, "  WARN: recording snapshot evidence: %v\n", r.err)
	}
}

// mutate remembers the first failure so later attempts still try.
func (r *evidenceRecorder) mutate(mutate func(*snapshotEvidence)) {
	if err := mutateEvidence(mutate); err != nil && r.err == nil {
		r.err = err
	}
}

var removeSnapshot = os.Remove

// Prune keeps the newest `keep` snapshots owned accepts, by modification time, name breaking ties.
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
	type snap struct {
		name string
		mod  time.Time
	}
	var snaps []snap
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !owned(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		snaps = append(snaps, snap{e.Name(), info.ModTime()})
	}
	if len(snaps) <= keep {
		return errors.Join(errs...)
	}
	slices.SortFunc(snaps, func(a, b snap) int {
		return cmp.Or(a.mod.Compare(b.mod), strings.Compare(a.name, b.name))
	})
	for _, s := range snaps[:len(snaps)-keep] {
		if err := removeSnapshot(filepath.Join(dir, s.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

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

// RestoreLocalSeed restores as a new single-member seed; resetting a prior cluster is the point.
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
	// NodeLockHeld says the caller holds the maintenance lock; Restore still takes the data lock.
	NodeLockHeld bool
}

// Every host effect is a field, so a run cannot act through a path the value does not carry.
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
	// Per-name supervised-watcher restarts, consulted only when no roster effect is set.
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
	restoreMoveDir         func(from, to string) error

	// Fields, not package variables, so parallel tests shorten them without shared state.
	kubectlCallTimeout time.Duration
	healthzWindow      time.Duration
	rolloutWait        time.Duration
}

// The production timing knobs; constants, so no run inherits another's shortening.
const (
	defaultKubectlCallTimeout = 30 * time.Second
	defaultHealthzWindow      = 2 * time.Minute
	defaultRolloutWait        = 60 * time.Second
	healthzPollInterval       = 2 * time.Second
)

// errUnsetEffect marks an effect the caller never set, what a zero value answers with.
var errUnsetEffect = errors.New("unset restore effect")

// unsetEffect fails closed, naming the field so a harness sees which stub is missing.
func unsetEffect(name string) error {
	return fmt.Errorf("%w: restoreEffects.%s is not set", errUnsetEffect, name)
}

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

// ErrWatchersNotRestarted marks a healthy restore that could not restart every watcher.
var ErrWatchersNotRestarted = errors.New("etcd restored, but not every component holding a pre-restore watch or start-up state was restarted")

// restartSupervised restarts a supervisor-run component; false when the roster has none.
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

func (fx *restoreEffects) supervisedRestart(name string) func() (bool, error) {
	switch name {
	case "kubelet":
		return fx.restoreKubelet
	case "operator":
		return fx.restoreOperator
	}
	return nil
}

func (fx *restoreEffects) restartSupervisedWatcher(name string) (bool, error) {
	if fx.compsStatus == nil {
		if restart := fx.supervisedRestart(name); restart != nil {
			return restart()
		}
	}
	return restartSupervised(fx, name)
}

func restartKubelet(fx *restoreEffects) (bool, error) { return restartSupervised(fx, "kubelet") }

// The operator carries pre-restore state (signing key, webhook, leader lease), so it restarts.
func restartOperator(fx *restoreEffects) (bool, error) { return restartSupervised(fx, "operator") }

// kubectl must reach this apiserver, not the VIP, which can be down or serve old data pre-rejoin.
func seedServer() string { return fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort) }

// localKubectl is kubectl against the seed's own apiserver.
func (fx *restoreEffects) localKubectl(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if fx.baseKubectl == nil {
		return nil, unsetEffect("baseKubectl")
	}
	cmd := fx.baseKubectl(ctx, append([]string{"--server", seedServer()}, args...)...)
	// A kubectl killed with ctx must not leave its output pipes held open.
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// A stalled apiserver request blocks kubectl before its own --timeout; each call gets a deadline.

// seedOutput runs one kubectl call under its own deadline, returning its output.
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

// waitSeedHealthz polls the seed's /healthz: the gate must be about THIS apiserver, not the VIP.
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

// SeedKubectlHint is the command line for an operator to run kubectl against the seed by hand.
func SeedKubectlHint(args string) string {
	return "stratum kubectl --server " + seedServer() + " " + args
}

// restoredStaticPods lists the static pods, besides etcd and apiserver, that hold watches.
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

// How a restart-set entry is restarted, one value per kind of component.
const (
	howSupervisor = "supervisor"
	howManifest   = "manifest"
	howRollout    = "rollout"
)

// RestartEntry is one component of the restore's restart set.
type RestartEntry struct {
	Component string
	How       string
	// Command is the arguments after kubectl for a workload, a whole command line otherwise.
	Command string
	// Repeat says whether the restart runs on every control plane or once for the cluster.
	Repeat bool

	display string
	// namespace and targets render one command per namespace; a hand-built entry carries neither.
	namespace string
	targets   []string
}

type RestartSet []RestartEntry

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

// Kill patterns so the supervisor restarts each watcher; a roster watcher without one is refused.
var supervisedWatcherCommands = map[string]string{
	"kubelet":  "sudo pkill -x kubelet",
	"operator": "sudo pkill -TERM -f '" + comps.OperatorCommandLinePattern() + "'",
}

var supervisedWatcherDisplay = map[string]string{
	"operator": "stratum operator",
}

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

var workloadDisplay = map[string]string{
	"cilium agent + operator": "Cilium agents and operator",
	"coredns":                 "CoreDNS",
}

// pkill -x matches 15 chars and bare -f hits sudo wrappers; the [x] brackets avoid self-match.
func pkillCommand(name string) string {
	return `sudo pkill -TERM -f '^([^[:space:]]*/)?[` + name[:1] + `]` + name[1:] + `([[:space:]]|$)'`
}

func (set RestartSet) RepeatCommands() []string {
	var commands []string
	for _, e := range set {
		if e.Repeat {
			commands = append(commands, e.Command)
		}
	}
	return commands
}

func (set RestartSet) RolloutRestartCommands(kubectl string) []string {
	var commands []string
	var namespaces []string
	targets := map[string][]string{}
	for _, e := range set {
		if e.How != howRollout {
			continue
		}
		if e.namespace == "" {
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

func (e RestartEntry) label() string {
	if e.display != "" {
		return e.display
	}
	return e.Component
}

func labels(entries []RestartEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.label()
	}
	return names
}

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

// RestartedProse names what the restore restarted, rendered from the set.
func (set RestartSet) RestartedProse() string {
	watchers, workloads := set.split()
	return "This node's " + englishList(labels(watchers)) + ", and the cluster's " +
		englishList(labels(workloads)) +
		", were restarted where the cluster runs them: they held watches, or start-up state, from before the restore."
}

func RolloutRestartCommands(kubectl string) []string {
	return RestoreRestartSet().RolloutRestartCommands(kubectl)
}

func SeedRolloutRestartCommands() []string {
	return RestoreRestartSet().RolloutRestartCommands("stratum kubectl --server " + seedServer())
}

func RepeatRestartCommands() []string { return RestoreRestartSet().RepeatCommands() }

type workloadResult struct {
	name      string
	present   bool // false when the cluster does not run it
	warn, err error
}

func (fx *restoreEffects) restartClusterWorkloads() []workloadResult {
	rs := rollout.Roller{Kubectl: fx.seedOutput, Hint: SeedKubectlHint, CallTimeout: fx.kubectlCallTimeout, Wait: fx.rolloutWait}.Restart()
	results := make([]workloadResult, len(rs))
	for i, r := range rs {
		results[i] = workloadResult{r.Name, r.Present, r.Warn, r.Err}
	}
	return results
}

// Pre-restore watchers are never told their resourceVersion view is stale, so they restart.
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
			// The roller restarts all workloads in one pass; only the first rollout entry rolls.
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

// Rendering takes images from this binary; an upgrade rollback must reuse the older manifests.
func startManifest(name string, manifest []byte) error {
	if manifest == nil {
		return staticpod.Start(name)
	}
	return staticpod.Put(name, manifest)
}

func newNonce() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

func (fx *restoreEffects) restoreCmd(w io.Writer, o RestoreOptions, token string) *exec.Cmd {
	cmd := exec.Command(fx.etcdutlPath, restoreArgs(o, token, fx.etcdDataDir)...)
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	return cmd
}

// A unique token gives a unique cluster-id, so stray old peers are rejected, not crash raft.
func restoreArgs(o RestoreOptions, token, dataDir string) []string {
	return []string{"snapshot", "restore", o.Snapshot,
		"--data-dir", dataDir,
		"--name", o.Name,
		"--initial-advertise-peer-urls", o.PeerURL,
		"--initial-cluster", o.InitialCluster,
		"--initial-cluster-token", token,
	}
}

// Restore restores a snapshot; rolls back only until etcd runs; not for live multi-member clusters.
func Restore(w io.Writer, o RestoreOptions) error {
	// One value per call, wired inline, so no stub lingers as a package variable.
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
	// These close over fx; by the time any of them runs, the value above is complete.
	fx.seedKubectl = fx.localKubectl
	fx.restoreWaitHealthz = func() error { return fx.waitSeedHealthz(fx.healthzWindow, healthzPollInterval) }
	fx.restoreWorkloads = func() []workloadResult { return fx.restartClusterWorkloads() }
	return runRestore(w, o, fx)
}

// runRestore runs the restore against fx; an unset effect stops the run before its host call.
func runRestore(w io.Writer, o RestoreOptions, fx *restoreEffects) error {
	if o.Snapshot == "" || o.PeerURL == "" || o.InitialCluster == "" {
		return fmt.Errorf("etcd restore: snapshot, peer URL and initial cluster are required")
	}
	// Node maintenance lock first: the data lock does not fence upgrades or CA rotation.
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
	// `stratum up` releases the data lock before its supervisor is visible; refuse that window.
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
	// A node staged by an older release has no etcdutl; finding out costs an outage.
	if ok, err := fx.pathExists(fx.etcdutlPath); err != nil || !ok {
		if err == nil {
			err = os.ErrNotExist
		}
		return fmt.Errorf("etcd restore: %s: %w; run `stratum stage` to install it (nothing was stopped)", fx.etcdutlPath, err)
	}

	// Both aside paths are chosen before anything stops: a same-second retry must not collide.
	asideCandidate, err := freePath(fx.etcdDataDir+".pre-restore-"+o.Stamp, fx.pathExists)
	if err != nil {
		return fmt.Errorf("etcd restore: %w", err)
	}
	failed, err := freePath(fx.etcdDataDir+".failed-restore-"+o.Stamp, fx.pathExists)
	if err != nil {
		return fmt.Errorf("etcd restore: %w", err)
	}
	r := &restoreRollback{failed: failed, manifests: map[string][]byte{}}
	// Stop removes the manifest; read it first so the rollback can put back these bytes.
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
	// If the data dir moves under a live process, health checks pass against old data.
	if fx.restoreStop == nil {
		return unsetEffect("restoreStop")
	}
	// Past this point a bare return leaves the control plane stopped; failures go through r.undo.
	if fx.restoreNonce == nil {
		return unsetEffect("restoreNonce")
	}
	// Missing wiring cannot be repaired by undo, so reject it while the node is untouched.
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

	// A missing data dir is what a restore is for; anything else that stops the check is not.
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

// restoreRollback records what Restore changed, so a failure can put it back.
type restoreRollback struct {
	failed    string   // where a failed restore is kept, chosen up front
	stopped   []string // components whose manifests were removed, in stop order
	aside     string   // where the original data dir was moved; "" if there was none
	ran       bool     // etcdutl has run, so the data dir may hold a failed restore
	manifests map[string][]byte
}

func (r *restoreRollback) start(fx *restoreEffects, name string) error {
	return fx.startPod(name, r.manifests[name])
}

func (r *restoreRollback) original() string {
	if r.aside == "" {
		return "there was no data dir before the restore"
	}
	return "the original data is kept at " + r.aside
}

// undo puts the node back; etcd restarts only on the original data, never on a failed restore.
func (r *restoreRollback) undo(w io.Writer, fx *restoreEffects, cause error) error {
	k8sutil.Step(w, "etcd restore FAILED — rolling back")
	var problems []string
	note := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	etcdStopFailed, back, kept := false, false, ""
	if r.ran {
		// An etcd starting or shutting down holds the data dir without listening; stop unconditionally.
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

// pathExists answers "no" only for not-existing; any other error is returned.
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

// freePath returns base, or base with the first free numeric suffix; other errors are returned.
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
