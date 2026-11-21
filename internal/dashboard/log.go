package dashboard

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// Stamps a fresh id per request; inbound X-Request-ID values are attacker-controlled text.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					log.Error("panic serving request", "id", requestID(r.Context()), "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
					if rw, ok := w.(*responseRecorder); !ok || !rw.wrote {
						// Not http.Error: it forces Content-Type to text/plain.
						h := w.Header()
						h.Del("Content-Length")
						h.Set("Content-Type", "application/json")
						h.Set("X-Content-Type-Options", "nosniff")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"error":"internal","message":"internal error"}`)
					}
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Flush/Hijack/Unwrap so httputil.ReverseProxy reaches the real writer; else streaming breaks.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func newRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Marks the recorder written, or a flushed-then-panicked stream gets a 500 appended on the wire.
func (r *responseRecorder) Flush() {
	r.wrote = true
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
		r.wrote = true
	}
	return c, rw, err
}

func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := newRecorder(w)
			r = r.WithContext(withActorHolder(r.Context()))
			next.ServeHTTP(rec, r)
			attrs := []any{
				"id", requestID(r.Context()),
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"bytes", rec.bytes, "dur_ms", time.Since(start).Milliseconds(),
				"remote", remoteIP(r),
			}
			if actor := actorFrom(r.Context()); actor != "" {
				attrs = append(attrs, "actor", actor)
			}
			log.Info("http", attrs...)
		})
	}
}

// X-Forwarded-For is deliberately ignored: caller-supplied, and the proxy scrubs it anyway.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
