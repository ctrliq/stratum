// The exported backup-evidence surface: the supported read-only interface an
// external pinned binary calls without importing engine internals. The tests
// drive BackupEvidence through the read seam the way the snapshot commands'
// tests drive snapshotDir and loadSnapshotConfig, so no test touches the
// host's node state, cluster config, snapshot directory or S3 target. What
// they prove is the contract a released consumer compiles and runs against:
// the options scope and bound the engine read, the caller's context reaches
// it unchanged, and every wrapper type restates the engine's evidence —
// including that a partial read stays a floor, never an exact total.
package cli

import (
	"context"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/backupevidence"
)

// useBackupEvidenceRead points the evidence read seam at fn, the same seam
// swap the snapshot commands' tests use for snapshotDir and
// loadSnapshotConfig: the exported call reaches the engine's assembled read
// through backupEvidenceRead, and a swapped fn stands in for it.
func useBackupEvidenceRead(t *testing.T, fn func(context.Context, backupevidence.EvidenceOptions) backupevidence.EvidenceResult) {
	t.Helper()
	old := backupEvidenceRead
	t.Cleanup(func() { backupEvidenceRead = old })
	backupEvidenceRead = fn
}

// evidenceTimes are the fixed UTC stamps the crafted evidence carries, so
// the restatement asserts exactly, whenever the suite runs.
var (
	evidenceSaveAttempt = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	evidenceUploadLast  = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	evidenceCreated     = time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC)
	evidenceOldCreated  = time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)
)

