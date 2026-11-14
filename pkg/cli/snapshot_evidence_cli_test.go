package cli

// The machine-readable half of `stratum snapshot list`: -o json emits the
// engine's backup evidence as one stable document — the same rows the
// backup_evidence MCP tool serves — while the default text table stays
// byte-for-byte what it always was. The tests drive the evidence through the
// read seam the snapshot commands' tests use (useBackupEvidenceRead,
// useSnapshotDir), so nothing touches the host's node state, cluster config,
// snapshot directory or S3 target.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/backupevidence"
	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
)

// runSnapshotList runs `stratum snapshot list` with ctx as the command's
// context and returns its output and the command's error, so a test can pin
// both the -o json rows and the refusals.
func runSnapshotList(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		args = []string{}
	}
	var out bytes.Buffer
	cmd := newSnapshotListCommand()
	cmd.SetOut(&out)
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// The crafted evidence, as the machine-readable contract states it: the
// fixed stamps and names engineEvidence carries, spelled out so a renamed or
// retyped field fails here instead of silently moving the wire format.
const (
	jsonSharedID    = "etcd-snapshot-cp-1-20261112-110000.db"
	jsonLocalOnlyID = "etcd-snapshot-cp-1-20261113-110000.db"
	jsonOldRemoteID = "etcd-snapshot-cp-1-20261108-110000.db"
	jsonDigest      = "sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
)

// snapshotListJSON mirrors the document `snapshot list -o json` prints: an
// independent statement of the machine-readable contract, field for field,
// so a change to the wire format fails this decode or the assertions below.
type snapshotListJSON struct {
	Policy struct {
		State           string   `json:"state"`
		IntervalSeconds int64    `json:"intervalSeconds"`
		IntervalFrom    string   `json:"intervalFrom"`
		Retain          int      `json:"retain"`
		RetainFrom      string   `json:"retainFrom"`
		Copy            string   `json:"copy"`
		CopyFrom        string   `json:"copyFrom"`
		Dir             string   `json:"dir"`
		DirFrom         string   `json:"dirFrom"`
		Warnings        []string `json:"warnings"`
	} `json:"policy"`
	NodeUID string `json:"nodeUID"`
	Node    string `json:"node"`
	Local   struct {
		Node      string                `json:"node"`
		Artifacts []snapshotListJSONArt `json:"artifacts"`
		Complete  bool                  `json:"complete"`
		Warnings  []string              `json:"warnings"`
	} `json:"local"`
	Remote struct {
		Copies   []snapshotListJSONArt `json:"copies"`
		Old      []snapshotListJSONArt `json:"old"`
		Complete bool                  `json:"complete"`
		Warnings []string              `json:"warnings"`
	} `json:"remote"`
	Backups struct {
		Node      string                  `json:"node"`
		Entries   []snapshotListJSONEntry `json:"entries"`
		Complete  bool                    `json:"complete"`
		Warnings  []string                `json:"warnings"`
		Retention []string                `json:"retention"`
	} `json:"backups"`
	Maintenance struct {
		Save              snapshotListJSONAttempt `json:"save"`
		Upload            snapshotListJSONAttempt `json:"upload"`
		Prune             snapshotListJSONAttempt `json:"prune"`
		LastSaveSuccess   string                  `json:"lastSaveSuccess"`
		LastUploadSuccess string                  `json:"lastUploadSuccess"`
		Warnings          []string                `json:"warnings"`
		AttemptsKnown     bool                    `json:"attemptsKnown"`
	} `json:"maintenance"`
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings"`
}

