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

func IsResponseTooLarge(err error) bool {
	var target *ErrResponseTooLarge
	return errors.As(err, &target)
}

type limitTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}

	// A watch outgrows any byte bound by design; exempt it before the length check.
	if req.URL.Query().Get("watch") == "true" {
		return resp, nil
	}

	// Content-Length is a hint only: absent for chunked and streamed responses.
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

type limitReader struct {
	inner io.ReadCloser
	left  int64
	err   *ErrResponseTooLarge
}

func (r *limitReader) Read(p []byte) (int, error) {
	// At zero left the next read is a one-byte probe telling EOF from overflow.
	if r.left < 0 {
		return 0, r.err
	}
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