// engineEvidence is the engine's assembled read over one working node: a
// configured policy with every provenance, two local artifacts (one with a
// known digest), two remote copies of which one is old, the census counting
// the shared snapshot once, and maintenance with typed attempts, separate
// last-success stamps and safe warnings. Every stage ran whole.
func engineEvidence() backupevidence.EvidenceResult {
	const (
		shared   = "etcd-snapshot-cp-1-20261112-110000.db" // local and remote
		localNew = "etcd-snapshot-cp-1-20261112-120000.db" // local only
		remote   = "etcd-snapshot-cp-1-20261108-110000.db" // remote only, old
		digest   = "sha256:" + "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	)
	art := func(id, loc string, created time.Time, size int64, dg string) backupevidence.Artifact {
		return backupevidence.Artifact{ID: id, Location: loc, Created: created, Size: size, Digest: dg}
	}
	return backupevidence.EvidenceResult{
		Policy: backupevidence.EffectivePolicy{
			State:        backupevidence.StateConfigured,
			Interval:     24 * time.Hour,
			IntervalFrom: backupevidence.FromClusterConfig,
			Retain:       3,
			RetainFrom:   backupevidence.FromSnapshotConf,
			Copy:         backupevidence.CopyRemoteRequired,
			CopyFrom:     backupevidence.FromSnapshotConf,
			Dir:          "/var/backups/etcd",
			DirFrom:      backupevidence.FromDefault,
		},
		Node: "cp-1",
		Local: backupevidence.LocalResult{
			Node: "cp-1",
			Artifacts: []backupevidence.Artifact{
				art(shared, "local", evidenceCreated, 10, digest),
				art(localNew, "local", evidenceCreated.Add(time.Minute), 16, backupevidence.DigestUnknown),
			},
			Complete: true,
		},
		Remote: backupevidence.RemoteResult{
			Copies: []backupevidence.Artifact{
				art(shared, "remote", evidenceCreated, 10, backupevidence.DigestUnknown),
				art(remote, "remote", evidenceOldCreated, 14, backupevidence.DigestUnknown),
			},
			Old:      []backupevidence.Artifact{art(remote, "remote", evidenceOldCreated, 14, backupevidence.DigestUnknown)},
			Complete: true,
		},
		Backups: backupevidence.Census{
			Node: "cp-1",
			Entries: []backupevidence.CensusEntry{
				{ID: shared, Local: &backupevidence.Artifact{ID: shared, Location: "local", Created: evidenceCreated, Size: 10, Digest: digest}, Remote: &backupevidence.Artifact{ID: shared, Location: "remote", Created: evidenceCreated, Size: 10, Digest: backupevidence.DigestUnknown}},
				{ID: localNew, Local: &backupevidence.Artifact{ID: localNew, Location: "local", Created: evidenceCreated.Add(time.Minute), Size: 16, Digest: backupevidence.DigestUnknown}},
				{ID: remote, Remote: &backupevidence.Artifact{ID: remote, Location: "remote", Created: evidenceOldCreated, Size: 14, Digest: backupevidence.DigestUnknown}, OldRemote: true},
			},
			Complete:  true,
			Retention: []string{"WARN: the remote copy " + remote + " was created " + evidenceOldCreated.UTC().Format(time.RFC3339) + ", before the retention cutoff; it predates the retention window or survived a failed prune"},
		},
		Maintenance: backupevidence.Maintenance{
			Save:              backupevidence.AttemptEvidence{AttemptedAt: evidenceSaveAttempt, Outcome: backupevidence.AttemptSucceeded},
			Upload:            backupevidence.AttemptEvidence{AttemptedAt: evidenceSaveAttempt.Add(time.Minute), Outcome: backupevidence.AttemptFailed},
			Prune:             backupevidence.AttemptEvidence{AttemptedAt: evidenceSaveAttempt.Add(2 * time.Minute), Outcome: backupevidence.AttemptFailed},
			LastSaveSuccess:   evidenceSaveAttempt,
			LastUploadSuccess: evidenceUploadLast,
			Warnings:          []string{"WARN: the recorded tick's remote snapshot prune failed; remote retention may not have held"},
			AttemptsKnown:     true,
		},
		Complete: true,
	}
}

// BackupEvidence restates the engine's assembled evidence through the
// exported value types: the policy with its states and provenance, the local
// artifacts, the remote copies, the deduplicated census and the maintenance
// diagnostics, each field for field, with the stable string values the
// machine-readable contract pins.
func TestBackupEvidenceRestatesTheEngineEvidence(t *testing.T) {
	want := engineEvidence()
	useBackupEvidenceRead(t, func(context.Context, backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		return want
	})

	got := BackupEvidence(context.Background())

	if got.Policy.State != BackupPolicyConfigured || string(got.Policy.State) != "configured" {
		t.Errorf("Policy.State = %q, want the configured state", got.Policy.State)
	}
	if got.Policy.Interval != 24*time.Hour || got.Policy.IntervalFrom != BackupFromClusterConfig || string(got.Policy.IntervalFrom) != "cluster-config" {
		t.Errorf("Policy.Interval = %v (%q), want 24h from the cluster config", got.Policy.Interval, got.Policy.IntervalFrom)
	}
	if got.Policy.Retain != 3 || got.Policy.RetainFrom != BackupFromSnapshotConf || string(got.Policy.RetainFrom) != "snapshot-conf" {
		t.Errorf("Policy.Retain = %d (%q), want 3 from the snapshot conf", got.Policy.Retain, got.Policy.RetainFrom)
	}
	if got.Policy.Copy != BackupCopyRemoteRequired || string(got.Policy.Copy) != "remote-required" {
		t.Errorf("Policy.Copy = %q, want the remote-required copy policy", got.Policy.Copy)
	}
	if got.Policy.CopyFrom != BackupFromSnapshotConf {
		t.Errorf("Policy.CopyFrom = %q, want the snapshot conf", got.Policy.CopyFrom)
	}
	if got.Policy.Dir != "/var/backups/etcd" || got.Policy.DirFrom != BackupFromDefault || string(got.Policy.DirFrom) != "default" {
		t.Errorf("Policy.Dir = %q (%q), want the built-in directory from the default provenance", got.Policy.Dir, got.Policy.DirFrom)
	}

	if got.Node != "cp-1" {
		t.Errorf("Node = %q, want the node the engine read scoped the evidence to", got.Node)
	}
	if got.Local.Node != "cp-1" || !got.Local.Complete {
		t.Errorf("Local = {Node: %q, Complete: %v}, want cp-1's complete local catalog", got.Local.Node, got.Local.Complete)
	}
	if len(got.Local.Artifacts) != 2 {
		t.Fatalf("Local.Artifacts has %d entries, want the two local artifacts", len(got.Local.Artifacts))
	}
	first := got.Local.Artifacts[0]
	if first.ID != "etcd-snapshot-cp-1-20261112-110000.db" || first.Location != "local" || first.Created != evidenceCreated || first.Size != 10 {
		t.Errorf("Local.Artifacts[0] = %+v, want the shared snapshot with its local location, creation time and size", first)
	}
	if first.Digest == BackupDigestUnknown || string(BackupDigestUnknown) != "unknown" {
		t.Errorf("Local.Artifacts[0].Digest = %q, want the sidecar-known digest, not %q", first.Digest, BackupDigestUnknown)
	}
	if second := got.Local.Artifacts[1]; second.Digest != BackupDigestUnknown {
		t.Errorf("Local.Artifacts[1].Digest = %q, want the unknown digest: no sidecar records one", second.Digest)
	}

	if !got.Remote.Complete || len(got.Remote.Copies) != 2 || len(got.Remote.Old) != 1 {
		t.Errorf("Remote = {Copies: %d, Old: %d, Complete: %v}, want the complete catalog with two copies, one old", len(got.Remote.Copies), len(got.Remote.Old), got.Remote.Complete)
	}

	if !got.Backups.Complete || len(got.Backups.Entries) != 3 {
		t.Fatalf("Backups = {Entries: %d, Complete: %v}, want the three distinct backup IDs counted once", len(got.Backups.Entries), got.Backups.Complete)
	}
	shared := got.Backups.Entries[0]
	if shared.ID != "etcd-snapshot-cp-1-20261112-110000.db" || shared.Local == nil || shared.Remote == nil || shared.OldRemote {
		t.Errorf("Backups.Entries[0] = %+v, want the snapshot saved and uploaded: both halves, not old", shared)
	}
	if shared.Local != nil && (shared.Local.Location != "local" || shared.Local.Size != 10) {
		t.Errorf("Backups.Entries[0].Local = %+v, want the local half with its location and size", *shared.Local)
	}
	remoteOnly := got.Backups.Entries[2]
	if remoteOnly.ID != "etcd-snapshot-cp-1-20261108-110000.db" || remoteOnly.Local != nil || remoteOnly.Remote == nil || !remoteOnly.OldRemote {
		t.Errorf("Backups.Entries[2] = %+v, want the old remote-only copy flagged", remoteOnly)
	}
	if len(got.Backups.Retention) != 1 || got.Backups.Retention[0] != want.Backups.Retention[0] {
		t.Errorf("Backups.Retention = %q, want the old remote copy named with its creation time", got.Backups.Retention)
	}

	m := got.Maintenance
	if m.Save != (BackupAttempt{AttemptedAt: evidenceSaveAttempt, Outcome: BackupAttemptSucceeded}) {
		t.Errorf("Maintenance.Save = %+v, want the typed succeeded attempt, never a raw error", m.Save)
	}
	if m.Upload.Outcome != BackupAttemptFailed || string(BackupAttemptFailed) != "failed" || m.Upload.AttemptedAt != evidenceSaveAttempt.Add(time.Minute) {
		t.Errorf("Maintenance.Upload = %+v, want the typed failed attempt", m.Upload)
	}
	if m.Prune.Outcome != BackupAttemptFailed {
		t.Errorf("Maintenance.Prune = %+v, want the independent typed failed prune", m.Prune)
	}
	if m.LastSaveSuccess != evidenceSaveAttempt {
		t.Errorf("Maintenance.LastSaveSuccess = %v, want the save's last success kept separate from the failing upload streak", m.LastSaveSuccess)
	}
	if m.LastUploadSuccess != evidenceUploadLast {
		t.Errorf("Maintenance.LastUploadSuccess = %v, want the upload's earlier success to survive the failing streak", m.LastUploadSuccess)
	}
	if !m.AttemptsKnown || len(m.Warnings) != 1 || m.Warnings[0] != want.Maintenance.Warnings[0] {
		t.Errorf("Maintenance = {AttemptsKnown: %v, Warnings: %q}, want the known record with its safe warning restated", m.AttemptsKnown, m.Warnings)
	}

	if !got.Complete {
		t.Errorf("Complete = false, want true: the engine read ran whole within its bounds")
	}
}

// The options scope and bound the engine read — the node the local half is
// named for, and the overall page, byte and time budgets — and a call with
// no options passes zero caps, which the engine read turns into its own
// built-in defaults: the wrapper never invents bounds of its own.
func TestBackupEvidenceOptionsScopeAndBoundTheEngineRead(t *testing.T) {
	var got backupevidence.EvidenceOptions
	useBackupEvidenceRead(t, func(_ context.Context, o backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		got = o
		return backupevidence.EvidenceResult{}
	})

	BackupEvidence(context.Background(),
		WithBackupEvidenceNode("cp-2"),
		WithBackupEvidenceMaxEntries(12),
		WithBackupEvidenceMaxBytes(1<<21),
		WithBackupEvidenceMaxTime(3*time.Second),
	)
	want := backupevidence.EvidenceOptions{Node: "cp-2", MaxEntries: 12, MaxBytes: 1 << 21, MaxTime: 3 * time.Second}
	if got != want {
		t.Errorf("the engine read got %+v, want %+v: every option must reach the read unchanged", got, want)
	}

	BackupEvidence(context.Background())
	if got != (backupevidence.EvidenceOptions{}) {
		t.Errorf("a bare call passed %+v, want zero caps the engine read defaults itself", got)
	}
}

// The caller's context reaches the engine read unchanged: the same context
// — its deadline, its cancellation, its values — so the read's bounds and
// the caller's stop both end the underlying local and remote I/O.
func TestBackupEvidencePropagatesTheCallerContext(t *testing.T) {
	type ctxKey struct{}
	var gotCtx context.Context
	useBackupEvidenceRead(t, func(ctx context.Context, _ backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		gotCtx = ctx
		return backupevidence.EvidenceResult{}
	})

	deadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), ctxKey{}, "publisher"), deadline)
	defer cancel()
	BackupEvidence(ctx)

	if gotCtx != ctx {
		t.Errorf("the engine read got a different context: the caller's deadline, cancellation and values must reach it unchanged")
	}
	if v, _ := gotCtx.Value(ctxKey{}).(string); v != "publisher" {
		t.Errorf("the context the read got lost the caller's values: %v", v)
	}
	if dl, ok := gotCtx.Deadline(); !ok || !dl.Equal(deadline) {
		t.Errorf("the context the read got carries deadline %v (ok %v), want the caller's %v", dl, ok, deadline)
	}
}

