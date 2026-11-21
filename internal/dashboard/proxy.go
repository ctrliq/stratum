package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"k8s.io/client-go/rest"
)

const (
	maxBodyBytes   = 4 << 20
	requestTimeout = 60 * time.Second
	// Refreshed on every read/write; reclaims an abandoned exec without a hard cap.
	idleDeadline = time.Hour
	// The URL prefix the SPA puts in front of every Kubernetes path.
	k8sPrefix = "/k8s"
)

// Never travel to the apiserver; Authorization is replaced with the session's own token.
var scrubbedRequestHeaders = []string{
	"Cookie", "Authorization", csrfHeader, "Forwarded",
}

// Impersonate-Extra-<key> is an open family no fixed list can cover; X-Forwarded- likewise.
var scrubbedRequestHeaderPrefixes = []string{"Impersonate-", "X-Forwarded-"}

type target struct {
	Scheme    string
	Host      string
	Transport http.RoundTripper
}

// The transport must carry no ambient identity; the session's token is the only one.
func localTarget(cfg *rest.Config) (*target, error) {
	cfg = rest.AnonymousClientConfig(cfg)
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return withIdleDeadline(c, idleDeadline), nil
	}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("apiserver transport: %w", err)
	}
	host := cfg.Host
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse apiserver host %q: %w", cfg.Host, err)
	}
	return &target{Scheme: u.Scheme, Host: u.Host, Transport: rt}, nil
}

// Preserves the encoded path so a %2F in a resource name reaches the apiserver intact.
func stripPrefix(u *url.URL, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return
	}
	if strings.HasPrefix(u.Path, prefix) {
		u.Path = u.Path[len(prefix):]
	}
	if u.RawPath != "" && strings.HasPrefix(u.RawPath, prefix) {
		u.RawPath = u.RawPath[len(prefix):]
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if u.RawPath != "" && !strings.HasPrefix(u.RawPath, "/") {
		u.RawPath = "/" + u.RawPath
	}
}

func rewriteRequest(out *http.Request, prefix string, t target) {
	stripPrefix(out.URL, prefix)
	out.URL.Scheme = t.Scheme
	out.URL.Host = t.Host
	out.Host = t.Host
	for _, h := range scrubbedRequestHeaders {
		out.Header.Del(h)
	}
	// delete, not Header.Del: Del canonicalises the key first and can miss a non-canonical entry.
	for name := range out.Header {
		for _, prefix := range scrubbedRequestHeaderPrefixes {
			if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				delete(out.Header, name)
				break
			}
		}
	}
	// After the scrub, never before: the caller's Authorization must not survive.
	if token, ok := tokenFrom(out.Context()); ok {
		out.Header.Set("Authorization", "Bearer "+token)
	}
}

// GET only: a caller must not lift the deadline with an Upgrade header or ?follow=.
func isStreaming(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	q := r.URL.Query()
	return q.Get("watch") == "true" || q.Get("watch") == "1" ||
		q.Get("follow") == "true" || q.Get("follow") == "1"
}

type proxy struct {
	rp        *httputil.ReverseProxy
	nodeShell bool
	log       *slog.Logger
}

func newProxy(t *target, enableNodeShell bool, log *slog.Logger) *proxy {
	p := &proxy{nodeShell: enableNodeShell, log: log}
	p.rp = &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { rewriteRequest(pr.Out, k8sPrefix, *t) },
		Transport:     t.Transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ModifyResponse: func(resp *http.Response) error {
			// The apiserver has no business setting cookies in our origin.
			resp.Header.Del("Set-Cookie")
			// sandbox replaces a workload's own policy; ReverseProxy would add one alongside it.
			resp.Header.Set("Content-Security-Policy", "sandbox")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				writeStatus(w, http.StatusGatewayTimeout, "Timeout", "the apiserver did not answer within "+requestTimeout.String(), "upstream-timeout")
			case errors.Is(err, context.Canceled):
				// Client went away; nothing to write.
			default:
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					writeStatus(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge", "request body exceeds 4 MiB", "body-too-large")
					return
				}
				log.Warn("proxy error", "path", r.URL.Path, "err", err)
				writeStatus(w, http.StatusBadGateway, "BadGateway", "upstream error: "+err.Error(), "upstream-error")
			}
		},
	}
	return p
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	stripped := *r.URL
	stripPrefix(&stripped, k8sPrefix)

	// Applied to every request, streaming or not: gating on isStreaming let a caller lift it.
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	if !isStreaming(r) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	// Before forwarding, always: a refusal after the apiserver created the pod is not a gate.
	if refused := p.checkNodeShell(w, r, stripped.Path); refused {
		return
	}

	p.rp.ServeHTTP(w, r)
}

// Status-shaped, plus {"error": code} so both SPA readers (.message and .error) understand.
func writeStatus(w http.ResponseWriter, code int, reason, msg, errCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    msg,
		"reason":     reason,
		"code":       code,
		"error":      errCode,
	})
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func withIdleDeadline(c net.Conn, idle time.Duration) net.Conn {
	return &idleConn{Conn: c, idle: idle}
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}
