// The supported read-only backup-evidence interface on pkg/cli's exported
// surface: what an external pinned binary — a node-owned publisher, an
// operator's collector — calls to read the engine's own view of what it
// does for backups, without importing engine internals or re-deriving the
// engine's config resolution. BackupEvidence delegates the whole read to the
// engine's assembled reader in internal/backupevidence, which owns the
// policy resolution, the catalogs and the bounds; what this file adds is the
// exported shape: wrapper value types restating the safe evidence fields, so
// no internal type ever crosses the sanctioned surface. Every field here
// mirrors its engine counterpart exactly — same names, same safe values —
// and adds nothing of its own.
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
	// State says what a consumer can rely on in the fields below.
	State BackupPolicyState
	// Interval is how often the scheduled loop takes a snapshot. Zero with
	// unknown provenance when the cluster config cannot be read.
	Interval     time.Duration
	IntervalFrom BackupProvenance
	// Retain is how many of this node's snapshots pruning keeps. Zero with
	// unknown provenance means unknown, not keep-all.
	Retain     int
	RetainFrom BackupProvenance
	// Copy is the copy requirement the effective S3 target imposes. Empty
	// with unknown provenance when the target cannot be determined.
	Copy     BackupCopyRequirement
	CopyFrom BackupProvenance
	// Dir is the local snapshot directory, from the engine's own
	// resolution. Empty with unknown provenance when the cluster config
	// cannot be read.
	Dir     string
	DirFrom BackupProvenance
	// Warnings carry the engine's WARN conditions that shaped this policy:
	// an unreadable snapshot.conf, a cluster config that cannot be read.
	// They describe the condition; they carry no raw errors.
	Warnings []string
}

// BackupArtifact is one backup file the evidence enumerated — a local
// snapshot in the snapshot directory or a remote copy on the S3 target.
type BackupArtifact struct {
	// ID is the artifact's stable identity: its file name, shared by the
	// local artifact and the remote copy of the same snapshot, so both count
	// once in a deduplicated census.
	ID string
	// LocationID is an opaque identity of the effective store; empty means unknown.
	LocationID string
	// CopyID identifies this backup at that store; empty means unknown.
	CopyID string
	// Location is "local" or "remote": the store the artifact was enumerated
	// in.
	Location string
	// Created is when the artifact came to be: the local file's modification
	// time, the remote object's last-modified time.
	Created time.Time
	// Size is the artifact's size in bytes.
	Size int64
	// Digest is the artifact's digest when one is cheaply known — a valid
	// sha256 sidecar beside the local file — else BackupDigestUnknown.
	Digest string
}

// BackupLocalCatalog is the bounded local half of the evidence: the snapshot
// files enumerated in the snapshot directory, within the read's bounds.
type BackupLocalCatalog struct {
	// Node scopes every artifact's local location to this node.
	Node string
	// Artifacts are the enumerated snapshots, sorted by ID. They are the
	// store's exact total only when Complete is true.
	Artifacts []BackupArtifact
	// Complete says the walk covered the whole directory within its bounds.
	// False means it stopped early — a cap, a cancellation, an unreadable
	// entry — and len(Artifacts) is a floor, never the store's total.
	Complete bool
	// Warnings carry the walk's WARN conditions as safe diagnostics.
	Warnings []string
}

// BackupRemoteCatalog is the bounded remote half of the evidence: the copies
// enumerated on the S3 target.
type BackupRemoteCatalog struct {
	// Copies are the enumerated remote copies, sorted by ID, each carrying
	// the remote location and an unknown digest. They are the remote
	// store's exact total only when Complete is true.
	Copies []BackupArtifact
	// Old is reserved for age diagnostics. It is empty under count retention.
	Old []BackupArtifact
	// Complete says the listing covered the whole store within its bounds.
	// False means it stopped short — denied, timeout, a cancellation, a cap
	// — and len(Copies) is a floor while the store's total is unknown.
	Complete bool
	// Warnings carry the listing's WARN conditions as safe diagnostics: they
	// name the condition, never the store's address or raw error.
	Warnings []string
}

// BackupCensusEntry is one stable backup ID counted exactly once in the
// deduplicated census: the local artifact, the remote copy, or both when the
// same snapshot exists in both places.
type BackupCensusEntry struct {
	// ID is the backup's stable identity, shared by its local artifact and
	// its remote copy.
	ID string
	// Local and Remote are the ID's local artifact and remote copy, each
	// nil when that location does not hold it.
	Local  *BackupArtifact
	Remote *BackupArtifact
	// OldRemote is reserved for age diagnostics; false under count retention.
	OldRemote bool
}

// BackupCensus is the deduplicated union of the local and remote catalogs:
// every stable backup ID counted once, with whether the union's total is
// exact or unknown. A partial catalog on either side never yields an exact
// total: the entries counted stand as a floor, and the true total is
// unknown.
type BackupCensus struct {
	// Node scopes the union's local half to the node whose store it is.
	Node string
	// Entries are the distinct backups, sorted by ID.
	Entries []BackupCensusEntry
	// Complete says both catalogs covered their stores within their bounds,
	// so the entry count is the exact total of distinct backups.
	Complete bool
	// Warnings carry the union's WARN conditions as safe diagnostics.
	Warnings []string
	// Retention is reserved for age diagnostics; empty under count retention.
	Retention []string
}