// A partial engine read stays partial in the exported result: the floors
// stand, the completeness flags and the warnings are restated, and the
// wrapper never turns a partial catalog into an exact total.
func TestBackupEvidenceKeepsPartialEvidenceAFloorNotATotal(t *testing.T) {
	partial := backupevidence.EvidenceResult{
		Policy: backupevidence.EffectivePolicy{State: backupevidence.StateUnknown, IntervalFrom: backupevidence.FromUnknown, RetainFrom: backupevidence.FromUnknown, CopyFrom: backupevidence.FromUnknown, DirFrom: backupevidence.FromUnknown},
		Local: backupevidence.LocalResult{
			Node:      "cp-1",
			Artifacts: []backupevidence.Artifact{{ID: "etcd-snapshot-cp-1-20261112-110000.db", Location: "local", Created: evidenceCreated, Size: 10, Digest: backupevidence.DigestUnknown}},
			Complete:  false,
			Warnings:  []string{"WARN: the local snapshot walk stopped early: the page cap of 1 entries was reached; the local catalog is not a complete total"},
		},
		Remote: backupevidence.RemoteResult{Complete: false, Warnings: []string{"WARN: the remote store refused the listing; the remote catalog is unknown and not a complete total"}},
		Backups: backupevidence.Census{
			Node:     "cp-1",
			Entries:  []backupevidence.CensusEntry{{ID: "etcd-snapshot-cp-1-20261112-110000.db", Local: &backupevidence.Artifact{ID: "etcd-snapshot-cp-1-20261112-110000.db", Location: "local", Created: evidenceCreated, Size: 10, Digest: backupevidence.DigestUnknown}}},
			Complete: false,
			Warnings: []string{"WARN: the union's total is unknown: a catalog in the union is partial, and a partial listing never yields an exact total"},
		},
		Maintenance: backupevidence.Maintenance{Warnings: []string{"WARN: the evidence read was cancelled before the attempt record was read; the maintenance diagnostics are unknown"}},
		Complete:    false,
		Warnings:    []string{"WARN: the evidence read was cancelled before the policy was resolved; the effective policy is unknown"},
	}
	useBackupEvidenceRead(t, func(context.Context, backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		return partial
	})

	got := BackupEvidence(context.Background())

	if got.Complete || got.Local.Complete || got.Remote.Complete || got.Backups.Complete || got.Maintenance.AttemptsKnown {
		t.Errorf("the partial read came back as complete somewhere: got Complete=%v Local=%v Remote=%v Backups=%v AttemptsKnown=%v",
			got.Complete, got.Local.Complete, got.Remote.Complete, got.Backups.Complete, got.Maintenance.AttemptsKnown)
	}
	if got.Policy.State != BackupPolicyUnknown || got.Policy.DirFrom != BackupFromUnknown {
		t.Errorf("the unknown policy was restated as %q/%q, want the unknown state with no invented values", got.Policy.State, got.Policy.DirFrom)
	}
	if len(got.Local.Artifacts) != 1 {
		t.Errorf("Local.Artifacts has %d entries, want the one-artifact floor the engine read described", len(got.Local.Artifacts))
	}
	if len(got.Local.Warnings) != 1 || got.Local.Warnings[0] != partial.Local.Warnings[0] {
		t.Errorf("Local.Warnings = %q, want the page-cap warning restated", got.Local.Warnings)
	}
	if len(got.Remote.Warnings) != 1 || got.Remote.Warnings[0] != partial.Remote.Warnings[0] {
		t.Errorf("Remote.Warnings = %q, want the refusal warning restated", got.Remote.Warnings)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != partial.Warnings[0] {
		t.Errorf("Warnings = %q, want the read's own warnings restated", got.Warnings)
	}
}
