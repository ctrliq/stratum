package mcp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// restoreCopiesHost is a Stratum node whose data dir has two restore copies
// beside it. The data lock lives beside DataDir, so both are redirected.
func restoreCopiesHost(t *testing.T) (pre, failed string) {
	t.Helper()
	fakeStratumHost(t)
	oldData, oldEtcd := version.DataDir, version.EtcdDataDir
	t.Cleanup(func() { version.DataDir, version.EtcdDataDir = oldData, oldEtcd })
	version.DataDir = filepath.Join(t.TempDir(), "stratum")
	version.EtcdDataDir = filepath.Join(version.DataDir, "etcd")
	pre = version.EtcdDataDir + ".pre-restore-20260926-120000"
	failed = version.EtcdDataDir + ".failed-restore-20260926-120100"
	for _, d := range []string{version.EtcdDataDir, pre, failed} {
		if err := os.MkdirAll(filepath.Join(d, "member"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "member", "db"), make([]byte, 1024), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return pre, failed
}

func listRestoreCopies(t *testing.T) SnapshotRestoreCopiesOut {
	t.Helper()
	_, out, err := SnapshotRestoreCopiesHandler()(t.Context(), nil, SnapshotRestoreCopiesIn{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSnapshotRestoreCopiesListsWithoutRemoving: the read tool
// reports each copy with a modified time that can be passed straight back,
// and removes nothing.
func TestSnapshotRestoreCopiesListsWithoutRemoving(t *testing.T) {
	pre, failed := restoreCopiesHost(t)
	out := listRestoreCopies(t)
	if len(out.Copies) != 2 || out.PageBytes != 2048 || out.Truncated {
		t.Fatalf("copies = %+v, page %d bytes, truncated %v; want the two copies, 2048 bytes, all of them",
			out.Copies, out.PageBytes, out.Truncated)
	}
	for _, c := range out.Copies {
		if _, err := time.Parse(time.RFC3339Nano, c.Modified); err != nil {
			t.Errorf("copy %s: modified %q does not parse: %v", c.Path, c.Modified, err)
		}
		if c.Size != "1.0 KiB" {
			t.Errorf("copy %s: size %q, want 1.0 KiB", c.Path, c.Size)
		}
	}
	if !strings.Contains(out.Note, "nothing was removed") {
		t.Errorf("note = %q", out.Note)
	}
	for _, p := range []string{pre, failed} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("listing removed %s: %v", p, err)
		}
	}
}

// TestSnapshotPruneRestoresRemovesOnlyWhatItIsNamed: the
// destructive tool removes only the copies it is passed, and only while they
// are unchanged since the listing; the rest are reported as left.
func TestSnapshotPruneRestoresRemovesOnlyWhatItIsNamed(t *testing.T) {
	pre, failed := restoreCopiesHost(t)
	listed := listRestoreCopies(t)
	var refs []RestoreCopyRef
	for _, c := range listed.Copies {
		refs = append(refs, RestoreCopyRef{Path: c.Path, Modified: c.Modified})
	}
	// The failed-restore copy changes after the listing; the pre-restore
	// copy does not.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(failed, later, later); err != nil {
		t.Fatal(err)
	}

	_, out, err := SnapshotPruneRestoresHandler()(t.Context(), nil, SnapshotPruneRestoresIn{Copies: refs})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done || !slices.Equal(out.Removed, []string{pre}) || !slices.Equal(out.Left, []string{failed}) {
		t.Errorf("out = %+v; want %s removed and %s left", out, pre, failed)
	}
	if _, err := os.Stat(pre); !os.IsNotExist(err) {
		t.Errorf("%s was not removed: %v", pre, err)
	}
	for _, p := range []string{failed, version.EtcdDataDir} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

// TestSnapshotPruneRestoresRefusesWhatItCannotTrust: no copies means no
// default, a modified time that is not the listing's is refused before
// anything is removed, and so is a machine that is not a Stratum node.
func TestSnapshotPruneRestoresRefusesWhatItCannotTrust(t *testing.T) {
	pre, _ := restoreCopiesHost(t)
	for _, tc := range []struct {
		name, want string
		in         SnapshotPruneRestoresIn
	}{
		{"no copies", "copies is required", SnapshotPruneRestoresIn{}},
		{"a modified time that is not the listing's", "not the time",
			SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{{Path: pre, Modified: "yesterday"}}}},
		{"an empty path", "is not a restore copy",
			SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{{Path: "", Modified: time.Now().Format(time.RFC3339Nano)}}}},
		{"a path that is not a restore copy", "is not a restore copy",
			SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{{Path: version.EtcdDataDir, Modified: time.Now().Format(time.RFC3339Nano)}}}},
		{"a restore copy name elsewhere", "is not a restore copy",
			SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{{Path: filepath.Join(t.TempDir(), filepath.Base(pre)), Modified: time.Now().Format(time.RFC3339Nano)}}}},
		{"the same copy named twice", "named more than once",
			SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{
				{Path: pre, Modified: time.Now().Format(time.RFC3339Nano)}, {Path: pre, Modified: "2000-01-01T00:00:00Z"}}}},
		{"one bad entry among good ones", "is not a restore copy",
			SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{
				{Path: pre, Modified: time.Now().Format(time.RFC3339Nano)}, {Path: "", Modified: time.Now().Format(time.RFC3339Nano)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := SnapshotPruneRestoresHandler()(t.Context(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %q", err, tc.want)
			}
			if _, err := os.Stat(pre); err != nil {
				t.Errorf("%s was removed: %v", pre, err)
			}
		})
	}
	t.Run("off a Stratum node", func(t *testing.T) {
		listed := listRestoreCopies(t)
		notStratumHost(t)
		refs := []RestoreCopyRef{{Path: listed.Copies[0].Path, Modified: listed.Copies[0].Modified}}
		if _, _, err := SnapshotPruneRestoresHandler()(t.Context(), nil, SnapshotPruneRestoresIn{Copies: refs}); err == nil {
			t.Fatal("removed restore copies on a machine that is not a Stratum node")
		}
		if _, err := os.Stat(listed.Copies[0].Path); err != nil {
			t.Errorf("%s was removed off a node: %v", listed.Copies[0].Path, err)
		}
	})
}

