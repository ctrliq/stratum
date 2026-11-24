package etcdsnap

// Every recording re-reads the record under the etcd data lock, so concurrent writers lose none.

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// redirectRecord points the evidence record and the etcd data lock at one temp tree.
func redirectRecord(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	swap(t, &version.K8sConfDir, filepath.Join(root, "config"))
	swap(t, &version.DataDir, filepath.Join(root, "data"))
	swap(t, &version.EtcdDataDir, filepath.Join(root, "etcd"))
}

func TestRecordConcurrentRecordingsLoseNoRecord(t *testing.T) {
	redirectRecord(t)
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
		set  func(*snapshotEvidence)
	}{
		{"save", func(e *snapshotEvidence) { e.recordSave(saveAt, nil) }},
		{"upload", func(e *snapshotEvidence) { e.recordUpload(uploadAt, nil) }},
		{"prune", func(e *snapshotEvidence) { e.recordPrune(pruneAt, pruneErr) }},
		{"warn 1", func(e *snapshotEvidence) { e.Warnings = append(e.Warnings, warns[0]) }},
		{"warn 2", func(e *snapshotEvidence) { e.Warnings = append(e.Warnings, warns[1]) }},
		{"warn 3", func(e *snapshotEvidence) { e.Warnings = append(e.Warnings, warns[2]) }},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(writers))
	for _, w := range writers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			// LOCK_NB contention refuses rather than blocks; retry like the loop's next tick.
			for try := 0; ; try++ {
				if err := mutateEvidence(w.set); err == nil {
					return
				}
				if try == 2000 {
					errs <- fmt.Errorf("%s: the recording never got the data lock", w.name)
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

	rec, err := loadEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Save.AttemptedAt.Equal(saveAt) || rec.Save.Result != "" || !rec.LastSaveSuccess.Equal(saveAt) {
		t.Errorf("the save recording was lost: %+v (last %v)", rec.Save, rec.LastSaveSuccess)
	}
	if !rec.Upload.AttemptedAt.Equal(uploadAt) || rec.Upload.Result != "" || !rec.LastUploadSuccess.Equal(uploadAt) {
		t.Errorf("the upload recording was lost: %+v (last %v)", rec.Upload, rec.LastUploadSuccess)
	}
	if !rec.Prune.AttemptedAt.Equal(pruneAt) || rec.Prune.Result != pruneErr.Error() {
		t.Errorf("the prune recording was lost: %+v", rec.Prune)
	}
	for _, w := range warns {
		if !slices.Contains(rec.Warnings, w) {
			t.Errorf("the warning %q was lost: %q", w, rec.Warnings)
		}
	}
}
