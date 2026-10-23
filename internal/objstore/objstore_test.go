package objstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnabled(t *testing.T) {
	if (*Target)(nil).Enabled() {
		t.Error("nil Target must not be Enabled")
	}
	if (&Target{}).Enabled() {
		t.Error("empty Target (no bucket) must not be Enabled")
	}
	if !(&Target{Bucket: "b"}).Enabled() {
		t.Error("Target with a bucket must be Enabled")
	}
}

func TestKey(t *testing.T) {
	cases := []struct {
		prefix, name, want string
	}{
		{"", "snap.db", "snap.db"},
		{"backups", "snap.db", "backups/snap.db"},
		{"backups/", "snap.db", "backups/snap.db"},
		{"a/b", "snap.db", "a/b/snap.db"},
	}
	for _, c := range cases {
		if got := (&Target{Prefix: c.prefix}).Key(c.name); got != c.want {
			t.Errorf("Key(prefix=%q,%q)=%q, want %q", c.prefix, c.name, got, c.want)
		}
	}
}

func TestWriteAtomic(t *testing.T) {
	write := func(s string) func(*os.File) error {
		return func(f *os.File) error { _, err := f.WriteString(s); return err }
	}
	t.Run("mode 0600", func(t *testing.T) {
		root, dir := openRoot(t)
		p := filepath.Join(dir, "x.db")
		if err := writeAtomic(root, "x.db", write("new")); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("stat = %v, %v; want 0600", fi, err)
		}
	})
	t.Run("failure keeps the existing file and leaves no temp", func(t *testing.T) {
		root, dir := openRoot(t)
		p := filepath.Join(dir, "x.db")
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := writeAtomic(root, "x.db", func(f *os.File) error { _, _ = f.WriteString("part"); return errors.New("boom") })
		if err == nil {
			t.Fatal("want error")
		}
		if b, _ := os.ReadFile(p); string(b) != "old" {
			t.Errorf("existing file = %q, want it untouched", b)
		}
		if es, _ := os.ReadDir(dir); len(es) != 1 {
			t.Errorf("dir holds %d entries, want only x.db", len(es))
		}
	})
	t.Run("symlink at the final path is replaced, not followed", func(t *testing.T) {
		root, dir := openRoot(t)
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "x.db")
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(root, "x.db", write("new")); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(victim); string(b) != "keep" {
			t.Errorf("symlink target = %q, want untouched", b)
		}
		if b, _ := os.ReadFile(p); string(b) != "new" {
			t.Errorf("x.db = %q, want new", b)
		}
	})
}
