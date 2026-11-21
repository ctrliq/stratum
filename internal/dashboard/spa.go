package dashboard

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// A miss here is a 404 from the API surface, not a client-side route.
var reservedPrefixes = []string{"/api/", k8sPrefix + "/", "/healthz"}

// spaHandler serves an fs.FS as a single-page app.
type spaHandler struct {
	fsys fs.FS
	// Stamps index.html so conditional requests work.
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
				// Immutable is only safe under /assets/: Vite content-hashes those.
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
		// A miss under /assets/ is a stale deployment; HTML at 200 blames the script.
		if strings.HasPrefix(p, "/assets/") {
			writeRawJSON(w, http.StatusNotFound, `{"error":"not-found","message":"no such asset"}`)
			return
		}
	}
	h.serveIndex(w, r)
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, st fs.FileInfo, f fs.File) {
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeRawJSON(w, http.StatusInternalServerError, `{"error":"internal","message":"asset is not seekable"}`)
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
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
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeRawJSON(w, http.StatusInternalServerError, `{"error":"internal","message":"index.html is not seekable"}`)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "index.html", h.modTime, rs)
}

const placeholderHTML = `<!doctype html><meta charset="utf-8"><title>Stratum dashboard</title>
<body style="font:14px system-ui;margin:3rem"><h1>Stratum dashboard</h1>
<p>The dashboard SPA is not embedded in this build (<code>noui</code>). The API is available under <code>/api/v1/</code> and the Kubernetes proxy under <code>/k8s/</code>.</p>`

// Not http.Error: that forces Content-Type to text/plain; the nosniff header is kept.
func writeRawJSON(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
