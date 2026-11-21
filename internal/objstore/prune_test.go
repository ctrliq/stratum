package objstore

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// a / delimiter leaves out keys nested below the requested prefix, as S3 does.
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

func stubDatedBucket(t *testing.T, objs map[string]string, refuse ...string) (*Target, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var deleted []string
	keys := slices.Sorted(maps.Keys(objs))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>`)
			for _, k := range keys {
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>1</Size><LastModified>%s</LastModified></Contents>`, k, objs[k])
			}
			b.WriteString(`</ListBucketResult>`)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(b.String()))
		case http.MethodDelete:
			k := strings.TrimPrefix(r.URL.Path, "/b/")
			mu.Lock()
			deleted = append(deleted, k)
			mu.Unlock()
			if slices.Contains(refuse, k) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
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
		return slices.Sorted(slices.Values(deleted))
	}
}

// the names suggest the opposite of LastModified: a clock stepped back.
func TestPruneKeepsTheNewestByLastModified(t *testing.T) {
	tgt, deleted := stubDatedBucket(t, map[string]string{
		"mine-2027-a.db": "2026-10-20T00:00:00.000Z",
		"mine-2027-b.db": "2026-10-20T12:00:00.000Z",
		"mine-2026-c.db": "2026-10-21T00:00:00.000Z",
	})
	owned := func(name string) bool { return strings.HasPrefix(name, "mine-") }
	if err := tgt.Prune(context.Background(), owned, 2); err != nil {
		t.Fatal(err)
	}
	if got := deleted(); !slices.Equal(got, []string{"mine-2027-a.db"}) {
		t.Errorf("deleted %q, want only the object uploaded first", got)
	}
}

func TestPruneTriesEveryExpiredObject(t *testing.T) {
	tgt, deleted := stubDatedBucket(t, map[string]string{
		"mine-1.db": "2026-10-20T01:00:00.000Z",
		"mine-2.db": "2026-10-20T02:00:00.000Z",
		"mine-3.db": "2026-10-20T03:00:00.000Z",
		"mine-4.db": "2026-10-20T04:00:00.000Z",
	}, "mine-1.db")
	owned := func(name string) bool { return strings.HasPrefix(name, "mine-") }
	err := tgt.Prune(context.Background(), owned, 1)
	if err == nil || !strings.Contains(err.Error(), "mine-1.db") {
		t.Fatalf("Prune = %v, want the refused delete of mine-1.db", err)
	}
	if got := deleted(); !slices.Equal(got, []string{"mine-1.db", "mine-2.db", "mine-3.db"}) {
		t.Errorf("deleted %q, want every expired object tried", got)
	}
}
