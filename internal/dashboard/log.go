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

// Request logging, cut down to the access log.
//
// It does not record every mutating proxied request or every Secret read: a
// proxy that acted as one privileged ServiceAccount would need that, since its
// log would be the only place a human action was attributed. Here each
// request carries the user's own bearer token, so the apiserver's audit log
// records the real user for every proxied call — a second, weaker copy in the
// dashboard's stdout would add nothing it does not already have. What is kept
// is the access log (so a refusal has a trace), the panic guard, and the
// streaming-safe recorder both need.

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// requestIDMiddleware stamps a fresh 16-hex id on each request and echoes it
// in X-Request-ID. Inbound ids are ignored: they are attacker-controlled text.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// recoverMiddleware turns a handler panic into a 500 and a log line.
// http.ErrAbortHandler is re-panicked as net/http expects.
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
						// Not http.Error: it forces Content-Type to text/plain,
						// so this JSON body would go out mislabelled.
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

// responseRecorder captures status and byte count while staying transparent to
// streaming and hijacking: it implements Flush, Hijack and Unwrap so
// httputil.ReverseProxy (via http.ResponseController) reaches the real writer.
// Without that, exec and log-follow break.
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

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush commits the response headers exactly as Write does, so it has to mark
// the recorder written. Otherwise a handler that flushes a stream and then
// panics looks untouched to recoverMiddleware, which appends a JSON 500 to a
// response already on the wire — and the access log reports the status the
// stream never had.
func (r *responseRecorder) Flush() {
	r.wrote = true
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// Hijack forwards to the underlying writer; the status is recorded as 101.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
		r.wrote = true
	}
	return c, rw, err
}

// accessLog logs one line per request, with the actor once the session layer
// has resolved it.
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

// remoteIP is the client IP without port. X-Forwarded-For is deliberately
// ignored: it is caller-supplied, and the proxy scrubs it anyway.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
