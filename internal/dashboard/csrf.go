package dashboard

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

type originPolicy struct {
	// Scheme kept: comparing only the host would let a plaintext page pass as same-origin.
	PublicOrigin string
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (p originPolicy) allowedOrigin(r *http.Request, origin string) bool {
	if origin == "" {
		return true
	}
	got, scheme, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	// A plaintext origin is foreign whenever the browser is known to arrive over TLS.
	if scheme == "http" && (r.TLS != nil || p.publicScheme() == "https") {
		return false
	}
	if p.PublicOrigin != "" {
		if want, _, ok := canonicalOrigin(p.PublicOrigin); ok && got == want {
			return true
		}
	}
	// r.Host carries no scheme, so borrow the origin's own; the ports still have to agree.
	if want, _, ok := canonicalOrigin(scheme + "://" + r.Host); ok && got == want {
		return true
	}
	return false
}

func (p originPolicy) publicScheme() string {
	_, scheme, ok := canonicalOrigin(p.PublicOrigin)
	if !ok {
		return ""
	}
	return scheme
}

func (p originPolicy) checkFetchMetadata(r *http.Request) string {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return "cross-site request"
	}
	if !p.allowedOrigin(r, r.Header.Get("Origin")) {
		return "foreign origin"
	}
	return ""
}

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

// Login has no session to derive a CSRF token from, so it needs this origin guard alone.
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

// Applies Origin / Sec-Fetch-Site rules plus the session CSRF token; runs inside requireSession.
func (m *sessions) csrf(policy originPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				// Browsers cannot add headers to a WebSocket handshake, so upgrades are gated on origin alone.
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
