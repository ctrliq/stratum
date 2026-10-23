package dashboard

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// The SPA handler.

// reservedPrefixes never fall back to index.html: a miss there is a 404 from
// the API surface, not a client-side route.
var reservedPrefixes = []string{"/api/", k8sPrefix + "/", "/healthz"}

// spaHandler serves an fs.FS as a single-page app.
type spaHandler struct {
	fsys fs.FS
	// modTime stamps index.html so conditional requests work; assets are
	// content-hashed and immutable.
	modTime time.Time
}

func newSPAHandler(fsys fs.FS) *spaHandler {
	return &spaHandler{fsys: fsys, modTime: time.Now()}
}

func reserved(p string) bool {
	for _, pre := range reservedPrefixes {
		if strings.HasPrefix(p, pre) || p == strings.TrimSuffix(pre, "/") {
			return true
		}
	}
	return false
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRawJSON(w, http.StatusMethodNotAllowed, `{"error":"method-not-allowed"}`)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	if reserved(p) {
		writeRawJSON(w, http.StatusNotFound, `{"error":"not-found","message":"no such route"}`)
		return
	}
	name := strings.TrimPrefix(p, "/")
	if name != "" && name != "index.html" {
		if f, err := h.fsys.Open(name); err == nil {
			st, serr := f.Stat()
			if serr == nil && !st.IsDir() {
				// A year and immutable is only safe for a content-hashed
				// name, where new bytes always arrive at a new URL. That is
				// everything Vite emits under /assets/ — the bundle, the CSS
				// and the fonts, which it rewrites the @font-face urls for.
				// Anything else (index.html, public/ files served at the
				// root) keeps its name across builds and must revalidate.
				if strings.HasPrefix(p, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				serveFile(w, r, name, st, f)
				return
			}
			_ = f.Close()
		}
		// An /assets/ URL is a build artefact, never a client-side route, so a
		// miss is a stale or broken deployment rather than something the router
		// should handle. Falling through would answer it with the SPA's HTML at
		// 200, and the browser would then report a MIME or syntax error against
		// a script that is really just absent — sending whoever debugs it after
		// the wrong bug.
		if strings.HasPrefix(p, "/assets/") {
			writeRawJSON(w, http.StatusNotFound, `{"error":"not-found","message":"no such asset"}`)
			return
		}
	}
	h.serveIndex(w, r)
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, st fs.FileInfo, f fs.File) {
	defer f.Close()
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, st.ModTime(), rs)
		return
	}
	// embed.FS files are ReadSeekers; other fs.FS implementations may not be.
	w.Header().Set("Content-Type", contentType(name))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.fsys.Open("index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, placeholderHTML)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "index.html", h.modTime, rs)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
}

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	}
	return "application/octet-stream"
}

const placeholderHTML = `<!doctype html><meta charset="utf-8"><title>Stratum dashboard</title>
<body style="font:14px system-ui;margin:3rem"><h1>Stratum dashboard</h1>
<p>The dashboard SPA is not embedded in this build (<code>noui</code>). The API is available under <code>/api/v1/</code> and the Kubernetes proxy under <code>/k8s/</code>.</p>`

// writeRawJSON serves an already-encoded JSON error body.
//
// Not http.Error: it sets Content-Type to text/plain unconditionally, so it
// overwrites a value set beforehand rather than deferring to it. The nosniff
// header http.Error also sets is worth keeping, so it is set here too.
func writeRawJSON(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
