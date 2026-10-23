package assets

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every staged member is labelled for its destination while still under its
// temp name, so a staged binary is never visible with the label the data
// directory hands a new file.
func TestStageLabelsEachMemberBeforeTheRename(t *testing.T) {
	exe := appendZip(t, map[string]string{
		"bin/containerd":            "containerd",
		"bin/runc":                  "runc",
		"images/stratum-images.tar": "oci-archive",
	})
	root := t.TempDir()
	labelled := map[string]string{}
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	labelLike = func(_ io.Writer, path, dst string) error {
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s labelled after it was renamed into place", dst)
		}
		if filepath.Dir(path) != filepath.Dir(dst) || !strings.HasPrefix(filepath.Base(path), "."+filepath.Base(dst)+".tmp-") {
			t.Errorf("labelled %s for %s, want its temp file", path, dst)
		}
		labelled[dst] = path
		return nil
	}
	if _, err := StageFrom(exe, root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"bin/containerd", "bin/runc", "images/stratum-images.tar"} {
		if _, ok := labelled[filepath.Join(root, rel)]; !ok {
			t.Errorf("%s not labelled; labelled %v", rel, labelled)
		}
	}
}

// A member that cannot be labelled is not staged: the old binary stays.
func TestStageStopsOnALabelFailure(t *testing.T) {
	exe := appendZip(t, map[string]string{"bin/runc": "new runc"})
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "runc"), []byte("old runc"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	boom := errors.New("chcon: Operation not permitted")
	labelLike = func(io.Writer, string, string) error { return boom }
	if _, err := StageFrom(exe, root); !errors.Is(err, boom) {
		t.Fatalf("StageFrom = %v, want the label error", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "bin", "runc")); string(b) != "old runc" {
		t.Errorf("runc = %q, want the old binary kept", b)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "bin", ".*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
