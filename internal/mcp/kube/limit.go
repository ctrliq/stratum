package kube

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultMaxResponseBytes caps a single API response.
const DefaultMaxResponseBytes int64 = 8 << 20

// ErrResponseTooLarge reports that a response exceeded the configured cap.
type ErrResponseTooLarge struct {
	URL   string
	Limit int64
}

func (e *ErrResponseTooLarge) Error() string {
	return fmt.Sprintf("response from %s exceeds the %d byte limit: narrow the request "+
		"(a namespace, a label selector, or a smaller limit)", e.URL, e.Limit)
}

// IsResponseTooLarge reports whether err is or wraps an ErrResponseTooLarge.
func IsResponseTooLarge(err error) bool {
	var target *ErrResponseTooLarge
	return errors.As(err, &target)
}

// limitTransport caps how much of each response body a client will read.
type limitTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}

	// A watch is a long-lived stream whose whole purpose is to exceed any
	// total-bytes bound; the cap is about one answer's size, not a session's.
	// Checked before Content-Length, or a watch that happens to advertise a
	// finite length above the cap is rejected despite being exempt.
	if req.URL.Query().Get("watch") == "true" {
		return resp, nil
	}

	// Content-Length is only a hint — it is absent for chunked responses and for
	// anything the apiserver streams, which is most list responses of the size
	// worth catching.
	if resp.ContentLength > t.limit {
		resp.Body.Close()
		return nil, &ErrResponseTooLarge{URL: req.URL.String(), Limit: t.limit}
	}

	resp.Body = &limitReader{
		inner: resp.Body,
		left:  t.limit,
		err:   &ErrResponseTooLarge{URL: req.URL.String(), Limit: t.limit},
	}
	return resp, nil
}

// limitReader fails the read once more than left bytes have been served.
type limitReader struct {
	inner io.ReadCloser
	left  int64
	err   *ErrResponseTooLarge
}

func (r *limitReader) Read(p []byte) (int, error) {
	// An exhausted budget is not yet an overflow: a body of exactly the limit
	// can report its end on a later read, so at zero the read below is a
	// one-byte probe that tells EOF from one byte too many.
	if r.left < 0 {
		return 0, r.err
	}
	// Read at most one byte past the budget.
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.inner.Read(p)
	r.left -= int64(n)
	if r.left < 0 {
		return 0, r.err
	}
	return n, err
}

func (r *limitReader) Close() error { return r.inner.Close() }
