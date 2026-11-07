package imagecache

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
)

type env struct {
	cache *Cache
	img   domain.Image
	key   ed25519.PrivateKey
	hits  *int
	body  *string
}

func setup(t *testing.T) env {
	t.Helper()
	body := "a disk of bytes"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/gone" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	pub, key, err := images.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.pub")
	if err := os.WriteFile(keys, []byte(images.FormatPublicKey(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := images.Digest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	img := domain.Image{ID: uuid.New(), Name: "forecrew-windows", Version: "1", OS: domain.OSWindows, Arch: domain.ArchAMD64, Format: domain.ImageVHDX,
		URI: srv.URL + "/disk", Digest: digest, BuiltAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	img.Signature = images.Sign(key, img)
	c := New(filepath.Join(dir, "images"), keys, ".vhdx", domain.ImageVHDX, nil)
	return env{c, img, key, &hits, &body}
}

func TestEnsureFetchesOnceAndVerifies(t *testing.T) {
	e := setup(t)
	path, err := e.cache.Ensure(context.Background(), e.img)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".vhdx" || filepath.Dir(path) != e.cache.Dir {
		t.Fatalf("path = %s", path)
	}
	if _, err := e.cache.Ensure(context.Background(), e.img); err != nil || *e.hits != 1 {
		t.Fatalf("second Ensure: %v, %d fetches", err, *e.hits)
	}
}

func TestEnsureRefuses(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	wrongFormat := e.img
	wrongFormat.Format = domain.ImageQCOW2
	if _, err := e.cache.Ensure(ctx, wrongFormat); err == nil || !strings.Contains(err.Error(), "vhdx") {
		t.Errorf("format: %v", err)
	}
	kernel := e.img
	kernel.Contents.Kernel = &domain.ImageKernel{URI: "https://x/k", Digest: e.img.Digest}
	if _, err := e.cache.Ensure(ctx, kernel); err == nil {
		t.Error("kernel image accepted")
	}
	unsigned := e.img
	unsigned.Signature = ""
	if _, err := e.cache.Ensure(ctx, unsigned); !errors.Is(err, images.ErrSignature) {
		t.Errorf("unsigned: %v", err)
	}
	badDigest := e.img
	badDigest.Digest = "sha256:zz"
	if _, err := e.cache.Ensure(ctx, badDigest); err == nil {
		t.Error("bad digest accepted")
	}
	// Signed by a key nobody trusts.
	_, other, _ := images.GenerateKey()
	foreign := e.img
	foreign.Signature = images.Sign(other, foreign)
	if _, err := e.cache.Ensure(ctx, foreign); !errors.Is(err, images.ErrSignature) {
		t.Errorf("foreign: %v", err)
	}
	// What the server sends is not what was signed.
	*e.body = "something else"
	if _, err := e.cache.Ensure(ctx, e.img); !errors.Is(err, images.ErrDigest) {
		t.Errorf("tampered: %v", err)
	}
	if entries, _ := os.ReadDir(e.cache.Dir); len(entries) != 0 {
		t.Errorf("left %d files", len(entries))
	}
	gone := e.img
	gone.URI = strings.Replace(e.img.URI, "/disk", "/gone", 1)
	gone.Signature = images.Sign(e.key, gone)
	*e.body = "a disk of bytes"
	if _, err := e.cache.Ensure(ctx, gone); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("gone: %v", err)
	}
	s3 := e.img
	s3.URI = "s3://bucket/disk"
	s3.Signature = images.Sign(e.key, s3)
	if _, err := e.cache.Ensure(ctx, s3); err == nil || !strings.Contains(err.Error(), "only http and https") {
		t.Errorf("s3: %v", err)
	}
}

func TestEnsureFetchesAgainWhatWasDamaged(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	path, err := e.cache.Path(e.img.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.cache.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cache.Ensure(ctx, e.img); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a disk of bytes" || *e.hits != 1 {
		t.Fatalf("file %q after %d fetches", got, *e.hits)
	}
	// Damaged and the server has nothing better.
	e2 := setup(t)
	*e2.body = "different"
	p2, _ := e2.cache.Path(e2.img.Digest)
	_ = os.MkdirAll(e2.cache.Dir, 0o700)
	_ = os.WriteFile(p2, []byte("damaged"), 0o600)
	if _, err := e2.cache.Ensure(ctx, e2.img); !errors.Is(err, images.ErrDigest) {
		t.Fatalf("err = %v", err)
	}
}

// A file that changes under the cache is checked again, not trusted for the
// life of the process.
func TestEnsureChecksAgainAFileThatChanged(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	path, err := e.cache.Ensure(ctx, e.img)
	if err != nil {
		t.Fatal(err)
	}
	// Same size, written since: the digest no longer matches, and the file is
	// fetched again.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len(diskBodyForTest))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cache.Ensure(ctx, e.img); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != diskBodyForTest || *e.hits != 2 {
		t.Fatalf("file %q after %d fetches", got, *e.hits)
	}
	// Truncated: a different size.
	if err := os.Truncate(path, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cache.Ensure(ctx, e.img); err != nil || *e.hits != 3 {
		t.Fatalf("err %v after %d fetches", err, *e.hits)
	}
	// Removed: fetched.
	_ = os.Remove(path)
	if _, err := e.cache.Ensure(ctx, e.img); err != nil || *e.hits != 4 {
		t.Fatalf("err %v after %d fetches", err, *e.hits)
	}
}

const diskBodyForTest = "a disk of bytes"

func TestPrune(t *testing.T) {
	e := setup(t)
	path, err := e.cache.Ensure(context.Background(), e.img)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(path, old, old)
	removed, err := e.cache.Prune(24 * time.Hour)
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	// Gone from disk, so it is verified again when it comes back.
	if _, err := e.cache.Ensure(context.Background(), e.img); err != nil || *e.hits != 2 {
		t.Fatalf("err %v, %d fetches", err, *e.hits)
	}
}
