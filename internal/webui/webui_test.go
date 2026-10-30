package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var built = fstest.MapFS{
	"index.html":          {Data: []byte("<!doctype html><title>Forecrew</title>")},
	"assets/app-1a2b.js":  {Data: []byte("console.log(1)")},
	"assets/app-1a2b.css": {Data: []byte("body{}")},
	"favicon.svg":         {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
}

func get(t *testing.T, h http.Handler, method, target string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return res, string(body)
}

func TestServesFiles(t *testing.T) {
	h := newHandler(built)
	tests := []struct {
		path, contentType, cache, body string
	}{
		{"/assets/app-1a2b.js", "text/javascript", "public, max-age=31536000, immutable", "console.log(1)"},
		{"/assets/app-1a2b.css", "text/css", "public, max-age=31536000, immutable", "body{}"},
		{"/favicon.svg", "image/svg+xml", "no-cache", "<svg"},
		{"/", "text/html", "no-cache", "Forecrew"},
		{"/index.html", "text/html", "no-cache", "Forecrew"},
	}
	for _, tt := range tests {
		res, body := get(t, h, "GET", tt.path)
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), tt.contentType) ||
			res.Header.Get("Cache-Control") != tt.cache || !strings.Contains(body, tt.body) {
			t.Errorf("%s: %d %q %q %q", tt.path, res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Cache-Control"), body)
		}
	}
}

func TestSPAFallback(t *testing.T) {
	h := newHandler(built)
	for _, p := range []string{"/inbox", "/goals/GOAL-3", "/jobs/JOB-1", "/../etc/passwd"} {
		res, body := get(t, h, "GET", p)
		if res.StatusCode != 200 || !strings.Contains(body, "Forecrew") || res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q cache %q", p, res.StatusCode, body, res.Header.Get("Cache-Control"))
		}
	}
}

func TestMissingAssetIsNotFound(t *testing.T) {
	h := newHandler(built)
	for _, p := range []string{"/assets/missing.js", "/assets/", "/assets/old-1a2b.css"} {
		if res, body := get(t, h, "GET", p); res.StatusCode != 404 || strings.Contains(body, "Forecrew") {
			t.Errorf("%s: %d %q", p, res.StatusCode, body)
		}
	}
}

func TestReservedPathsAreNotTakenOver(t *testing.T) {
	h := newHandler(built)
	for _, p := range []string{"/api/v1/nope", "/api", "/mcp", "/mcp/x", "/webhooks/github", "/worker/v1/lease", "/healthz"} {
		if res, body := get(t, h, "GET", p); res.StatusCode != 404 || strings.Contains(body, "Forecrew") {
			t.Errorf("%s: %d %q", p, res.StatusCode, body)
		}
	}
	// A path that only starts with a reserved word is a UI route.
	if res, _ := get(t, h, "GET", "/apiary"); res.StatusCode != 200 {
		t.Errorf("/apiary: %d", res.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHandler(built)
	for _, p := range []string{"/", "/assets/app-1a2b.js", "/api/x"} {
		res, _ := get(t, h, "GET", p)
		if csp := res.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'") {
			t.Errorf("%s: CSP %q", p, csp)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", p)
		}
	}
}

func TestMethods(t *testing.T) {
	h := newHandler(built)
	if res, _ := get(t, h, "POST", "/"); res.StatusCode != 405 || res.Header.Get("Allow") == "" {
		t.Errorf("POST: %d", res.StatusCode)
	}
	if res, body := get(t, h, "HEAD", "/inbox"); res.StatusCode != 200 || body != "" {
		t.Errorf("HEAD: %d %q", res.StatusCode, body)
	}
}

func TestNotBuilt(t *testing.T) {
	h := newHandler(fstest.MapFS{".gitkeep": {}})
	res, body := get(t, h, "GET", "/inbox")
	if res.StatusCode != http.StatusServiceUnavailable || body != "UI not built: run make web\n" ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || res.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("%d %q %v", res.StatusCode, body, res.Header)
	}
	if res, body := get(t, h, "HEAD", "/"); res.StatusCode != http.StatusServiceUnavailable || body != "" {
		t.Errorf("HEAD: %d %q", res.StatusCode, body)
	}
}

func TestHandlerUsesEmbeddedDist(t *testing.T) {
	// The repository holds only a .gitkeep, so a plain checkout is "not built"; a
	// local `make web` makes it serve index.html. Either way the handler works.
	res, _ := get(t, Handler(), "GET", "/")
	if res.StatusCode != 200 && res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d", res.StatusCode)
	}
}
