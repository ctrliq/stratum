package objstore

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
)

type stubListObj struct {
	key      string
	size     int64
	modified string
}

// one value in the stub XML and the assertions, so they cannot drift.
var listObjTime = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

func listObj(key string, size int64) stubListObj {
	return stubListObj{key: key, size: size, modified: "2026-11-12T00:00:00.000Z"}
}

func listObjs(keys ...string) []stubListObj {
	objs := make([]stubListObj, len(keys))
	for i, k := range keys {
		objs[i] = listObj(k, 1)
	}
	return objs
}

func listPageXML(next string, objs ...stubListObj) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name>`)
	if next != "" {
		b.WriteString(`<IsTruncated>true</IsTruncated><NextContinuationToken>` + next + `</NextContinuationToken>`)
	} else {
		b.WriteString(`<IsTruncated>false</IsTruncated>`)
	}
	for _, o := range objs {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`, o.key, o.size, o.modified)
	}
	b.WriteString(`</ListBucketResult>`)
	return b.String()
}

const deniedBody = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`

func denyList(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(deniedBody))
}

func targetAt(url string) *Target {
	return &Target{Endpoint: url, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

func stubListing(t *testing.T, pages ...[]stubListObj) (*Target, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		tok := r.URL.Query().Get("continuation-token")
		mu.Lock()
		tokens = append(tokens, tok)
		mu.Unlock()
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
		_, _ = w.Write([]byte(listPageXML(next, pages[i]...)))
	}))
	t.Cleanup(srv.Close)
	return targetAt(srv.URL), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(tokens)
	}
}

func names(objs []Object) []string {
	ns := make([]string, len(objs))
	for i, o := range objs {
		ns[i] = o.Name
	}
	return ns
}

func TestListBoundedFollowsTruncatedPagination(t *testing.T) {
	tgt, tokens := stubListing(t,
		[]stubListObj{listObj("b.db", 2), listObj("a.db", 1)},
		[]stubListObj{listObj("c.db", 3)},
	)

	r := tgt.ListBounded(context.Background(), ListOptions{})

	if r.End != ListEndedComplete {
		t.Errorf("End = %q, want complete: every truncated page was followed to the end", r.End)
	}
	if len(r.Objects) != 3 {
		t.Fatalf("Objects = %+v, want the 3 objects the two pages held", r.Objects)
	}
	wantSizes := map[string]int64{"a.db": 1, "b.db": 2, "c.db": 3}
	for _, o := range r.Objects {
		if o.Size != wantSizes[o.Name] || !o.Modified.Equal(listObjTime) {
			t.Errorf("object %s = size %d modified %v, want size %d and the page's modification time", o.Name, o.Size, o.Modified, wantSizes[o.Name])
		}
	}
	if got := names(r.Objects); !slices.Equal(got, []string{"a.db", "b.db", "c.db"}) {
		t.Errorf("Objects = %q, want the names sorted", got)
	}
	if got := tokens(); !slices.Equal(got, []string{"", "page1"}) {
		t.Errorf("continuation tokens = %q, want the untokened first page then the truncated page's token", got)
	}
}

func TestListBoundedDeduplicatesKeysAcrossPages(t *testing.T) {
	tgt, _ := stubListing(t,
		[]stubListObj{listObj("a.db", 1), listObj("b.db", 1)},
		[]stubListObj{listObj("b.db", 1), listObj("c.db", 1)},
	)

	r := tgt.ListBounded(context.Background(), ListOptions{})

	if r.End != ListEndedComplete {
		t.Errorf("End = %q, want complete: the deduplicated listing still reached the end", r.End)
	}
	if got := names(r.Objects); !slices.Equal(got, []string{"a.db", "b.db", "c.db"}) {
		t.Errorf("Objects = %q, want each key once: a duplicate across pages is the same object", got)
	}
}

func TestListBoundedSortsTheFloorItReturnsWhenAListingFailsMidway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("continuation-token") != "" {
			denyList(w)
			return
		}
		// the first page's objects are unsorted, so only the listing's sort can sort them
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(listPageXML("page1", listObj("z.db", 1), listObj("a.db", 1))))
	}))
	t.Cleanup(srv.Close)

	r := targetAt(srv.URL).ListBounded(context.Background(), ListOptions{})

	if r.End != ListEndedDenied {
		t.Errorf("End = %q, want denied: the store refused the page after the first", r.End)
	}
	if got := names(r.Objects); !slices.Equal(got, []string{"a.db", "z.db"}) {
		t.Errorf("Objects = %q, want the first page's objects sorted by name: a floor keeps the sorted contract every other end keeps", got)
	}
}

func TestListBoundedEmptyBucketIsAnExactZero(t *testing.T) {
	tgt, _ := stubListing(t, []stubListObj{})

	r := tgt.ListBounded(context.Background(), ListOptions{})

	if r.End != ListEndedComplete || len(r.Objects) != 0 {
		t.Errorf("empty bucket = %+v, want a complete exact zero", r)
	}
}

func TestListBoundedHungListingIsBounded(t *testing.T) {
	hungStore := func(t *testing.T) (list func(ctx context.Context, o ListOptions) ListResult, sawTheStoreGiveUp func()) {
		t.Helper()
		sawCancel := make(chan struct{})
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // parked: the store never answers
			once.Do(func() { close(sawCancel) })
		}))
		t.Cleanup(srv.Close)
		tgt := targetAt(srv.URL)
		list = func(ctx context.Context, o ListOptions) ListResult {
			done := make(chan ListResult, 1)
			go func() { done <- tgt.ListBounded(ctx, o) }()
			select {
			case r := <-done:
				return r
			case <-time.After(10 * time.Second):
				t.Fatal("the listing did not return within its bound")
				return ListResult{}
			}
		}
		sawTheStoreGiveUp = func() {
			select {
			case <-sawCancel:
			case <-time.After(5 * time.Second):
				t.Error("the store never saw its request context end: the caller's context did not reach the S3 call")
			}
		}
		return list, sawTheStoreGiveUp
	}

	t.Run("time bound", func(t *testing.T) {
		list, sawTheStoreGiveUp := hungStore(t)
		r := list(context.Background(), ListOptions{MaxTime: 100 * time.Millisecond})
		if r.End != ListEndedTimeout {
			t.Errorf("End = %q, want timeout: the time bound ended the listing", r.End)
		}
		sawTheStoreGiveUp()
	})
	t.Run("caller cancellation", func(t *testing.T) {
		list, sawTheStoreGiveUp := hungStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		r := list(ctx, ListOptions{MaxTime: time.Minute})
		if r.End != ListEndedCancelled {
			t.Errorf("End = %q, want cancelled: the caller cancelled the request", r.End)
		}
		sawTheStoreGiveUp()
	})
}

func TestListBoundedDeniedListingIsClassifiedDenied(t *testing.T) {
	t.Run("refused outright", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			denyList(w)
		}))
		t.Cleanup(srv.Close)

		r := targetAt(srv.URL).ListBounded(context.Background(), ListOptions{})

		if r.End != ListEndedDenied {
			t.Errorf("End = %q, want denied: a 403 is the store refusing the listing", r.End)
		}
		if len(r.Objects) != 0 {
			t.Errorf("Objects = %+v, want none: nothing was listed before the refusal", r.Objects)
		}
	})
	t.Run("refused mid-listing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("continuation-token") != "" {
				denyList(w)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(listPageXML("page1", listObj("a.db", 1))))
		}))
		t.Cleanup(srv.Close)

		r := targetAt(srv.URL).ListBounded(context.Background(), ListOptions{})

		if r.End != ListEndedDenied {
			t.Errorf("End = %q, want denied: the store refused the listing's continuation", r.End)
		}
		if got := names(r.Objects); !slices.Equal(got, []string{"a.db"}) {
			t.Errorf("Objects = %q, want the first page's objects as a floor, never a total", got)
		}
	})
}

func TestListBoundedStopsAtThePageCap(t *testing.T) {
	tgt, tokens := stubListing(t,
		[]stubListObj{listObj("a.db", 1)},
		[]stubListObj{listObj("b.db", 1)},
		[]stubListObj{listObj("c.db", 1)},
	)

	r := tgt.ListBounded(context.Background(), ListOptions{MaxPages: 2})

	if r.End != ListEndedCapped {
		t.Errorf("End = %q, want capped: the page cap stopped the listing before the end", r.End)
	}
	if got := names(r.Objects); !slices.Equal(got, []string{"a.db", "b.db"}) {
		t.Errorf("Objects = %q, want only the objects the two capped pages held", got)
	}
	if got := tokens(); len(got) != 2 {
		t.Errorf("the listing asked for %d pages (%q), want only the 2 the cap allowed", len(got), got)
	}
}

func TestListBoundedStopsAtTheByteCap(t *testing.T) {
	tgt, _ := stubListing(t,
		[]stubListObj{listObj("a.db", 100), listObj("b.db", 100)},
	)

	r := tgt.ListBounded(context.Background(), ListOptions{MaxBytes: 150})

	if r.End != ListEndedCapped {
		t.Errorf("End = %q, want capped: the byte cap stopped the listing", r.End)
	}
	if got := names(r.Objects); !slices.Equal(got, []string{"a.db"}) {
		t.Errorf("Objects = %q, want only the first object: the second crossed the byte cap", got)
	}
}
