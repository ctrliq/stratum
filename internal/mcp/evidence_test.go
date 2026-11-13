package mcp

// The backup_evidence tool: the MCP half of the machine-readable backup
// evidence `stratum snapshot list -o json` prints — the same typed rows,
// served through the catalog at the read tier, so a client with no flags
// sees the engine's own view of what it does for backups.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// backupEvidenceRow returns the catalog's backup_evidence descriptor.
func backupEvidenceRow(t *testing.T) *Tool {
	t.Helper()
	cat := testCatalog()
	for i := range cat {
		if cat[i].Name == "backup_evidence" {
			return &cat[i]
		}
	}
	return nil
}

// TestBackupEvidenceShipsInTheCatalogAtTheReadTier: the evidence contract is
// exposed as a read-tier tool that reaches no Kubernetes API, with the
// description a model chooses it by and a handler that actually registers.
func TestBackupEvidenceShipsInTheCatalogAtTheReadTier(t *testing.T) {
	row := backupEvidenceRow(t)
	if row == nil {
		t.Fatal("the catalog carries no backup_evidence row: the read-only evidence contract is not exposed to MCP clients")
	}
	if row.Tier != TierRead {
		t.Errorf("backup_evidence ships at tier %s; evidence is a read and must register with no flags", row.Tier)
	}
	if row.Credential != CredentialNone {
		t.Errorf("backup_evidence declares credential %q; the evidence read reaches no Kubernetes API", row.Credential)
	}
	if row.Description == "" {
		t.Error("backup_evidence carries no description: a model chooses tools by description")
	}
	if row.Add == nil {
		t.Error("backup_evidence carries no handler-adder: the tool would register and never answer")
	}
}

// TestBackupEvidenceIsAdvertisedToTheClient: registering the catalog
// advertises the tool over the wire, annotated read-only like every tool a
// client may call with no flags.
func TestBackupEvidenceIsAdvertisedToTheClient(t *testing.T) {
	cs := fencedSession(t, func(s *sdk.Server) {
		for _, d := range testCatalog() {
			d.Add(s, catalogTool(d.Name, d.Description, d.Tier))
		}
	})

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var tool *sdk.Tool
	for i := range res.Tools {
		if res.Tools[i].Name == "backup_evidence" {
			tool = res.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("tools/list carries no backup_evidence: the read tier advertises every engine tool with no flags")
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("backup_evidence is advertised as %+v; a read-tier tool must carry the read-only hint", tool.Annotations)
	}
	if tool.InputSchema == nil {
		t.Error("backup_evidence has no input schema; typed inputs are what make the caps describable")
	}
}

// useBackupEvidenceRead points the tool's evidence reader seam at fn, the
// same seam swap the snapshot handlers' tests use for snapshotDir and
// loadSnapshotConfig: the tool serves what the supported surface reads, and
// a swapped fn stands in for the surface.
func useBackupEvidenceRead(t *testing.T, fn func(context.Context, BackupEvidenceIn) BackupEvidenceOut) {
	t.Helper()
	old := BackupEvidenceRead
	t.Cleanup(func() { BackupEvidenceRead = old })
	BackupEvidenceRead = fn
}

// craftedBackupEvidence is one complete evidence document, built the way the
// supported surface restates the engine's read: the fixed stamps make the
// over-the-wire comparison exact, whenever the suite runs.
func craftedBackupEvidence() BackupEvidenceOut {
	art := func(id, loc, created string, size int64, digest string) BackupArtifactRow {
		return BackupArtifactRow{ID: id, Location: loc, Created: created, Size: size, Digest: digest}
	}
	const shared = "etcd-snapshot-cp-1-20261112-110000.db"
	return BackupEvidenceOut{
		Policy: BackupPolicyRow{
			State: "configured", IntervalSeconds: 86400, IntervalFrom: "cluster-config",
			Retain: 3, RetainFrom: "snapshot-conf",
			Copy: "remote-required", CopyFrom: "snapshot-conf",
			Dir: "/var/backups/etcd", DirFrom: "default",
			Warnings: []string{},
		},
		Node: "cp-1",
		Local: BackupLocalRow{
			Node: "cp-1",
			Artifacts: []BackupArtifactRow{
				art(shared, "local", "2026-11-12T11:00:00Z", 10, "sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"),
				art("etcd-snapshot-cp-1-20261112-120000.db", "local", "2026-11-12T11:01:00Z", 16, "unknown"),
			},
			Complete: true,
			Warnings: []string{},
		},
		Remote: BackupRemoteRow{
			Copies: []BackupArtifactRow{
				art(shared, "remote", "2026-11-12T11:00:00Z", 10, "unknown"),
				art("etcd-snapshot-cp-1-20261108-110000.db", "remote", "2026-11-08T11:00:00Z", 14, "unknown"),
			},
			Old:      []BackupArtifactRow{art("etcd-snapshot-cp-1-20261108-110000.db", "remote", "2026-11-08T11:00:00Z", 14, "unknown")},
			Complete: true,
			Warnings: []string{},
		},
		Backups: BackupCensusRow{
			Node: "cp-1",
			Entries: []BackupCensusEntryRow{
				{
					ID:     shared,
					Local:  &BackupArtifactRow{ID: shared, Location: "local", Created: "2026-11-12T11:00:00Z", Size: 10, Digest: "sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"},
					Remote: &BackupArtifactRow{ID: shared, Location: "remote", Created: "2026-11-12T11:00:00Z", Size: 10, Digest: "unknown"},
				},
				{
					ID:        "etcd-snapshot-cp-1-20261108-110000.db",
					Remote:    &BackupArtifactRow{ID: "etcd-snapshot-cp-1-20261108-110000.db", Location: "remote", Created: "2026-11-08T11:00:00Z", Size: 14, Digest: "unknown"},
					OldRemote: true,
				},
			},
			Complete:  true,
			Warnings:  []string{},
			Retention: []string{"WARN: the remote copy etcd-snapshot-cp-1-20261108-110000.db was created 2026-11-08T11:00:00Z, before the retention cutoff; it predates the retention window or survived a failed prune"},
		},
		Maintenance: BackupMaintenanceRow{
			Save:              BackupAttemptRow{AttemptedAt: "2026-11-12T10:00:00Z", Outcome: "succeeded"},
			Upload:            BackupAttemptRow{AttemptedAt: "2026-11-12T10:01:00Z", Outcome: "failed"},
			Prune:             BackupAttemptRow{AttemptedAt: "2026-11-12T10:02:00Z", Outcome: "failed"},
			LastSaveSuccess:   "2026-11-12T10:00:00Z",
			LastUploadSuccess: "2026-11-12T08:00:00Z",
			Warnings:          []string{"WARN: the recorded tick's remote snapshot prune failed; remote retention may not have held"},
			AttemptsKnown:     true,
		},
		Complete: true,
		Warnings: []string{},
	}
}

// TestBackupEvidenceReturnsTheRowsOverTheWire: the registered tool answers a
// call with the typed evidence rows as structured content, and the caller's
// caps reach the evidence reader unchanged — the read's bounds are the
// caller's when it sets them.
func TestBackupEvidenceReturnsTheRowsOverTheWire(t *testing.T) {
	fakeStratumHost(t)

	want := craftedBackupEvidence()
	var gotIn BackupEvidenceIn
	useBackupEvidenceRead(t, func(_ context.Context, in BackupEvidenceIn) BackupEvidenceOut {
		gotIn = in
		return want
	})

	row := backupEvidenceRow(t)
	if row == nil {
		t.Fatal("the catalog carries no backup_evidence row")
	}
	cs := fencedSession(t, func(s *sdk.Server) {
		row.Add(s, catalogTool(row.Name, row.Description, row.Tier))
	})

	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      "backup_evidence",
		Arguments: map[string]any{"max_entries": 7, "max_bytes": 2048, "max_seconds": 5},
	})
	if err != nil {
		t.Fatalf("call backup_evidence: %v", err)
	}
	if res.IsError {
		t.Fatalf("backup_evidence reported an error result: %+v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var got BackupEvidenceOut
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode backup_evidence's structured content %s: %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backup_evidence returned a different document.\n got: %+v\nwant: %+v", got, want)
	}
	if gotIn != (BackupEvidenceIn{MaxEntries: 7, MaxBytes: 2048, MaxSeconds: 5}) {
		t.Errorf("the evidence reader got %+v; the caller's caps must reach the supported surface unchanged", gotIn)
	}
}

