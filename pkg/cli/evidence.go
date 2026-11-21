package cli

import (
	"context"
	"time"

	"github.com/ctrliq/stratum/internal/backupevidence"
)

// BackupProvenance names the source a resolved backup policy value came
// from: the cluster config the controller was started with, the
// operator-managed snapshot.conf that wins outright over it, the built-in
// default, or unknown.
type BackupProvenance string

const (
	// BackupFromClusterConfig: spec.snapshots in the cluster config file the
	// controller was started with.
	BackupFromClusterConfig BackupProvenance = "cluster-config"
	// BackupFromSnapshotConf: the operator-managed snapshot.conf, which wins
	// outright over the cluster config when it exists.
	BackupFromSnapshotConf BackupProvenance = "snapshot-conf"
	// BackupFromDefault: the built-in default the engine applies when
	// nothing configures the value.
	BackupFromDefault BackupProvenance = "default"
	// BackupFromUnknown: the value cannot be determined; the value beside it
	// carries no information, and no default is invented for it.
	BackupFromUnknown BackupProvenance = "unknown"
)

// BackupPolicyState summarizes what a consumer can rely on in the resolved
// backup policy.
type BackupPolicyState string

const (
	// BackupPolicyConfigured: the scheduled snapshot loop runs with at least
	// one operator-configured value.
	BackupPolicyConfigured BackupPolicyState = "configured"
	// BackupPolicyDisabled: the controller never starts the snapshot loop;
	// the other fields still report the configured policy.
	BackupPolicyDisabled BackupPolicyState = "disabled"
	// BackupPolicyUnconfigured: nothing configures snapshots anywhere; the
	// loop runs on the built-in defaults.
	BackupPolicyUnconfigured BackupPolicyState = "unconfigured"
	// BackupPolicyUnknown: the cluster config the controller was started
	// with cannot be read, so the schedule is unknown.
	BackupPolicyUnknown BackupPolicyState = "unknown"
	// BackupPolicyUnreadable: snapshot.conf exists but cannot be read; the
	// loop keeps every snapshot and uploads nothing, and no threshold is
	// invented for what the file would have said.
	BackupPolicyUnreadable BackupPolicyState = "unreadable"
)

// BackupCopyRequirement is what the scheduled loop does with every snapshot
// beyond the local file it writes.
type BackupCopyRequirement string

const (
	// BackupCopyLocalOnly: no S3 target is in force; snapshots stay in the
	// snapshot directory.
	BackupCopyLocalOnly BackupCopyRequirement = "local-only"
	// BackupCopyRemoteRequired: an S3 target is in force; every scheduled
	// snapshot is also uploaded to it.
	BackupCopyRemoteRequired BackupCopyRequirement = "remote-required"
)

// BackupAttemptOutcome is a recorded attempt's typed safe result: what the
// attempt did, never the error text it failed with.
type BackupAttemptOutcome string

const (
	// BackupAttemptSucceeded: the attempt's record carries no failure.
	BackupAttemptSucceeded BackupAttemptOutcome = "succeeded"
	// BackupAttemptFailed: the attempt's record carries a failure; the
	// maintenance warnings state its condition where they can.
	BackupAttemptFailed BackupAttemptOutcome = "failed"
)

// BackupDigestUnknown is an artifact's digest value when nothing cheaply
// knows one: no sidecar records it, and a remote listing exposes no digest.
// The evidence never hashes a snapshot to guess one.
const BackupDigestUnknown = "unknown"

// BackupPolicy is the effective backup policy the scheduled snapshot loop
// enforces, every resolved value paired with its provenance. The engine
// resolves it from its own configuration — the cluster config the
// controller was started with, snapshot.conf winning outright over it — so
// a consumer never re-derives the resolution. The reader resolves current
// files; cluster-config edits may differ from the running controller's
// loaded copy until it restarts. Snapshot.conf is reloaded on every tick.
type BackupPolicy struct {
	// What a consumer can rely on in the fields below.
	State BackupPolicyState
	// How often the loop takes a snapshot; zero when the cluster config cannot be read.
	Interval     time.Duration
	IntervalFrom BackupProvenance
	// Snapshots pruning keeps; zero with unknown provenance is unknown, not keep-all.
	Retain     int
	RetainFrom BackupProvenance
	// The copy requirement the effective S3 target imposes; empty when unknown.
	Copy     BackupCopyRequirement
	CopyFrom BackupProvenance
	// The local snapshot directory; empty when the cluster config cannot be read.
	Dir     string
	DirFrom BackupProvenance
	// The engine's WARN conditions that shaped the policy; no raw errors.
	Warnings []string
}

