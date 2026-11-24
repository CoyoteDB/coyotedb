package agent

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"coyotedb.com/coyote/dashboard"
)

// dashCSP is dashboard/API.md §2.5. The build has no inline scripts or
// styles, so it needs no hashes or nonces.
const dashCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders go on every response the dashboard handler writes.
func securityHeaders(h http.Header, csp string) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// staticTypes are fixed here rather than taken from the OS: on Windows
// the registry can map .js to text/plain, which nosniff then refuses.
var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png",
	".ico": "image/x-icon", ".woff2": "font/woff2", ".woff": "font/woff", ".txt": "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

// staticHandler serves the embedded dashboard (API.md §16): files from
// dist/, index.html for any other GET outside /v1/, the fallback page when
// the binary was built without the dashboard.
type staticHandler struct {
	files fs.FS
	built bool
	start time.Time
}

func newStaticHandler(files fs.FS, built bool) *staticHandler {
	return &staticHandler{files: files, built: built, start: time.Now()}
}

func newDashboardStatic() *staticHandler { return newStaticHandler(dashboard.FS(), dashboard.Built()) }

func (s *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.built {
		securityHeaders(w.Header(), dashboard.FallbackCSP)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(dashboard.Fallback))
		}
		return
	}
	securityHeaders(w.Header(), dashCSP)
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	b, err := fs.ReadFile(s.files, name)
	if err != nil || strings.HasPrefix(name, ".") {
		// Unknown path: the app routes in the browser. A missing hashed
		// asset is a real 404 (an old page asking for a file of another
		// build), so the browser does not get HTML as a script.
		if strings.HasPrefix(name, "assets/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		name = "index.html"
		if b, err = fs.ReadFile(s.files, name); err != nil {
			http.Error(w, "dashboard files are missing from this build", http.StatusInternalServerError)
			return
		}
	}
	ctype := staticTypes[path.Ext(name)]
	if ctype == "" {
		ctype = mime.TypeByExtension(path.Ext(name))
	}
	if ctype == "" {
		ctype = http.DetectContentType(b)
	}
	w.Header().Set("Content-Type", ctype)
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, s.start, bytes.NewReader(b))
}
