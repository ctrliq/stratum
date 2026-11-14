package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// From here: the scheduled save records its attempts. The loop's per-tick
// seam — snapshotSave, the same seam the loop tests drive — must leave the
// tick's evidence in the durable snapshot evidence record under the config
// area: the save and upload attempts with their outcomes and last-success
// stamps, the prune attempt independent of them, and the warnings the tick
// ran with. A fresh read of the file after the tick is the
// restart-persistence proof: nothing carries over but through the file.

// redirectEvidenceArea points the snapshot evidence record and the etcd data
// lock at one temp tree: the scheduled save records its attempts through the
// lock into the record, so a test that drives the seam must redirect both or
// they land under the host's /var/lib/stratum.
func redirectEvidenceArea(t *testing.T) {
	t.Helper()
	oldConf, oldData, oldEtcd := version.K8sConfDir, version.DataDir, version.EtcdDataDir
	root := t.TempDir()
	version.K8sConfDir = filepath.Join(root, "config")
	version.DataDir = filepath.Join(root, "data")
	version.EtcdDataDir = filepath.Join(root, "etcd")
	t.Cleanup(func() {
		version.K8sConfDir, version.DataDir, version.EtcdDataDir = oldConf, oldData, oldEtcd
	})
}

// stubSnapshotConf stands where the operator's snapshot.conf stands: the
// no-conf node's read, base untouched with the environment's S3 credentials
// overlaid exactly as the real read overlays them, so the test's cluster
// config supplies every setting, nothing leaks in from /etc, and the upload
// reaches the test's own bucket the way a credentialed node's would.
func stubSnapshotConf(t *testing.T) {
	t.Helper()
	old := snapshotLoadConfig
	snapshotLoadConfig = func(base etcdsnap.Config) (etcdsnap.Config, error) {
		if v := os.Getenv("STRATUM_S3_ACCESS_KEY"); v != "" {
			base.S3.AccessKey = v
		}
		if v := os.Getenv("STRATUM_S3_SECRET_KEY"); v != "" {
			base.S3.SecretKey = v
		}
		return base, nil
	}
	t.Cleanup(func() { snapshotLoadConfig = old })
}

// breakSnapshotConf stands for a snapshot.conf that exists but cannot be
// read: the tick still takes the snapshot, keeps every one and uploads
// nothing, and warns about the conf on its way to the evidence record.
func breakSnapshotConf(t *testing.T) {
	t.Helper()
	old := snapshotLoadConfig
	snapshotLoadConfig = func(etcdsnap.Config) (etcdsnap.Config, error) {
		return etcdsnap.Config{}, errors.New("snapshot.conf: permission denied")
	}
	t.Cleanup(func() { snapshotLoadConfig = old })
}

// recordedAttempt and recordedEvidence are the durable snapshot evidence
// record as these tests read it back from disk, json tags and all. The
// record's own reader (internal/backupevidence) resolves its policy through
// this package, so it cannot be imported from here; the etcdsnap-side
// recording tests pin this file's shape against that reader.
type recordedAttempt struct {
	AttemptedAt time.Time `json:"attemptedAt"`
	Result      string    `json:"result"`
}

type recordedEvidence struct {
	SchemaVersion     int             `json:"schemaVersion"`
	Save              recordedAttempt `json:"save"`
	Upload            recordedAttempt `json:"upload"`
	Prune             recordedAttempt `json:"prune"`
	LastSaveSuccess   time.Time       `json:"lastSaveSuccess"`
	LastUploadSuccess time.Time       `json:"lastUploadSuccess"`
	Warnings          []string        `json:"warnings"`
}

// readEvidence reads the durable record fresh from disk.
func readEvidence(t *testing.T) recordedEvidence {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml"))
	if err != nil {
		t.Fatalf("read the snapshot evidence record: %v", err)
	}
	var rec recordedEvidence
	if err := yaml.Unmarshal(b, &rec); err != nil {
		t.Fatalf("parse the snapshot evidence record (%s): %v", b, err)
	}
	return rec
}