// BackupAttempt is one recorded maintenance attempt as safe diagnostics:
// when it ran and the typed outcome. A zero AttemptedAt means no attempt is
// recorded; when BackupMaintenance.AttemptsKnown is false the history is unknown.
type BackupAttempt struct {
	// AttemptedAt is when the loop started the attempt.
	AttemptedAt time.Time
	// Outcome is the typed safe result; empty when no attempt is recorded.
	Outcome BackupAttemptOutcome
}

// BackupMaintenance is the scheduled snapshot loop's durable maintenance
// state as safe diagnostics: the latest save, upload and prune attempts,
// when the save and the upload last succeeded, and the scheduling and
// retention warnings the loop's last tick ran with. The recorded line's own
// text — which can carry the raw error the tick printed — is never echoed.
type BackupMaintenance struct {
	// Save, Upload and Prune are the latest recorded attempts. Prune is
	// independent of Save and Upload: a save that succeeded while pruning
	// failed is a working backup with broken retention.
	Save   BackupAttempt
	Upload BackupAttempt
	Prune  BackupAttempt
	// LastSaveSuccess and LastUploadSuccess are when the save and the
	// upload last succeeded, kept separate from the latest attempts so a
	// streak of failures does not erase when backups last worked.
	LastSaveSuccess   time.Time
	LastUploadSuccess time.Time
	// Warnings state the record's scheduling and retention warnings as typed
	// safe reasons.
	Warnings []string
	// AttemptsKnown says the record was read or does not exist. False means
	// the diagnostics are unknown because the record is unreadable or the
	// request ended — never that no attempt was made.
	AttemptsKnown bool
}

// BackupEvidenceResult is one bounded read of the engine's backup evidence:
// the effective policy with every value's provenance, the local artifacts,
// the remote copies, the deduplicated census counting each backup once
// across both locations, and the maintenance diagnostics. Complete says
// every stage ran whole within the bounds — false means some stage's
// evidence is a floor or unknown, never that it is zero.
type BackupEvidenceResult struct {
	// Policy is the effective backup policy the scheduled loop enforces.
	Policy BackupPolicy
	// Node scopes the local half: the node whose store the local catalog
	// read.
	Node string
	// NodeUID is the immutable Kubernetes node identity; empty means unknown.
	NodeUID string
	// Local is the bounded local catalog, and Remote the bounded remote
	// catalog, each exact only when its own Complete says so.
	Local  BackupLocalCatalog
	Remote BackupRemoteCatalog
	// Backups is the deduplicated census: every stable backup ID once, with
	// its local artifact and its remote copy, exact only when both catalogs
	// were complete.
	Backups BackupCensus
	// Maintenance is the loop's durable maintenance state as safe
	// diagnostics.
	Maintenance BackupMaintenance
	// Complete says the whole read ran whole within its bounds.
	Complete bool
	// Warnings aggregate the policy's, the catalogs', the maintenance and
	// the read's own safe diagnostics.
	Warnings []string
}

// backupEvidenceOptions carries what the exported options set on one read.
// Zero caps mean the engine read's built-in defaults, never unbounded reads;
// the wrapper never invents bounds of its own.
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

// backupEvidenceRead is the engine's assembled evidence read behind a seam,
// mirroring the snapshotDir and loadSnapshotConfig seams the snapshot
// commands read through: the exported call reaches the engine reader
// through here, and tests stand in for the read instead of touching the
// host's node state, cluster config, snapshot directory or S3 target.
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

// backupPolicyFrom restates the engine's effective policy field for field,
// with the wrapper's provenance, state and copy types carrying the same
// stable string values.
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

// backupArtifactFrom restates one enumerated artifact.
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

// backupArtifactsFrom restates a whole catalog half's artifacts.
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

// backupLocalFrom restates the bounded local catalog.
func backupLocalFrom(l backupevidence.LocalResult) BackupLocalCatalog {
	return BackupLocalCatalog{
		Node:      l.Node,
		Artifacts: backupArtifactsFrom(l.Artifacts),
		Complete:  l.Complete,
		Warnings:  l.Warnings,
	}
}

// backupRemoteFrom restates the bounded remote catalog.
func backupRemoteFrom(r backupevidence.RemoteResult) BackupRemoteCatalog {
	return BackupRemoteCatalog{
		Copies:   backupArtifactsFrom(r.Copies),
		Old:      backupArtifactsFrom(r.Old),
		Complete: r.Complete,
		Warnings: r.Warnings,
	}
}

// backupCensusFrom restates the deduplicated census, giving each entry its
// own artifact values so no pointer reaches back into the engine's result.
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

// backupMaintenanceFrom restates the loop's durable maintenance state, with
// each attempt's typed outcome.
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

// backupAttemptFrom restates one recorded attempt with its typed outcome.
func backupAttemptFrom(a backupevidence.AttemptEvidence) BackupAttempt {
	return BackupAttempt{
		AttemptedAt: a.AttemptedAt,
		Outcome:     BackupAttemptOutcome(a.Outcome),
	}
}

// backupEvidenceResultFrom restates the engine's assembled evidence result
// through the exported value types; nothing internal crosses.
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
