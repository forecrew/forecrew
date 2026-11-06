// Package webui serves the single-page web UI that is built into
// internal/webui/dist (see docs/ui.md). The build output is not committed:
// without it the handler answers with a "UI not built" page, so Go builds and
// tests never need Node.
package webui

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// ContentSecurityPolicy is sent with every response. The UI loads only its own
// scripts, styles, fonts and API.
const ContentSecurityPolicy = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// notBuilt is the page served when dist holds no build.
const notBuilt = "UI not built: run make web\n"

// reserved are the paths of the other handlers; the UI never answers for them.
var reserved = []string{"/api", "/auth", "/mcp", "/webhooks", "/worker", "/healthz"}

// Handler serves the built UI: files that exist, and index.html for any other
// path (client-side routes) except the reserved ones.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil { // dist is a constant directory of the embed, so this cannot happen
		panic(err)
	}
	return newHandler(sub)
}

func newHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name, ok := resolve(fsys, r.URL.Path)
		switch {
		case !ok:
			http.NotFound(w, r)
		case name == "":
			notBuiltPage(w, r)
		default:
			if strings.HasPrefix(name, "assets/") {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				h.Set("Cache-Control", "no-cache")
			}
			serveFile(w, r, fsys, name)
		}
	})
}

// resolve returns the file to serve for a URL path: the file itself when it
// exists, else index.html (client-side routes). ok is false for the paths of
// the other handlers and for a missing asset: a stale hashed file must not be
// answered with HTML. An empty name means there is no build.
func resolve(fsys fs.FS, urlPath string) (name string, ok bool) {
	if isReserved(urlPath) {
		return "", false
	}
	name = strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if isFile(fsys, name) && name != "index.html" {
		return name, true
	}
	if strings.HasPrefix(urlPath, "/assets/") {
		return "", false
	}
	if !isFile(fsys, "index.html") {
		return "", true
	}
	return "index.html", true
}

func notBuiltPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(notBuilt))
	}
}

func isReserved(p string) bool {
	for _, prefix := range reserved {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func isFile(fsys fs.FS, name string) bool {
	st, err := fs.Stat(fsys, name)
	return err == nil && !st.IsDir()
}

// serveFile writes name with the content type of its extension.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	f, err := fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok { // embed.FS files are seekers; this keeps other file systems working
		b, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(b)
	}
	http.ServeContent(w, r, name, time.Time{}, rs)
}
