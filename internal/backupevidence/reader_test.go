// From here: the assembled read-only Evidence() reader — one bounded call
// composing the effective policy (policy.go), the durable attempt record
// (state.go) and the local and remote catalogs (local.go, remote.go) into
// the evidence an external consumer reads. The tests prove the composed
// result shape, the redaction (no credential, executable path or raw
// storage error ever reaches the evidence) and the overall time, byte and
// page budget across the combined reads.
package backupevidence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// evidenceTime is the fixed UTC clock the reader's retention window is
// pinned to in these tests, so which remote copies predate the window —
// Retain snapshots at the configured interval back from now — asserts
// exactly, whenever the suite runs.
var evidenceTime = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// pinEvidenceClock points the reader's clock seam at evidenceTime.
func pinEvidenceClock(t *testing.T) {
	t.Helper()
	old := evidenceNow
	evidenceNow = func() time.Time { return evidenceTime }
	t.Cleanup(func() { evidenceNow = old })
}

// copyTime is a fixed UTC creation time inside the retention windows these
// tests draw from the pinned clock.
func copyTime(min int) time.Time {
	return time.Date(2026, 11, 12, 11, min, 0, 0, time.UTC)
}

// oldCopyTime is a fixed UTC creation time before every retention window
// these tests draw.
func oldCopyTime() time.Time {
	return time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)
}

// seedAttempts writes the durable attempt record the reader loads, through
// the redirected evidence paths, the same file the loop's recording writes.
func seedAttempts(t *testing.T, seed func(*Attempts)) {
	t.Helper()
	withEvidencePaths(t)
	a := &Attempts{}
	seed(a)
	if err := SaveAttempts(a); err != nil {
		t.Fatalf("seed the attempt record: %v", err)
	}
}

// dumpEvidence renders the whole result — every string field included — so
// a leak anywhere in the composed evidence shows in the dump.
func dumpEvidence(t *testing.T, res EvidenceResult) string {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal the evidence result: %v", err)
	}
	return string(b)
}

// hungRemoteStore is a store that never answers: the parked handler holds
// every listing until its request context ends, closing sawCancel when it
// does — the proof a caller's context reached the S3 call.
func hungRemoteStore(t *testing.T) (*objstore.Target, chan struct{}) {
	t.Helper()
	sawCancel := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // parked: the store never answers
		once.Do(func() { close(sawCancel) })
	}))
	t.Cleanup(srv.Close)
	return &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}, sawCancel
}

