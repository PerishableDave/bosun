// Package webui serves Bosun's built-in web UI: plain HTML, CSS and ES-module
// JavaScript embedded into the binary. There is no build step and nothing is
// loaded from the network. The UI is a client of the /v1 API.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed web
var embedded embed.FS

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".json": "application/json",
}

// csp restricts the page to its own origin; the UI needs no inline script,
// inline style, or third-party resource.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves "/" (index.html, never cached) and "/static/..." (the
// assets). Any other path is a 404. Mount it for everything outside /v1/.
func Handler() http.Handler {
	root, err := fs.Sub(embedded, "web")
	if err != nil {
		panic(err) // the embed directive guarantees "web" exists
	}
	return handler(root)
}

func handler(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := ""
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			name = "index.html"
		case strings.HasPrefix(r.URL.Path, "/static/") && len(r.URL.Path) > len("/static/"):
			if n := path.Clean(r.URL.Path[1:]); strings.HasPrefix(n, "static/") {
				name = n
			}
		}
		if name == "" || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		ct := contentTypes[path.Ext(name)]
		if ct == "" {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		h.Set("X-Content-Type-Options", "nosniff")
		if name == "index.html" {
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Security-Policy", csp)
		} else {
			// Assets are small and embedded; revalidating on every load keeps
			// upgrades simple (no hashed file names).
			h.Set("Cache-Control", "no-cache")
		}
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(b)
	})
}