// BackupArtifact is one backup file the evidence enumerated — a local
// snapshot in the snapshot directory or a remote copy on the S3 target.
type BackupArtifact struct {
	// Stable identity: the file name, shared by the local artifact and its remote copy.
	ID string
	// An opaque identity of the effective store; empty means unknown.
	LocationID string
	// Identifies this backup at that store; empty means unknown.
	CopyID string
	// "local" or "remote": the store the artifact was enumerated in.
	Location string
	// The local file's modification time, or the remote object's last-modified time.
	Created time.Time
	// The artifact's size in bytes.
	Size int64
	// A sha256 sidecar beside the local file, else BackupDigestUnknown.
	Digest string
}

// BackupLocalCatalog is the bounded local half of the evidence: the snapshot
// files enumerated in the snapshot directory, within the read's bounds.
type BackupLocalCatalog struct {
	// Scopes every artifact's local location to this node.
	Node string
	// The enumerated snapshots, sorted by ID; the exact total only when Complete.
	Artifacts []BackupArtifact
	// False means the walk stopped early and len(Artifacts) is a floor.
	Complete bool
	// The walk's WARN conditions as safe diagnostics.
	Warnings []string
}

// BackupRemoteCatalog is the bounded remote half of the evidence: the copies
// enumerated on the S3 target. On a multi-controller cluster the target's
// prefix is shared — every controller uploads under it and prunes only its
// own — so the copies span every controller, not only this node's: the
// cluster's off-box inventory.
type BackupRemoteCatalog struct {
	// The enumerated remote copies, sorted by ID; the exact total only when Complete.
	Copies []BackupArtifact
	// Reserved for age diagnostics; empty under count retention.
	Old []BackupArtifact
	// False means the listing stopped short and len(Copies) is a floor.
	Complete bool
	// The listing's WARN conditions; they name the condition, never the address.
	Warnings []string
}

// BackupCensusEntry is one stable backup ID counted exactly once in the
// deduplicated census: the local artifact, the remote copy, or both when the
// same snapshot exists in both places.
type BackupCensusEntry struct {
	// Shared by the backup's local artifact and its remote copy.
	ID string
	// Nil when that location does not hold the backup.
	Local  *BackupArtifact
	Remote *BackupArtifact
	// Reserved for age diagnostics; false under count retention.
	OldRemote bool
}

// BackupCensus is the deduplicated union of the local and remote catalogs:
// every stable backup ID counted once, with whether the union's total is
// exact or unknown. A partial catalog on either side never yields an exact
// total: the entries counted stand as a floor, and the true total is
// unknown.
type BackupCensus struct {
	// Scopes the union's local half to the node whose store it is.
	Node string
	// The distinct backups, sorted by ID; a remote-only entry can be another controller's.
	Entries []BackupCensusEntry
	// True when both catalogs covered their stores, so the count is the exact total.
	Complete bool
	// The union's WARN conditions as safe diagnostics.
	Warnings []string
	// Reserved for age diagnostics; empty under count retention.
	Retention []string
}

// BackupAttempt is one recorded maintenance attempt as safe diagnostics:
// when it ran and the typed outcome. A zero AttemptedAt means no attempt is
// recorded; when BackupMaintenance.AttemptsKnown is false the history is unknown.
type BackupAttempt struct {
	// When the loop started the attempt.
	AttemptedAt time.Time
	// The typed safe result; empty when no attempt is recorded.
	Outcome BackupAttemptOutcome
}

