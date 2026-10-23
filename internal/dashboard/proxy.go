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
	// maxBodyBytes caps non-streaming request bodies.
	maxBodyBytes = 4 << 20
	// requestTimeout bounds non-streaming requests end to end.
	requestTimeout = 60 * time.Second
	// idleDeadline is refreshed on every read/write of a streaming connection,
	// so an abandoned exec is reclaimed without a hard cap on session length.
	idleDeadline = time.Hour
	// k8sPrefix is the URL prefix the SPA puts in front of every Kubernetes
	// path; it is also what /api/v1/config reports as k8sBase.
	k8sPrefix = "/k8s"
)

// scrubbedRequestHeaders never travel to the apiserver. The session cookie
// and CSRF token are ours alone, Authorization is replaced below with the
// session's own token rather than forwarded from the caller, and X-Forwarded-*
// would let a dashboard user spoof audit-relevant metadata on the apiserver
// side.
var scrubbedRequestHeaders = []string{
	"Cookie", "Authorization", csrfHeader, "Forwarded",
}

// scrubbedRequestHeaderPrefixes are dropped by prefix rather than exact name.
//
// Impersonate-Extra-<key> is an open family, so no fixed list can cover it;
// Impersonate-User, -Group and -Uid share the prefix and come along. Under
// bearer passthrough a forwarded impersonation header would be honoured by the
// apiserver wherever the *user's own* token holds the impersonate verb, which
// is a privilege the dashboard has no business helping anyone exercise
// implicitly. The guard belongs here, not in an RBAC file someone may widen.
// X-Forwarded- is a family too, and listing its members by name was an
// overclaim: the comment above and SECURITY.md both said "X-Forwarded-*" while
// the code matched five exact names, so X-Forwarded-Server and anything else a
// caller invented came through. net/http's ReverseProxy drops only the three
// it manages itself. Nothing downstream of us should read forwarded metadata a
// dashboard user supplied, so the whole family goes.
var scrubbedRequestHeaderPrefixes = []string{"Impersonate-", "X-Forwarded-"}

// target is where the apiserver is reached.
type target struct {
	Scheme    string
	Host      string
	Transport http.RoundTripper
}

// localTarget builds the target for this cluster's apiserver from a
// rest.Config.
//
// Every credential in the config is cleared first: the transport carries TLS
// and nothing else, so it has no ambient identity at all. That is the whole
// point of bearer passthrough — the only identity on a proxied request is the
// one the session supplies. It also fixes the failure mode: a request that
// somehow arrives with no token gets a 401 from the apiserver instead of
// silently running as whatever account started the process.
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

// stripPrefix removes prefix from u.Path and u.RawPath, preserving the encoded
// form so a %2F in a resource name reaches the apiserver intact. A path equal
// to the prefix becomes "/".
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

// rewriteRequest strips the prefix, retargets, scrubs caller-supplied headers
// and installs the session's bearer token. Unit-testable without a
// ProxyRequest.
func rewriteRequest(out *http.Request, prefix string, t target) {
	stripPrefix(out.URL, prefix)
	out.URL.Scheme = t.Scheme
	out.URL.Host = t.Host
	out.Host = t.Host
	for _, h := range scrubbedRequestHeaders {
		out.Header.Del(h)
	}
	// Header keys arrive canonicalised (Impersonate-Extra-Scopes), so a
	// case-insensitive prefix test catches every member. delete, not
	// Header.Del: Del canonicalises the key before deleting, so for an entry
	// stored non-canonically it removes a key that is not there and the header
	// survives to the apiserver.
	for name := range out.Header {
		for _, prefix := range scrubbedRequestHeaderPrefixes {
			if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				delete(out.Header, name)
				break
			}
		}
	}
	// After the scrub, never before: the caller's own Authorization must not
	// survive, and this one must.
	if token, ok := tokenFrom(out.Context()); ok {
		out.Header.Set("Authorization", "Bearer "+token)
	}
}

// isStreaming reports whether a request must not get a deadline: watches, log
// follows and protocol upgrades (exec/attach/port-forward over WebSocket).
//
// Only a GET qualifies. Otherwise the caller decides whether our deadline
// applies, just by putting an Upgrade header or ?follow=true or ?follow=1 on
// any method, and a deadline the caller can switch off is not a deadline. The
// apiserver parses query booleans with strconv.ParseBool, so follow=1 is a
// log follow the same way follow=true is, and both stream here, on a GET
// only. The apiserver classifies long-running requests by verb rather than by
// our headers and bounds the rest with its own --request-timeout, so this is
// defence in depth rather than a live hole.
//
// Every streaming shape the dashboard opens is a GET: the browser WebSocket
// API issues a GET handshake (terminal.jsx), and watch and log-follow are GETs
// in the Kubernetes API. The cost is that a SPDY exec, which kubectl sends as
// a POST, would now take the deadline — but that arrives only if someone
// points kubectl at /k8s, and a browser cannot speak SPDY.
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

// proxy forwards /k8s/... to the apiserver as the session's user.
//
// There is one target and no cache or per-cluster manager: the dashboard serves
// the cluster it runs in and nothing else. There is no protected-namespace
// deny-list either — one would only be needed if the proxy held one privileged
// ServiceAccount that a dashboard user could abuse. Here the caller's own RBAC
// is what the apiserver evaluates, so a deny-list would only forbid through the
// dashboard what the same person can do with kubectl. The node-shell gate is
// the one remaining server-side refusal, and it guards a privilege the SPA
// manufactures rather than one the proxy holds.
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
			// pods/proxy and services/proxy return whatever a workload serves,
			// from our origin. Sandboxed, its HTML cannot run script as the
			// signed-in user. The sandbox replaces the workload's policy;
			// ReverseProxy adds it alongside the dashboard's own policy.
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

	// The 4 MiB cap is applied to every request, streaming or not: an upgrade
	// handshake has no body worth the name, and checkNodeShell below reads a
	// pod create into memory whole. Gating the cap on isStreaming let a caller
	// lift it by adding `Upgrade: websocket` to a POST.
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	// The DEADLINE is what a stream cannot have: a watch, a log follow and an
	// exec are open-ended by design, and idleDeadline on the dialled
	// connection is what reclaims an abandoned one instead.
	if !isStreaming(r) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	// Before forwarding, always: a refusal that ran after the apiserver had
	// already created the pod would not be a gate.
	if refused := p.checkNodeShell(w, r, stripped.Path); refused {
		return
	}

	p.rp.ServeHTTP(w, r)
}

// writeStatus writes a Kubernetes Status-shaped body that also carries the
// dashboard's {"error": code} field, so both the SPA's k8sutil.js (which reads
// .message) and its session.js (which reads .error) understand a refusal.
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

// idleConn refreshes an idle deadline on every read and write.
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
