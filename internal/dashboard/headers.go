package dashboard

import "net/http"

// contentSecurityPolicy is derived from what the built dashboard actually
// loads, not from a template:
//
//   - no inline <script> or <style> and no eval, so script-src needs neither
//     'unsafe-inline' nor 'unsafe-eval'
//   - many React style={{...}} props, which become inline style attributes
//     and do need 'unsafe-inline' under style-src
//   - one WebSocket, same-origin, for the exec terminal, which connect-src
//     'self' covers
//   - inline SVG icon data, hence img-src data:
//   - no web workers and no blob: URLs
//
// There are no third-party origins at all: the fonts, tokens and icons
// are vendored, so every directive is 'self' and an air-gapped cluster
// reaches for nothing it cannot see.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders applies the response headers as middleware rather than per
// handler, which is the point: the ones that matter are the ones nobody
// remembers to add to a new route.
//
// Set, not Add, and set before the handler runs so a handler that wants to
// override one still can.
func securityHeaders(tlsTerminated bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			// frame-ancestors above is the modern control; this is for
			// anything that still only understands the old header.
			h.Set("X-Frame-Options", "DENY")
			// The path carries resource names and the query can carry a field
			// selector. None of that belongs in a Referer, and the dashboard
			// loads nothing off-origin for one to be sent to anyway.
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", contentSecurityPolicy)
			// Only where TLS is actually served. Announcing HSTS over plain
			// HTTP would be wrong, and the loopback default is plain HTTP.
			if tlsTerminated {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
