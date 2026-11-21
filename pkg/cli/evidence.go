package cli

import (
	"context"
	"time"

	"github.com/ctrliq/stratum/internal/backupevidence"
)

// BackupProvenance names the source a resolved backup policy value came from.
type BackupProvenance string

const (
	// spec.snapshots in the cluster config the controller was started with.
	BackupFromClusterConfig BackupProvenance = "cluster-config"
	// The operator-managed snapshot.conf, which wins outright over the cluster config.
	BackupFromSnapshotConf BackupProvenance = "snapshot-conf"
	// The built-in default applied when nothing configures the value.
	BackupFromDefault BackupProvenance = "default"
	// The value cannot be determined; the paired value carries no information.
	BackupFromUnknown BackupProvenance = "unknown"
)

// BackupPolicyState summarizes what a consumer can rely on in the resolved policy.
type BackupPolicyState string

const (
	// The scheduled loop runs with at least one operator-configured value.
	BackupPolicyConfigured BackupPolicyState = "configured"
	// The controller never starts the loop; the other fields still report the policy.
	BackupPolicyDisabled BackupPolicyState = "disabled"
	// Nothing configures snapshots anywhere; the loop runs on the defaults.
	BackupPolicyUnconfigured BackupPolicyState = "unconfigured"
	// The cluster config cannot be read, so the schedule is unknown.
	BackupPolicyUnknown BackupPolicyState = "unknown"
	// snapshot.conf exists but cannot be read; the loop prunes nothing and uploads nothing.
	BackupPolicyUnreadable BackupPolicyState = "unreadable"
)

// What the scheduled loop does with every snapshot beyond the local file.
type BackupCopyRequirement string

const (
	// No S3 target is in force; snapshots stay in the snapshot directory.
	BackupCopyLocalOnly BackupCopyRequirement = "local-only"
	// An S3 target is in force; every scheduled snapshot is also uploaded.
	BackupCopyRemoteRequired BackupCopyRequirement = "remote-required"
)

// A recorded attempt's typed safe result: what the attempt did, never the error text.
type BackupAttemptOutcome string

const (
	// The attempt's record carries no failure.
	BackupAttemptSucceeded BackupAttemptOutcome = "succeeded"
	// The record carries a failure; the warnings state its condition where they can.
	BackupAttemptFailed BackupAttemptOutcome = "failed"
)

// No sidecar records a digest and no listing exposes one; the read never hashes a snapshot.
const BackupDigestUnknown = "unknown"

// The effective policy the scheduled snapshot loop enforces; every value carries its provenance.
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

// One backup file the evidence enumerated: local or remote.
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

// The bounded local half of the evidence: snapshots in the snapshot directory.
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

// The bounded remote half of the evidence; the shared S3 prefix spans every controller.
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

// One stable backup ID counted once in the census: local, remote, or both.
type BackupCensusEntry struct {
	// Shared by the backup's local artifact and its remote copy.
	ID string
	// Nil when that location does not hold the backup.
	Local  *BackupArtifact
	Remote *BackupArtifact
	// Reserved for age diagnostics; false under count retention.
	OldRemote bool
}

// The deduplicated union of the two catalogs; exact only when both were complete.
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

// One recorded maintenance attempt as safe diagnostics.
type BackupAttempt struct {
	// When the loop started the attempt.
	AttemptedAt time.Time
	// The typed safe result; empty when no attempt is recorded.
	Outcome BackupAttemptOutcome
}

// The loop's durable maintenance state; the record's raw error text is never echoed.
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

// One bounded read of the engine's backup evidence; Complete says every stage ran whole.
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

type BackupEvidenceOption func(*backupEvidenceOptions)

// Empty means this host's short name, the way the engine's snapshot naming resolves it.
func WithBackupEvidenceNode(node string) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.node = node }
}

// Zero or negative means the engine read's built-in page budget, never unbounded.
func WithBackupEvidenceMaxEntries(maxEntries int) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.maxEntries = maxEntries }
}

// Zero or negative means the engine read's built-in byte budget.
func WithBackupEvidenceMaxBytes(maxBytes int64) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.maxBytes = maxBytes }
}

// Zero or negative means the engine read's built-in deadline.
func WithBackupEvidenceMaxTime(maxTime time.Duration) BackupEvidenceOption {
	return func(o *backupEvidenceOptions) { o.maxTime = maxTime }
}

// Seam so tests stand in for the read without touching node state or the cluster.
var backupEvidenceRead = backupevidence.Evidence

// Returns the engine's read-only view of what it does for backups, in one bounded call.
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
