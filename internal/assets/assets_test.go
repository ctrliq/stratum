package assets

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// appendZip builds a fake stratum executable: some leading bytes standing in for
// the ELF, then a concatenated zip. Exercises the real Stage path.
func appendZip(t *testing.T, members map[string]string) string {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("#!/bin/sh\nexit 0\n") // stands in for the ELF payload
	zw := zip.NewWriter(&buf)
	for name, body := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "stratum")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStage(t *testing.T) {
	exe := appendZip(t, map[string]string{
		"bin/etcd":                  "etcd-binary-contents",
		"bin/runc":                  "runc",
		"images/stratum-images.tar": "oci-archive",
	})
	root := t.TempDir()

	staged, err := StageFrom(exe, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 3 {
		t.Fatalf("staged %d members, want 3: %v", len(staged), staged)
	}

	got, err := os.ReadFile(filepath.Join(root, "bin", "etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "etcd-binary-contents" {
		t.Errorf("etcd contents = %q", got)
	}

	// Binaries must be executable; images must not be.
	for _, tc := range []struct {
		rel  string
		mode os.FileMode
	}{
		{filepath.Join("bin", "etcd"), 0o755},
		{filepath.Join("images", "stratum-images.tar"), 0o644},
	} {
		fi, err := os.Stat(filepath.Join(root, tc.rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != tc.mode {
			t.Errorf("%s mode = %v, want %v", tc.rel, fi.Mode().Perm(), tc.mode)
		}
		// The staging cache key: staged mtime must equal the executable's.
		exeInfo, err := os.Stat(exe)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(exeInfo.ModTime()) {
			t.Errorf("%s mtime = %v, want executable mtime %v", tc.rel, fi.ModTime(), exeInfo.ModTime())
		}
	}
}

// A second start must not rewrite the payload — that is the fast-restart
// property, and it matters: the payload includes ~315MB of
// container images. Detected by making the staged dirs read-only.
func TestStageSkipsWhenUpToDate(t *testing.T) {
	exe := appendZip(t, map[string]string{
		"bin/etcd":                  "etcd-binary-contents",
		"images/stratum-images.tar": "oci-archive",
	})
	root := t.TempDir()

	if _, err := StageFrom(exe, root); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{BinDir, ImagesDir} {
		if err := os.Chmod(filepath.Join(root, d), 0o555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(filepath.Join(root, d), 0o755)
	}

	staged, err := StageFrom(exe, root)
	if err != nil {
		t.Fatalf("second Stage rewrote the payload instead of skipping: %v", err)
	}
	if len(staged) != 2 {
		t.Fatalf("staged %v, want both members reported without rewriting", staged)
	}
}

// A changed executable must re-stage even though the name and size match.
func TestStageRewritesOnMtimeChange(t *testing.T) {
	exe := appendZip(t, map[string]string{"bin/etcd": "aaaa"})
	root := t.TempDir()
	if _, err := StageFrom(exe, root); err != nil {
		t.Fatal(err)
	}

	exe2 := appendZip(t, map[string]string{"bin/etcd": "bbbb"}) // same size, new mtime
	if _, err := StageFrom(exe2, root); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(root, "bin", "etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bbbb" {
		t.Errorf("contents = %q, want the new payload", got)
	}
}

func TestStageNoPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stratum")
	if err := os.WriteFile(path, []byte("no zip here"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := StageFrom(path, t.TempDir()); !errors.Is(err, ErrNoPayload) {
		t.Errorf("err = %v, want ErrNoPayload", err)
	}
}

// Members outside the known payload directories are dropped, which is also what
// makes traversal impossible.
func TestStageRejectsUnknownAndEscapingMembers(t *testing.T) {
	exe := appendZip(t, map[string]string{
		"bin/etcd":            "ok",
		"../../escaped":       "nope",
		"bin/../../escaped2":  "nope",
		"etc/passwd":          "nope",
		"images/../bin/sneak": "nope-but-lands-in-bin",
	})
	root := t.TempDir()

	staged, err := StageFrom(exe, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range staged {
		abs := filepath.Join(root, rel)
		if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			t.Errorf("staged outside root: %s", abs)
		}
	}
	parent := filepath.Dir(root)
	for _, name := range []string{"escaped", "escaped2"} {
		if _, err := os.Stat(filepath.Join(parent, name)); err == nil {
			t.Errorf("archive member escaped the staging root: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "passwd")); err == nil {
		t.Error("member outside bin/ and images/ was staged")
	}
}

func TestPayloadPath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"bin/etcd", "bin/etcd", true},
		{"images/stratum-images.tar", "images/stratum-images.tar", true},
		{"images/../bin/etcd", "bin/etcd", true},
		{"../../escaped", "", false},
		{"bin/../../escaped", "", false},
		{"etc/passwd", "", false},
		{"bin/sub/nested", "", false},
		{"bin/", "", false},
		{"loose-file", "", false},
	} {
		got, ok := payloadPath(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("payloadPath(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLookupPrefersStaged(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, BinDir), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, BinDir, "etcd")
	if err := os.WriteFile(want, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Lookup(root, "etcd")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Lookup = %q, want %q", got, want)
	}
}

func TestLookupFallsBackToPATH(t *testing.T) {
	got, err := Lookup(t.TempDir(), "sh")
	if err != nil {
		t.Fatalf("Lookup(sh) = %v; expected a PATH hit", err)
	}
	if got == "" {
		t.Error("empty path")
	}
}
