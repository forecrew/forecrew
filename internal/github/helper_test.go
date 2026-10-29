package github_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func privateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKey
}

func pkcs1PEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// env is a fake GitHub API with a Client pointed at it. Installation lookup
// and token minting are pre-registered for acme/app (installation 42).
type env struct {
	t      *testing.T
	mux    *http.ServeMux
	clock  *testkit.Clock
	client *github.Client

	mu          sync.Mutex
	lookups     int
	tokenBodies []map[string]any
	apiAuth     []string // Authorization headers of non-App calls
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, mux: http.NewServeMux(), clock: testkit.NewClock(t0)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		if !strings.HasPrefix(r.URL.Path, "/app/") && !strings.HasSuffix(r.URL.Path, "/installation") {
			e.apiAuth = append(e.apiAuth, r.Header.Get("Authorization"))
		}
		e.mu.Unlock()
		e.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	e.mux.HandleFunc("GET /repos/acme/app/installation", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "need jwt", http.StatusUnauthorized)
			return
		}
		e.mu.Lock()
		e.lookups++
		e.mu.Unlock()
		fmt.Fprint(w, `{"id": 42}`)
	})
	e.mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.tokenBodies = append(e.tokenBodies, body)
		n := len(e.tokenBodies)
		e.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token": "ghs_%d", "expires_at": %q}`, n, e.clock.Now().Add(time.Hour).Format(time.RFC3339))
	})

	c, err := github.New(github.Config{
		AppID: 123456, PrivateKey: pkcs1PEM(privateKey(t)), BaseURL: srv.URL,
		Now: e.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.client = c
	return e
}

// fixture returns the bytes of testdata/<dir>/<name>.
func fixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve answers pattern ("GET /repos/...") with the API fixture and status.
func (e *env) serve(pattern string, status int, name string) {
	body := fixture(e.t, "api", name)
	e.mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

// handle registers a custom handler.
func (e *env) handle(pattern string, h http.HandlerFunc) { e.mux.HandleFunc(pattern, h) }

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ecKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