// stubFailingUpload answers every request with a 500, so the local snapshot
// is taken but its S3 copy fails.
func stubFailingUpload(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// stubRefusingRemotePrune accepts uploads, lists the host's own two older
// scheduled objects and refuses every DELETE with a 403: the save and the
// upload succeed while remote retention is refused.
func stubRefusingRemotePrune(t *testing.T, host string) string {
	t.Helper()
	old := fmt.Sprintf("etcd-snapshot-%s-20260101-000000.db", host)
	recent := fmt.Sprintf("etcd-snapshot-%s-20260102-000000.db", host)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.Header().Set("ETag", "\"snapshot\"")
			return
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>`+
				`<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>`+
				`<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-01-02T00:00:00.000Z</LastModified></Contents>`+
				`</ListBucketResult>`, old, recent)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusForbidden)
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestEvidenceRecordingPersistsAFailedSave pins the loop's core failure
// evidence: a tick whose etcdctl run fails leaves a failed save attempt in
// the durable record — with the failure named and no last-success stamp —
// while a later successful tick replaces the attempt with the success and
// sets the stamp to that tick, never leaving the failure's stamp behind.
func TestEvidenceRecordingPersistsAFailedSave(t *testing.T) {
	redirectEvidenceArea(t)
	stubSnapshotConf(t)
	snapDir := t.TempDir()
	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    dir: "+snapDir+"\n    retain: 3\n")

	fakeEtcdctl(t, "exit 1")
	if err := snapshotSave(context.Background(), cfg); err == nil {
		t.Fatal("a failing etcdctl reported a successful save")
	}
	rec := readEvidence(t)
	if rec.Save.AttemptedAt.IsZero() {
		t.Error("the failed save attempt was not recorded: no attempted-at")
	}
	if rec.Save.Result == "" {
		t.Error("the failed save attempt was recorded without the failure that named it")
	}
	if !rec.LastSaveSuccess.IsZero() {
		t.Errorf("a failed save moved the last-success stamp to %v", rec.LastSaveSuccess)
	}
	if !rec.Upload.AttemptedAt.IsZero() || !rec.Prune.AttemptedAt.IsZero() {
		t.Errorf("a tick whose save failed recorded attempts it never ran: upload %+v prune %+v", rec.Upload, rec.Prune)
	}
	if len(rec.Warnings) != 0 {
		t.Errorf("a plain failed tick recorded warnings: %q", rec.Warnings)
	}
	if rec.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", rec.SchemaVersion)
	}

	// The next tick succeeds: the latest attempt is the success, the stamp is
	// this tick's, and the local prune that ran beside it is recorded clean.
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	if err := snapshotSave(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	rec = readEvidence(t)
	if rec.Save.Result != "" || rec.Save.AttemptedAt.IsZero() {
		t.Errorf("the successful retry did not replace the failed attempt: %+v", rec.Save)
	}
	if rec.LastSaveSuccess.IsZero() || !rec.LastSaveSuccess.Equal(rec.Save.AttemptedAt) {
		t.Errorf("last save success = %v, want the successful tick's %v", rec.LastSaveSuccess, rec.Save.AttemptedAt)
	}
	if rec.Prune.AttemptedAt.IsZero() || rec.Prune.Result != "" {
		t.Errorf("the clean local prune was not recorded clean: %+v", rec.Prune)
	}
	if !rec.Upload.AttemptedAt.IsZero() {
		t.Errorf("an upload attempt was recorded although no S3 target is configured: %+v", rec.Upload)
	}
	if dbs, _ := filepath.Glob(filepath.Join(snapDir, "*.db")); len(dbs) != 1 {
		t.Errorf("local snapshots = %v, want the one the successful tick wrote", dbs)
	}
}

// TestEvidenceRecordingKeepsLocalEvidenceWhenTheUploadFails pins the local
// evidence contract: a tick whose S3 upload fails still records the local
// save as a success with its last-success stamp — the artifact exists on the
// node — while the upload attempt records the failure and moves no
// last-success stamp of its own.
func TestEvidenceRecordingKeepsLocalEvidenceWhenTheUploadFails(t *testing.T) {
	redirectEvidenceArea(t)
	stubSnapshotConf(t)
	t.Setenv("STRATUM_S3_ACCESS_KEY", "k")
	t.Setenv("STRATUM_S3_SECRET_KEY", "s")
	snapDir := t.TempDir()
	endpoint := stubFailingUpload(t)
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    dir: "+snapDir+"\n    retain: 2\n    s3:\n      bucket: b\n      endpoint: "+endpoint+"\n      pathStyle: true\n")

	if err := snapshotSave(context.Background(), cfg); err == nil {
		t.Fatal("the failed S3 upload was not reported by the save")
	}
	dbs, _ := filepath.Glob(filepath.Join(snapDir, "*.db"))
	if len(dbs) != 1 {
		t.Fatalf("local snapshots = %v, want the artifact the tick saved before the upload failed", dbs)
	}

	rec := readEvidence(t)
	if rec.Save.Result != "" || rec.Save.AttemptedAt.IsZero() {
		t.Errorf("the local save was not recorded as a success: %+v", rec.Save)
	}
	if rec.LastSaveSuccess.IsZero() || !rec.LastSaveSuccess.Equal(rec.Save.AttemptedAt) {
		t.Errorf("last save success = %v, want the tick's save attempt at %v: the failed upload must not erase it", rec.LastSaveSuccess, rec.Save.AttemptedAt)
	}
	if rec.Upload.AttemptedAt.IsZero() || rec.Upload.Result == "" {
		t.Errorf("the failed upload attempt was not recorded: %+v", rec.Upload)
	}
	if !strings.Contains(rec.Upload.Result, "upload") {
		t.Errorf("the upload attempt's result does not name the upload: %q", rec.Upload.Result)
	}
	if !rec.LastUploadSuccess.IsZero() {
		t.Errorf("a failed upload moved the upload last-success stamp to %v", rec.LastUploadSuccess)
	}
	if rec.Prune.AttemptedAt.IsZero() || rec.Prune.Result != "" {
		t.Errorf("the local prune beside the failed upload was not recorded clean: %+v", rec.Prune)
	}
	if len(rec.Warnings) != 0 {
		t.Errorf("a failed upload recorded warnings: %q", rec.Warnings)
	}
}

// TestEvidenceRecordingRecordsAPruneRefusalAsAWarning pins the independence
// of retention evidence: a tick whose remote prune is refused (S3 answers
// 403 to the delete) does not fail the save, records the refusal as the
// prune attempt and as a warning of its own, and leaves the save and upload
// success evidence — the attempts and both last-success stamps — untouched
// beside it.
func TestEvidenceRecordingRecordsAPruneRefusalAsAWarning(t *testing.T) {
	redirectEvidenceArea(t)
	stubSnapshotConf(t)
	t.Setenv("STRATUM_S3_ACCESS_KEY", "k")
	t.Setenv("STRATUM_S3_SECRET_KEY", "s")
	snapDir := t.TempDir()
	hn, err := host.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := stubRefusingRemotePrune(t, hn)
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    dir: "+snapDir+"\n    retain: 1\n    s3:\n      bucket: b\n      endpoint: "+endpoint+"\n      pathStyle: true\n")

	if err := snapshotSave(context.Background(), cfg); err != nil {
		t.Fatalf("a refused prune must not fail the save: %v", err)
	}
	if dbs, _ := filepath.Glob(filepath.Join(snapDir, "*.db")); len(dbs) != 1 {
		t.Fatalf("local snapshots = %v, want the one the tick saved", dbs)
	}

	rec := readEvidence(t)
	if rec.Save.Result != "" || rec.Save.AttemptedAt.IsZero() || !rec.LastSaveSuccess.Equal(rec.Save.AttemptedAt) {
		t.Errorf("the save's success evidence was disturbed by the prune refusal: %+v (last %v)", rec.Save, rec.LastSaveSuccess)
	}
	if rec.Upload.Result != "" || rec.Upload.AttemptedAt.IsZero() || !rec.LastUploadSuccess.Equal(rec.Upload.AttemptedAt) {
		t.Errorf("the upload's success evidence was disturbed by the prune refusal: %+v (last %v)", rec.Upload, rec.LastUploadSuccess)
	}
	if rec.Prune.AttemptedAt.IsZero() || rec.Prune.Result == "" {
		t.Errorf("the refused prune was not recorded as the prune attempt: %+v", rec.Prune)
	}
	if !strings.Contains(rec.Prune.Result, "S3 snapshot prune") {
		t.Errorf("the prune attempt's result does not name the refused S3 prune: %q", rec.Prune.Result)
	}
	if len(rec.Warnings) != 1 || !strings.Contains(rec.Warnings[0], "WARN: S3 snapshot prune") {
		t.Errorf("the prune refusal was not recorded as a warning of its own: %q", rec.Warnings)
	}
}

// TestEvidenceRecordingRecordsTheSchedulingWarningOfAnUnreadableConf pins
// the warnings half of the record: the WARN an unreadable snapshot.conf
// earns on its way to the daemon's log is recorded beside the tick's
// attempts, while the tick itself still saves, keeps every snapshot and
// uploads nothing — so no prune or upload attempt exists to record.
func TestEvidenceRecordingRecordsTheSchedulingWarningOfAnUnreadableConf(t *testing.T) {
	redirectEvidenceArea(t)
	breakSnapshotConf(t)
	snapDir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    dir: "+snapDir+"\n")

	if err := snapshotSave(context.Background(), cfg); err != nil {
		t.Fatalf("the snapshot is still taken beside the unreadable conf: %v", err)
	}
	rec := readEvidence(t)
	if len(rec.Warnings) != 1 {
		t.Fatalf("warnings = %q, want the one the tick printed", rec.Warnings)
	}
	if !strings.Contains(rec.Warnings[0], "cannot read snapshot config") || !strings.Contains(rec.Warnings[0], "pruning skipped") {
		t.Errorf("the recorded warning is not the one the tick printed: %q", rec.Warnings[0])
	}
	if rec.Save.Result != "" || rec.Save.AttemptedAt.IsZero() {
		t.Errorf("the save the tick still took was not recorded: %+v", rec.Save)
	}
	if !rec.Upload.AttemptedAt.IsZero() || !rec.Prune.AttemptedAt.IsZero() {
		t.Errorf("an unreadable conf recorded an upload or prune attempt it never runs: upload %+v prune %+v", rec.Upload, rec.Prune)
	}
}
