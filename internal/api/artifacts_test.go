package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

func serveArtifact(e *env, a domain.Artifact, content string) {
	e.svc.OpenArtifactFunc = func(_ context.Context, _ domain.Actor, id string) (domain.Artifact, io.ReadCloser, error) {
		if id != a.ID.String() {
			return domain.Artifact{}, nil, fmt.Errorf("artifact %s: %w", id, mcp.ErrNotFound)
		}
		return a, io.NopCloser(strings.NewReader(content)), nil
	}
}

func TestArtifactDownload(t *testing.T) {
	tests := []struct {
		contentType, name string
		disposition       string
		served            string
	}{
		{"text/plain", "logs/run.log", `inline; filename=run.log`, "text/plain; charset=utf-8"},
		{"text/plain; charset=iso-8859-1", "a.txt", `inline; filename=a.txt`, "text/plain; charset=utf-8"},
		{"image/png", "shots/home.png", `inline; filename=home.png`, "image/png"},
		{"image/jpeg", "a.jpg", `inline; filename=a.jpg`, "image/jpeg"},
		{"application/json", "r.json", `inline; filename=r.json`, "application/json"},
		{"text/html", "report.html", `attachment; filename=report.html`, "application/octet-stream"},
		{"TEXT/HTML; charset=utf-8", "report2.html", `attachment; filename=report2.html`, "application/octet-stream"},
		{"image/svg+xml", "logo.svg", `attachment; filename=logo.svg`, "application/octet-stream"},
		{"application/xhtml+xml", "x.xhtml", `attachment; filename=x.xhtml`, "application/octet-stream"},
		{"text/xml", "reports/unit.xml", `attachment; filename=unit.xml`, "application/octet-stream"},
		{"application/octet-stream", "core.bin", `attachment; filename=core.bin`, "application/octet-stream"},
		{"not a type;;", "weird", `attachment; filename=weird`, "application/octet-stream"},
		{"", "empty", `attachment; filename=empty`, "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.contentType+" "+tt.name, func(t *testing.T) {
			e := newEnv(t)
			content := "<script>alert(1)</script>"
			a := domain.Artifact{ID: domain.NewID(), Name: tt.name, ContentType: tt.contentType, Size: int64(len(content))}
			serveArtifact(e, a, content)
			r := e.do(t, "GET", "/api/v1/artifacts/"+a.ID.String(), "", "bearer")
			if r.status != 200 || r.body != content {
				t.Fatalf("%d %q", r.status, r.body)
			}
			h := r.header
			if h.Get("Content-Disposition") != tt.disposition || h.Get("Content-Type") != tt.served {
				t.Errorf("disposition %q, type %q; want %q, %q", h.Get("Content-Disposition"), h.Get("Content-Type"), tt.disposition, tt.served)
			}
			if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
				h.Get("Content-Length") != fmt.Sprint(len(content)) {
				t.Errorf("headers = %v", h)
			}
		})
	}
}

func TestArtifactDownloadErrors(t *testing.T) {
	e := newEnv(t)
	a := domain.Artifact{ID: domain.NewID(), Name: "a.txt", ContentType: "text/plain"}
	serveArtifact(e, a, "")
	if r := e.do(t, "GET", "/api/v1/artifacts/"+domain.NewID().String(), "", "bearer"); r.status != 404 || errorCode(t, r) != "not_found" {
		t.Errorf("unknown: %d %s", r.status, r.body)
	}
	if r := e.do(t, "GET", "/api/v1/artifacts/"+a.ID.String(), "", ""); r.status != 401 {
		t.Errorf("without a token: %d", r.status)
	}
	e.svc.OpenArtifactFunc = func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error) {
		return domain.Artifact{}, nil, fmt.Errorf("no storage: %w", mcp.ErrUnavailable)
	}
	if r := e.do(t, "GET", "/api/v1/artifacts/"+a.ID.String(), "", "bearer"); r.status != 503 || errorCode(t, r) != "unavailable" {
		t.Errorf("unavailable: %d %s", r.status, r.body)
	}
}

func TestArtifactDownloadViaSession(t *testing.T) {
	e := newEnv(t)
	a := domain.Artifact{ID: domain.NewID(), Name: "a.txt", ContentType: "text/plain", Size: 2}
	serveArtifact(e, a, "hi")
	if r := e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, ""); r.status != 200 {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	if r := e.do(t, "GET", "/api/v1/artifacts/"+a.ID.String(), "", ""); r.status != 200 || r.body != "hi" {
		t.Errorf("with the session cookie: %d %q", r.status, r.body)
	}
}

// A storage that fails halfway must not look like a complete download.
func TestArtifactDownloadCutShort(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "acme"}
	if err := st.Accounts().Create(t.Context(), &acc); err != nil {
		t.Fatal(err)
	}
	if err := st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: acc.ID, Name: "laptop", TokenHash: mcp.HashToken(token)}); err != nil {
		t.Fatal(err)
	}
	svc := &testkit.ToolService{}
	svc.OpenArtifactFunc = func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error) {
		body := io.NopCloser(io.MultiReader(strings.NewReader("part"), iotest.ErrReader(errors.New("storage down"))))
		return domain.Artifact{Name: "a.txt", ContentType: "text/plain", Size: 100}, body, nil
	}
	hs := httptest.NewServer(api.New(api.Config{Tools: svc, Artifacts: svc, Tokens: st.MCPTokens(), Accounts: st.Accounts(), Logger: slog.New(slog.DiscardHandler)}).Handler())
	t.Cleanup(hs.Close)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", hs.URL+"/api/v1/artifacts/"+domain.NewID().String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hs.Client().Do(req)
	if err != nil {
		return // the connection was cut before the headers left
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("a truncated download read cleanly")
	}
}

func TestNoArtifactEndpointWithoutOpener(t *testing.T) {
	st := testkit.NewStore(nil)
	h := api.New(api.Config{Tools: &testkit.ToolService{}, Tokens: st.MCPTokens(), Accounts: st.Accounts(), Logger: slog.New(slog.DiscardHandler)}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/artifacts/"+domain.NewID().String(), nil))
	if rec.Code != 404 {
		t.Errorf("status = %d", rec.Code)
	}
}