// BackupMaintenance is the scheduled snapshot loop's durable maintenance
// state as safe diagnostics: the latest save, upload and prune attempts,
// when the save and the upload last succeeded, and the scheduling and
// retention warnings the loop's last tick ran with. The recorded line's own
// text — which can carry the raw error the tick printed — is never echoed.
type BackupMaintenance struct {
	// A save that succeeded while pruning failed is a working backup with broken retention.
	Save   BackupAttempt
	Upload BackupAttempt
	Prune  BackupAttempt
	// Separate from the latest attempts so a failure streak does not erase the last success.
	LastSaveSuccess   time.Time
	LastUploadSuccess time.Time
	// The record's scheduling and retention warnings as typed safe reasons.
	Warnings []string
	// False means the diagnostics are unknown, never that no attempt was made.
	AttemptsKnown bool
}

// BackupEvidenceResult is one bounded read of the engine's backup evidence:
// the effective policy with every value's provenance, the local artifacts,
// the remote copies, the deduplicated census counting each backup once
// across both locations, and the maintenance diagnostics. Complete says
// every stage ran whole within the bounds — false means some stage's
// evidence is a floor or unknown, never that it is zero.
type BackupEvidenceResult struct {
	// The effective backup policy the scheduled loop enforces.
	Policy BackupPolicy
	// The node whose store the local catalog read.
	Node string
	// The immutable Kubernetes node identity; empty means unknown.
	NodeUID string
	// Each catalog is exact only when its own Complete says so.
	Local  BackupLocalCatalog
	Remote BackupRemoteCatalog
	// The deduplicated census, exact only when both catalogs were complete.
	Backups BackupCensus
	// The loop's durable maintenance state as safe diagnostics.
	Maintenance BackupMaintenance
	// True when the whole read ran whole within its bounds.
	Complete bool
	// Aggregates the policy's, catalogs', maintenance and read's own diagnostics.
	Warnings []string
}

// Zero caps mean the engine read's built-in defaults, never unbounded reads.
type backupEvidenceOptions struct {
	node       string
	maxEntries int
	maxBytes   int64
	maxTime    time.Duration
}

// BackupEvidenceOption configures one backup-evidence read.
type BackupEvidenceOption func(*backupEvidenceOptions)

// WithBackupEvidenceNode names the node the local half is scoped to. Empty —
// the default — means this host's short name, the way the engine's own
// snapshot naming resolves it.
func WithBackupEvidenceNode(node string) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.node = node }
}

// WithBackupEvidenceMaxEntries caps how many backups — local artifacts and
// remote copies together — one read may describe. Zero or negative means the
// engine read's built-in page budget, never an unbounded read.
func WithBackupEvidenceMaxEntries(maxEntries int) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.maxEntries = maxEntries }
}

// WithBackupEvidenceMaxBytes caps how many artifact bytes the read may
// account for across the local walk and the remote listing together. Zero
// or negative means the engine read's built-in byte budget.
func WithBackupEvidenceMaxBytes(maxBytes int64) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.maxBytes = maxBytes }
}

// WithBackupEvidenceMaxTime bounds how long the whole read may take,
// however patient the caller's own context is. Zero or negative means the
// engine read's built-in deadline.
func WithBackupEvidenceMaxTime(maxTime time.Duration) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.maxTime = maxTime }
}

// Seam so tests stand in for the read without touching node state or the cluster.
var backupEvidenceRead = backupevidence.Evidence

// BackupEvidence returns the engine's read-only view of what it does for
// backups, in one bounded call: the effective policy the scheduled loop
// enforces with every value's provenance, the local artifacts in the
// snapshot directory, the remote copies on the S3 target, the deduplicated
// census counting each backup once across both locations, and the loop's
// maintenance diagnostics — typed attempt outcomes, last-success stamps,
// scheduling and retention warnings.
//
// The whole read is the engine's own: the caller's context propagates into
// every local and remote read the engine makes, and the engine enforces the
// overall time, page and byte budgets — the caller's caps when the options
// set them, the engine's built-in defaults otherwise. A stage the bounds
// stop is unknown, never zero: a partial catalog is a floor and the census
// is never an exact total.
//
// Every diagnostic is a typed safe reason: no credential, no executable
// path and no raw storage error reaches the result.
func BackupEvidence(ctx context.Context, opts ...BackupEvidenceOption) BackupEvidenceResult {
	var o backupEvidenceOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return backupEvidenceResultFrom(backupEvidenceRead(ctx, backupevidence.EvidenceOptions{
		Node:       o.node,
		MaxEntries: o.maxEntries,
		MaxBytes:   o.maxBytes,
		MaxTime:    o.maxTime,
	}))
}

