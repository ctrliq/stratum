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

// stubListObj is one object a stub listing serves.
type stubListObj struct {
	key      string
	size     int64
	modified string
}

// listObjTime is the fixed LastModified listObj serves, so the times a
// listing reports assert exactly.
var listObjTime = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

// listObj builds a stub object with listObjTime as its LastModified.
func listObj(key string, size int64) stubListObj {
	return stubListObj{key: key, size: size, modified: "2026-11-12T00:00:00.000Z"}
}

// listObjs builds the size-1 stub objects the named keys hold.
func listObjs(keys ...string) []stubListObj {
	objs := make([]stubListObj, len(keys))
	for i, k := range keys {
		objs[i] = listObj(k, 1)
	}
	return objs
}

// listPageXML renders one ListObjectsV2 response: truncated with next as the
// continuation token when another page follows, complete otherwise.
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

// deniedBody is the AccessDenied XML a refusing stub serves with 403.
const deniedBody = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`

// denyList refuses a listing the way a store refuses one it is not allowed to
// answer.
func denyList(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(deniedBody))
}

// targetAt points a path-style target at a stub server.
func targetAt(url string) *Target {
	return &Target{Endpoint: url, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
}

// stubListing serves ListObjectsV2 a page at a time like S3: the request's
// continuation token picks the page (no token is the first), and a page with
// a successor is truncated carrying the successor's token. It records every
// continuation token the SDK asked for, so a test can pin the pagination.
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

// names projects a listing's objects to their names, in order.
func names(objs []Object) []string {
	ns := make([]string, len(objs))
	for i, o := range objs {
		ns[i] = o.Name
	}
	return ns
}

// A truncated listing is followed through its continuation tokens to the
// end: every page's objects come back once, with the sizes and modification
// times their pages carried, and the listing ends complete — the objects are
// the store's exact total. The pagination is pinned on the wire: the listing
// asks for the first page untokened and every next page with the token the
// page before it returned.
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

// A key the store lists again on a later page is one object, not two: the
// same key across pages is the same object, so wherever the listing feeds a
// count it counts once. The listing still ends complete when it reaches the
// end.
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

// A store holding nothing under the target's prefix lists zero objects and
// ends complete: an exact zero, never an unknown.
func TestListBoundedEmptyBucketIsAnExactZero(t *testing.T) {
	tgt, _ := stubListing(t, []stubListObj{})

	r := tgt.ListBounded(context.Background(), ListOptions{})

	if r.End != ListEndedComplete || len(r.Objects) != 0 {
		t.Errorf("empty bucket = %+v, want a complete exact zero", r)
	}
}

// A store that never answers is hung up on, not waited out: the caller's
// context — which the listing propagates into the S3 call — ends the request,
// the store sees its request context end, and the listing returns within its
// bound, classified by what stopped it — a time bound as timeout, a caller
// cancellation as cancelled — with the objects it already listed as a floor.
// Each subtest runs against its own store and its own proof channel, so the
// cancellation it asserts is its own request's: a channel shared between the
// subtests is closed once, by whichever ran first, and the later subtest
// would read the first one's proof — a vacuous check that cannot fail.
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

// A store that refuses the listing with 403 is classified denied — the
// store's answer, distinct from an ordinary failure — and the objects already
// listed stand as a floor while the listing stops, never becoming a total.
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

// The page cap bounds how deep a listing goes: it stops after that many
// pages, classified capped, without asking for the page after the cap.
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

// The byte cap bounds the object bytes a listing accounts for: it stops
// before the object that would cross the cap, classified capped, and the
// objects it did list stand as a floor.
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
