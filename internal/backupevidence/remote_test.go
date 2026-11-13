package backupevidence

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/objstore"
)

// remoteTime is a fixed UTC time the stub remote store's LastModified values
// parse to, so the creation times the remote catalog reports assert exactly.
func remoteTime(hour, min int) time.Time {
	return time.Date(2026, 7, 4, hour, min, 0, 0, time.UTC)
}

// remoteObj is one object the stub remote store serves: its base name is the
// stable backup ID, its modified stamp the creation time the catalog reports.
type remoteObj struct {
	name     string
	size     int64
	modified time.Time
}

// ro builds a stub remote object with the given creation time.
func ro(name string, size int64, at time.Time) remoteObj {
	return remoteObj{name: name, size: size, modified: at}
}

// remoteXML renders one ListObjectsV2 response over the objects: truncated
// with next as the continuation token when another page follows, complete
// otherwise.
func remoteXML(next string, objs ...remoteObj) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name>`)
	if next != "" {
		b.WriteString(`<IsTruncated>true</IsTruncated><NextContinuationToken>` + next + `</NextContinuationToken>`)
	} else {
		b.WriteString(`<IsTruncated>false</IsTruncated>`)
	}
	for _, o := range objs {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
			o.name, o.size, o.modified.Format("2006-01-02T15:04:05.000Z"))
	}
	b.WriteString(`</ListBucketResult>`)
	return b.String()
}

// stubRemote serves ListObjectsV2 a page at a time like S3: the request's
// continuation token picks the page (no token is the first), and a page with
// a successor is truncated carrying the successor's token.
func stubRemote(t *testing.T, pages ...[]remoteObj) *objstore.Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		tok := r.URL.Query().Get("continuation-token")
		i := 0
		if tok != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(tok, "page"))
			if err != nil || n < 1 {
				http.Error(w, "unknown continuation token", http.StatusBadRequest)
				return
			}
			i = n
		}
		if i >= len(pages) {
			http.Error(w, "unknown page", http.StatusBadRequest)
			return
		}
		next := ""
		if i+1 < len(pages) {
			next = "page" + strconv.Itoa(i+1)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(remoteXML(next, pages[i]...)))
	}))
	t.Cleanup(srv.Close)
	return &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

// The remote catalog lists every remote copy the store holds with its stable
// name-derived ID, its remote location, its creation time (the object's
// LastModified), its size and an unknown digest — a listing exposes no
// digest — and is complete, in a stable order, when the whole store was
// listed within the bounds.
func TestRemoteCatalogsRemoteCopies(t *testing.T) {
	s1 := "etcd-snapshot-cp-1-20260704-010000.db"
	s2 := "pre-upgrade-20260704-020000.db"
	tgt := stubRemote(t, []remoteObj{ro(s2, 16, remoteTime(0, 2)), ro(s1, 10, remoteTime(0, 1))})

	r := Remote(context.Background(), RemoteOptions{Target: tgt})

	if !r.Complete {
		t.Error("Complete = false, want true: the whole store was listed within the bounds")
	}
	if len(r.Warnings) != 0 {
		t.Errorf("a healthy listing warned: %q", r.Warnings)
	}
	want := []Artifact{
		{ID: s1, Location: "remote", Created: remoteTime(0, 1), Size: 10, Digest: DigestUnknown},
		{ID: s2, Location: "remote", Created: remoteTime(0, 2), Size: 16, Digest: DigestUnknown},
	}
	if len(r.Copies) != len(want) {
		t.Fatalf("Copies = %+v, want %d entries: %q and %q", r.Copies, len(want), s1, s2)
	}
	for i, w := range want {
		if got := r.Copies[i]; got.ID != w.ID || got.Location != w.Location || got.Size != w.Size || !got.Created.Equal(w.Created) || got.Digest != w.Digest {
			t.Errorf("Copies[%d] = %+v, want %+v", i, got, w)
		}
	}
}

// A configured store holding nothing lists zero remote copies exactly, and a
// node with no remote target configured holds zero remote copies exactly too:
// neither is an unknown.
func TestRemoteEmptyStoreIsACompleteExactZero(t *testing.T) {
	t.Run("empty bucket", func(t *testing.T) {
		tgt := stubRemote(t, []remoteObj{})
		r := Remote(context.Background(), RemoteOptions{Target: tgt})
		if !r.Complete || len(r.Copies) != 0 || len(r.Warnings) != 0 {
			t.Errorf("empty store = %+v, want a complete exact zero with no warnings", r)
		}
	})
	t.Run("no target configured", func(t *testing.T) {
		r := Remote(context.Background(), RemoteOptions{})
		if !r.Complete || len(r.Copies) != 0 || len(r.Warnings) != 0 {
			t.Errorf("unconfigured store = %+v, want a complete exact zero: nothing outside the node holds a copy", r)
		}
	})
}

// A remote copy created before the retention cutoff — one predating the
// retention window or surviving a failed prune — is flagged with its creation
// time while staying listed and counted with every other copy.
func TestRemoteFlagsOldCopiesPastRetention(t *testing.T) {
	cutoff := remoteTime(0, 2)
	oldID := "etcd-snapshot-cp-1-20260704-010000.db"
	newID := "etcd-snapshot-cp-1-20260704-030000.db"
	tgt := stubRemote(t, []remoteObj{
		ro(oldID, 10, remoteTime(0, 1)), // created before the cutoff
		ro(newID, 10, remoteTime(0, 4)), // created after it
	})

	r := Remote(context.Background(), RemoteOptions{Target: tgt, RetentionCutoff: cutoff})

	if !r.Complete || len(r.Copies) != 2 {
		t.Fatalf("catalog = %+v, want both copies listed and the catalog complete", r)
	}
	if len(r.Old) != 1 || r.Old[0].ID != oldID || !r.Old[0].Created.Equal(remoteTime(0, 1)) {
		t.Errorf("Old = %+v, want %s with its creation time", r.Old, oldID)
	}
}

// A store that never answers is bounded, not waited out: the request context
// — which the catalog propagates to the store — ends the underlying listing,
// the store sees its request context end, and the catalog returns within its
// time bound with unknown completeness and a safe warning that names the
// condition — never the store's address or any raw error.
func TestRemoteHungListingReturnsBounded(t *testing.T) {
	sawCancel := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // parked: the store never answers
		once.Do(func() { close(sawCancel) })
	}))
	t.Cleanup(srv.Close)
	tgt := &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	t.Cleanup(cancel)
	done := make(chan RemoteResult, 1)
	go func() { done <- Remote(ctx, RemoteOptions{Target: tgt}) }()

	var r RemoteResult
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the remote catalog did not return within its time bound")
	}
	if r.Complete {
		t.Error("Complete = true, want false: a hung listing is not a total")
	}
	if len(r.Copies) != 0 {
		t.Errorf("Copies = %+v, want none: nothing was listed before the hang", r.Copies)
	}
	w := strings.Join(r.Warnings, "\n")
	if !strings.Contains(w, "time bound") {
		t.Errorf("warnings do not say the listing exceeded its time bound: %q", w)
	}
	if strings.Contains(w, srv.URL) {
		t.Errorf("the store's address escaped into the warnings: %q", w)
	}
	select {
	case <-sawCancel:
	case <-time.After(5 * time.Second):
		t.Error("the store never saw its request context end: the caller's context did not reach the S3 call")
	}
}

// A store that refuses the listing with 403 leaves the remote catalog
// unknown, never zero: the copies already listed stand, completeness is
// false, and the warning names the refusal without the store's raw error
// text.
func TestRemoteDeniedListingIsUnknownNotZero(t *testing.T) {
	refusing := func(t *testing.T) *objstore.Target {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
		}))
		t.Cleanup(srv.Close)
		return &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
	}

	t.Run("refused outright", func(t *testing.T) {
		r := Remote(context.Background(), RemoteOptions{Target: refusing(t)})
		if r.Complete {
			t.Error("Complete = true, want false: a refused listing is not a total")
		}
		if len(r.Copies) != 0 {
			t.Errorf("Copies = %+v, want none: nothing was listed before the refusal", r.Copies)
		}
		w := strings.Join(r.Warnings, "\n")
		if !strings.Contains(w, "refused") {
			t.Errorf("warnings do not say the store refused the listing: %q", w)
		}
		if strings.Contains(w, "AccessDenied") {
			t.Errorf("the store's raw error escaped into the warnings: %q", w)
		}
	})
	t.Run("refused mid-listing", func(t *testing.T) {
		s1 := "etcd-snapshot-cp-1-20260704-010000.db"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("continuation-token") != "" {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(remoteXML("page1", ro(s1, 10, remoteTime(0, 1)))))
		}))
		t.Cleanup(srv.Close)
		tgt := &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}

		r := Remote(context.Background(), RemoteOptions{Target: tgt})

		if r.Complete {
			t.Error("Complete = true, want false: a partial listing is not a total")
		}
		if len(r.Copies) != 1 || r.Copies[0].ID != s1 {
			t.Errorf("Copies = %+v, want the first page's copy as a floor", r.Copies)
		}
		w := strings.Join(r.Warnings, "\n")
		if !strings.Contains(w, "refused") || strings.Contains(w, "AccessDenied") {
			t.Errorf("warnings = %q, want the refusal named with no raw error text", w)
		}
	})
}

// The union of the local and remote catalogs counts each stable backup ID
// once: the same snapshot saved locally and uploaded off-box is one backup in
// two locations, not two. Both catalogs complete, the union's entries are the
// exact total.
func TestRemoteUnionDeduplicatesByStableBackupID(t *testing.T) {
	s1 := "etcd-snapshot-cp-1-20260704-010000.db" // local only
	s2 := "etcd-snapshot-cp-1-20260704-020000.db" // local and remote
	s3 := "etcd-snapshot-cp-1-20260704-030000.db" // remote only
	local := LocalResult{
		Node:     "cp-1",
		Complete: true,
		Artifacts: []Artifact{
			{ID: s2, Location: "local", Created: remoteTime(0, 2), Size: 12, Digest: DigestUnknown},
			{ID: s1, Location: "local", Created: remoteTime(0, 1), Size: 10, Digest: DigestUnknown},
		},
	}
	remote := RemoteResult{
		Complete: true,
		Copies: []Artifact{
			{ID: s2, Location: "remote", Created: remoteTime(0, 2), Size: 12, Digest: DigestUnknown},
			{ID: s3, Location: "remote", Created: remoteTime(0, 3), Size: 14, Digest: DigestUnknown},
		},
	}

	c := Union(local, remote)

	if !c.Complete {
		t.Error("Complete = false, want true: both catalogs were complete, so the union's total is exact")
	}
	if len(c.Entries) != 3 {
		t.Fatalf("Entries = %+v, want 3: %q and %q share one ID and count once", c.Entries, s1, s3)
	}
	if got := []string{c.Entries[0].ID, c.Entries[1].ID, c.Entries[2].ID}; !slices.Equal(got, []string{s1, s2, s3}) {
		t.Errorf("Entries = %q, want them sorted by ID", got)
	}
	for _, e := range c.Entries {
		switch e.ID {
		case s1:
			if e.Local == nil || e.Remote != nil {
				t.Errorf("entry %s = %+v, want the local artifact alone", s1, e)
			}
		case s2:
			if e.Local == nil || e.Remote == nil {
				t.Errorf("entry %s = %+v, want both locations: the same snapshot in two places counts once", s2, e)
			}
		case s3:
			if e.Local != nil || e.Remote == nil {
				t.Errorf("entry %s = %+v, want the remote copy alone", s3, e)
			}
		}
	}
	if c.Node != "cp-1" {
		t.Errorf("Node = %q, want the local catalog's node scoping the union", c.Node)
	}
}

// A partial listing in the union leaves the deduplicated total unknown —
// however many entries the union counted, they are a floor, never the total.
// Even a partial listing that described nothing is an unknown, not a zero,
// and a partial local half is just as unknown-making as a partial remote one.
func TestRemoteUnionNeverYieldsAnExactTotalFromAPartialListing(t *testing.T) {
	s1 := "etcd-snapshot-cp-1-20260704-010000.db"
	local := LocalResult{
		Node:      "cp-1",
		Complete:  true,
		Artifacts: []Artifact{{ID: s1, Location: "local", Created: remoteTime(0, 1), Size: 10, Digest: DigestUnknown}},
	}

	t.Run("partial remote with copies listed", func(t *testing.T) {
		s3 := "etcd-snapshot-cp-1-20260704-030000.db"
		remote := RemoteResult{
			Copies:   []Artifact{{ID: s3, Location: "remote", Created: remoteTime(0, 3), Size: 14, Digest: DigestUnknown}},
			Warnings: []string{"WARN: the remote store refused the listing; the remote catalog is unknown and not a complete total"},
		}
		c := Union(local, remote)
		if c.Complete {
			t.Error("Complete = true, want false: a partial listing never yields an exact total")
		}
		if len(c.Entries) != 2 {
			t.Errorf("Entries = %+v, want the floor: the local snapshot and the one copy the partial listing described", c.Entries)
		}
		if w := strings.Join(c.Warnings, "\n"); !strings.Contains(w, "unknown") {
			t.Errorf("warnings do not say the union's total is unknown: %q", w)
		}
	})
	t.Run("partial remote with nothing listed", func(t *testing.T) {
		remote := RemoteResult{
			Warnings: []string{"WARN: the remote store refused the listing; the remote catalog is unknown and not a complete total"},
		}
		c := Union(local, remote)
		if c.Complete {
			t.Error("Complete = true, want false: a refused listing is an unknown remote half, never a zero")
		}
		if len(c.Entries) != 1 {
			t.Errorf("Entries = %+v, want the local snapshot alone — a floor, not the total", c.Entries)
		}
		if w := strings.Join(c.Warnings, "\n"); !strings.Contains(w, "unknown") {
			t.Errorf("warnings do not say the union's total is unknown: %q", w)
		}
	})
	t.Run("partial local half", func(t *testing.T) {
		partialLocal := LocalResult{Node: "cp-1", Warnings: []string{"WARN: the local snapshot walk stopped early: the page cap of 2 entries was reached; the local catalog is not a complete total"}}
		remote := RemoteResult{Complete: true, Copies: []Artifact{{ID: s1, Location: "remote", Created: remoteTime(0, 1), Size: 10, Digest: DigestUnknown}}}
		c := Union(partialLocal, remote)
		if c.Complete {
			t.Error("Complete = true, want false: a partial local half leaves the union's total unknown too")
		}
		if w := strings.Join(c.Warnings, "\n"); !strings.Contains(w, "unknown") {
			t.Errorf("warnings do not say the union's total is unknown: %q", w)
		}
	})
}

// Old remote copies are counted once in the deduplicated union, flagged on
// their entry, and surfaced to retention diagnostics with their creation
// times — whether the local half also holds them or not.
func TestRemoteUnionSurfacesOldCopiesToRetention(t *testing.T) {
	oldID := "etcd-snapshot-cp-1-20260704-010000.db" // local and an old remote copy
	newID := "etcd-snapshot-cp-1-20260704-030000.db" // a remote copy inside the window
	created := remoteTime(0, 1)
	local := LocalResult{
		Node:      "cp-1",
		Complete:  true,
		Artifacts: []Artifact{{ID: oldID, Location: "local", Created: created, Size: 10, Digest: DigestUnknown}},
	}
	remote := RemoteResult{
		Complete: true,
		Copies: []Artifact{
			{ID: newID, Location: "remote", Created: remoteTime(0, 3), Size: 10, Digest: DigestUnknown},
			{ID: oldID, Location: "remote", Created: created, Size: 10, Digest: DigestUnknown},
		},
		Old: []Artifact{{ID: oldID, Location: "remote", Created: created, Size: 10, Digest: DigestUnknown}},
	}

	c := Union(local, remote)

	if len(c.Entries) != 2 {
		t.Fatalf("Entries = %+v, want 2: the old copy counts once beside the new one", c.Entries)
	}
	if !c.Entries[0].OldRemote || c.Entries[0].Local == nil {
		t.Errorf("entry for %s = %+v, want the local artifact and the old-remote flag", oldID, c.Entries[0])
	}
	if c.Entries[1].OldRemote {
		t.Errorf("entry for %s = %+v, want no old-remote flag: it is inside the retention window", newID, c.Entries[1])
	}
	if len(c.Retention) != 1 {
		t.Fatalf("Retention = %q, want the one old copy's diagnostic", c.Retention)
	}
	want := fmt.Sprintf("%s was created %s", oldID, created.UTC().Format(time.RFC3339))
	if !strings.Contains(c.Retention[0], want) {
		t.Errorf("Retention = %q, want it to name %s with its creation time", c.Retention[0], want)
	}
}
