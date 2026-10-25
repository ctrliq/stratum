package etcdsnap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

func TestScheduledName(t *testing.T) {
	n := ScheduledName("stratum-cp1", "20260704-120000")
	if n != "etcd-snapshot-stratum-cp1-20260704-120000.db" {
		t.Errorf("ScheduledName = %q", n)
	}
	if !strings.HasPrefix(n, scheduledHostPrefix("stratum-cp1")) {
		t.Errorf("%q lacks host prefix %q", n, scheduledHostPrefix("stratum-cp1"))
	}
}

func write(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// TestPruneKeepsNewest: Prune keeps the newest N of THIS host's snapshots and
// leaves other hosts' snapshots and pre-upgrade snapshots untouched.
func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	host := "cp1"
	// three cp1 snapshots (timestamps sort chronologically), plus an unrelated
	// host and a pre-upgrade snapshot that must survive.
	write(t, dir, ScheduledName(host, "20260704-010000"))
	write(t, dir, ScheduledName(host, "20260704-020000"))
	write(t, dir, ScheduledName(host, "20260704-030000"))
	write(t, dir, ScheduledName("cp2", "20260704-030000"))
	write(t, dir, "pre-upgrade-20260704-030000.db")

	if err := Prune(dir, hostSnapshot(host), 2); err != nil {
		t.Fatal(err)
	}
	if exists(dir, ScheduledName(host, "20260704-010000")) {
		t.Error("oldest cp1 snapshot should have been pruned")
	}
	for _, keep := range []string{
		ScheduledName(host, "20260704-020000"),
		ScheduledName(host, "20260704-030000"),
		ScheduledName("cp2", "20260704-030000"),
		"pre-upgrade-20260704-030000.db",
	} {
		if !exists(dir, keep) {
			t.Errorf("%s should have survived prune", keep)
		}
	}
}

// TestPruneLeavesAHostWhoseNameExtendsThisOne: host "cp" must not count
// "cp-2"'s snapshots as its own. They sort first ('-' < '0'), so a prefix
// match pruned all of cp-2's off-box copies in a shared bucket.
func TestPruneLeavesAHostWhoseNameExtendsThisOne(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		ScheduledName("cp", "20260704-010000"),
		ScheduledName("cp", "20260704-020000"),
		ScheduledName("cp-2", "20260704-030000"),
		ScheduledName("cp-2", "20260704-040000"),
		ScheduledName("cp", "manual"),
	} {
		write(t, dir, n)
	}
	if err := Prune(dir, hostSnapshot("cp"), 1); err != nil {
		t.Fatal(err)
	}
	if exists(dir, ScheduledName("cp", "20260704-010000")) {
		t.Error("the older cp snapshot should have been pruned")
	}
	for _, keep := range []string{
		ScheduledName("cp", "20260704-020000"),
		ScheduledName("cp-2", "20260704-030000"),
		ScheduledName("cp-2", "20260704-040000"),
		ScheduledName("cp", "manual"),
	} {
		if !exists(dir, keep) {
			t.Errorf("%s should have survived cp's prune", keep)
		}
	}
}

func TestPruneNoopWhenUnderKeep(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ScheduledName("cp1", "20260704-010000"))
	if err := Prune(dir, hostSnapshot("cp1"), 5); err != nil {
		t.Fatal(err)
	}
	if !exists(dir, ScheduledName("cp1", "20260704-010000")) {
		t.Error("prune removed a snapshot while under the keep threshold")
	}
	// keep<=0 must never prune
	if err := Prune(dir, hostSnapshot("cp1"), 0); err != nil {
		t.Fatal(err)
	}
	if !exists(dir, ScheduledName("cp1", "20260704-010000")) {
		t.Error("keep=0 must be a no-op, not prune-all")
	}
}

// captureStdout runs fn with the process's stdout swapped for a pipe and
// returns what fn printed. The prune prints with fmt.Printf to the process's
// own stdout, so a pipe swap is the only way to see its output; the reader
// goroutine keeps a full pipe from deadlocking the writer. Copied from
// pkg/cli/snapshot_dir_test.go, which needed it for the same reason.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		old := os.Stdout
		os.Stdout = w
		defer func() { os.Stdout = old; _ = w.Close() }()
		fn()
	}()
	return <-done
}