// TestSnapshotRestoreCopiesPages: the listing is bounded like
// every MCP list. A page says it is truncated and carries a token; the next
// page picks up where it left off; a token from before the copies changed is
// refused rather than silently skipping or repeating one.
func TestSnapshotRestoreCopiesPages(t *testing.T) {
	pre, failed := restoreCopiesHost(t)
	call := func(in SnapshotRestoreCopiesIn) (SnapshotRestoreCopiesOut, error) {
		_, out, err := SnapshotRestoreCopiesHandler()(t.Context(), nil, in)
		return out, err
	}
	first, err := call(SnapshotRestoreCopiesIn{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Copies) != 1 || !first.Truncated || first.Continue == "" || first.Count != 1 || first.Limit != 1 {
		t.Fatalf("first page = %+v; want one copy, truncated, with a continue token", first)
	}
	second, err := call(SnapshotRestoreCopiesIn{Limit: 1, Continue: first.Continue})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Copies) != 1 || second.Truncated || second.Continue != "" {
		t.Fatalf("second page = %+v; want the last copy, not truncated", second)
	}
	got := []string{first.Copies[0].Path, second.Copies[0].Path}
	slices.Sort(got)
	want := []string{failed, pre}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the two pages returned %q, want each copy once: %q", got, want)
	}

	if err := os.MkdirAll(version.EtcdDataDir+".pre-restore-20260926-130000", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := call(SnapshotRestoreCopiesIn{Limit: 1, Continue: first.Continue}); err == nil ||
		!strings.Contains(err.Error(), "stale continue_token") {
		t.Errorf("a token from before a copy was added = %v, want it refused as stale", err)
	}
}
