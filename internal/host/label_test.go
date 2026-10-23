package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unit file and the other files written atomically are labelled for their
// path while still under the temp name.
func TestWriteFileAtomicallyLabelsBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stratum.service")
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	var labelled []string
	labelLike = func(tmp, dst string) error {
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s labelled after it was renamed into place", dst)
		}
		if filepath.Dir(tmp) != dir || !strings.HasPrefix(filepath.Base(tmp), ".stratum.service.tmp-") {
			t.Errorf("labelled %s, want the temp file", tmp)
		}
		labelled = append(labelled, dst)
		return nil
	}
	if err := WriteFileAtomically(path, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(labelled) != 1 || labelled[0] != path {
		t.Errorf("labelled %q, want [%s]", labelled, path)
	}
}

func TestWriteFileAtomicallyStopsOnALabelFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stratum.service")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	boom := errors.New("chcon: Operation not permitted")
	labelLike = func(string, string) error { return boom }
	if err := WriteFileAtomically(path, []byte("new"), 0o644); !errors.Is(err, boom) {
		t.Fatalf("WriteFileAtomically = %v, want the label error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("%s = %q, want the old contents kept", path, b)
	}
}

func TestAtomicWriteLabelWarningsUseStderr(t *testing.T) {
	dir := t.TempDir()
	stderr, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldLabel := labelLike
	t.Cleanup(func() { labelLike = oldLabel; stderr.Close() })
	const warning = "WARN: policy missing; the next start relabels the file\n"
	labelLike = func(_, _ string) error {
		_, err := fmt.Fprint(stderr, warning)
		return err
	}
	var writeErr error
	stdout := quiet(t, func() {
		writeErr = WriteFileAtomically(filepath.Join(dir, "certificate.key"), []byte("key"), 0o600)
	})
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if stdout != "" {
		t.Errorf("implicit label warning corrupts process stdout: %q", stdout)
	}
	got, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != warning {
		t.Errorf("stderr = %q, want diagnostic %q", got, warning)
	}
}
