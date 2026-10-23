package objstore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// stubBucket lists keys for a ListObjectsV2 and records every DELETE. Like S3,
// a "/" delimiter leaves out keys nested below the requested prefix.
func stubBucket(t *testing.T, keys ...string) (*Target, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>`)
			q := r.URL.Query()
			for _, k := range keys {
				if q.Get("delimiter") == "/" && strings.Contains(strings.TrimPrefix(k, q.Get("prefix")), "/") {
					continue
				}
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-10-20T00:00:00.000Z</LastModified></Contents>`, k)
			}
			b.WriteString(`</ListBucketResult>`)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(b.String()))
		case http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/b/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	tgt := &Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
	return tgt, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(deleted)
	}
}

// TestPruneDeletesOnlyTheOldestOwnedObjects: Prune keeps the newest `keep` of
// the objects owned accepts and never touches the rest of the bucket.
func TestPruneDeletesOnlyTheOldestOwnedObjects(t *testing.T) {
	tgt, deleted := stubBucket(t, "mine-1.db", "mine-2.db", "mine-3.db", "theirs-0.db")
	owned := func(name string) bool { return strings.HasPrefix(name, "mine-") }

	if err := tgt.Prune(context.Background(), owned, 2); err != nil {
		t.Fatal(err)
	}
	if got := deleted(); !slices.Equal(got, []string{"mine-1.db"}) {
		t.Errorf("deleted %q, want only the oldest owned object", got)
	}

	if err := tgt.Prune(context.Background(), owned, 0); err != nil {
		t.Fatal(err)
	}
	if got := deleted(); len(got) != 1 {
		t.Errorf("keep=0 deleted %q; it must be a no-op, not prune-all", got[1:])
	}
}

// TestPruneIgnoresNestedKeys: objects under a sub-prefix (another cluster's
// prefix, say) are not this target's, so they neither count toward keep nor
// get their base name deleted at the top level.
func TestPruneIgnoresNestedKeys(t *testing.T) {
	tgt, deleted := stubBucket(t, "cp1-1.db", "cp1-2.db", "cp1-3.db", "other/cp1-4.db", "other/cp1-5.db")
	owned := func(name string) bool { return strings.HasPrefix(name, "cp1-") }

	if err := tgt.Prune(context.Background(), owned, 2); err != nil {
		t.Fatal(err)
	}
	if got := deleted(); !slices.Equal(got, []string{"cp1-1.db"}) {
		t.Errorf("deleted %q, want only the oldest top-level object", got)
	}
}