// refusingRemoteStore answers every listing with a 403 whose body carries
// the given marker, so the redaction test can seed a raw error the
// evidence must never echo.
func refusingRemoteStore(t *testing.T, marker string) *objstore.Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>` + marker + `</Message></Error>`))
	}))
	t.Cleanup(srv.Close)
	return &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

// One bounded call assembles the whole evidence view: the effective policy
// with every value's provenance, the local artifacts with node-scoped
// location, creation time, size and digest-or-unknown, the remote copies
// with the same shape, the deduplicated census counting each backup once,
// and the maintenance diagnostics — typed attempt outcomes with their
// timestamps, the separate last-success stamps, and the scheduling and
// retention warnings the loop's record carries, stated as typed safe
// reasons. Every read ran whole within the default bounds, so the result
// is complete and exact.
func TestReaderComposesPolicyArtifactsCopiesAndMaintenance(t *testing.T) {
	pinEvidenceClock(t)
	withEvidencePaths(t)
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000") // local and remote
	s2 := etcdsnap.ScheduledName("cp-1", "20261112-120000") // local only
	s3 := etcdsnap.ScheduledName("cp-1", "20261112-130000") // remote only
	writeSnapshot(t, dir, s1, 10, copyTime(0))
	writeSnapshot(t, dir, s2, 16, copyTime(1))
	hexd := strings.Repeat("a1b2", 16) // 64 hex characters, the sha256sum layout
	if err := os.WriteFile(filepath.Join(dir, s1+".sha256"), []byte(hexd+"  "+s1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tgt := stubRemote(t, []remoteObj{ro(s1, 12, copyTime(0)), ro(s3, 14, copyTime(2))})
	recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n    intervalHours: 24\n    dir: "+dir+"\n")
	snapshotConfSupplies(t, etcdsnap.Config{Retain: 3, S3: *tgt})
	saved := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	seedAttempts(t, func(a *Attempts) {
		a.RecordSave(saved, nil)
		a.RecordUpload(saved.Add(time.Minute), nil)
		a.RecordPrune(saved.Add(2*time.Minute), errors.New("prune /var/backups/etcd: permission denied"))
		a.SetWarnings(
			"stratum: WARN: cannot read snapshot config, pruning skipped this tick (snapshot still taken and kept, no S3 upload): read /etc/stratum/snapshot.conf: is a directory",
			"WARN: S3 snapshot prune: delete "+s1+": 403 from the store",
		)
	})

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})

	if res.Policy.State != StateConfigured {
		t.Errorf("Policy.State = %q, want %q: the cluster config and conf both configure the loop", res.Policy.State, StateConfigured)
	}
	if res.Policy.Interval != 24*time.Hour || res.Policy.IntervalFrom != FromClusterConfig {
		t.Errorf("Policy.Interval = %v (%s), want 24h from the cluster config", res.Policy.Interval, res.Policy.IntervalFrom)
	}
	if res.Policy.Retain != 3 || res.Policy.RetainFrom != FromSnapshotConf {
		t.Errorf("Policy.Retain = %d (%s), want 3 from the conf, which wins outright", res.Policy.Retain, res.Policy.RetainFrom)
	}
	if res.Policy.Copy != CopyRemoteRequired || res.Policy.CopyFrom != FromSnapshotConf {
		t.Errorf("Policy.Copy = %q (%s), want remote-required from the conf's target", res.Policy.Copy, res.Policy.CopyFrom)
	}
	if res.Policy.Dir != dir || res.Policy.DirFrom != FromClusterConfig {
		t.Errorf("Policy.Dir = %q (%s), want the configured directory", res.Policy.Dir, res.Policy.DirFrom)
	}
	if res.Node != "cp-1" {
		t.Errorf("Node = %q, want the node the options scoped the read to", res.Node)
	}

	if !res.Local.Complete || len(res.Local.Warnings) != 0 {
		t.Errorf("Local = %+v, want a complete walk with no warnings", res.Local)
	}
	if len(res.Local.Artifacts) != 2 {
		t.Fatalf("Local.Artifacts = %+v, want %q and %q", res.Local.Artifacts, s1, s2)
	}
	if a := res.Local.Artifacts[0]; a.ID != s1 || a.Location != "local" || a.Size != 10 || !a.Created.Equal(copyTime(0)) || a.Digest != "sha256:"+hexd {
		t.Errorf("Local.Artifacts[0] = %+v, want %q local, size 10, the sidecar's digest and its creation time", a, s1)
	}
	if a := res.Local.Artifacts[1]; a.ID != s2 || a.Location != "local" || a.Size != 16 || !a.Created.Equal(copyTime(1)) || a.Digest != DigestUnknown {
		t.Errorf("Local.Artifacts[1] = %+v, want %q local, size 16, digest unknown", a, s2)
	}

	if !res.Remote.Complete || len(res.Remote.Warnings) != 0 {
		t.Errorf("Remote = %+v, want a complete listing with no warnings", res.Remote)
	}
	if len(res.Remote.Copies) != 2 || res.Remote.Copies[0].ID != s1 || res.Remote.Copies[1].ID != s3 {
		t.Fatalf("Remote.Copies = %+v, want %q and %q sorted by ID", res.Remote.Copies, s1, s3)
	}
	if c := res.Remote.Copies[0]; c.Location != "remote" || c.Size != 12 || !c.Created.Equal(copyTime(0)) || c.Digest != DigestUnknown {
		t.Errorf("Remote.Copies[0] = %+v, want %q remote, size 12, digest unknown", c, s1)
	}

	if !res.Backups.Complete {
		t.Error("Backups.Complete = false, want true: both catalogs were complete, so the census is exact")
	}
	if got := []string{res.Backups.Entries[0].ID, res.Backups.Entries[1].ID, res.Backups.Entries[2].ID}; !slices.Equal(got, []string{s1, s2, s3}) {
		t.Fatalf("Backups.Entries = %q, want %q, %q and %q sorted by ID", got, s1, s2, s3)
	}
	if e := res.Backups.Entries[0]; e.Local == nil || e.Remote == nil {
		t.Errorf("entry %s = %+v, want both locations: one snapshot in two places counts once", s1, e)
	}
	if e := res.Backups.Entries[1]; e.Local == nil || e.Remote != nil {
		t.Errorf("entry %s = %+v, want the local artifact alone", s2, e)
	}
	if e := res.Backups.Entries[2]; e.Local != nil || e.Remote == nil {
		t.Errorf("entry %s = %+v, want the remote copy alone", s3, e)
	}

	m := res.Maintenance
	if !m.AttemptsKnown {
		t.Error("Maintenance.AttemptsKnown = false, want true: the record was read")
	}
	if !m.Save.AttemptedAt.Equal(saved) || m.Save.Outcome != AttemptSucceeded {
		t.Errorf("Maintenance.Save = %+v, want the successful save attempt at %v", m.Save, saved)
	}
	if !m.Upload.AttemptedAt.Equal(saved.Add(time.Minute)) || m.Upload.Outcome != AttemptSucceeded {
		t.Errorf("Maintenance.Upload = %+v, want the successful upload attempt at %v", m.Upload, saved.Add(time.Minute))
	}
	if !m.Prune.AttemptedAt.Equal(saved.Add(2*time.Minute)) || m.Prune.Outcome != AttemptFailed {
		t.Errorf("Maintenance.Prune = %+v, want the failed prune attempt at %v, typed, not its raw error", m.Prune, saved.Add(2*time.Minute))
	}
	if !m.LastSaveSuccess.Equal(saved) || !m.LastUploadSuccess.Equal(saved.Add(time.Minute)) {
		t.Errorf("Maintenance last-success stamps = %v/%v, want %v/%v", m.LastSaveSuccess, m.LastUploadSuccess, saved, saved.Add(time.Minute))
	}
	if len(m.Warnings) != 2 {
		t.Fatalf("Maintenance.Warnings = %q, want the conf and remote-prune conditions stated as typed safe reasons", m.Warnings)
	}
	if w := m.Warnings[0]; !strings.Contains(w, "snapshot config") || !strings.Contains(w, "kept every snapshot") {
		t.Errorf("Maintenance.Warnings[0] = %q, want the unreadable-conf condition stated safely", w)
	}
	if w := m.Warnings[1]; !strings.Contains(w, "remote snapshot prune") {
		t.Errorf("Maintenance.Warnings[1] = %q, want the refused remote prune stated safely", w)
	}

	if !res.Complete {
		t.Error("Complete = false, want true: every stage ran whole within the bounds")
	}
	w := strings.Join(res.Warnings, "\n")
	if !strings.Contains(w, "kept every snapshot") || !strings.Contains(w, "remote snapshot prune") {
		t.Errorf("Warnings do not aggregate the maintenance conditions: %q", w)
	}
	if strings.Contains(w, "permission denied") || strings.Contains(w, "403 from the store") {
		t.Errorf("the recorded raw error text escaped into the warnings: %q", w)
	}
}

// The census counts each backup once by its stable ID — the same snapshot
// saved locally and uploaded off-box is one backup in two locations — and
// a remote copy created before the retention window the effective policy
// draws (Retain snapshots at the configured interval back from now) is
// surfaced to the retention diagnostics with its creation time.
func TestReaderCountsEachBackupOnceAndSurfacesOldRemoteCopies(t *testing.T) {
	pinEvidenceClock(t)
	withEvidencePaths(t)
	dir := t.TempDir()
	oldID := etcdsnap.ScheduledName("cp-1", "20261108-110000") // local and an old remote copy
	newID := etcdsnap.ScheduledName("cp-1", "20261112-120000") // a remote copy inside the window
	writeSnapshot(t, dir, oldID, 10, copyTime(3))

	tgt := stubRemote(t, []remoteObj{ro(newID, 10, copyTime(0)), ro(oldID, 10, oldCopyTime())})
	recordedConfig(t, "spec:\n  snapshots:\n    retain: 2\n    intervalHours: 24\n    dir: "+dir+"\n")
	snapshotConfSupplies(t, etcdsnap.Config{Retain: 2, S3: *tgt})

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})

	if !res.Complete {
		t.Errorf("Complete = false, want true: every read ran whole: %+v", res)
	}
	if len(res.Backups.Entries) != 2 {
		t.Fatalf("Backups.Entries = %+v, want 2: %q exists in both places and counts once", res.Backups.Entries, oldID)
	}
	e := res.Backups.Entries[0]
	if e.ID != oldID || e.Local == nil || e.Remote == nil || !e.OldRemote {
		t.Errorf("entry %s = %+v, want both locations and the old-remote flag", oldID, e)
	}
	if e := res.Backups.Entries[1]; e.ID != newID || e.Local != nil || e.Remote == nil || e.OldRemote {
		t.Errorf("entry %s = %+v, want the remote copy alone inside the window", newID, e)
	}
	if len(res.Backups.Retention) != 1 {
		t.Fatalf("Backups.Retention = %q, want the one old copy's diagnostic", res.Backups.Retention)
	}
	if want := oldID + " was created " + oldCopyTime().UTC().Format(time.RFC3339); !strings.Contains(res.Backups.Retention[0], want) {
		t.Errorf("Backups.Retention = %q, want it to name %s with its creation time", res.Backups.Retention[0], want)
	}
}

// Nothing the evidence resolves on its way to the result ever leaks: the
// S3 target the remote listing resolves carries credentials and never
// appears; the attempt record's raw error text — an etcdctl executable
// path, an upload error naming the store — never crosses; a raw local
// read error and a raw store refusal are turned into safe diagnostics that
// name the condition. The evidence stays usable beside every failure: the
// readable artifact is described and the refusal is named.
func TestReaderNeverLeaksCredentialsExecutablePathsOrRawErrors(t *testing.T) {
	withEvidencePaths(t)
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000") // the read that fails, seeded with a raw error
	s2 := etcdsnap.ScheduledName("cp-1", "20261112-120000") // the read that succeeds
	writeSnapshot(t, dir, s1, 10, copyTime(0))
	writeSnapshot(t, dir, s2, 16, copyTime(1))

	old := localEntryInfo
	t.Cleanup(func() { localEntryInfo = old })
	localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
		if name == s1 {
			return 0, time.Time{}, errors.New("stat /var/backups/etcd/" + s1 + ": permission denied AKIA7QLEAKEDKEY 0xdeadbeef")
		}
		return old(ctx, dir, name)
	}

	tgt := refusingRemoteStore(t, "AKIA7QLEAKEDKEY denied")
	recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n    intervalHours: 24\n    dir: "+dir+"\n")
	snapshotConfSupplies(t, etcdsnap.Config{Retain: 3, S3: objstore.Target{
		Endpoint: tgt.Endpoint, Bucket: tgt.Bucket, AccessKey: "AKIA7QLEAKEDKEY", SecretKey: "hush-hush-supersecret", PathStyle: true,
	}})
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	seedAttempts(t, func(a *Attempts) {
		a.RecordSave(at, errors.New("fork/exec /opt/stratum/bin/etcdctl: permission denied 0xfeedface"))
		a.RecordUpload(at.Add(time.Minute), errors.New("PutObject https://127.0.0.1:1/b/key: 403 AKIA7QLEAKEDKEY"))
		a.RecordPrune(at.Add(2*time.Minute), errors.New("S3 snapshot prune: delete refused 0xdeadbeef"))
		a.SetWarnings(
			"WARN: S3 snapshot prune: AKIA7QLEAKEDKEY refused at /opt/stratum/bin/aws",
			"WARN: something novel 0xfeedface",
		)
	})

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})
	dump := dumpEvidence(t, res)

	for _, leak := range []string{"AKIA7QLEAKEDKEY", "hush-hush-supersecret", "/opt/stratum/bin/etcdctl", "0xdeadbeef", "0xfeedface", "fork/exec", "PutObject", tgt.Endpoint} {
		if strings.Contains(dump, leak) {
			t.Errorf("%q escaped into the evidence:\n%s", leak, dump)
		}
	}

	// The evidence stays usable and named: the readable artifact stands, the
	// failed read and the refusal are named by their conditions, and the
	// attempts carry typed outcomes with their timestamps.
	if res.Complete {
		t.Error("Complete = true, want false: a local read failed and the store refused the listing")
	}
	if len(res.Local.Artifacts) != 1 || res.Local.Artifacts[0].ID != s2 {
		t.Errorf("Local.Artifacts = %+v, want the readable %q", res.Local.Artifacts, s2)
	}
	w := strings.Join(res.Local.Warnings, "\n")
	if !strings.Contains(w, s1) || !strings.Contains(w, "cannot be read") {
		t.Errorf("the local warnings do not name the unreadable snapshot: %q", w)
	}
	if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "refused") {
		t.Errorf("the remote warnings do not name the refusal: %q", w)
	}
	m := res.Maintenance
	if m.Save.Outcome != AttemptFailed || !m.Save.AttemptedAt.Equal(at) {
		t.Errorf("Maintenance.Save = %+v, want the typed failed outcome at %v, never the raw error", m.Save, at)
	}
	if m.Upload.Outcome != AttemptFailed || m.Prune.Outcome != AttemptFailed {
		t.Errorf("Maintenance outcomes = %q/%q, want both typed as failed", m.Upload.Outcome, m.Prune.Outcome)
	}
	if len(m.Warnings) != 2 {
		t.Fatalf("Maintenance.Warnings = %q, want the remote-prune condition and the unstateable warning", m.Warnings)
	}
	if w0 := m.Warnings[0]; !strings.Contains(w0, "remote snapshot prune") {
		t.Errorf("Maintenance.Warnings[0] = %q, want the refused remote prune stated safely", w0)
	}
	if w1 := m.Warnings[1]; !strings.Contains(w1, "cannot state safely") {
		t.Errorf("Maintenance.Warnings[1] = %q, want the recorded warning's condition named without echoing its text", w1)
	}
}

// The overall time budget is one deadline across every read the call
// makes. A hung local read is ended by the caller's deadline, and a store
// that never answers is ended by the overall budget the caller set — the
// store sees its request context end, so the deadline reached the S3 call
// itself. Either way the call returns bounded, the stages that never ran
// are unknown rather than zero, and the result says it is not complete.
func TestReaderHonorsTheOverallDeadlineAcrossReads(t *testing.T) {
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000")
	writeSnapshot(t, dir, s1, 10, copyTime(0))

	t.Run("the caller's deadline ends a hung local read", func(t *testing.T) {
		withEvidencePaths(t)
		recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
		noSnapshotConf(t)
		old := localEntryInfo
		t.Cleanup(func() { localEntryInfo = old })
		localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
			<-ctx.Done() // parked: the read hangs until the request is cancelled
			return 0, time.Time{}, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		t.Cleanup(cancel)

		start := time.Now()
		res := Evidence(ctx, EvidenceOptions{Node: "cp-1"})
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("the read took %v, want it ended by the caller's deadline", took)
		}
		if res.Local.Complete || len(res.Local.Artifacts) != 0 {
			t.Errorf("Local = %+v, want no artifacts and no complete total from a cancelled walk", res.Local)
		}
		if w := strings.Join(res.Local.Warnings, "\n"); !strings.Contains(w, "cancel") {
			t.Errorf("the local warnings do not say the walk was cancelled: %q", w)
		}
		if res.Remote.Complete {
			t.Error("Remote.Complete = true, want false: the store may hold anything, the listing never ran")
		}
		if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "cancel") {
			t.Errorf("the remote warnings do not say the listing never ran: %q", w)
		}
		if res.Complete || res.Backups.Complete {
			t.Error("the result claims completeness from a read its own deadline cut short")
		}
	})
	t.Run("the overall time budget reaches the remote listing", func(t *testing.T) {
		withEvidencePaths(t)
		tgt, sawCancel := hungRemoteStore(t)
		recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 3, S3: *tgt})

		res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxTime: 150 * time.Millisecond})

		select {
		case <-sawCancel:
		case <-time.After(5 * time.Second):
			t.Error("the store never saw its request context end: the overall deadline did not reach the S3 listing")
		}
		if !res.Local.Complete || len(res.Local.Artifacts) != 1 {
			t.Errorf("Local = %+v, want the local half read whole before the listing hung", res.Local)
		}
		if res.Remote.Complete {
			t.Error("Remote.Complete = true, want false: a hung listing is not a total")
		}
		if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "time bound") {
			t.Errorf("the remote warnings do not say the listing exceeded its time bound: %q", w)
		}
		if res.Complete {
			t.Error("Complete = true, want false: the remote half never finished")
		}
	})
}

// The overall page budget counts every backup the call describes — each
// local artifact and each remote copy. The local walk draws from the budget
// first; when it alone consumes the page budget the remote listing never
// runs and says its catalog is unknown, and when the listing describes more
// copies than the budget left, the copies that fit stand as a floor with
// the catalog marked not a complete total — never presented as the store's
// exact total.
func TestReaderEnforcesTheOverallPageBudgetAcrossReads(t *testing.T) {
	t.Run("the local walk alone consumes the page budget", func(t *testing.T) {
		withEvidencePaths(t)
		dir := t.TempDir()
		s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000")
		s2 := etcdsnap.ScheduledName("cp-1", "20261112-120000")
		s3 := etcdsnap.ScheduledName("cp-1", "20261112-130000")
		writeSnapshot(t, dir, s1, 10, copyTime(0))
		writeSnapshot(t, dir, s2, 10, copyTime(1))
		tgt := stubRemote(t, []remoteObj{ro(s1, 10, copyTime(0)), ro(s3, 10, copyTime(2))})
		recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 3, S3: *tgt})

		res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxEntries: 2})

		if !res.Local.Complete || len(res.Local.Artifacts) != 2 {
			t.Errorf("Local = %+v, want both artifacts: the walk covered the store exactly at the cap", res.Local)
		}
		if res.Remote.Complete || len(res.Remote.Copies) != 0 {
			t.Errorf("Remote = %+v, want no copies and no complete total: the page budget was spent", res.Remote)
		}
		if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "page budget") || !strings.Contains(w, "local walk") {
			t.Errorf("the remote warnings do not name the overall page budget the local walk consumed: %q", w)
		}
		if res.Backups.Complete || len(res.Backups.Entries) != 2 {
			t.Errorf("Backups = %+v, want the two local artifacts as a floor and no exact total", res.Backups)
		}
		if res.Complete {
			t.Error("Complete = true, want false: the remote half of the budget never ran")
		}
	})
	t.Run("the remote listing shares what remains of the page budget", func(t *testing.T) {
		pinEvidenceClock(t)
		withEvidencePaths(t)
		dir := t.TempDir()
		s1 := etcdsnap.ScheduledName("cp-1", "20261112-090000")    // local only
		rNew := etcdsnap.ScheduledName("cp-1", "20261112-100000")  // new: inside the window, kept by the budget
		rOld1 := etcdsnap.ScheduledName("cp-1", "20261112-120000") // old: local and remote, dropped by the budget
		rOld2 := etcdsnap.ScheduledName("cp-1", "20261112-130000") // old: remote only, dropped by the budget
		writeSnapshot(t, dir, s1, 10, copyTime(0))
		writeSnapshot(t, dir, rOld1, 10, copyTime(3))
		tgt := stubRemote(t, []remoteObj{
			ro(rNew, 10, copyTime(0)),
			ro(rOld1, 10, oldCopyTime()), // sorts after rNew, so the budget drops it
			ro(rOld2, 10, oldCopyTime()),
		})
		recordedConfig(t, "spec:\n  snapshots:\n    retain: 2\n    intervalHours: 24\n    dir: "+dir+"\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 2, S3: *tgt})

		res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxEntries: 3})

		if len(res.Local.Artifacts) != 2 || !res.Local.Complete {
			t.Fatalf("Local = %+v, want both local artifacts within the budget", res.Local)
		}
		if len(res.Remote.Copies) != 1 || res.Remote.Copies[0].ID != rNew {
			t.Errorf("Remote.Copies = %+v, want only %q: one entry remained of the page budget", res.Remote.Copies, rNew)
		}
		if res.Remote.Complete {
			t.Error("Remote.Complete = true, want false: the budget stopped the listing early")
		}
		if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "page budget") {
			t.Errorf("the remote warnings do not name the overall page budget: %q", w)
		}
		// The old-copy flags follow the copies that survived the budget: a
		// dropped copy is flagged nowhere, least of all on an entry whose
		// remote half no longer exists.
		if len(res.Remote.Old) != 0 {
			t.Errorf("Remote.Old = %+v, want none: both old copies were dropped by the budget", res.Remote.Old)
		}
		if len(res.Backups.Entries) != 3 {
			t.Fatalf("Backups.Entries = %+v, want 3: two local artifacts and the one copy the budget allowed", res.Backups.Entries)
		}
		for _, e := range res.Backups.Entries {
			if e.OldRemote {
				t.Errorf("entry %s carries the old-remote flag without a remote copy in the budget: %+v", e.ID, e)
			}
		}
		if len(res.Backups.Retention) != 0 {
			t.Errorf("Backups.Retention = %q, want none: the old copies were dropped by the budget", res.Backups.Retention)
		}
		if res.Complete || res.Backups.Complete {
			t.Error("the result claims an exact total from a listing the page budget stopped early")
		}
	})
}

// The overall byte budget counts the artifact bytes both walks account
// for. The local walk draws first; when it alone consumes the byte budget
// the remote listing never runs, and the listing that does run gets what
// remains — a copy that would fit the full budget but not the remainder
// stops it, so the combined reads never account past the cap.
func TestReaderEnforcesTheOverallByteBudgetAcrossReads(t *testing.T) {
	t.Run("the local walk alone consumes the byte budget", func(t *testing.T) {
		withEvidencePaths(t)
		dir := t.TempDir()
		s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000")
		s2 := etcdsnap.ScheduledName("cp-1", "20261112-120000")
		writeSnapshot(t, dir, s1, 150, copyTime(0))
		writeSnapshot(t, dir, s2, 60, copyTime(1))
		tgt := stubRemote(t, []remoteObj{ro(s2, 10, copyTime(0))})
		recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 3, S3: *tgt})

		res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxBytes: 150})

		if res.Local.Complete || len(res.Local.Artifacts) != 1 || res.Local.Artifacts[0].ID != s1 {
			t.Errorf("Local = %+v, want only %q: the second artifact crossed the byte budget", res.Local, s1)
		}
		if w := strings.Join(res.Local.Warnings, "\n"); !strings.Contains(w, "byte cap") {
			t.Errorf("the local warnings do not name the byte cap: %q", w)
		}
		if res.Remote.Complete || len(res.Remote.Copies) != 0 {
			t.Errorf("Remote = %+v, want no copies and no complete total: the byte budget was spent", res.Remote)
		}
		if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "byte budget") || !strings.Contains(w, "local walk") {
			t.Errorf("the remote warnings do not name the overall byte budget the local walk consumed: %q", w)
		}
		if res.Complete || len(res.Backups.Entries) != 1 {
			t.Errorf("the result = %+v, want the one described artifact as a floor and no completeness", res)
		}
	})
	t.Run("the remote listing shares what remains of the byte budget", func(t *testing.T) {
		withEvidencePaths(t)
		dir := t.TempDir()
		s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000")
		writeSnapshot(t, dir, s1, 100, copyTime(0))
		tgt := stubRemote(t, []remoteObj{ro(s1, 60, copyTime(0))}) // fits the full 150, not the remaining 50
		recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 3, S3: *tgt})

		res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxBytes: 150})

		if !res.Local.Complete || len(res.Local.Artifacts) != 1 {
			t.Errorf("Local = %+v, want the one artifact accounted whole", res.Local)
		}
		if res.Remote.Complete || len(res.Remote.Copies) != 0 {
			t.Errorf("Remote = %+v, want the copy that crossed the remaining byte budget dropped", res.Remote)
		}
		if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "stopped early") {
			t.Errorf("the remote warnings do not say the listing stopped at its byte bound: %q", w)
		}
		if res.Complete {
			t.Error("Complete = true, want false: the remaining byte budget stopped the listing")
		}
	})
}

// A node whose loop has never run and whose store has never held a
// snapshot is a complete exact zero through the whole assembled read — the
// empty local store, the unconfigured remote, the absent attempt record —
// never an unknown, and never a warning.
func TestReaderOnAFreshNodeIsACompleteExactZero(t *testing.T) {
	withEvidencePaths(t)
	dir := t.TempDir()
	recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
	noSnapshotConf(t)

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})

	if res.Policy.State != StateConfigured {
		t.Errorf("Policy.State = %q, want %q: the config sets the directory", res.Policy.State, StateConfigured)
	}
	if res.Policy.Retain != version.SnapshotRetainDefault || res.Policy.RetainFrom != FromDefault {
		t.Errorf("Policy.Retain = %d (%s), want the built-in default", res.Policy.Retain, res.Policy.RetainFrom)
	}
	if res.Policy.Copy != CopyLocalOnly || res.Policy.CopyFrom != FromDefault {
		t.Errorf("Policy.Copy = %q (%s), want local-only by default", res.Policy.Copy, res.Policy.CopyFrom)
	}
	if !res.Local.Complete || len(res.Local.Artifacts) != 0 {
		t.Errorf("Local = %+v, want a complete exact zero", res.Local)
	}
	if !res.Remote.Complete || len(res.Remote.Copies) != 0 {
		t.Errorf("Remote = %+v, want a complete exact zero: no target is configured", res.Remote)
	}
	if !res.Maintenance.AttemptsKnown || !res.Maintenance.Save.AttemptedAt.IsZero() || !res.Maintenance.Prune.AttemptedAt.IsZero() {
		t.Errorf("Maintenance = %+v, want known with no attempts recorded yet", res.Maintenance)
	}
	if !res.Backups.Complete || len(res.Backups.Entries) != 0 {
		t.Errorf("Backups = %+v, want a complete exact zero", res.Backups)
	}
	if !res.Complete {
		t.Error("Complete = false, want true: a fresh node's evidence is exact, not unknown")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a fresh node warned: %q", res.Warnings)
	}
}

// A cluster config the controller was started with but can no longer be
// read leaves the snapshot directory unknown, and the reader invents no
// directory to walk: the local catalog is unknown and not a complete
// total — never an exact zero — while the stages that could run (a
// readable conf's target, the attempt record) still report.
func TestReaderWithAnUnknownSnapshotDirectoryReportsAnUnknownLocalCatalog(t *testing.T) {
	withEvidencePaths(t)
	recordedConfig(t, "spec: [not a mapping\n")
	noSnapshotConf(t)

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})

	if res.Policy.State != StateUnknown || res.Policy.Dir != "" || res.Policy.DirFrom != FromUnknown {
		t.Errorf("Policy = %+v, want an unknown state with no invented directory", res.Policy)
	}
	if res.Local.Complete || len(res.Local.Artifacts) != 0 {
		t.Errorf("Local = %+v, want no artifacts and no complete total: the directory is unknown", res.Local)
	}
	if w := strings.Join(res.Local.Warnings, "\n"); !strings.Contains(w, "snapshot directory is unknown") {
		t.Errorf("the local warnings do not name the unknown snapshot directory: %q", w)
	}
	if res.Remote.Complete || len(res.Remote.Copies) != 0 {
		t.Errorf("Remote = %+v, want an unknown catalog with no copies: the cluster config that would carry the target cannot be read, so none is knowable", res.Remote)
	}
	if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "target") || !strings.Contains(w, "unknown and not a complete total") {
		t.Errorf("the remote warnings do not name the target that could not be determined and the unknown catalog: %q", w)
	}
	if res.Backups.Complete || len(res.Backups.Entries) != 0 {
		t.Errorf("Backups = %+v, want no entries and no complete total: the local half is unknown", res.Backups)
	}
	if res.Complete {
		t.Error("Complete = true, want false: the local catalog is unknown")
	}
}

// An unscoped request whose local walk never runs still names the node:
// the reader resolves this host's short name itself — the same resolution
// the walk would have made — and the name reaches the local catalog the
// document reads (its local.node), not just the census's node: a result
// that named the node only in Backups.Node would leave the document's local
// half saying whose store it describes nowhere.
func TestReaderResolvesTheNodeForAnUnscopedRequestWhoseWalkNeverRan(t *testing.T) {
	withEvidencePaths(t)
	// An unreadable cluster config and no conf: the snapshot directory is
	// unknown, so the local walk never runs.
	recordedConfig(t, "spec: [not a mapping\n")
	noSnapshotConf(t)
	old := localHostname
	t.Cleanup(func() { localHostname = old })
	localHostname = func() (string, error) { return "cp-9", nil }

	res := Evidence(context.Background(), EvidenceOptions{})

	if res.Local.Node != "cp-9" {
		t.Errorf("Local.Node = %q, want %q: the reader resolved the hostname after the walk it never made, and the local catalog the document reads must carry it", res.Local.Node, "cp-9")
	}
	if res.Backups.Node != "cp-9" || res.Node != "cp-9" {
		t.Errorf("Backups.Node = %q and Node = %q, want both %q: the census and the result agree with the local catalog", res.Backups.Node, res.Node, "cp-9")
	}
	if w := strings.Join(res.Warnings, "\n"); strings.Contains(w, "name could not be determined") {
		t.Errorf("the reader warned about the node name it resolved: %q", w)
	}
}

// A snapshot.conf that was readable for months with an S3 target and then
// broke leaves the target undeterminable — while the copies it already
// uploaded still sit on the store, the record's last successful upload
// says so. The reader must not report that store as a complete exact zero:
// a target that could not be determined is an unknown remote catalog with a
// safe warning, the same refusal the local half makes about a directory it
// cannot determine, while the stages that could run (the local walk in the
// readable cluster config's directory, the attempt record) still report.
func TestReaderWithAnUnreadableConfReportsAnUnknownRemoteCatalog(t *testing.T) {
	withEvidencePaths(t)
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20261112-110000")
	writeSnapshot(t, dir, s1, 10, copyTime(0))
	recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n    intervalHours: 24\n    dir: "+dir+"\n")
	unreadableSnapshotConf(t)
	uploaded := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	seedAttempts(t, func(a *Attempts) {
		a.RecordSave(uploaded, nil)
		a.RecordUpload(uploaded.Add(time.Minute), nil)
	})

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})
	dump := dumpEvidence(t, res)

	if res.Policy.State != StateUnreadable || res.Policy.CopyFrom != FromUnknown {
		t.Errorf("Policy = %+v, want the unreadable state with an unknown copy requirement", res.Policy)
	}
	if !res.Local.Complete || len(res.Local.Artifacts) != 1 || res.Local.Artifacts[0].ID != s1 {
		t.Errorf("Local = %+v, want the readable cluster config's directory walked whole", res.Local)
	}
	if !res.Maintenance.AttemptsKnown || res.Maintenance.LastUploadSuccess.IsZero() {
		t.Errorf("Maintenance = %+v, want the record's last successful upload: the store plausibly holds the copies it uploaded", res.Maintenance)
	}
	if res.Remote.Complete {
		t.Error("Remote.Complete = true, want false: a target that could not be determined is an unknown catalog, never a complete exact zero")
	}
	if len(res.Remote.Copies) != 0 {
		t.Errorf("Remote.Copies = %+v, want none: no target was resolved to list", res.Remote.Copies)
	}
	if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "target") || !strings.Contains(w, "unknown and not a complete total") {
		t.Errorf("the remote warnings do not name the target that could not be determined and the unknown catalog: %q", w)
	}
	if res.Backups.Complete || res.Complete {
		t.Error("the result claims completeness from a remote catalog the read could not determine")
	}
	if strings.Contains(dump, "/etc/stratum/snapshot.conf") || strings.Contains(dump, "is a directory") {
		t.Errorf("the unreadable conf's raw error text escaped into the evidence:\n%s", dump)
	}
}

// A cluster config the controller was started with — carrying the S3 target
// its loop uploaded to — that has since become unreadable leaves the target
// as undeterminable as an unreadable snapshot.conf does: the copies that
// loop already uploaded may still sit on the store, and the record's last
// successful upload says so. The reader must not report that store as a
// complete exact zero: the remote catalog is unknown with a safe warning,
// the same refusal the unreadable conf's arm makes, while the stages that
// could run (the attempt record) still report.
func TestReaderWithAnUnreadableClusterConfigAndNoConfReportsAnUnknownRemoteCatalog(t *testing.T) {
	withEvidencePaths(t)
	recordedConfig(t, "spec: [not a mapping\n")
	noSnapshotConf(t)
	uploaded := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	seedAttempts(t, func(a *Attempts) {
		a.RecordSave(uploaded, nil)
		a.RecordUpload(uploaded.Add(time.Minute), nil)
	})

	res := Evidence(context.Background(), EvidenceOptions{Node: "cp-1"})
	dump := dumpEvidence(t, res)

	if res.Policy.State != StateUnknown || res.Policy.CopyFrom != FromUnknown {
		t.Errorf("Policy = %+v, want the unknown state with an unknown copy requirement", res.Policy)
	}
	if !res.Maintenance.AttemptsKnown || res.Maintenance.LastUploadSuccess.IsZero() {
		t.Errorf("Maintenance = %+v, want the record's last successful upload: the store plausibly holds the copies it uploaded", res.Maintenance)
	}
	if res.Remote.Complete {
		t.Error("Remote.Complete = true, want false: a target that could not be determined is an unknown catalog, never a complete exact zero")
	}
	if len(res.Remote.Copies) != 0 {
		t.Errorf("Remote.Copies = %+v, want none: no target was resolved to list", res.Remote.Copies)
	}
	if w := strings.Join(res.Remote.Warnings, "\n"); !strings.Contains(w, "target") || !strings.Contains(w, "unknown and not a complete total") {
		t.Errorf("the remote warnings do not name the target that could not be determined and the unknown catalog: %q", w)
	}
	if res.Backups.Complete || res.Complete {
		t.Error("the result claims completeness from a remote catalog the read could not determine")
	}
	if strings.Contains(dump, "not a mapping") {
		t.Errorf("the unreadable cluster config's raw error text escaped into the evidence:\n%s", dump)
	}
}
