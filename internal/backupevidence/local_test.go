package backupevidence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
)

func TestLocalExcludesNonregularSnapshots(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, "real.db", 10, snapTime(1))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"directory-link.db": "nested.db", "file-link.db": "real.db"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	res := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
	if !res.Complete || len(res.Artifacts) != 1 || res.Artifacts[0].ID != "real.db" {
		t.Fatalf("nonregular entries changed the exact snapshot total: %+v", res)
	}
}

func TestLocalExcludesSnapshotReplacedWithSymlink(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, "replaced.db", 10, snapTime(1))
	old := localEntryInfo
	t.Cleanup(func() { localEntryInfo = old })
	localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		return old(ctx, dir, name)
	}
	res := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
	if !res.Complete || len(res.Artifacts) != 0 {
		t.Fatalf("a replaced snapshot was counted: %+v", res)
	}
}

func TestLocalRejectsFIFOsWithoutBlocking(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		name := "directory"
		if sidecar {
			name = "digest sidecar"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "store")
			if sidecar {
				writeSnapshot(t, dir, "snap.db", 10, snapTime(1))
				fifo = filepath.Join(dir, "snap.db.sha256")
			} else {
				dir = fifo
			}
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			done := make(chan LocalResult, 1)
			go func() { done <- Local(ctx, LocalOptions{Dir: dir, Node: "cp-1"}) }()
			var res LocalResult
			select {
			case res = <-done:
			case <-time.After(300 * time.Millisecond):
				t.Error("the FIFO blocked the local catalog past its deadline")
				// Release a blocked open/read so the failing test leaves no reader.
				f, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if _, err := f.Write([]byte("invalid digest\n")); err != nil {
					t.Fatal(err)
				}
				res = <-done
			}
			if len(res.Warnings) == 0 {
				t.Error("a nonregular input must produce a safe diagnostic")
			}
			if sidecar && (len(res.Artifacts) != 1 || res.Artifacts[0].Digest != DigestUnknown) {
				t.Errorf("the unusable sidecar lost the local artifact: %+v", res)
			}
			if !sidecar && res.Complete {
				t.Error("a FIFO directory was reported as an exact catalog")
			}
		})
	}
}

// snapTime is a fixed UTC time for the snapshots a test writes, so the
// creation times the catalog reports assert exactly.
func snapTime(min int) time.Time {
	return time.Date(2026, 7, 4, 0, min, 0, 0, time.UTC)
}

