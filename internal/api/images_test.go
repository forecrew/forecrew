package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/mcp"
)

// fakeImages is an api.ImageAdmin that records what it is asked.
type fakeImages struct {
	mu      sync.Mutex
	added   []images.AddInput
	deleted []string
	err     error
}

func (f *fakeImages) AddImage(_ context.Context, _ domain.Actor, in images.AddInput) (mcp.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, in)
	return mcp.ImageInfo{Name: in.Name, Version: in.Version, Scope: "organization"}, f.err
}

func (f *fakeImages) DeleteImage(_ context.Context, _ domain.Actor, ref string) (mcp.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ref)
	return mcp.ImageInfo{}, f.err
}

func TestImageRoutes(t *testing.T) {
	e := newEnv(t)
	e.svc.ListImagesFunc = func(context.Context, domain.Actor, mcp.ListImagesInput) ([]mcp.ImageInfo, error) { return nil, nil }

	r := e.as(t, token, "GET", "/api/v1/images?name=linux&os=linux&arch=amd64&format=rootfs&limit=7", "")
	if r.status != http.StatusOK || strings.TrimSpace(r.body) != "[]" {
		t.Fatalf("list = %d %s", r.status, r.body)
	}
	calls := e.svc.Calls()
	if got := calls[len(calls)-1].Input; got != (mcp.ListImagesInput{Name: "linux", OS: "linux", Arch: "amd64", Format: "rootfs", Limit: 7}) {
		t.Errorf("list input = %+v", got)
	}
	if r := e.as(t, token, "GET", "/api/v1/images?limit=x", ""); r.status != http.StatusBadRequest {
		t.Errorf("bad limit = %d", r.status)
	}

	body := `{"name":"linux","version":"1","os":"linux","arch":"amd64","format":"qcow2","uri":"https://i.example.com/l.qcow2","digest":"sha256:` + strings.Repeat("a", 64) + `","signature":"s","built_at":"2026-01-01T00:00:00Z","instance":true}`
	if r := e.as(t, token, "POST", "/api/v1/images", body); r.status != http.StatusCreated {
		t.Fatalf("add = %d %s", r.status, r.body)
	}
	if r := e.as(t, token, "POST", "/api/v1/images", `{"bogus":1}`); r.status != http.StatusBadRequest {
		t.Errorf("unknown field = %d", r.status)
	}
	if r := e.as(t, token, "DELETE", "/api/v1/images/abc", ""); r.status != http.StatusOK {
		t.Fatalf("delete = %d %s", r.status, r.body)
	}
	if len(e.images.added) != 1 || !e.images.added[0].Instance || e.images.added[0].Name != "linux" || e.images.added[0].Digest == "" ||
		len(e.images.deleted) != 1 || e.images.deleted[0] != "abc" {
		t.Errorf("admin saw %+v, %v", e.images.added, e.images.deleted)
	}
}