func backupPolicyFrom(p backupevidence.EffectivePolicy) BackupPolicy {
	return BackupPolicy{
		State:        BackupPolicyState(p.State),
		Interval:     p.Interval,
		IntervalFrom: BackupProvenance(p.IntervalFrom),
		Retain:       p.Retain,
		RetainFrom:   BackupProvenance(p.RetainFrom),
		Copy:         BackupCopyRequirement(p.Copy),
		CopyFrom:     BackupProvenance(p.CopyFrom),
		Dir:          p.Dir,
		DirFrom:      BackupProvenance(p.DirFrom),
		Warnings:     p.Warnings,
	}
}

func backupArtifactFrom(a backupevidence.Artifact) BackupArtifact {
	return BackupArtifact{
		ID:         a.ID,
		Location:   a.Location,
		LocationID: a.LocationID,
		CopyID:     a.CopyID,
		Created:    a.Created,
		Size:       a.Size,
		Digest:     a.Digest,
	}
}

func backupArtifactsFrom(as []backupevidence.Artifact) []BackupArtifact {
	if as == nil {
		return nil
	}
	out := make([]BackupArtifact, len(as))
	for i, a := range as {
		out[i] = backupArtifactFrom(a)
	}
	return out
}

func backupLocalFrom(l backupevidence.LocalResult) BackupLocalCatalog {
	return BackupLocalCatalog{
		Node:      l.Node,
		Artifacts: backupArtifactsFrom(l.Artifacts),
		Complete:  l.Complete,
		Warnings:  l.Warnings,
	}
}

func backupRemoteFrom(r backupevidence.RemoteResult) BackupRemoteCatalog {
	return BackupRemoteCatalog{
		Copies:   backupArtifactsFrom(r.Copies),
		Old:      backupArtifactsFrom(r.Old),
		Complete: r.Complete,
		Warnings: r.Warnings,
	}
}

// Each entry gets its own artifact values so no pointer aliases the engine's result.
func backupCensusFrom(c backupevidence.Census) BackupCensus {
	out := BackupCensus{
		Node:      c.Node,
		Complete:  c.Complete,
		Warnings:  c.Warnings,
		Retention: c.Retention,
	}
	if c.Entries == nil {
		return out
	}
	out.Entries = make([]BackupCensusEntry, len(c.Entries))
	for i, e := range c.Entries {
		entry := BackupCensusEntry{ID: e.ID, OldRemote: e.OldRemote}
		if e.Local != nil {
			local := backupArtifactFrom(*e.Local)
			entry.Local = &local
		}
		if e.Remote != nil {
			remote := backupArtifactFrom(*e.Remote)
			entry.Remote = &remote
		}
		out.Entries[i] = entry
	}
	return out
}

func backupMaintenanceFrom(m backupevidence.Maintenance) BackupMaintenance {
	return BackupMaintenance{
		Save:              backupAttemptFrom(m.Save),
		Upload:            backupAttemptFrom(m.Upload),
		Prune:             backupAttemptFrom(m.Prune),
		LastSaveSuccess:   m.LastSaveSuccess,
		LastUploadSuccess: m.LastUploadSuccess,
		Warnings:          m.Warnings,
		AttemptsKnown:     m.AttemptsKnown,
	}
}

func backupAttemptFrom(a backupevidence.AttemptEvidence) BackupAttempt {
	return BackupAttempt{
		AttemptedAt: a.AttemptedAt,
		Outcome:     BackupAttemptOutcome(a.Outcome),
	}
}

func backupEvidenceResultFrom(r backupevidence.EvidenceResult) BackupEvidenceResult {
	return BackupEvidenceResult{
		Policy:      backupPolicyFrom(r.Policy),
		Node:        r.Node,
		NodeUID:     r.NodeUID,
		Local:       backupLocalFrom(r.Local),
		Remote:      backupRemoteFrom(r.Remote),
		Backups:     backupCensusFrom(r.Backups),
		Maintenance: backupMaintenanceFrom(r.Maintenance),
		Complete:    r.Complete,
		Warnings:    r.Warnings,
	}
}
