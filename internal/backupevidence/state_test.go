package backupevidence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// From here: the durable attempt-state record, the snapshot evidence file —
// the renewal-record precedent applied to the scheduled snapshot loop. The
// tests prove durability (a save->reload cycle), independence (prune attempts
// beside save and upload ones), and the mutation owner's concurrency
// discipline (the one nodestate holds for node.yaml).

// withEvidencePaths points the snapshot evidence file and the etcd data lock
// at one temp tree: the mutation owner takes the lock on the data dir, so a
// test must redirect both it and the record's directory or the lock file lands
// in /var/lib/stratum.
func withEvidencePaths(t *testing.T) {
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

// attemptsEqual reports whether two attempt records hold the same evidence,
// comparing timestamps as instants: a round trip through the file re-parses
// them, and wall-clock representations need not match bit for bit.
func attemptsEqual(got, want *Attempts) error {
	if got.SchemaVersion != want.SchemaVersion {
		return fmt.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if !got.Save.AttemptedAt.Equal(want.Save.AttemptedAt) || got.Save.Result != want.Save.Result {
		return fmt.Errorf("save attempt = %+v, want %+v", got.Save, want.Save)
	}
	if !got.Upload.AttemptedAt.Equal(want.Upload.AttemptedAt) || got.Upload.Result != want.Upload.Result {
		return fmt.Errorf("upload attempt = %+v, want %+v", got.Upload, want.Upload)
	}
	if !got.Prune.AttemptedAt.Equal(want.Prune.AttemptedAt) || got.Prune.Result != want.Prune.Result {
		return fmt.Errorf("prune attempt = %+v, want %+v", got.Prune, want.Prune)
	}
	if !got.LastSaveSuccess.Equal(want.LastSaveSuccess) {
		return fmt.Errorf("last save success = %v, want %v", got.LastSaveSuccess, want.LastSaveSuccess)
	}
	if !got.LastUploadSuccess.Equal(want.LastUploadSuccess) {
		return fmt.Errorf("last upload success = %v, want %v", got.LastUploadSuccess, want.LastUploadSuccess)
	}
	if !slices.Equal(got.Warnings, want.Warnings) {
		// The record's own writers set warnings in order; only the
		// concurrent test appends them from racing writers, whose order is
		// whoever got the lock last, so its caller compares sorted copies.
		gotW, wantW := slices.Clone(got.Warnings), slices.Clone(want.Warnings)
		slices.Sort(gotW)
		slices.Sort(wantW)
		if !slices.Equal(gotW, wantW) {
			return fmt.Errorf("warnings = %q, want %q", got.Warnings, want.Warnings)
		}
	}
	return nil
}

// TestStateAttemptsSurviveRestart is the core durability proof: every attempt
// the loop records — the latest save and upload attempts, their separate
// last-success timestamps, the independent prune attempt, the warnings —
// survives a save->reload cycle, and the file carries the record's schema
// version. The reload is a fresh load from disk: nothing of the in-memory
// record may carry over but through the file. A first tick succeeds and a
// second fails, so the record must hold the failed attempts while the
// earlier successes survive them.
func TestStateAttemptsSurviveRestart(t *testing.T) {
	withEvidencePaths(t)
	saved := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	uploaded := saved.Add(90 * time.Second)
	nextSave := saved.Add(12 * time.Hour) // the next tick, which fails
	nextUpload := nextSave.Add(90 * time.Second)
	pruned := nextSave.Add(time.Minute)

	a := &Attempts{}
	a.RecordSave(saved, nil)
	a.RecordUpload(uploaded, nil)
	a.RecordSave(nextSave, errors.New("etcdctl snapshot save: etcd is not healthy"))
	a.RecordUpload(nextUpload, errors.New("upload snapshot.db: connection refused"))
	a.RecordPrune(pruned, errors.New("prune /var/backups/etcd: permission denied"))
	a.SetWarnings("WARN: cannot read the snapshot config; the scheduled loop keeps every snapshot and uploads nothing")
	if err := SaveAttempts(a); err != nil {
		t.Fatalf("SaveAttempts: %v", err)
	}

	reloaded, err := LoadAttempts()
	if err != nil {
		t.Fatalf("LoadAttempts after a restart: %v", err)
	}
	want := &Attempts{
		SchemaVersion:     AttemptsSchemaVersion,
		Save:              Attempt{AttemptedAt: nextSave, Result: "etcdctl snapshot save: etcd is not healthy"},
		Upload:            Attempt{AttemptedAt: nextUpload, Result: "upload snapshot.db: connection refused"},
		Prune:             Attempt{AttemptedAt: pruned, Result: "prune /var/backups/etcd: permission denied"},
		LastSaveSuccess:   saved,
		LastUploadSuccess: uploaded,
		Warnings:          []string{"WARN: cannot read the snapshot config; the scheduled loop keeps every snapshot and uploads nothing"},
	}
	if err := attemptsEqual(reloaded, want); err != nil {
		t.Error(err)
	}
}

// TestStateFailedRetriesPersistAfterRestart walks the loop's own write path —
// the mutation owner — across a restart: a successful tick is recorded, the
// process restarts, the next tick fails, and the failed retry persists while
// the previous success survives it. That is the pair the record exists for:
// an operator reading the node after a crash sees both that backups stopped
// succeeding and when the last one that did.
func TestStateFailedRetriesPersistAfterRestart(t *testing.T) {
	withEvidencePaths(t)
	ok := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	uploadOK := ok.Add(time.Minute)
	fail := ok.Add(12 * time.Hour)
	saveErr := errors.New("etcdctl snapshot save: etcd is not healthy")
	uploadErr := errors.New("upload snapshot.db: connection refused")

	if err := MutateAttempts(func(st *Attempts) error {
		st.RecordSave(ok, nil)
		st.RecordUpload(uploadOK, nil)
		return nil
	}); err != nil {
		t.Fatalf("record the successful tick: %v", err)
	}
	// The restart: everything after this reads only the file.
	if got, err := LoadAttempts(); err != nil {
		t.Fatalf("load after the successful tick: %v", err)
	} else if !got.LastSaveSuccess.Equal(ok) || !got.LastUploadSuccess.Equal(uploadOK) {
		t.Errorf("first successes = %v/%v, want %v/%v", got.LastSaveSuccess, got.LastUploadSuccess, ok, uploadOK)
	}

	if err := MutateAttempts(func(st *Attempts) error {
		st.RecordSave(fail, saveErr)
		st.RecordUpload(fail.Add(time.Minute), uploadErr)
		return nil
	}); err != nil {
		t.Fatalf("record the failed retry: %v", err)
	}
	got, err := LoadAttempts()
	if err != nil {
		t.Fatalf("load after the failed retry: %v", err)
	}
	if !got.Save.AttemptedAt.Equal(fail) || got.Save.Result != saveErr.Error() {
		t.Errorf("the failed save attempt did not persist: %+v", got.Save)
	}
	if !got.LastSaveSuccess.Equal(ok) {
		t.Errorf("the failed retry lost the previous save success: last = %v, want %v", got.LastSaveSuccess, ok)
	}
	if !got.Upload.AttemptedAt.Equal(fail.Add(time.Minute)) || got.Upload.Result != uploadErr.Error() {
		t.Errorf("the failed upload attempt did not persist: %+v", got.Upload)
	}
	if !got.LastUploadSuccess.Equal(uploadOK) {
		t.Errorf("the failed retry lost the previous upload success: last = %v, want %v", got.LastUploadSuccess, uploadOK)
	}
}

// TestStatePruneAttemptsAreIndependent pins the record's independence: a
// prune attempt is neither folded into the save attempt nor able to clobber
// it, so a save that succeeded while pruning failed reads as a working
// backup with broken retention — two conditions that need different operator
// action.
func TestStatePruneAttemptsAreIndependent(t *testing.T) {
	withEvidencePaths(t)
	saved := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	uploaded := saved.Add(time.Second)
	pruned := saved.Add(time.Minute)
	pruned2 := pruned.Add(12 * time.Hour)
	pruneErr := errors.New("s3 prune: access denied")

	record := func(fn func(*Attempts)) {
		t.Helper()
		if err := MutateAttempts(func(st *Attempts) error { fn(st); return nil }); err != nil {
			t.Fatalf("MutateAttempts: %v", err)
		}
	}
	record(func(st *Attempts) { st.RecordSave(saved, nil); st.RecordUpload(uploaded, nil) })
	record(func(st *Attempts) { st.RecordPrune(pruned, pruneErr) })

	got, err := LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Prune.AttemptedAt.Equal(pruned) || got.Prune.Result != pruneErr.Error() {
		t.Errorf("the failed prune attempt did not persist: %+v", got.Prune)
	}
	if !got.Save.AttemptedAt.Equal(saved) || got.Save.Result != "" || !got.LastSaveSuccess.Equal(saved) {
		t.Errorf("recording the prune changed the save record: save = %+v, last success = %v", got.Save, got.LastSaveSuccess)
	}
	if !got.Upload.AttemptedAt.Equal(uploaded) || got.Upload.Result != "" {
		t.Errorf("recording the prune changed the upload record: %+v", got.Upload)
	}

	// A later successful prune updates only the prune attempt.
	record(func(st *Attempts) { st.RecordPrune(pruned2, nil) })
	got, err = LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Prune.AttemptedAt.Equal(pruned2) || got.Prune.Result != "" {
		t.Errorf("the successful prune attempt did not persist: %+v", got.Prune)
	}
	if !got.Save.AttemptedAt.Equal(saved) || !got.LastSaveSuccess.Equal(saved) {
		t.Errorf("the successful prune changed the save record: save = %+v, last success = %v", got.Save, got.LastSaveSuccess)
	}
}

// TestStateWriteIsAtomicAndPrivate pins the write itself to the node-file
// rule: the record lands through the shared atomic writer at mode 0600 — a
// torn write must never leave a half-parsable record where a working one was,
// and a node-local record of failures must not be world-readable — and the
// directory is left holding the record alone, no stray temp name beside it.
func TestStateWriteIsAtomicAndPrivate(t *testing.T) {
	withEvidencePaths(t)
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	a := &Attempts{}
	a.RecordSave(at, nil)
	a.SetWarnings("WARN: cannot read the snapshot config")
	if err := SaveAttempts(a); err != nil {
		t.Fatalf("SaveAttempts: %v", err)
	}

	fi, err := os.Stat(EvidencePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the record was written as %04o, want 0600", perm)
	}

	// The bytes are the shared atomic writer's bytes for the same record —
	// the owner delegates its whole write, nothing of its own beside it.
	other := t.TempDir()
	b, err := yaml.Marshal(a) // a carries the schema version the save stamped
	if err != nil {
		t.Fatal(err)
	}
	atomicPath := filepath.Join(other, "snapshot-evidence.yaml")
	if err := atomicfile.WriteFile(atomicPath, b, 0o600); err != nil {
		t.Fatalf("atomicfile.WriteFile: %v", err)
	}
	saved, err := os.ReadFile(EvidencePath())
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(atomicPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(written) {
		t.Errorf("SaveAttempts wrote different bytes than the shared atomic writer:\n%s\n%s", saved, written)
	}
	if !strings.Contains(string(saved), "schemaVersion: 1") {
		t.Errorf("the record does not carry its schema version:\n%s", saved)
	}

	entries, err := os.ReadDir(version.K8sConfDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshot-evidence.yaml" {
		t.Errorf("the config dir holds %v, want the record alone", entries)
	}
}

// TestStateMissingRecordIsNoAttempts: a node whose loop has never run has no
// file. The loader reports that as fs.ErrNotExist — no attempt recorded yet,
// never an invented empty record — the first mutation runs against a fresh
// empty record, and a mutation that fails writes nothing.
func TestStateMissingRecordIsNoAttempts(t *testing.T) {
	withEvidencePaths(t)
	if _, err := LoadAttempts(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadAttempts = %v, want fs.ErrNotExist on a node with no record", err)
	}

	var saw *Attempts
	if err := MutateAttempts(func(st *Attempts) error {
		// Capture the record the mutation is handed before recording into
		// it: saw must hold what the owner read, not what this callback then
		// wrote.
		cur := *st
		saw = &cur
		st.RecordSave(time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC), nil)
		return nil
	}); err != nil {
		t.Fatalf("the first mutation: %v", err)
	}
	if saw == nil || !saw.Save.AttemptedAt.IsZero() {
		t.Errorf("the first mutation ran against %+v, want a fresh empty record", saw)
	}
	if got, err := LoadAttempts(); err != nil || got.Save.AttemptedAt.IsZero() {
		t.Errorf("the first save did not land: %+v, %v", got, err)
	}

	// A mutation that fails writes nothing; what the file already holds
	// stays.
	boom := errors.New("boom")
	if err := MutateAttempts(func(*Attempts) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("MutateAttempts = %v, want the mutation's error", err)
	}
	if got, err := LoadAttempts(); err != nil || got.Save.AttemptedAt.IsZero() {
		t.Errorf("a failed mutation changed the record: %+v, %v", got, err)
	}
}

// TestStateUnchangedMutationWritesNothing pins the no-change contract: a
// mutate that reports ErrUnchanged — a tick that finds nothing to record — is
// a success the owner answers without saving, so a healthy loop must not
// rewrite the file with the bytes it just read, tick after tick. The file's
// mtime is forced into the past first, so no save can hide behind timestamp
// granularity: any write moves it forward.
func TestStateUnchangedMutationWritesNothing(t *testing.T) {
	withEvidencePaths(t)
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	base := &Attempts{}
	base.RecordSave(at, nil)
	if err := SaveAttempts(base); err != nil {
		t.Fatalf("SaveAttempts: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(EvidencePath(), past, past); err != nil {
		t.Fatal(err)
	}

	saves := 0
	if err := MutateAttemptsSave(func(*Attempts) error { return ErrUnchanged },
		func(*Attempts) error { saves++; return nil }); err != nil {
		t.Fatalf("a no-change mutation = %v, want success", err)
	}
	if saves != 0 {
		t.Errorf("the no-change mutation saved %d times, want 0", saves)
	}
	fi, err := os.Stat(EvidencePath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().After(past) {
		t.Errorf("the record was rewritten anyway (mtime %s)", fi.ModTime())
	}
	if got, err := LoadAttempts(); err != nil || !got.Save.AttemptedAt.Equal(at) {
		t.Errorf("the no-change mutation changed the record: %+v, %v", got, err)
	}

	// The control: a real change still writes — ErrUnchanged is the
	// mutate's report, not a blanket write skip.
	if err := MutateAttemptsSave(func(st *Attempts) error { st.RecordPrune(at.Add(time.Hour), nil); return nil },
		SaveAttempts); err != nil {
		t.Fatalf("a real change = %v, want it written", err)
	}
	if got, err := LoadAttempts(); err != nil || !got.Prune.AttemptedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("a real change did not reach the record: %+v, %v", got, err)
	}
}

// TestStateRefusesWhileTheDataLockIsHeld pins the owner's lock contract: it
// takes the etcd data lock itself, so while a rejoin, a restore or an `up`
// holds it the recording refuses instead of writing between that operation's
// read and write, and the record is left untouched. Callers retry on the
// next tick, the way the loop's own poll does.
func TestStateRefusesWhileTheDataLockIsHeld(t *testing.T) {
	withEvidencePaths(t)
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	base := &Attempts{}
	base.RecordSave(at, nil)
	if err := SaveAttempts(base); err != nil {
		t.Fatalf("SaveAttempts: %v", err)
	}
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := MutateAttempts(func(st *Attempts) error { st.RecordPrune(at.Add(time.Hour), nil); return nil }); err == nil {
		t.Fatal("the owner wrote while another operation held the data lock")
	}
	got, err := LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if err := attemptsEqual(got, base); err != nil {
		t.Errorf("the refused recording changed the record: %v", err)
	}
}

// TestStateConcurrentWritersLoseNoRecord is the concurrent-update proof: the
// save, upload and prune recordings plus three warning appends — six writers,
// none holding the lock itself — all land on the one file. Every mutation
// re-reads under the lock what the previous writer recorded, so all six
// survive; a save of a pre-loaded copy would keep only the last writer's
// record and lose the rest.
func TestStateConcurrentWritersLoseNoRecord(t *testing.T) {
	withEvidencePaths(t)
	saveAt := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	uploadAt := saveAt.Add(time.Minute)
	pruneAt := saveAt.Add(2 * time.Minute)
	pruneErr := errors.New("s3 prune: access denied")
	warns := []string{
		"WARN: cannot read the snapshot config",
		"WARN: cannot read the cluster config the controller was started with",
		"WARN: the snapshot directory is almost full",
	}

	writers := []struct {
		name string
		set  func(*Attempts)
	}{
		{"save", func(st *Attempts) { st.RecordSave(saveAt, nil) }},
		{"upload", func(st *Attempts) { st.RecordUpload(uploadAt, nil) }},
		{"prune", func(st *Attempts) { st.RecordPrune(pruneAt, pruneErr) }},
		{"warn 1", func(st *Attempts) { st.Warnings = append(st.Warnings, warns[0]) }},
		{"warn 2", func(st *Attempts) { st.Warnings = append(st.Warnings, warns[1]) }},
		{"warn 3", func(st *Attempts) { st.Warnings = append(st.Warnings, warns[2]) }},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(writers))
	for _, w := range writers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Contention on the lock refuses rather than blocks (LOCK_NB);
			// retry the way the loop's next tick does.
			for try := 0; ; try++ {
				if err := MutateAttempts(func(st *Attempts) error { w.set(st); return nil }); err == nil {
					return
				}
				if try == 2000 {
					errs <- fmt.Errorf("%s: the mutation never got the data lock", w.name)
					return
				}
				time.Sleep(500 * time.Microsecond)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	got, err := LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	want := &Attempts{
		SchemaVersion:     AttemptsSchemaVersion,
		Save:              Attempt{AttemptedAt: saveAt},
		Upload:            Attempt{AttemptedAt: uploadAt},
		Prune:             Attempt{AttemptedAt: pruneAt, Result: pruneErr.Error()},
		LastSaveSuccess:   saveAt,
		LastUploadSuccess: uploadAt,
		Warnings:          warns,
	}
	if err := attemptsEqual(got, want); err != nil {
		t.Error(err)
	}
}

// TestStateSchemaVersion pins the version discipline: the writer stamps the
// record's schema version however the caller's copy reads, and a record a
// newer binary wrote refuses to load rather than be misread — a newer
// revision may mean something else by a field. An empty but valid record
// loads as no attempts yet: a node whose loop has never run one.
func TestStateSchemaVersion(t *testing.T) {
	withEvidencePaths(t)
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)

	a := &Attempts{}
	a.RecordSave(at, nil)
	if err := SaveAttempts(a); err != nil {
		t.Fatalf("SaveAttempts: %v", err)
	}
	if a.SchemaVersion != AttemptsSchemaVersion {
		t.Errorf("the save left the caller's copy at schema version %d, want it stamped %d", a.SchemaVersion, AttemptsSchemaVersion)
	}
	if got, err := LoadAttempts(); err != nil {
		t.Fatalf("LoadAttempts: %v", err)
	} else if got.SchemaVersion != AttemptsSchemaVersion {
		t.Errorf("the record carries schema version %d, want %d", got.SchemaVersion, AttemptsSchemaVersion)
	}

	if err := os.WriteFile(EvidencePath(), []byte("schemaVersion: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAttempts(); err == nil {
		t.Fatal("a record from a newer schema loaded; its fields may mean something this binary does not know")
	} else if !strings.Contains(err.Error(), "schema") {
		t.Errorf("the refusal does not say what it refused: %v", err)
	}

	if err := os.WriteFile(EvidencePath(), []byte("schemaVersion: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAttempts()
	if err != nil {
		t.Fatalf("an empty but valid record must load: %v", err)
	}
	if !got.Save.AttemptedAt.IsZero() || !got.Upload.AttemptedAt.IsZero() || !got.Prune.AttemptedAt.IsZero() ||
		!got.LastSaveSuccess.IsZero() || !got.LastUploadSuccess.IsZero() || got.Warnings != nil {
		t.Errorf("an empty record loaded as %+v, want no attempts recorded", got)
	}
}
