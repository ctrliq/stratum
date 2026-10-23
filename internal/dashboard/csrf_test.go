package dashboard

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The origin check is the ONLY gate on a WebSocket upgrade — a browser cannot
// put the CSRF header on a handshake — so a same-host plaintext origin getting
// through is enough for a page on http://<host> to open wss://<host>/.../exec
// and hold a shell as the victim. The cookie travels because the socket's own
// target is TLS.
func TestAllowedOrigin(t *testing.T) {
	for _, tc := range []struct {
		name         string
		publicOrigin string
		host         string
		tls          bool
		origin       string
		want         bool
	}{
		// No public origin, TLS terminated here.
		{name: "same origin over TLS", host: "dash.example", tls: true, origin: "https://dash.example", want: true},
		{name: "plaintext origin while we serve TLS", host: "dash.example", tls: true, origin: "http://dash.example"},
		{name: "another host over TLS", host: "dash.example", tls: true, origin: "https://evil.example"},

		// Behind a TLS terminator: r.TLS is nil, so only --public-url knows
		// the browser is on HTTPS. This is the case the scheme check exists
		// for.
		{name: "plaintext origin behind an https terminator", publicOrigin: "https://dash.example",
			host: "dash.example", origin: "http://dash.example"},
		{name: "the declared public origin itself", publicOrigin: "https://dash.example",
			host: "dash.example", origin: "https://dash.example", want: true},
		{name: "a second https host behind the terminator", publicOrigin: "https://dash.example",
			host: "10.0.0.5:8443", origin: "https://10.0.0.5:8443", want: true},
		{name: "plaintext on a second host behind the terminator", publicOrigin: "https://dash.example",
			host: "10.0.0.5:8443", origin: "http://10.0.0.5:8443"},

		// A declared plaintext deployment: http is what users arrive on, so
		// it is not foreign there.
		{name: "plaintext origin in a declared plaintext deployment", publicOrigin: "http://dash.example",
			host: "dash.example", origin: "http://dash.example", want: true},

		// The loopback default: no public origin, no TLS.
		{name: "loopback same origin", host: "127.0.0.1:8443", origin: "http://127.0.0.1:8443", want: true},
		{name: "loopback, foreign host", host: "127.0.0.1:8443", origin: "http://evil.example"},

		// Shapes that must never pass.
		{name: "the null origin", host: "dash.example", origin: "null"},
		{name: "a non-http scheme", host: "dash.example", origin: "file://"},
		{name: "garbage", host: "dash.example", origin: "not a url"},
		// Ports still have to agree, which is what stops :80 matching :443.
		{name: "same host, wrong port", host: "dash.example:8443", origin: "https://dash.example:9443"},
		// A non-browser client sends no Origin and has no cookie to abuse.
		{name: "no origin header", host: "dash.example", origin: "", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := originPolicy{PublicOrigin: tc.publicOrigin}
			r := httptest.NewRequest(http.MethodGet, "/k8s/api/v1/namespaces/d/pods/p/exec", nil)
			r.Host = tc.host
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := p.allowedOrigin(r, tc.origin); got != tc.want {
				t.Fatalf("allowedOrigin(%q) with public=%q host=%q tls=%t = %t, want %t",
					tc.origin, tc.publicOrigin, tc.host, tc.tls, got, tc.want)
			}
		})
	}
}

func TestPublicScheme(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://dash.example", "https"},
		{"http://dash.example:8080", "http"},
		{"", ""},
		{"dash.example", ""},
		{"ftp://dash.example", ""},
	} {
		p := originPolicy{PublicOrigin: tc.in}
		if got := p.publicScheme(); got != tc.want {
			t.Errorf("publicScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Sec-Fetch-Site is checked before the origin, and covers the browsers that
// send it even when an Origin would have passed.
func TestCheckFetchMetadata(t *testing.T) {
	p := originPolicy{PublicOrigin: "https://dash.example"}
	for _, tc := range []struct{ site, origin, wantReason string }{
		{"same-origin", "https://dash.example", ""},
		{"none", "", ""},
		{"", "https://dash.example", ""},
		{"cross-site", "https://dash.example", "cross-site request"},
		{"same-site", "https://dash.example", "cross-site request"},
		{"same-origin", "http://dash.example", "foreign origin"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
		r.Host = "dash.example"
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := p.checkFetchMetadata(r); got != tc.wantReason {
			t.Errorf("site=%q origin=%q: reason = %q, want %q", tc.site, tc.origin, got, tc.wantReason)
		}
	}
}
