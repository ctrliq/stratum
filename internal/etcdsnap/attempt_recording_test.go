// External test package: the one place that can pin the record's writer against its reader.
package etcdsnap_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/backupevidence"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// redirectEvidence points the evidence record and the etcd data lock at one temp tree.
func redirectEvidence(t *testing.T) {
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

// fakeEtcdctl writes an etcdctl script whose $3 is the snapshot path.
func fakeEtcdctl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "etcdctl"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := version.BinDir
	version.BinDir = dir
	t.Cleanup(func() { version.BinDir = old })
}

// stubFailingUpload answers the upload with a 500, so the local snapshot survives.
func stubFailingUpload(t *testing.T) *objstore.Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

// stubRefusingPrune accepts uploads and refuses every DELETE with a 403.
func stubRefusingPrune(t *testing.T) *objstore.Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.Header().Set("ETag", "\"snapshot\"")
			return
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>`+
				`<Contents><Key>etcd-snapshot-cp1-20260101-000000.db</Key><Size>1</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>`+
				`<Contents><Key>etcd-snapshot-cp1-20260102-000000.db</Key><Size>1</Size><LastModified>2026-01-02T00:00:00.000Z</LastModified></Contents>`+
				`</ListBucketResult>`)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusForbidden)
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

func TestAttemptFailedSavePersists(t *testing.T) {
	redirectEvidence(t)
	fakeEtcdctl(t, "exit 1")
	_, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: t.TempDir(), Host: "cp1", Name: "snap.db", Evidence: true,
	})
	if err == nil {
		t.Fatal("a failing etcdctl reported a successful save")
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatalf("the failed save left no record the backup-evidence reader can load: %v", err)
	}
	if rec.Save.AttemptedAt.IsZero() || rec.Save.Result == "" {
		t.Errorf("the failed save attempt was not recorded as the reader reads it: %+v", rec.Save)
	}
	if !rec.LastSaveSuccess.IsZero() {
		t.Errorf("a failed save moved the last-success stamp to %v", rec.LastSaveSuccess)
	}
	if !rec.Upload.IsZero() || !rec.Prune.IsZero() {
		t.Errorf("a tick whose save failed recorded attempts it never ran: upload %+v prune %+v", rec.Upload, rec.Prune)
	}
	if len(rec.Warnings) != 0 {
		t.Errorf("a plain failed tick recorded warnings: %q", rec.Warnings)
	}
	if rec.SchemaVersion != backupevidence.AttemptsSchemaVersion {
		t.Errorf("SchemaVersion = %d, want the reader's %d: the loop must write the record the evidence interface reads", rec.SchemaVersion, backupevidence.AttemptsSchemaVersion)
	}
}

func TestAttemptFailedUploadKeepsTheLocalArtifactRecorded(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	_, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap.db", Evidence: true, S3: stubFailingUpload(t),
	})
	if err == nil {
		t.Fatal("the failed S3 upload was not reported by the save")
	}
	if _, err := os.Stat(filepath.Join(dir, "snap.db")); err != nil {
		t.Fatalf("the local artifact the upload failed on is missing: %v", err)
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatalf("the record the backup-evidence reader loads: %v", err)
	}
	if rec.Save.Result != "" || rec.Save.AttemptedAt.IsZero() {
		t.Errorf("the local save was not recorded as a success: %+v", rec.Save)
	}
	if !rec.LastSaveSuccess.Equal(rec.Save.AttemptedAt) {
		t.Errorf("last save success = %v, want the local save's %v: the failed upload must not erase it", rec.LastSaveSuccess, rec.Save.AttemptedAt)
	}
	if rec.Upload.AttemptedAt.IsZero() || rec.Upload.Result == "" || !strings.Contains(rec.Upload.Result, "upload") {
		t.Errorf("the failed upload attempt was not recorded naming the upload: %+v", rec.Upload)
	}
	if !rec.LastUploadSuccess.IsZero() {
		t.Errorf("a failed upload moved the upload last-success stamp to %v", rec.LastUploadSuccess)
	}
	if !rec.Prune.IsZero() {
		t.Errorf("a save that keeps every snapshot (no retention asked) recorded a prune attempt: %+v", rec.Prune)
	}
}

func TestAttemptPruneRefusalRecordsAWarning(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	_, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap.db", Retain: 1, Evidence: true, S3: stubRefusingPrune(t),
	})
	if err != nil {
		t.Fatalf("a refused prune must not fail the save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snap.db")); err != nil {
		t.Fatalf("the local artifact is missing: %v", err)
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatalf("the record the backup-evidence reader loads: %v", err)
	}
	if rec.Save.Result != "" || !rec.LastSaveSuccess.Equal(rec.Save.AttemptedAt) {
		t.Errorf("the save's success evidence was disturbed by the prune refusal: %+v (last %v)", rec.Save, rec.LastSaveSuccess)
	}
	if rec.Upload.Result != "" || !rec.LastUploadSuccess.Equal(rec.Upload.AttemptedAt) {
		t.Errorf("the upload's success evidence was disturbed by the prune refusal: %+v (last %v)", rec.Upload, rec.LastUploadSuccess)
	}
	if rec.Prune.AttemptedAt.IsZero() || rec.Prune.Result == "" || !strings.Contains(rec.Prune.Result, "S3 snapshot prune") {
		t.Errorf("the refused prune was not recorded as the prune attempt naming the S3 prune: %+v", rec.Prune)
	}
	if len(rec.Warnings) != 1 || !strings.Contains(rec.Warnings[0], "WARN: S3 snapshot prune") {
		t.Errorf("the prune refusal was not recorded as a warning of its own: %q", rec.Warnings)
	}
}

func TestAttemptRecordingNeverFailsTheSaveItRecords(t *testing.T) {
	redirectEvidence(t)
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	var buf strings.Builder
	path, err := etcdsnap.SaveContext(context.Background(), &buf, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap.db", Retain: 2, Evidence: true,
	})
	if err != nil {
		t.Fatalf("the save must not be failed by its own evidence: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the snapshot the save took is missing: %v", err)
	}
	if !strings.Contains(buf.String(), "WARN: recording snapshot evidence") {
		t.Errorf("the refused recording was not warned about beside the save's result: %q", buf.String())
	}
	if _, err := backupevidence.LoadAttempts(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("evidence record = %v, want none while the data lock is held (the next tick retries)", err)
	}
}

func TestAttemptFailedRetriesPersistAfterRestart(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()

	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap1.db", Evidence: true, S3: stubRefusingPrune(t),
	}); err != nil {
		t.Fatalf("the successful tick: %v", err)
	}
	first, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatalf("the record after the successful tick: %v", err)
	}
	if first.Save.Result != "" || first.Upload.Result != "" || first.LastSaveSuccess.IsZero() || first.LastUploadSuccess.IsZero() {
		t.Fatalf("the successful tick did not record its successes: %+v", first)
	}

	fakeEtcdctl(t, "exit 1")
	if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap2.db", Evidence: true, S3: stubFailingUpload(t),
	}); err == nil {
		t.Fatal("a failing etcdctl reported a successful save")
	}

	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatalf("the record after the failed retry: %v", err)
	}
	if rec.Save.Result == "" || !rec.Save.AttemptedAt.After(first.Save.AttemptedAt) {
		t.Errorf("the failed save attempt did not persist: %+v", rec.Save)
	}
	if !rec.LastSaveSuccess.Equal(first.LastSaveSuccess) {
		t.Errorf("the failed retry moved the last save success: %v, want the previous tick's %v", rec.LastSaveSuccess, first.LastSaveSuccess)
	}
	if !rec.Upload.AttemptedAt.Equal(first.Upload.AttemptedAt) || rec.Upload.Result != first.Upload.Result {
		t.Errorf("the failed retry disturbed the recorded upload attempt: %+v, want the previous tick's %+v", rec.Upload, first.Upload)
	}
	if !rec.LastUploadSuccess.Equal(first.LastUploadSuccess) {
		t.Errorf("the failed retry moved the last upload success: %v, want the previous tick's %v", rec.LastUploadSuccess, first.LastUploadSuccess)
	}
}

func TestAttemptRecordIsAtomicAndPrivate(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap.db", Evidence: true,
	}); err != nil {
		t.Fatalf("the save: %v", err)
	}

	p := filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("the record the save should have written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the record was written as %04o, want 0600", perm)
	}
	entries, err := os.ReadDir(version.K8sConfDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshot-evidence.yaml" {
		t.Errorf("the config dir holds %v, want the record alone", entries)
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatalf("the record the backup-evidence reader loads: %v", err)
	}
	if rec.SchemaVersion != backupevidence.AttemptsSchemaVersion {
		t.Errorf("SchemaVersion = %d, want the reader's %d: the loop must write the record the evidence interface reads",
			rec.SchemaVersion, backupevidence.AttemptsSchemaVersion)
	}
	if rec.Save.AttemptedAt.IsZero() || rec.Save.Result != "" {
		t.Errorf("the save the record carries: %+v, want the successful attempt the loop made", rec.Save)
	}
}

func TestAttemptRecordingRefusesAFifoRecord(t *testing.T) {
	redirectEvidence(t)
	if err := os.MkdirAll(version.K8sConfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	var buf strings.Builder
	path, err := etcdsnap.SaveContext(context.Background(), &buf, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Name: "snap.db", Evidence: true,
	})
	if err != nil {
		t.Fatalf("the save must not be failed by its own evidence: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the snapshot the save took is missing: %v", err)
	}
	if !strings.Contains(buf.String(), "WARN: recording snapshot evidence") {
		t.Errorf("the refused recording was not warned about beside the save's result: %q", buf.String())
	}
	if fi, err := os.Lstat(filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml")); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the record path = %v, %v; want the FIFO untouched by the refused recording", fi, err)
	}
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatalf("the etcd data lock was not released after the refused recording: %v", err)
	}
	release()
}

func TestCancelledUploadDoesNotRecordSkippedPrune(t *testing.T) {
	redirectEvidence(t)
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	_, err := etcdsnap.SaveContext(ctx, io.Discard, etcdsnap.SaveOptions{Dir: t.TempDir(), Host: "cp1", Name: "snap.db", Retain: 1, Evidence: true, S3: &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}})
	if err == nil {
		t.Fatal("upload unexpectedly succeeded")
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Prune.IsZero() && rec.Prune.Result == "" {
		t.Fatalf("cancelled tick recorded successful prune that never ran: %+v", rec.Prune)
	}
}
