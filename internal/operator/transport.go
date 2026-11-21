package operator

import (
	"context"
	"io"
	"net/http"
)

// Bounds requests by the owning Helm action's lifetime even when Helm uses Background.
type contextTransport struct {
	ctx  context.Context
	next http.RoundTripper
}

func (t *contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	cleanup := func() {
		stop()
		cancel()
	}
	resp, err := t.next.RoundTrip(req.Clone(ctx))
	if err != nil {
		cleanup()
		return resp, err
	}
	// Cancellation stays active until the body is closed; early cancel breaks watch streams.
	if resp.Body == nil {
		cleanup()
	} else {
		resp.Body = &contextBody{ReadCloser: resp.Body, cleanup: cleanup}
	}
	return resp, nil
}

type contextBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *contextBody) Close() error {
	defer b.cleanup()
	return b.ReadCloser.Close()
}