// writeSnapshot writes one snapshot file of the given size with a fixed
// modification time — the creation time the catalog reports.
func writeSnapshot(t *testing.T, dir, name string, size int, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte("s"), size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// The catalog walks the node's snapshot directory: every snapshot file (the
// .db naming every save path uses) with its stable name-derived ID, the
// node-scoped location, its creation time, its size, and a digest only when a
// sidecar records one. Everything else in the directory — partial saves,
// sidecars, unrelated files, nested directories — is not an artifact and
// raises no diagnostic.
func TestLocalCatalogsSnapshotDirEntries(t *testing.T) {
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20260704-010000")
	s2 := "pre-upgrade-20260704-020000.db"
	writeSnapshot(t, dir, s1, 10, snapTime(1))
	writeSnapshot(t, dir, s2, 16, snapTime(2))
	for _, noise := range []string{s1 + ".part", "README", "unrelated.db.sha256"} {
		if err := os.WriteFile(filepath.Join(dir, noise), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "nested.db"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})

	if !r.Complete {
		t.Error("Complete = false, want true: a readable directory within the bounds was fully walked")
	}
	if len(r.Warnings) != 0 {
		t.Errorf("a healthy walk warned: %q", r.Warnings)
	}
	if r.Node != "cp-1" {
		t.Errorf("Node = %q, want the node the options scoped the walk to", r.Node)
	}
	want := []Artifact{
		{ID: s1, Location: "local", Created: snapTime(1), Size: 10, Digest: DigestUnknown},
		{ID: s2, Location: "local", Created: snapTime(2), Size: 16, Digest: DigestUnknown},
	}
	if len(r.Artifacts) != len(want) {
		t.Fatalf("Artifacts = %+v, want %d entries: %q and %q", r.Artifacts, len(want), s1, s2)
	}
	for i, w := range want {
		if got := r.Artifacts[i]; got.ID != w.ID || got.Location != w.Location || got.Size != w.Size || !got.Created.Equal(w.Created) || got.Digest != w.Digest {
			t.Errorf("Artifacts[%d] = %+v, want %+v", i, got, w)
		}
	}
}

// An empty store is a valid, complete zero — not an unknown — and a directory
// that was never created, the store before the first snapshot, is the same
// zero.
func TestLocalEmptyStoreIsACompleteExactZero(t *testing.T) {
	t.Run("empty dir", func(t *testing.T) {
		r := Local(context.Background(), LocalOptions{Dir: t.TempDir(), Node: "cp-1"})
		if !r.Complete || len(r.Artifacts) != 0 || len(r.Warnings) != 0 {
			t.Errorf("empty store = %+v, want a complete exact zero with no warnings", r)
		}
	})
	t.Run("never-created dir", func(t *testing.T) {
		r := Local(context.Background(), LocalOptions{Dir: filepath.Join(t.TempDir(), "absent"), Node: "cp-1"})
		if !r.Complete || len(r.Artifacts) != 0 || len(r.Warnings) != 0 {
			t.Errorf("never-created store = %+v, want a complete exact zero with no warnings", r)
		}
	})
}

// The local store keeps no ETag, so a digest is reported only when a sha256
// sidecar beside the snapshot records one — the catalog never hashes a
// snapshot to guess one. A sidecar that exists but carries no sha256 leaves
// the digest unknown with a safe warning; the artifact itself stays fully
// described and the catalog stays complete.
func TestLocalReportsADigestOnlyWhenASidecarKnowsIt(t *testing.T) {
	newDir := func(t *testing.T) (string, string) {
		t.Helper()
		dir := t.TempDir()
		s := etcdsnap.ScheduledName("cp-1", "20260704-010000")
		writeSnapshot(t, dir, s, 10, snapTime(1))
		return dir, s
	}
	writeSidecar := func(t *testing.T, dir, s, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, s+".sha256"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("valid sidecar", func(t *testing.T) {
		dir, s := newDir(t)
		hexd := strings.Repeat("a1b2", 16) // 64 hex characters, the sha256sum layout
		writeSidecar(t, dir, s, hexd+"  "+s+"\n")
		r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
		if !r.Complete || len(r.Warnings) != 0 || len(r.Artifacts) != 1 {
			t.Fatalf("sidecar walk = %+v, want one artifact, complete, no warnings", r)
		}
		if d := r.Artifacts[0].Digest; d != "sha256:"+hexd {
			t.Errorf("Digest = %q, want sha256:%s from the sidecar", d, hexd)
		}
	})
	t.Run("no sidecar", func(t *testing.T) {
		dir, _ := newDir(t)
		r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
		if !r.Complete || len(r.Warnings) != 0 || len(r.Artifacts) != 1 {
			t.Fatalf("sidecar-less walk = %+v, want one artifact, complete, no warnings", r)
		}
		if d := r.Artifacts[0].Digest; d != DigestUnknown {
			t.Errorf("Digest = %q, want %q: nothing cheaply knows a local digest", d, DigestUnknown)
		}
	})
	t.Run("malformed sidecar", func(t *testing.T) {
		dir, s := newDir(t)
		writeSidecar(t, dir, s, "not a digest\n")
		r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
		if !r.Complete || len(r.Artifacts) != 1 {
			t.Fatalf("malformed-sidecar walk = %+v, want the artifact still fully described", r)
		}
		if d := r.Artifacts[0].Digest; d != DigestUnknown {
			t.Errorf("Digest = %q, want %q: a sidecar without a sha256 is never guessed from", d, DigestUnknown)
		}
		w := strings.Join(r.Warnings, "\n")
		if !strings.Contains(w, s) || !strings.Contains(w, "digest") {
			t.Errorf("warnings do not name the snapshot and its unknown digest: %q", w)
		}
	})
}

// A cancelled request stops the walk where it stands: the entries it already
// described are returned, the rest of the store is never read, and the result
// says it is not a complete total — never a raw error. A context that already
// ended launches no read at all.
func TestLocalCancellationStopsTheWalk(t *testing.T) {
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20260704-010000")
	s2 := etcdsnap.ScheduledName("cp-1", "20260704-020000")
	writeSnapshot(t, dir, s1, 10, snapTime(1))
	writeSnapshot(t, dir, s2, 10, snapTime(2))

	t.Run("mid-walk", func(t *testing.T) {
		old := localEntryInfo
		t.Cleanup(func() { localEntryInfo = old })
		var calls int
		localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
			calls++
			<-ctx.Done() // parked: the read hangs until the request is cancelled
			return 0, time.Time{}, ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		time.AfterFunc(50*time.Millisecond, cancel)

		r := Local(ctx, LocalOptions{Dir: dir, Node: "cp-1"})

		if r.Complete {
			t.Error("Complete = true, want false: a cancelled walk is not a total")
		}
		if len(r.Artifacts) != 0 {
			t.Errorf("Artifacts = %+v, want none: no entry finished reading before the cancellation", r.Artifacts)
		}
		if w := strings.Join(r.Warnings, "\n"); !strings.Contains(w, "cancel") {
			t.Errorf("warnings do not say the walk was cancelled: %q", w)
		}
		if calls != 1 {
			t.Errorf("the walk read %d entries around the cancellation, want exactly the 1 it hung on", calls)
		}
	})
	t.Run("before the walk", func(t *testing.T) {
		old := localOpen
		t.Cleanup(func() { localOpen = old })
		var opens int
		localOpen = func(name string) (*os.File, error) {
			opens++
			return old(name)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		r := Local(ctx, LocalOptions{Dir: dir, Node: "cp-1"})

		if r.Complete || len(r.Artifacts) != 0 {
			t.Errorf("pre-cancelled walk = %+v, want no artifacts and no complete total", r)
		}
		if w := strings.Join(r.Warnings, "\n"); !strings.Contains(w, "cancel") {
			t.Errorf("warnings do not say the walk was cancelled: %q", w)
		}
		if opens != 0 {
			t.Errorf("the pre-cancelled walk opened the directory %d times, want 0", opens)
		}
	})
}

// The page cap bounds how deep the walk goes: it stops after that many
// artifacts and says the catalog is not a complete total. The directory
// streams its entries in a filesystem-defined order, so which entries survive
// the cap is not the contract: the test counts the per-entry reads through
// the seam and asserts the walk stopped after exactly the cap's number.
func TestLocalStopsAtThePageCap(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := 1; i <= 3; i++ {
		n := etcdsnap.ScheduledName("cp-1", fmt.Sprintf("20260704-0100%02d", i))
		names = append(names, n)
		writeSnapshot(t, dir, n, 10, snapTime(i))
	}

	old := localEntryInfo
	t.Cleanup(func() { localEntryInfo = old })
	reads := 0
	localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
		reads++
		return old(ctx, dir, name)
	}

	r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1", MaxEntries: 2})

	if r.Complete {
		t.Error("Complete = true, want false: the page cap stopped the walk")
	}
	if len(r.Artifacts) != 2 {
		t.Fatalf("Artifacts = %+v, want the 2 the page cap allowed", r.Artifacts)
	}
	for i, a := range r.Artifacts {
		if !slices.Contains(names, a.ID) {
			t.Errorf("Artifacts[%d].ID = %q, want one of the written snapshots %q", i, a.ID, names)
		}
	}
	if reads != 2 {
		t.Errorf("the walk read %d entries, want exactly the 2 the page cap allowed", reads)
	}
	if w := strings.Join(r.Warnings, "\n"); !strings.Contains(w, "page cap") {
		t.Errorf("warnings do not name the page cap: %q", w)
	}
}

// The byte cap bounds the artifact bytes the walk accounts for: it stops
// before the entry that would cross the cap and says the catalog is not a
// complete total. The directory streams its entries in a filesystem-defined
// order, so which snapshot stays below the cap is not the contract: the test
// asserts the stop and that the surviving artifact is one it wrote.
func TestLocalStopsAtTheByteCap(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		etcdsnap.ScheduledName("cp-1", "20260704-010000"),
		etcdsnap.ScheduledName("cp-1", "20260704-020000"),
	}
	for i, n := range names {
		writeSnapshot(t, dir, n, 100, snapTime(i+1))
	}

	r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1", MaxBytes: 150})

	if r.Complete {
		t.Error("Complete = true, want false: the byte cap stopped the walk")
	}
	if len(r.Artifacts) != 1 {
		t.Fatalf("Artifacts = %+v, want the 1 the byte cap allowed", r.Artifacts)
	}
	if a := r.Artifacts[0]; !slices.Contains(names, a.ID) || a.Size != 100 {
		t.Errorf("Artifacts[0] = %+v, want one of %q with its 100 bytes accounted", a, names)
	}
	if w := strings.Join(r.Warnings, "\n"); !strings.Contains(w, "byte cap") {
		t.Errorf("warnings do not name the byte cap: %q", w)
	}
}

// An entry the walk cannot read becomes a safe diagnostic that names the
// snapshot and what is unknown about it — never the raw storage error — and
// the catalog says it is not a complete total. An entry that vanished between
// the listing and the read is not a diagnostic at all: it is no longer an
// artifact. A directory the walk cannot even open yields no artifacts and the
// same safe shape.
func TestLocalUnreadableEntriesBecomeSafeDiagnostics(t *testing.T) {
	dir := t.TempDir()
	s1 := etcdsnap.ScheduledName("cp-1", "20260704-010000")
	s2 := etcdsnap.ScheduledName("cp-1", "20260704-020000")
	writeSnapshot(t, dir, s1, 10, snapTime(1))
	writeSnapshot(t, dir, s2, 10, snapTime(2))

	t.Run("unreadable entry", func(t *testing.T) {
		old := localEntryInfo
		t.Cleanup(func() { localEntryInfo = old })
		localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
			if name == s2 {
				return 0, time.Time{}, errors.New("open: permission denied 0xdeadbeef")
			}
			return old(ctx, dir, name)
		}
		r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
		if r.Complete {
			t.Error("Complete = true, want false: an entry could not be described")
		}
		if len(r.Artifacts) != 1 || r.Artifacts[0].ID != s1 {
			t.Errorf("Artifacts = %+v, want only the readable %s", r.Artifacts, s1)
		}
		w := strings.Join(r.Warnings, "\n")
		if !strings.Contains(w, s2) {
			t.Errorf("warnings do not name the unreadable snapshot: %q", w)
		}
		if strings.Contains(w, "0xdeadbeef") {
			t.Errorf("the raw storage error escaped into the warnings: %q", w)
		}
	})
	t.Run("vanished entry", func(t *testing.T) {
		old := localEntryInfo
		t.Cleanup(func() { localEntryInfo = old })
		localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
			if name == s2 {
				return 0, time.Time{}, fs.ErrNotExist
			}
			return old(ctx, dir, name)
		}
		r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
		if !r.Complete || len(r.Warnings) != 0 || len(r.Artifacts) != 1 || r.Artifacts[0].ID != s1 {
			t.Errorf("vanished-entry walk = %+v, want %s alone, complete and silent", r, s1)
		}
	})
	t.Run("unreadable directory", func(t *testing.T) {
		old := localOpen
		t.Cleanup(func() { localOpen = old })
		localOpen = func(string) (*os.File, error) {
			return nil, errors.New("open /var/backups/etcd: permission denied 0xfeedface")
		}
		r := Local(context.Background(), LocalOptions{Dir: dir, Node: "cp-1"})
		if r.Complete || len(r.Artifacts) != 0 {
			t.Errorf("unreadable-directory walk = %+v, want no artifacts and no complete total", r)
		}
		w := strings.Join(r.Warnings, "\n")
		if !strings.Contains(w, "cannot be read") {
			t.Errorf("warnings do not say the directory cannot be read: %q", w)
		}
		if strings.Contains(w, "0xfeedface") {
			t.Errorf("the raw storage error escaped into the warnings: %q", w)
		}
	})
}