// The tool is a Stratum read like its neighbours: off a Stratum node it
// refuses with the shared explanation, not with evidence about nothing.
func TestBackupEvidenceRefusesAMachineThatIsNotAStratumHost(t *testing.T) {
	notStratumHost(t)
	_, _, err := BackupEvidenceHandler()(context.Background(), nil, BackupEvidenceIn{})
	if !errors.Is(err, errNotStratumHost) {
		t.Fatalf("err = %v, want the not-a-Stratum-node refusal", err)
	}
}

// The call's context is handed to the evidence surface unchanged, so the
// read's cancellation — the client's disconnect — ends the underlying local
// and remote I/O.
func TestBackupEvidenceHandsTheCallContextToTheSurface(t *testing.T) {
	fakeStratumHost(t)
	var gotCtx context.Context
	useBackupEvidenceRead(t, func(ctx context.Context, _ BackupEvidenceIn) BackupEvidenceOut {
		gotCtx = ctx
		return BackupEvidenceOut{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := BackupEvidenceHandler()(ctx, nil, BackupEvidenceIn{}); err != nil {
		t.Fatalf("backup_evidence: %v", err)
	}
	if gotCtx != ctx {
		t.Error("the evidence surface got a context other than the caller's: the read's cancellation would never fire")
	}
}

// A server built without the CLI's wiring says so, rather than serving
// invented evidence: the engine's MCP package cannot reach the supported
// surface on its own.
func TestBackupEvidenceWithoutAWiredReaderSaysSo(t *testing.T) {
	fakeStratumHost(t)
	useBackupEvidenceRead(t, nil)

	_, _, err := BackupEvidenceHandler()(context.Background(), nil, BackupEvidenceIn{})
	if err == nil || !strings.Contains(err.Error(), "supported pkg/cli evidence surface") {
		t.Fatalf("err = %v, want the refusal naming the supported pkg/cli evidence surface", err)
	}
}
