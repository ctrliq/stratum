package kube

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveBytes returns a server writing n bytes, optionally without a
// Content-Length so the body must actually be read to discover its size —
// which is the case that matters, because the apiserver streams list
// responses and declares no length for them.
func serveBytes(t *testing.T, n int, declareLength bool) *httptest.Server {
	t.Helper()
	body := strings.Repeat("x", n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !declareLength {
			// Forcing chunked encoding: without a declared length the
			// transport streams and ContentLength arrives as -1.
			w.Header().Set("Transfer-Encoding", "chunked")
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveThenEnd streams head, flushes, and only then sends tail and ends the
// response, so the client reads the end of the body separately from its
// bytes — as with an apiserver that streams a list and then closes it.
func serveThenEnd(t *testing.T, head, tail int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", head))
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, strings.Repeat("x", tail))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// lateEOF never returns data and io.EOF from the same Read.
type lateEOF struct{ r io.Reader }

func (l lateEOF) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		return n, nil
	}
	return 0, err
}

func get(t *testing.T, srv *httptest.Server, limit int64, query string) ([]byte, error) {
	t.Helper()
	client := &http.Client{Transport: &limitTransport{base: http.DefaultTransport, limit: limit}}
	resp, err := client.Get(srv.URL + query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func TestResponseLimit(t *testing.T) {
	const limit = 1024

	t.Run("under the limit passes", func(t *testing.T) {
		got, err := get(t, serveBytes(t, limit-1, false), limit, "")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != limit-1 {
			t.Errorf("read %d bytes, want %d", len(got), limit-1)
		}
	})

	// The boundary is the case a naive io.LimitReader gets wrong: a body of
	// exactly the limit is within budget, and must not be reported as an
	// overflow just because the reader had no room left to prove otherwise.
	t.Run("exactly the limit passes", func(t *testing.T) {
		got, err := get(t, serveBytes(t, limit, false), limit, "")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != limit {
			t.Errorf("read %d bytes, want %d", len(got), limit)
		}
	})

	// net/http usually hands back EOF with the last bytes, which hides the
	// boundary; here the end arrives on its own read.
	t.Run("exactly the limit passes when the end arrives late", func(t *testing.T) {
		got, err := get(t, serveThenEnd(t, limit, 0), limit, "")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != limit {
			t.Errorf("read %d bytes, want %d", len(got), limit)
		}
	})

	t.Run("one byte over fails when it arrives late", func(t *testing.T) {
		_, err := get(t, serveThenEnd(t, limit, 1), limit, "")
		if !IsResponseTooLarge(err) {
			t.Fatalf("err = %v, want ErrResponseTooLarge", err)
		}
	})

	// The Content-Length pre-check and the reader must agree on the boundary.
	t.Run("a declared length of exactly the limit passes", func(t *testing.T) {
		got, err := get(t, serveBytes(t, limit, true), limit, "")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != limit {
			t.Errorf("read %d bytes, want %d", len(got), limit)
		}
	})

	t.Run("one byte over fails", func(t *testing.T) {
		_, err := get(t, serveBytes(t, limit+1, false), limit, "")
		if !IsResponseTooLarge(err) {
			t.Fatalf("err = %v, want ErrResponseTooLarge", err)
		}
	})

	// A declared Content-Length lets us refuse before transferring the body.
	t.Run("declared length fails without transferring", func(t *testing.T) {
		_, err := get(t, serveBytes(t, limit*4, true), limit, "")
		if !IsResponseTooLarge(err) {
			t.Fatalf("err = %v, want ErrResponseTooLarge", err)
		}
	})

	// A watch is an unbounded stream by design.
	t.Run("a watch is exempt", func(t *testing.T) {
		got, err := get(t, serveBytes(t, limit*3, false), limit, "?watch=true")
		if err != nil {
			t.Fatalf("watch read: %v", err)
		}
		if len(got) != limit*3 {
			t.Errorf("read %d bytes, want the full %d", len(got), limit*3)
		}
	})

	t.Run("the error says what to do about it", func(t *testing.T) {
		_, err := get(t, serveBytes(t, limit+1, false), limit, "")
		msg := err.Error()
		for _, want := range []string{"exceeds", "narrow the request", "namespace", "selector"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q is missing %q — a bare size error tells a model nothing actionable", msg, want)
			}
		}
	})

	// The reader on its own, with EOF always on a separate read and reads of
	// every size, so no transport buffering decides the outcome.
	t.Run("the reader tells EOF from overflow at the boundary", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			size     int
			overflow bool
		}{
			{"under", limit - 1, false},
			{"exact", limit, false},
			{"one over", limit + 1, true},
		} {
			for _, chunk := range []int{1, 7, limit, limit * 2} {
				r := &limitReader{
					inner: io.NopCloser(lateEOF{bytes.NewReader(make([]byte, tc.size))}),
					left:  limit,
					err:   &ErrResponseTooLarge{URL: "http://test", Limit: limit},
				}
				buf := make([]byte, chunk)
				total := 0
				var err error
				for {
					var n int
					n, err = r.Read(buf)
					total += n
					if err != nil {
						break
					}
				}
				if tc.overflow {
					if !IsResponseTooLarge(err) {
						t.Errorf("%s, %d-byte reads: err = %v, want ErrResponseTooLarge", tc.name, chunk, err)
					}
					// Once over, it stays over.
					if _, again := r.Read(buf); !IsResponseTooLarge(again) {
						t.Errorf("%s, %d-byte reads: read after overflow = %v, want ErrResponseTooLarge", tc.name, chunk, again)
					}
					continue
				}
				if err != io.EOF || total != tc.size {
					t.Errorf("%s, %d-byte reads: read %d bytes, err %v; want %d bytes and io.EOF", tc.name, chunk, total, err, tc.size)
				}
			}
		}
	})

	t.Run("a negative limit disables the cap", func(t *testing.T) {
		c := NewConnector(Options{MaxResponseBytes: -1})
		if got := c.opts.maxBytes(); got != -1 {
			t.Errorf("maxBytes = %d, want -1 (disabled)", got)
		}
		if got := (Options{}).maxBytes(); got != DefaultMaxResponseBytes {
			t.Errorf("zero value maxBytes = %d, want the default %d", got, DefaultMaxResponseBytes)
		}
	})
}
