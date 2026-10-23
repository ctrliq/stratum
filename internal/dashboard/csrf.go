package dashboard

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// originPolicy decides whether a request's Origin / Sec-Fetch-Site are
// acceptable.
type originPolicy struct {
	// PublicOrigin is scheme://host[:port] from --public-url, or "". It keeps
	// the scheme deliberately: an origin is scheme, host and port together,
	// and comparing only the host lets a plaintext page on the same name pass
	// as same-origin against a TLS server.
	PublicOrigin string
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// allowedOrigin reports whether origin matches the request host or the
// configured public host. An empty origin is allowed (non-browser client, or
// a same-origin GET); "null" is not.
func (p originPolicy) allowedOrigin(r *http.Request, origin string) bool {
	if origin == "" {
		return true
	}
	got, scheme, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	// A plaintext origin is foreign whenever we know the browser arrives over
	// TLS — because we terminated it ourselves, or because --public-url says
	// so. Behind a TLS-terminating proxy r.TLS is nil, so the connection
	// cannot answer this and the public origin's scheme has to.
	//
	// The second half matters: without it the host fallback below rebuilt the
	// origin to compare against from the CALLER'S scheme plus r.Host, so
	// http://<the same host> was accepted even with an https public origin —
	// reintroducing exactly the bug canonicalOrigin was written to stop. It
	// bites hardest on a WebSocket upgrade, which a browser cannot give the
	// CSRF header and which is therefore gated on origin alone: a page served
	// on http://<host> could open wss://<host>/k8s/.../exec, the cookie would
	// go along because the socket's own target is TLS, and the caller would
	// have a shell as the victim.
	if scheme == "http" && (r.TLS != nil || p.publicScheme() == "https") {
		return false
	}
	if p.PublicOrigin != "" {
		if want, _, ok := canonicalOrigin(p.PublicOrigin); ok && got == want {
			return true
		}
	}
	// r.Host carries no scheme, so borrow the origin's own. The ports still
	// have to agree, which is what stops :80 matching :443.
	if want, _, ok := canonicalOrigin(scheme + "://" + r.Host); ok && got == want {
		return true
	}
	return false
}

// publicScheme is the scheme --public-url named, or "" when it was not given
// or could not be parsed.
func (p originPolicy) publicScheme() string {
	_, scheme, ok := canonicalOrigin(p.PublicOrigin)
	if !ok {
		return ""
	}
	return scheme
}

// checkFetchMetadata returns a reason string on rejection, "" when acceptable.
func (p originPolicy) checkFetchMetadata(r *http.Request) string {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return "cross-site request"
	}
	if !p.allowedOrigin(r, r.Header.Get("Origin")) {
		return "foreign origin"
	}
	return ""
}

// canonicalOrigin reduces an origin to lower-cased scheme://host[:port], with
// the default port for THAT scheme removed, and returns the scheme alongside.
func canonicalOrigin(s string) (canonical, scheme string, ok bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", "", false
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host, scheme, true
}

// guard applies the Origin / Sec-Fetch-Site rules on its own, without
// requiring a session, for the unauthenticated endpoints.
//
// POST /api/v1/login needs it and the session csrf() below cannot give it:
// that middleware reads the session to derive the expected token, and at login
// there is no session yet. Unguarded, a cross-site form POST (a simple
// request, so no CORS preflight to stop it) submits the ATTACKER'S bearer
// token, the browser stores the Set-Cookie that comes back — SameSite governs
// when a cookie is SENT, not whether it is accepted — and the victim then
// drives the dashboard as whoever that token belongs to, believing it is their
// own session. Classic login CSRF / session fixation.
//
// An absent Origin still passes, so a curl or CI client is unaffected: the
// threat is a browser being steered, and a browser always sends one
// cross-site.
func (p originPolicy) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) {
			if reason := p.checkFetchMetadata(r); reason != "" {
				writeJSONError(w, http.StatusForbidden, "csrf", reason)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// csrf requires a valid csrfHeader for the session in the request context on
// every non-safe method, and applies the Origin / Sec-Fetch-Site rules. It
// must run inside requireSession.
func (m *sessions) csrf(policy originPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				// Browsers cannot add custom headers to a WebSocket handshake,
				// so the exec/attach upgrades are gated on Origin and
				// Sec-Fetch-Site alone.
				if r.Header.Get("Upgrade") != "" {
					if reason := policy.checkFetchMetadata(r); reason != "" {
						writeJSONError(w, http.StatusForbidden, "csrf", reason)
						return
					}
				}
				next.ServeHTTP(w, r)
				return
			}
			if reason := policy.checkFetchMetadata(r); reason != "" {
				writeJSONError(w, http.StatusForbidden, "csrf", reason)
				return
			}
			s, ok := sessionFrom(r.Context())
			if !ok || !m.checkCSRF(s, r.Header.Get(csrfHeader)) {
				writeJSONError(w, http.StatusForbidden, "csrf", "missing or invalid "+csrfHeader)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