// TestPruneScheduled_ReturnsTheLocalFailure: PruneScheduled reports a failed
// prune by RETURNING an error that names the local prune, and prints
// nothing, so the CLI, the scheduled loop and the snapshot tools learn of
// the failure as data instead of matching the printed text.
func TestPruneScheduled_ReturnsTheLocalFailure(t *testing.T) {
	// A regular file where a directory is expected: ReadDir fails without the
	// prune deleting anything.
	dir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var errs []error
	out := captureStdout(t, func() {
		errs = PruneScheduled(dir, "cp1", 1, nil)
	})
	if out != "" {
		t.Errorf("PruneScheduled printed the failure instead of returning it:\n%s", out)
	}
	if len(errs) != 1 {
		t.Fatalf("PruneScheduled returned %v, want one error for the failed local prune", errs)
	}
	if !strings.Contains(errs[0].Error(), "local snapshot prune") {
		t.Errorf("the returned error does not name the local prune: %v", errs[0])
	}
}

func TestListLocal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ScheduledName("cp1", "20260704-010000"))
	write(t, dir, "pre-upgrade-20260704-020000.db")
	write(t, dir, "not-a-snapshot.txt")
	items, err := List(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("List returned %d items, want 2 (.db only)", len(items))
	}
	for _, it := range items {
		if it.Location != "local" {
			t.Errorf("item %s location=%q, want local", it.Name, it.Location)
		}
	}
}

// --- snapshot.conf credential handling ---

// clearS3Env blanks every credential var applyEnv reads so a test sees only
// what it sets itself, whatever the developer's or CI's environment holds.
func clearS3Env(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"STRATUM_S3_ACCESS_KEY", "STRATUM_S3_SECRET_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
	} {
		t.Setenv(k, "")
	}
}

func confPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "snapshot.conf")
}