type snapshotListJSONArt struct {
	LocationID string `json:"locationID"`
	CopyID     string `json:"copyID"`
	ID         string `json:"id"`
	Location   string `json:"location"`
	Created    string `json:"created"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
}

type snapshotListJSONEntry struct {
	ID        string               `json:"id"`
	Local     *snapshotListJSONArt `json:"local"`
	Remote    *snapshotListJSONArt `json:"remote"`
	OldRemote bool                 `json:"oldRemote"`
}

type snapshotListJSONAttempt struct {
	AttemptedAt string `json:"attemptedAt"`
	Outcome     string `json:"outcome"`
}

// TestSnapshotListJSONEmitsTheEvidenceRows: -o json emits the same evidence
// rows the backup_evidence tool serves — the effective policy with every
// value's provenance, the local artifacts, the remote copies, the
// deduplicated census counting each backup once, and the maintenance
// diagnostics — with RFC 3339 stamps and whole-second durations. A stage the
// bounds stop stays a floor: the completeness flags say which totals are
// exact.
func TestSnapshotListJSONEmitsTheEvidenceRows(t *testing.T) {
	useBackupEvidenceRead(t, func(context.Context, backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		return engineEvidence()
	})

	out, err := runSnapshotList(t, nil, "-o", "json")
	if err != nil {
		t.Fatalf("snapshot list -o json: %v", err)
	}
	var got snapshotListJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the JSON output does not parse: %v\n%s", err, out)
	}

	if p := got.Policy; p.State != "configured" || p.IntervalSeconds != 86400 || p.IntervalFrom != "cluster-config" ||
		p.Retain != 3 || p.RetainFrom != "snapshot-conf" ||
		p.Copy != "remote-required" || p.CopyFrom != "snapshot-conf" ||
		p.Dir != "/var/backups/etcd" || p.DirFrom != "default" || len(p.Warnings) != 0 {
		t.Errorf("policy = %+v, want the configured policy with every provenance", got.Policy)
	}
	if got.Node != "cp-1" {
		t.Errorf("node = %q, want cp-1", got.Node)
	}

	if !got.Local.Complete || got.Local.Node != "cp-1" || len(got.Local.Artifacts) != 2 {
		t.Fatalf("local = %+v, want cp-1's complete catalog with two artifacts", got.Local)
	}
	shared := got.Local.Artifacts[0]
	if shared.ID != jsonSharedID || shared.Location != "local" || shared.Created != "2026-11-12T11:00:00Z" ||
		shared.Size != 10 || shared.Digest != jsonDigest {
		t.Errorf("local.Artifacts[0] = %+v, want the shared snapshot with its stamp, size and sidecar digest", shared)
	}
	if localOnly := got.Local.Artifacts[1]; localOnly.ID != jsonLocalOnlyID || localOnly.Digest != "unknown" ||
		localOnly.Created != "2026-11-13T11:00:00Z" || localOnly.Size != 16 {
		t.Errorf("local.Artifacts[1] = %+v, want the local-only snapshot with the unknown digest", localOnly)
	}

	if !got.Remote.Complete || len(got.Remote.Copies) != 2 || len(got.Remote.Old) != 1 {
		t.Errorf("remote = %+v, want the complete catalog with two copies of which one is old", got.Remote)
	}
	if len(got.Remote.Old) == 1 && got.Remote.Old[0].ID != jsonOldRemoteID {
		t.Errorf("remote.Old = %+v, want the copy that predates the retention cutoff", got.Remote.Old)
	}

	if !got.Backups.Complete || got.Backups.Node != "cp-1" || len(got.Backups.Entries) != 3 {
		t.Fatalf("backups = %+v, want the complete census counting the three distinct backup IDs once", got.Backups)
	}
	remoteOnly := got.Backups.Entries[0]
	if remoteOnly.ID != jsonOldRemoteID || remoteOnly.Local != nil || remoteOnly.Remote == nil || !remoteOnly.OldRemote {
		t.Errorf("backups.Entries[0] = %+v, want the old remote-only copy flagged, first in the ID-sorted census", remoteOnly)
	}
	both := got.Backups.Entries[1]
	if both.ID != jsonSharedID || both.Local == nil || both.Remote == nil || both.OldRemote ||
		both.Local.Size != 10 || both.Remote.Location != "remote" {
		t.Errorf("backups.Entries[1] = %+v, want the saved-and-uploaded snapshot with both halves, not old", both)
	}
	localOnly := got.Backups.Entries[2]
	if localOnly.ID != jsonLocalOnlyID || localOnly.Local == nil || localOnly.Remote != nil {
		t.Errorf("backups.Entries[2] = %+v, want the newest snapshot local only: its tick's upload failed", localOnly)
	}
	if len(got.Backups.Retention) != 1 || !strings.Contains(got.Backups.Retention[0], jsonOldRemoteID) ||
		!strings.Contains(got.Backups.Retention[0], "2026-11-08T11:00:00Z") {
		t.Errorf("backups.Retention = %q, want the old remote copy named with its creation time", got.Backups.Retention)
	}

	m := got.Maintenance
	if m.Save != (snapshotListJSONAttempt{AttemptedAt: "2026-11-13T11:00:00Z", Outcome: "succeeded"}) {
		t.Errorf("maintenance.save = %+v, want the typed succeeded attempt", m.Save)
	}
	if m.Upload != (snapshotListJSONAttempt{AttemptedAt: "2026-11-13T11:01:00Z", Outcome: "failed"}) {
		t.Errorf("maintenance.upload = %+v, want the typed failed attempt", m.Upload)
	}
	if m.Prune != (snapshotListJSONAttempt{AttemptedAt: "2026-11-13T11:02:00Z", Outcome: "failed"}) {
		t.Errorf("maintenance.prune = %+v, want the independent typed failed prune", m.Prune)
	}
	if m.LastSaveSuccess != "2026-11-13T11:00:00Z" || m.LastUploadSuccess != "2026-11-12T11:00:00Z" {
		t.Errorf("maintenance last-success stamps = %q/%q, want each success kept separate from the failing streak",
			m.LastSaveSuccess, m.LastUploadSuccess)
	}
	if !m.AttemptsKnown || len(m.Warnings) != 1 ||
		m.Warnings[0] != "WARN: the recorded tick's remote snapshot prune failed; remote retention may not have held" {
		t.Errorf("maintenance = %+v, want the known record with its safe warning restated", m)
	}

	if !got.Complete || len(got.Warnings) != 1 ||
		got.Warnings[0] != "WARN: the recorded tick's remote snapshot prune failed; remote retention may not have held" {
		t.Errorf("complete = %v with warnings %q, want the whole read's verdict restated beside the composed stage warnings",
			got.Complete, got.Warnings)
	}
}

// TestSnapshotListJSONRefusesAnUnknownOutput: -o takes the one
// machine-readable format; anything else is refused rather than silently
// falling back to the text table.
func TestSnapshotListJSONRefusesAnUnknownOutput(t *testing.T) {
	_, err := runSnapshotList(t, nil, "-o", "yaml")
	if err == nil || !strings.Contains(err.Error(), "json") {
		t.Fatalf("err = %v, want a refusal naming json as the only machine-readable format", err)
	}
}

// The JSON rows come from one bounded read through the supported pkg/cli
// evidence surface: the command's own context reaches the read — so a
// Ctrl-C ends the underlying local and remote I/O — and a bare -o json
// passes no caps, so the engine read applies its built-in budgets. The CLI
// never invents bounds of its own.
func TestSnapshotListJSONReadsThroughTheSupportedSurface(t *testing.T) {
	var gotCtx context.Context
	var gotOpts backupevidence.EvidenceOptions
	useBackupEvidenceRead(t, func(ctx context.Context, o backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		gotCtx, gotOpts = ctx, o
		return backupevidence.EvidenceResult{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := runSnapshotList(t, ctx, "-o", "json"); err != nil {
		t.Fatalf("snapshot list -o json: %v", err)
	}
	if gotCtx != ctx {
		t.Error("the evidence read got a context other than the command's: the caller's cancellation must reach the read")
	}
	if gotOpts != (backupevidence.EvidenceOptions{}) {
		t.Errorf("the engine read got %+v, want zero caps it turns into its built-in budgets", gotOpts)
	}
}

// textTableRow pads one row of the native text table to the fixed widths it
// has always printed: 46, 6 and 10. Building the expected bytes here, not
// calling the command's own formatting, is what makes the pin independent.
func textTableRow(name, where, size, modified string) string {
	pad := func(s string, w int) string {
		return s + strings.Repeat(" ", w-len(s))
	}
	return pad(name, 46) + " " + pad(where, 6) + " " + pad(size, 10) + " " + modified + "\n"
}

// TestSnapshotListKeepsTheTextTableUnchanged: the native text table is the
// default and stays byte-for-byte what it was — the same fixed-width rows,
// printed without any evidence read behind them. The JSON rows are additive,
// not a replacement.
func TestSnapshotListKeepsTheTextTableUnchanged(t *testing.T) {
	dir := useSnapshotDir(t)
	const name = "snap-1.db"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	called := false
	useBackupEvidenceRead(t, func(context.Context, backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		called = true
		return backupevidence.EvidenceResult{}
	})

	out, err := runSnapshotList(t, nil)
	if err != nil {
		t.Fatalf("snapshot list: %v", err)
	}
	want := textTableRow("NAME", "WHERE", "SIZE", "MODIFIED (UTC)") +
		textTableRow(name, "local", "10", "2026-03-04 05:06")
	if out != want {
		t.Errorf("the default text table changed.\n got:\n%q\nwant:\n%q", out, want)
	}
	if called {
		t.Error("the default text table read the backup evidence; the native listing must stay the listing it was")
	}
}

// The backup_evidence MCP tool reads through the same supported surface the
// JSON flag reads: the CLI wires the reader at startup — the engine's MCP
// package cannot import this one — and the caller's caps reach the engine
// read unchanged, so one evidence contract stands behind both surfaces.
func TestSnapshotListJSONBacksTheMCPToolWithTheSameSurface(t *testing.T) {
	if mcpsrv.BackupEvidenceRead == nil {
		t.Fatal("the backup_evidence tool has no reader: the CLI must wire the supported evidence surface in at startup")
	}

	var gotOpts backupevidence.EvidenceOptions
	useBackupEvidenceRead(t, func(_ context.Context, o backupevidence.EvidenceOptions) backupevidence.EvidenceResult {
		gotOpts = o
		return engineEvidence()
	})

	out := mcpsrv.BackupEvidenceRead(context.Background(), mcpsrv.BackupEvidenceIn{MaxEntries: 7, MaxBytes: 2048, MaxSeconds: 5})
	if out.Node != "cp-1" || out.Policy.State != "configured" || out.Policy.IntervalSeconds != 86400 ||
		len(out.Local.Artifacts) != 2 || len(out.Backups.Entries) != 3 || !out.Maintenance.AttemptsKnown {
		t.Errorf("the tool's reader returned %+v; it must restate the surface's evidence", out)
	}
	if out.Local.Artifacts[0].Created != "2026-11-12T11:00:00Z" || out.Local.Artifacts[0].Digest == "unknown" ||
		out.Local.Artifacts[1].Digest != "unknown" {
		t.Errorf("the tool's reader restated the artifacts as %+v; stamps and digests must match the surface's", out.Local.Artifacts)
	}
	want := backupevidence.EvidenceOptions{MaxEntries: 7, MaxBytes: 2048, MaxTime: 5 * time.Second}
	if gotOpts != want {
		t.Errorf("the engine read got %+v, want %+v: the tool's caps must reach the read unchanged", gotOpts, want)
	}
}
