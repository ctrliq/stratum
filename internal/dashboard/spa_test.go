package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSPACaching(t *testing.T) {
	h := newSPAHandler(fstest.MapFS{
		"index.html":                  {Data: []byte("<!doctype html>spa")},
		"assets/index-Cz580Qpd.js":    {Data: []byte("bundle")},
		"assets/Inter-Bold-BOs.woff2": {Data: []byte("font")},
		"favicon.svg":                 {Data: []byte("<svg/>")},
	})
	for _, tc := range []struct{ path, wantCache, wantType string }{
		// Vite content-hashes everything under assets/, fonts included, so a
		// new build always lands on a new URL.
		{"/assets/index-Cz580Qpd.js", "public, max-age=31536000, immutable", "text/javascript; charset=utf-8"},
		{"/assets/Inter-Bold-BOs.woff2", "public, max-age=31536000, immutable", "font/woff2"},
		// public/ files keep their names across builds; a year would pin a
		// stale favicon past an upgrade.
		{"/favicon.svg", "no-cache", "image/svg+xml"},
		{"/", "no-cache", "text/html; charset=utf-8"},
		{"/nodes", "no-cache", "text/html; charset=utf-8"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d", tc.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.wantCache {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, got, tc.wantCache)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.wantType {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.path, got, tc.wantType)
		}
	}
}

// A missing asset must not be answered with the SPA. Everything under
// /assets/ is content-hashed, so a request for one that is not there is a
// deployment that lost a file or a page holding a URL from an older build —
// and a 200 of text/html in place of the bundle turns that into a console
// error about JavaScript syntax, which is a bad place to start looking.
func TestSPAMissingAssetIs404NotTheIndex(t *testing.T) {
	h := newSPAHandler(fstest.MapFS{
		"index.html":               {Data: []byte("<!doctype html>spa")},
		"assets/index-Cz580Qpd.js": {Data: []byte("bundle")},
	})
	// Only paths INSIDE /assets/ qualify. A bare /assets cleans to one path
	// segment, which registry.js accepts as a route id, so a consumer may
	// legitimately registerScreen('assets') and that must still reach the SPA.
	for _, p := range []string{"/assets/index-deleted.js", "/assets/nested/gone.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404 (the SPA HTML would masquerade as the asset)", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct == "text/html; charset=utf-8" {
			t.Errorf("GET %s served %s; the browser will blame the script, not the deployment", p, ct)
		}
	}
	// The real asset and genuine client-side routes are untouched.
	for _, tc := range []struct {
		path string
		want int
	}{{"/assets/index-Cz580Qpd.js", http.StatusOK}, {"/nodes", http.StatusOK}, {"/", http.StatusOK},
		{"/assets", http.StatusOK}, {"/assets/", http.StatusOK}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s: got %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestSPARejectsNonReads(t *testing.T) {
	h := newSPAHandler(fstest.MapFS{"index.html": {Data: []byte("spa")}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nodes", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a client route: got %d, want 405", rec.Code)
	}
}

// Without an embedded dist (-tags noui) the handler must still answer, and
// must say why rather than serving a blank page.
func TestSPAPlaceholderWhenNothingIsEmbedded(t *testing.T) {
	rec := httptest.NewRecorder()
	newSPAHandler(fstest.MapFS{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if body := rec.Body.String(); body != placeholderHTML {
		t.Fatalf("body = %q, want the placeholder", body)
	}
}