// The catalog's location is node-scoped: the result names the node whose
// store the walk read — the options' Node when the caller scopes it (the
// earlier tests assert that), else this host's short name. A host whose name
// cannot be determined warns safely and reports no node.
func TestLocalScopesTheCatalogToTheNode(t *testing.T) {
	dir := t.TempDir()
	s := etcdsnap.ScheduledName("cp-1", "20260704-010000")
	writeSnapshot(t, dir, s, 10, snapTime(1))

	t.Run("default from the host", func(t *testing.T) {
		old := localHostname
		t.Cleanup(func() { localHostname = old })
		localHostname = func() (string, error) { return "cp-9", nil }
		r := Local(context.Background(), LocalOptions{Dir: dir})
		if r.Node != "cp-9" {
			t.Errorf("Node = %q, want the host's short name", r.Node)
		}
	})
	t.Run("host name unknown", func(t *testing.T) {
		old := localHostname
		t.Cleanup(func() { localHostname = old })
		localHostname = func() (string, error) { return "", errors.New("hostname: nope 0xcafe") }
		r := Local(context.Background(), LocalOptions{Dir: dir})
		if r.Node != "" {
			t.Errorf("Node = %q, want none: the host name could not be determined", r.Node)
		}
		w := strings.Join(r.Warnings, "\n")
		if !strings.Contains(w, "node") || strings.Contains(w, "0xcafe") {
			t.Errorf("warnings = %q, want a safe warning about the node name and no raw error", w)
		}
	})
}
