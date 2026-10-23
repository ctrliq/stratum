package objstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// stubS3 serves body for every GET, or a 500 when fail is set.
func stubS3(t *testing.T, body string, fail bool) *Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

// openRoot opens a fresh temp dir as a Root, and returns it with its path.
func openRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func TestDownload(t *testing.T) {
	ctx := context.Background()
	t.Run("0600 even in a 0755 dir", func(t *testing.T) {
		root, dir := openRoot(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "x.db")
		if err := stubS3(t, "snap", false).Download(ctx, root, "x.db"); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("stat = %v, %v; want 0600", fi, err)
		}
		if b, _ := os.ReadFile(p); string(b) != "snap" {
			t.Errorf("content = %q", b)
		}
	})
	t.Run("symlink at the target is replaced, its target untouched", func(t *testing.T) {
		root, dir := openRoot(t)
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "x.db")
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
		if err := stubS3(t, "snap", false).Download(ctx, root, "x.db"); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(victim); string(b) != "keep" {
			t.Errorf("victim = %q, want untouched", b)
		}
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("target still a symlink: %v, %v", fi, err)
		}
		if b, _ := os.ReadFile(p); string(b) != "snap" {
			t.Errorf("x.db = %q", b)
		}
	})
	t.Run("failed download keeps the existing file", func(t *testing.T) {
		root, dir := openRoot(t)
		p := filepath.Join(dir, "x.db")
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := stubS3(t, "", true).Download(ctx, root, "x.db"); err == nil {
			t.Fatal("want error")
		}
		if b, _ := os.ReadFile(p); string(b) != "old" {
			t.Errorf("x.db = %q, want old", b)
		}
		if es, _ := os.ReadDir(dir); len(es) != 1 {
			t.Errorf("dir holds %d entries, want only x.db", len(es))
		}
	})
}

func TestRemoveStaleTemps(t *testing.T) {
	root, dir := openRoot(t)
	old := time.Now().Add(-time.Hour)
	write := func(name string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write(".x.db.123456", old)               // stale temp: removed
	write(".x.db.789", time.Now())           // recent temp, maybe a live download: kept
	write("x.db", old)                       // the snapshot itself: kept
	write("y.db", old)                       // unrelated: kept
	write(".x.db.bak", old)                  // not digits after the name: kept
	write(".x.db.", old)                     // no random part: kept
	write(".x.db.4294967296", old)           // overflows uint32, tempName never makes it: kept
	write(".x.db.000123", old)               // leading zeros, tempName never makes them: kept
	write(".x.db.99999999999999999999", old) // far past uint32: kept
	write(".x.db.4294967295", old)           // largest uint32, a genuine temp: removed
	write(".x.db.0", old)                    // smallest uint32, a genuine temp: removed
	write(".y.db.123456", old)               // another name's temp: kept
	if err := os.Mkdir(filepath.Join(dir, ".x.db.555"), 0o700); err != nil {
		t.Fatal(err)
	}
	// a symlink is not a regular file: it is left, and never followed
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, ".x.db.666")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveStaleTemps(root, "x.db", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	var got []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		got = append(got, e.Name())
	}
	want := []string{".x.db.", ".x.db.000123", ".x.db.4294967296", ".x.db.555", ".x.db.666", ".x.db.789", ".x.db.99999999999999999999", ".x.db.bak", ".y.db.123456", "x.db", "y.db"}
	if !slices.Equal(got, want) {
		t.Errorf("dir = %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim = %q", b)
	}
}

// TestDownloadSurvivesADirSwap pins that Download writes through the handle it
// was given, so swapping the directory's path for a symlink after the check
// redirects nothing.
func TestDownloadSurvivesADirSwap(t *testing.T) {
	root, dir := openRoot(t)
	victim := t.TempDir()
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatal(err)
	}
	if err := stubS3(t, "snap", false).Download(context.Background(), root, "x.db"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(moved, "x.db")); string(b) != "snap" {
		t.Errorf("checked dir x.db = %q, want snap", b)
	}
	if es, _ := os.ReadDir(victim); len(es) != 0 {
		t.Errorf("victim holds %d entries; the download followed the swapped symlink", len(es))
	}
}