func writeConf(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSaveConfigNeverPersistsEnvCredentials: an operator who passes S3
// credentials by environment did so precisely to keep them off disk. Loading
// the config (which overlays the env creds so they can be USED) and saving it
// back must not write those secret VALUES into snapshot.conf.
func TestSaveConfigNeverPersistsEnvCredentials(t *testing.T) {
	const (
		envAccess = "AKIAENVACCESSKEY0001"
		envSecret = "s3cr3t-from-the-environment-do-not-write-me"
	)
	for _, tc := range []struct {
		name                 string
		accessVar, secretVar string
	}{
		{name: "STRATUM_S3_*", accessVar: "STRATUM_S3_ACCESS_KEY", secretVar: "STRATUM_S3_SECRET_KEY"},
		{name: "AWS_*", accessVar: "AWS_ACCESS_KEY_ID", secretVar: "AWS_SECRET_ACCESS_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearS3Env(t)
			t.Setenv(tc.accessVar, envAccess)
			t.Setenv(tc.secretVar, envSecret)

			path := confPath(t)
			// The starting conf has a bucket but no credentials on disk — the
			// shape `stratum snapshot schedule --s3-bucket ...` produces.
			writeConf(t, path, "RETAIN=5\nS3_BUCKET=backups\n")

			cfg, err := loadConfigFrom(path, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			// The env creds must still be usable in memory: that is the point
			// of the overlay.
			if cfg.S3.AccessKey != envAccess || cfg.S3.SecretKey != envSecret {
				t.Fatalf("env creds not overlaid onto the loaded config: access=%q secret=%q", cfg.S3.AccessKey, cfg.S3.SecretKey)
			}

			cfg.Retain = 7 // an unrelated schedule change, as `snapshot schedule` makes
			if err := saveConfigTo(path, cfg); err != nil {
				t.Fatal(err)
			}

			got := readConf(t, path)
			if strings.Contains(got, envSecret) {
				t.Errorf("env-sourced S3 secret key was written to disk; snapshot.conf is:\n%s", got)
			}
			if strings.Contains(got, envAccess) {
				t.Errorf("env-sourced S3 access key was written to disk; snapshot.conf is:\n%s", got)
			}
			// The rest of the config must still round-trip.
			if !strings.Contains(got, "S3_BUCKET=backups") || !strings.Contains(got, "RETAIN=7") {
				t.Errorf("non-credential config lost on save; snapshot.conf is:\n%s", got)
			}
		})
	}
}

// TestSaveConfigPreservesFileCredentials: credentials the operator put in
// snapshot.conf by hand are that file's own state. Rewriting the file for an
// unrelated reason must not silently drop them.
func TestSaveConfigPreservesFileCredentials(t *testing.T) {
	clearS3Env(t)
	const (
		fileAccess = "AKIAFILEACCESSKEY001"
		fileSecret = "s3cr3t-already-on-disk"
	)
	path := confPath(t)
	writeConf(t, path, "RETAIN=5\nS3_BUCKET=backups\nS3_ACCESS_KEY="+fileAccess+"\nS3_SECRET_KEY="+fileSecret+"\n")

	cfg, err := loadConfigFrom(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Retain = 7
	if err := saveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}

	got := readConf(t, path)
	if !strings.Contains(got, fileAccess) || !strings.Contains(got, fileSecret) {
		t.Errorf("file-sourced credentials were dropped on save; snapshot.conf is:\n%s", got)
	}
}

// TestSaveConfigKeepsFileCredentialsWhenEnvOverrides: both sources present.
// The environment wins for USING the credentials, but the file keeps its own
// value — we neither overwrite the operator's file with the env secret nor
// delete what they put there.
func TestSaveConfigKeepsFileCredentialsWhenEnvOverrides(t *testing.T) {
	clearS3Env(t)
	const (
		fileSecret = "s3cr3t-already-on-disk"
		envSecret  = "s3cr3t-from-the-environment-do-not-write-me"
	)
	t.Setenv("STRATUM_S3_ACCESS_KEY", "AKIAENVACCESSKEY0001")
	t.Setenv("STRATUM_S3_SECRET_KEY", envSecret)

	path := confPath(t)
	writeConf(t, path, "RETAIN=5\nS3_BUCKET=backups\nS3_ACCESS_KEY=AKIAFILEACCESSKEY001\nS3_SECRET_KEY="+fileSecret+"\n")

	cfg, err := loadConfigFrom(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.SecretKey != envSecret {
		t.Fatalf("env should win for the in-memory (effective) credential, got %q", cfg.S3.SecretKey)
	}
	if err := saveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}

	got := readConf(t, path)
	if strings.Contains(got, envSecret) {
		t.Errorf("env-sourced secret overwrote the on-disk credential; snapshot.conf is:\n%s", got)
	}
	if !strings.Contains(got, fileSecret) {
		t.Errorf("the operator's on-disk credential was dropped; snapshot.conf is:\n%s", got)
	}
}

func TestSaveConfigPreservesCredentialsWithoutBucket(t *testing.T) {
	for _, tc := range []struct{ name, credentials string }{
		{"both keys", "S3_ACCESS_KEY=file-access\nS3_SECRET_KEY=file-secret\n"},
		{"access only", "S3_ACCESS_KEY=file-access\n"},
		{"secret only", "S3_SECRET_KEY=file-secret\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearS3Env(t)
			t.Setenv("STRATUM_S3_ACCESS_KEY", "env-access")
			t.Setenv("STRATUM_S3_SECRET_KEY", "env-secret")
			path := confPath(t)
			writeConf(t, path, "RETAIN=5\n"+tc.credentials)
			cfg, err := loadConfigFrom(path, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.HasPersistedCredentials() {
				t.Fatal("file credentials were not recorded")
			}
			cfg.Retain = 7
			if err := saveConfigTo(path, cfg); err != nil {
				t.Fatal(err)
			}
			got := readConf(t, path)
			if !strings.Contains(got, tc.credentials) || !strings.Contains(got, "RETAIN=7\n") {
				t.Fatalf("persisted credentials or retention lost: %s", got)
			}
			if strings.Contains(got, "env-access") || strings.Contains(got, "env-secret") || strings.Contains(got, "S3_BUCKET=") {
				t.Fatalf("save added environment credentials or a bucket: %s", got)
			}
		})
	}
}

// TestSaveConfigTightensModeTo0600: snapshot.conf can hold S3_SECRET_KEY, so
// the file must end 0600 even when a previous write (or the operator) left it
// wider. os.WriteFile only chmod'ed files it CREATED, so a 0644 conf stayed
// 0644 forever; the atomic write replaces the file wholesale, which closes it.
func TestSaveConfigTightensModeTo0600(t *testing.T) {
	clearS3Env(t)
	path := confPath(t)
	if err := os.WriteFile(path, []byte("RETAIN=5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfigTo(path, Config{Retain: 7}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("saved snapshot.conf mode = %v, want 0600", got)
	}
}

// TestSaveConfigRoundTrips: what `snapshot schedule` writes back must read
// back identically — a dropped field is a silently reverted schedule, and for
// RETAIN that means the next prune deletes backups.
func TestSaveConfigRoundTrips(t *testing.T) {
	clearS3Env(t)
	path := confPath(t)
	cfg := Config{
		Retain: 9,
		S3: objstore.Target{
			Bucket: "backups", Endpoint: "http://minio:9000",
			Region: "us-east-1", Prefix: "etcd/cp1", PathStyle: true,
		},
	}
	cfg.persistedAccessKey = "file-access"
	cfg.persistedSecretKey = "file-secret"
	if err := saveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfigFrom(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// loadConfigFrom overlays the file's credentials onto the effective S3,
	// so a faithful round trip reads them back there too.
	wantS3 := cfg.S3
	wantS3.AccessKey, wantS3.SecretKey = "file-access", "file-secret"
	if got.Retain != cfg.Retain || got.S3Target() == nil || *got.S3Target() != wantS3 {
		t.Errorf("round-trip mismatch:\n got retain=%d S3=%+v\nwant retain=%d S3=%+v", got.Retain, got.S3, cfg.Retain, cfg.S3)
	}
	if got.persistedAccessKey != "file-access" || got.persistedSecretKey != "file-secret" {
		t.Errorf("persisted credentials lost: access=%q secret=%q", got.persistedAccessKey, got.persistedSecretKey)
	}
}

// TestSaveConfigLeavesNoStrayTempFiles: the atomic write stages a temp file
// in snapshot.conf's own directory; on success the rename must have consumed
// it, leaving nothing for an operator (or a file reader) to trip over.
func TestSaveConfigLeavesNoStrayTempFiles(t *testing.T) {
	clearS3Env(t)
	path := confPath(t)
	if err := saveConfigTo(path, Config{Retain: 5}); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != filepath.Base(path) {
			t.Errorf("stray entry %q left in the config directory after save", e.Name())
		}
	}
}

func TestLoadConfigOrMissingFileUsesBase(t *testing.T) {
	t.Setenv("STRATUM_S3_ACCESS_KEY", "env-access")
	t.Setenv("STRATUM_S3_SECRET_KEY", "env-secret")
	base := Config{Retain: 2, S3: objstore.Target{Bucket: "b", Prefix: "p"}}
	c, err := loadConfigFrom(filepath.Join(t.TempDir(), "snapshot.conf"), base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retain != 2 || c.S3.Bucket != "b" || c.S3.Prefix != "p" {
		t.Errorf("base not applied: %+v", c)
	}
	if c.S3.AccessKey != "env-access" || c.S3.SecretKey != "env-secret" {
		t.Errorf("env credentials not overlaid: %+v", c.S3)
	}
}

func TestLoadConfigOrExistingFileWinsOutright(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.conf")
	if err := os.WriteFile(path, []byte("RETAIN=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Config{Retain: 2, S3: objstore.Target{Bucket: "b"}}
	c, err := loadConfigFrom(path, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retain != 7 || c.S3Target() != nil {
		t.Errorf("file should win without inheriting base S3: %+v", c)
	}
}

// `snapshot schedule --retain 3` on a node with no snapshot.conf must write a
// file that still carries the S3 target config.yaml supplied: the file wins
// once it exists, so dropping the target here silently stops uploads.
func TestScheduleOverMissingFileKeepsBaseS3Target(t *testing.T) {
	clearS3Env(t)
	path := filepath.Join(t.TempDir(), "snapshot.conf")
	base := Config{Retain: 10, S3: objstore.Target{Bucket: "b", Prefix: "p", Endpoint: "http://minio:9000", PathStyle: true}}
	c, err := loadConfigFrom(path, base)
	if err != nil {
		t.Fatal(err)
	}
	c.Retain = 3
	if err := saveConfigTo(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfigFrom(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := objstore.Target{Bucket: "b", Prefix: "p", Endpoint: "http://minio:9000", PathStyle: true}
	if got.Retain != 3 || got.S3Target() == nil || *got.S3Target() != want {
		t.Errorf("persisted = %+v, want retain 3 and S3 target %+v", got, want)
	}
}

func TestFromFileDistinguishesMissingFromRead(t *testing.T) {
	clearS3Env(t)
	path := filepath.Join(t.TempDir(), "snapshot.conf")
	missing, err := loadConfigFrom(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if missing.FromFile() {
		t.Error("a missing file reported FromFile")
	}
	if err := os.WriteFile(path, []byte("RETAIN=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := loadConfigFrom(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !read.FromFile() {
		t.Error("an existing file did not report FromFile")
	}
}

// fakeEtcdctl points version.BinDir at a temp dir whose etcdctl runs body —
// the same shape as internal/k8sutil's fakeKubectl. The save path shells out
// to etcdctl, so a test drives the real run without a node. The invocation is
// `etcdctl snapshot save PATH`, so the script's $3 is the snapshot path.
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

// TestSaveContextAbortsTheInFlightEtcdctlRun checks shutdown cancellation for
// the scheduled snapshot: the run group joins the snapshot loop at shutdown,
// so an etcdctl run that is in flight when the context ends must be aborted,
// not waited out — under systemd's KillMode=process that lingering window is
// exactly where a snapshot dies mid-write. The fake etcdctl marks the moment
// its run starts, writes the .part file used by real etcdctl, and then
// lingers: the marker proves the abort hit a real
// in-flight run rather than a pre-call check, and the written file proves
// the abort removed the truncated snapshot a killed mid-write run leaves
// behind instead of leaving it for retention to count. Same real-time shape
// as internal/k8sutil's TestWaitHealthzContextEndsWithItsContext: a subprocess
// kill cannot run on the fake clock.
func TestSaveContextAbortsTheInFlightEtcdctlRun(t *testing.T) {
	dir := t.TempDir()
	fakeEtcdctl(t, ": > \"$3.part\"\ntouch \"$3.started\"\nexec sleep 60")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := SaveContext(ctx, io.Discard, SaveOptions{Dir: dir, Host: "cp1", Name: "snap.db"})
		done <- err
	}()

	marker := filepath.Join(dir, "snap.db.started")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the etcdctl snapshot run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SaveContext reported success for a snapshot run it aborted")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("SaveContext did not return after its context ended: the in-flight etcdctl run was waited out")
	}
	if took := time.Since(begin); took > 10*time.Second {
		t.Errorf("took %s to abort a run whose etcdctl sleeps 60s; the kill must be prompt", took)
	}
	if _, err := os.Stat(filepath.Join(dir, "snap.db")); err == nil {
		t.Error("a snapshot file exists although the etcdctl run was aborted mid-flight")
	}
	if _, err := os.Stat(filepath.Join(dir, "snap.db.part")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial snapshot remains after cancellation: %v", err)
	}
}

func TestFailedSnapshotPreservesExistingBackup(t *testing.T) {
	for _, body := range []string{"exit 1", ": > \"$3.part\"\nexit 1"} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snap.db")
			if err := os.WriteFile(path, []byte("complete backup"), 0o600); err != nil {
				t.Fatal(err)
			}
			fakeEtcdctl(t, body)
			if err := SnapshotContext(context.Background(), path); err == nil {
				t.Fatal("failed etcdctl reported success")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "complete backup" {
				t.Fatalf("previous backup changed: contents = %q, err = %v", got, err)
			}
			if _, err := os.Stat(path + ".part"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("partial snapshot remains after failure: %v", err)
			}
		})
	}
}

func TestSaveContextCancelsInFlightS3Retention(t *testing.T) {
	for _, stage := range []string{"list", "delete"} {
		t.Run(stage, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					w.Header().Set("ETag", "\"snapshot\"")
					return
				}
				if r.Method == http.MethodGet && stage == "delete" {
					w.Header().Set("Content-Type", "application/xml")
					w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>etcd-snapshot-cp1-20260101-000000.db</Key><Size>1</Size></Contents><Contents><Key>etcd-snapshot-cp1-20260102-000000.db</Key><Size>1</Size></Contents></ListBucketResult>`))
					return
				}
				close(blocked)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer srv.Close()
			defer close(release)
			fakeEtcdctl(t, "printf 'snapshot' > \"$3\"")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := SaveContext(ctx, io.Discard, SaveOptions{Dir: t.TempDir(), Host: "cp1", Retain: 1,
					S3: &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}})
				done <- err
			}()
			select {
			case <-blocked:
			case err := <-done:
				t.Fatalf("save returned before retention: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("retention request never started")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("successful snapshot should survive best-effort retention cancellation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("save remained blocked in retention after cancellation")
			}
		})
	}
}

// TestSaveContextWithNoTimeLeftNeverLaunchesEtcdctl: a context that is
// already cancelled must not start a new snapshot run at all.
func TestSaveContextWithNoTimeLeftNeverLaunchesEtcdctl(t *testing.T) {
	dir := t.TempDir()
	fakeEtcdctl(t, "touch \"$3.started\"\nexit 0")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := SaveContext(ctx, io.Discard, SaveOptions{Dir: dir, Host: "cp1", Name: "snap.db"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the refusal wrapping context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "snap.db.started")); statErr == nil {
		t.Error("the etcdctl run launched although the context was already cancelled")
	}
}

// TestSaveConfigFailedWriteLeavesOldConfIntact is a regression test for
// atomic writes: a write interrupted partway through (disk full, the
// process killed) must not leave a truncated snapshot.conf — a parsed
// truncated conf silently reverts RETAIN and drops the S3 target, so the
// next prune deletes backups.
// WriteFileAtomically's temp-file-then-rename means a failure before the
// rename never touches the file already at path: a direct os.WriteFile
// would have truncated the destination by the time CreateTemp's equivalent
// failure lands, so the old content would be gone even though the write
// as a whole failed.
func TestSaveConfigFailedWriteLeavesOldConfIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}
	clearS3Env(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.conf")
	old := "RETAIN=5\nS3_BUCKET=backups\n"
	writeConf(t, path, old)

	// Readable and searchable but not writable: CreateTemp in this directory
	// is denied, simulating a write failure without needing to actually fill
	// the disk.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	// A different RETAIN, so the content comparison fails and saveConfigTo
	// must try to write — a no-op short-circuit is not what is under test here.
	if err := saveConfigTo(path, Config{Retain: 7}); err == nil {
		t.Fatal("saveConfigTo succeeded despite an unwritable directory")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("snapshot.conf gone after the failed save: %v", err)
	}
	if string(got) != old {
		t.Fatalf("failed save corrupted the old snapshot.conf: got %q, want %q", got, old)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("failed save changed the snapshot.conf mode: got %v, want %v", perm, os.FileMode(0o600))
	}
	// No stray temp file either: the failed attempt's temp file must have been
	// cleaned up, leaving only the conf itself in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshot.conf" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed save left stray files in the config directory: %v", names)
	}
}

// TestSaveContextStillSavesWithALiveContext pins the save's own behavior
// through the context change: a live context must not change what a
// scheduled save does.
func TestSaveContextStillSavesWithALiveContext(t *testing.T) {
	dir := t.TempDir()
	fakeEtcdctl(t, ": > \"$3\"\nexit 0")

	path, err := SaveContext(context.Background(), io.Discard, SaveOptions{Dir: dir, Host: "cp1", Name: "snap.db"})
	if err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	if want := filepath.Join(dir, "snap.db"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "snap.db")); err != nil {
		t.Errorf("the snapshot the etcdctl run wrote is missing: %v", err)
	}
}

// ── the stdout-guard retirement pins ─────────────────────────────────────────
//
// The save's warnings, the restore's progress and the write-tier SaveConfig
// seam all take an io.Writer FIRST and write there, so they need no
// internal/mcp guardStdout/discardStdout wrap.

// captureRealStdout captures the process's ACTUAL stdout while fn runs, so a
// test can prove an operation left it alone.
func captureRealStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()

	fn()

	os.Stdout = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

// TestSaveContext_WritesItsWarningsToTheWriter pins the save path's half of
// the convention: SaveContext takes the writer after its context, and a failed
// retention — a warning, not an error — is written there for the CLI and the
// scheduled loop, never printed to os.Stdout. The etcdctl child's progress
// must follow the same writer. The S3 stub uploads the snapshot
// (PUT answers 200) and refuses every listing (GET answers 403), so the save
// succeeds and its prune fails deterministically.
func TestSaveContext_WritesItsWarningsToTheWriter(t *testing.T) {
	dir := t.TempDir()
	fakeEtcdctl(t, "printf 'snapshot' > \"$3\"\nprintf 'snapshot child progress\\n'")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.Header().Set("ETag", "\"snapshot\"")
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	var (
		path string
		err  error
	)
	onStdout := captureRealStdout(t, func() {
		path, err = SaveContext(context.Background(), &buf, SaveOptions{Dir: dir, Host: "cp1", Name: "snap.db", Retain: 5,
			S3: &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}})
	})
	if err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	if path != filepath.Join(dir, "snap.db") {
		t.Errorf("path = %q, want %q", path, filepath.Join(dir, "snap.db"))
	}
	for _, want := range []string{"snapshot child progress", "WARN:", "S3 snapshot prune"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the save's progress is missing %q; the writer the save was handed has:\n%s", want, buf.String())
		}
	}
	if onStdout != "" {
		t.Errorf("the save printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// TestSaveConfig_TakesTheWriterTheWriteTierHandsIt pins the write-tier config
// seam: SaveConfig takes the writer first — held at compile level, since the
// writer cannot be observed through a function that prints nothing — and the
// file the snapshot schedule owns is still written, exercised through
// saveConfigTo: SaveConfig writes the package's const ConfigPath, which a
// fixture cannot redirect.
func TestSaveConfig_TakesTheWriterTheWriteTierHandsIt(t *testing.T) {
	var _ func(io.Writer, Config) error = SaveConfig
	path := confPath(t)

	var err error
	onStdout := captureRealStdout(t, func() {
		err = saveConfigTo(path, Config{Retain: 5, S3: objstore.Target{Bucket: "backups"}})
	})
	if err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	if got := readConf(t, path); !strings.Contains(got, "RETAIN=5") || !strings.Contains(got, "S3_BUCKET=backups") {
		t.Errorf("snapshot.conf = %q; the schedule's config must still round-trip", got)
	}
	if onStdout != "" {
		t.Errorf("the config save wrote on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// TestRestore_RunsOnTheWriterItIsHanded pins the restore's half: the runner
// takes the writer first and every "etcd restore:" step and the closing
// "etcd restored from ..." line reach it, including etcdutl's child output,
// never os.Stdout. The fake-effects
// node stands in for the control plane, exactly as the fence and rollback
// tests drive it. RestoreLocalSeed, the entry point internal/mcp calls, takes
// the writer first by the same convention — a compile-level pin, because a
// run built from its production effects would stop this node's control plane.
func TestRestore_RunsOnTheWriterItIsHanded(t *testing.T) {
	var _ func(io.Writer, string) error = RestoreLocalSeed

	n := newFakeNode(t)
	if err := os.WriteFile(n.fx.etcdutlPath, []byte("#!/bin/sh\nprintf 'restore child progress\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRun := n.fx.restoreRun
	n.fx.restoreRun = func(cmd *exec.Cmd) error {
		if err := cmd.Run(); err != nil {
			return err
		}
		return fakeRun(cmd)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = runRestore(&buf, RestoreOptions{Snapshot: snap, PeerURL: testPeer, InitialCluster: testLedger, Stamp: testStamp}, n.fx)
	})
	if err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	for _, want := range []string{"etcd restore: stopping apiserver + etcd", "restore child progress", "etcd restored from"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the restore's progress is missing %q; the writer it was handed has:\n%s", want, buf.String())
		}
	}
	if onStdout != "" {
		t.Errorf("the restore printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

func writeAt(t *testing.T, dir, name string, mod time.Time) {
	t.Helper()
	write(t, dir, name)
	if err := os.Chtimes(filepath.Join(dir, name), mod, mod); err != nil {
		t.Fatal(err)
	}
}

// TestPruneKeepsTheNewestByModTimeAfterAClockStepBack: snapshots taken after
// the clock stepped back carry earlier-looking names. Prune must keep the
// snapshots that were written last, not the ones whose names sort last.
func TestPruneKeepsTheNewestByModTimeAfterAClockStepBack(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	stale1 := ScheduledName("cp1", "20270101-000000")
	stale2 := ScheduledName("cp1", "20270101-120000")
	fresh := ScheduledName("cp1", "20261020-120000")
	writeAt(t, dir, stale1, base)
	writeAt(t, dir, stale2, base.Add(12*time.Hour))
	writeAt(t, dir, fresh, base.Add(24*time.Hour))

	if err := Prune(dir, hostSnapshot("cp1"), 2); err != nil {
		t.Fatal(err)
	}
	if !exists(dir, fresh) {
		t.Errorf("Prune deleted %s, the snapshot written last", fresh)
	}
	if exists(dir, stale1) {
		t.Errorf("Prune kept %s, the snapshot written first", stale1)
	}
	if !exists(dir, stale2) {
		t.Errorf("Prune deleted %s, the second newest by modification time", stale2)
	}
}

// TestPruneTriesEveryExpiredSnapshot: one snapshot that cannot be removed must
// not stop retention for the others, and its failure is still returned.
func TestPruneTriesEveryExpiredSnapshot(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var names []string
	for i := range 5 {
		n := ScheduledName("cp1", fmt.Sprintf("20261020-0%d0000", i))
		writeAt(t, dir, n, base.Add(time.Duration(i)*time.Hour))
		names = append(names, n)
	}
	stuck := filepath.Join(dir, names[0])
	old := removeSnapshot
	removeSnapshot = func(p string) error {
		if p == stuck {
			return &os.PathError{Op: "remove", Path: p, Err: os.ErrPermission}
		}
		return os.Remove(p)
	}
	t.Cleanup(func() { removeSnapshot = old })

	err := Prune(dir, hostSnapshot("cp1"), 2)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Prune = %v, want the stuck snapshot's permission error", err)
	}
	for _, n := range names[1:3] {
		if exists(dir, n) {
			t.Errorf("expired %s survived because an older snapshot could not be removed", n)
		}
	}
	for _, n := range names[3:] {
		if !exists(dir, n) {
			t.Errorf("Prune removed %s, one of the newest two", n)
		}
	}
}

// TestSaveContextStillTrimsLocalSnapshotsWhenTheUploadFails: an S3 outage must
// not stop local retention, or /var/backups/etcd fills the disk etcd shares.
// The upload error is still returned, and the bucket is not pruned, since the
// newest snapshot never reached it.
func TestSaveContextStillTrimsLocalSnapshotsWhenTheUploadFails(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-48 * time.Hour)
	var older []string
	for i := range 4 {
		n := ScheduledName("cp1", fmt.Sprintf("20200101-0%d0000", i))
		writeAt(t, dir, n, base.Add(time.Duration(i)*time.Hour))
		older = append(older, n)
	}
	fakeEtcdctl(t, "printf 'snapshot' > \"$3\"")
	var s3Calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Calls = append(s3Calls, r.Method)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	path, err := SaveContext(context.Background(), io.Discard, SaveOptions{Dir: dir, Host: "cp1", Retain: 2,
		S3: &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}})
	if err == nil || !strings.Contains(err.Error(), "S3 upload failed") {
		t.Fatalf("SaveContext = %v, want the upload error", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("the new local snapshot is missing: %v", serr)
	}
	for _, n := range older[:3] {
		if exists(dir, n) {
			t.Errorf("%s survived: a failed upload skipped local retention", n)
		}
	}
	if !exists(dir, older[3]) {
		t.Errorf("%s, the newest older snapshot, was removed; retain=2 keeps it with the new one", older[3])
	}
	for _, m := range s3Calls {
		if m != http.MethodPut {
			t.Errorf("S3 saw a %s after the upload failed; the bucket must not be pruned", m)
		}
	}
}
