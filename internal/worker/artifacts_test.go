package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func tempFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadArtifact(t *testing.T) {
	id := domain.NewID()
	content := strings.Repeat("<testsuite/>\n", 1000)
	sum := sha256.Sum256([]byte(content))
	var got struct {
		method, path, auth, contentType string
		length                          int64
		body                            string
	}
	artifactID := domain.NewID()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.auth, got.contentType, got.length = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.ContentLength
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": artifactID, "name": "reports/unit-1.xml", "size": len(b), "sha256": hex.EncodeToString(sum[:])})
	})
	a, err := c.UploadArtifact(t.Context(), id, "reports/unit-1.xml", tempFile(t, "unit.xml", content))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != artifactID || a.Size != int64(len(content)) || a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("artifact = %+v", a)
	}
	if got.method != "PUT" || got.path != "/worker/v1/jobs/"+id.String()+"/artifacts/reports/unit-1.xml" ||
		got.auth != "Bearer fcw_secret-token" || got.length != int64(len(content)) || got.body != content {
		t.Errorf("request = %+v", got)
	}
	if !strings.HasPrefix(got.contentType, "text/xml") && !strings.HasPrefix(got.contentType, "application/xml") {
		t.Errorf("content type of .xml = %q", got.contentType)
	}
}

func TestUploadArtifactContentType(t *testing.T) {
	tests := []struct{ file, content, want string }{
		{"runlog", "plain text\n", "text/plain; charset=utf-8"},
		{"shot", "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 20), "image/png"},
		{"blob", "\x00\x01\x02", "application/octet-stream"},
		{"empty", "", "text/plain; charset=utf-8"},
		{"data.json", "{}", "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			var gotType, gotBody string
			var gotLength int64
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotType, gotBody, gotLength = r.Header.Get("Content-Type"), string(b), r.ContentLength
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"` + domain.NewID().String() + `"}`))
			})
			if _, err := c.UploadArtifact(t.Context(), domain.NewID(), "x/"+tt.file, tempFile(t, tt.file, tt.content)); err != nil {
				t.Fatal(err)
			}
			if gotLength != int64(len(tt.content)) { // never chunked, the empty file included
				t.Errorf("Content-Length = %d, want %d", gotLength, len(tt.content))
			}
			if gotType != tt.want || gotBody != tt.content {
				t.Errorf("type %q body %q, want %q, whole file", gotType, gotBody, tt.want)
			}
		})
	}
}

func TestUploadArtifactRetries(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		switch calls.Add(1) {
		case 1:
			writeErr(w, http.StatusInternalServerError, "internal", "boom")
		case 2:
			// A dropped connection.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + domain.NewID().String() + `","size":5}`))
		}
	})
	a, err := c.UploadArtifact(t.Context(), domain.NewID(), "a.log", tempFile(t, "a.log", "hello"))
	if err != nil || a.Size != 5 || calls.Load() != 3 {
		t.Fatalf("artifact %+v, err %v, %d calls", a, err, calls.Load())
	}
	for i, b := range bodies {
		if b != "hello" {
			t.Errorf("attempt %d sent %q, want the whole file again", i+1, b)
		}
	}
}

func TestUploadArtifactGivesUp(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeErr(w, http.StatusBadGateway, "internal", "down")
	})
	_, err := c.UploadArtifact(t.Context(), domain.NewID(), "a.log", tempFile(t, "a.log", "x"))
	if apiErr, ok := errors.AsType[*APIError](err); !ok || apiErr.Status != 502 || calls.Load() != uploadAttempts {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.UploadArtifact(canceled, domain.NewID(), "a.log", tempFile(t, "a.log", "x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestUploadArtifactPermanentErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{"lease lost", 409, "lease_lost", ErrLeaseLost},
		{"too large", 413, "too_large", ErrTooLarge},
		{"no storage", 503, "unavailable", ErrEvidenceUnavailable},
		{"revoked", 401, "unauthorized", ErrUnauthorized},
		{"unknown job", 404, "not_found", ErrJobNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeErr(w, tt.status, tt.code, "no")
			})
			_, err := c.UploadArtifact(t.Context(), domain.NewID(), "a.log", tempFile(t, "a.log", "x"))
			if !errors.Is(err, tt.want) || calls.Load() != 1 {
				t.Fatalf("err = %v after %d calls, want %v once", err, calls.Load(), tt.want)
			}
		})
	}
}

func TestUploadArtifactBadInput(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	dir := t.TempDir()
	for name, path := range map[string]string{
		"../escape.log": tempFile(t, "a", "x"), "a.log": filepath.Join(dir, "missing"), "d.log": dir,
	} {
		if _, err := c.UploadArtifact(t.Context(), domain.NewID(), name, path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests sent for unusable files", calls.Load())
	}
	// A 200 is not the 201 the contract promises, and neither is garbage.
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("nope"))
		},
	} {
		c := newTestClient(t, h)
		if _, err := c.UploadArtifact(t.Context(), domain.NewID(), "a.log", tempFile(t, "a.log", "x")); err == nil {
			t.Error("bad answer accepted")
		}
	}
}

func TestUploadArtifactNetworkErrorDoesNotLeakToken(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "fcw_secret-token", nil)
	c.BackoffMin, c.BackoffMax = 1, 2
	_, err := c.UploadArtifact(t.Context(), domain.NewID(), "a.log", tempFile(t, "a.log", "x"))
	if err == nil || strings.Contains(err.Error(), "fcw_secret-token") || strings.Contains(err.Error(), "127.0.0.1:1/worker") {
		t.Fatalf("err = %v", err)
	}
}
