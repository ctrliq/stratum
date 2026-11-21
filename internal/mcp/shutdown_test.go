package mcp

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Once a shutdown starts, every request gets an immediate 503 that closes the connection.
func TestRefuseWhileStopping(t *testing.T) {
	var stopping atomic.Bool
	reached := 0
	h := refuseWhileStopping(&stopping, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || reached != 1 {
		t.Fatalf("before shutdown: code %d, reached %d; want the request passed through", rec.Code, reached)
	}

	stopping.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("during shutdown: code %d, want 503", rec.Code)
	}
	if rec.Header().Get("Connection") != "close" {
		t.Errorf("during shutdown: Connection = %q, want close so a reconnect cannot reuse the connection", rec.Header().Get("Connection"))
	}
	if reached != 1 {
		t.Errorf("a request during shutdown reached the SDK handler")
	}
}
