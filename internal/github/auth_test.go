package github_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

func TestNewRejectsBadConfig(t *testing.T) {
	good := pkcs1PEM(privateKey(t))
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(privateKey(t))
	ec, _ := x509.MarshalPKCS8PrivateKey(ecKey(t))
	tests := []struct {
		name string
		cfg  github.Config
		ok   bool
	}{
		{"pkcs1", github.Config{AppID: 1, PrivateKey: good}, true},
		{"pkcs8", github.Config{AppID: 1, PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})}, true},
		{"base url without slash", github.Config{AppID: 1, PrivateKey: good, BaseURL: "http://localhost:1/api"}, true},
		{"no app id", github.Config{PrivateKey: good}, false},
		{"not pem", github.Config{AppID: 1, PrivateKey: []byte("nope")}, false},
		{"garbage der", github.Config{AppID: 1, PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}})}, false},
		{"not rsa", github.Config{AppID: 1, PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ec})}, false},
		{"bad base url", github.Config{AppID: 1, PrivateKey: good, BaseURL: "http://[::1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := github.New(tt.cfg)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestJWT(t *testing.T) {
	e := newEnv(t)
	var jwt string
	e.handle("GET /repos/acme/jwt/installation", func(w http.ResponseWriter, r *http.Request) {
		jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, _ = w.Write([]byte(`{"id": 1}`))
	})
	ok, err := e.client.IsInstalled(t.Context(), "acme/jwt")
	noErr(t, err)
	if !ok {
		t.Fatal("not installed")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", jwt)
	}
	dec := base64.RawURLEncoding
	hdr, _ := dec.DecodeString(parts[0])
	if string(hdr) != `{"alg":"RS256","typ":"JWT"}` {
		t.Fatalf("header = %s", hdr)
	}
	raw, _ := dec.DecodeString(parts[1])
	var claims struct {
		Iss      string
		Iat, Exp int64
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "123456" || claims.Iat != t0.Add(-time.Minute).Unix() || claims.Exp != t0.Add(9*time.Minute).Unix() {
		t.Fatalf("claims = %+v", claims)
	}
	sig, _ := dec.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&privateKey(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
}

func TestIsInstalled(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/none/installation", http.StatusNotFound, "error_not_found.json")
	e.handle("GET /repos/acme/boom/installation", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	ctx := t.Context()
	if ok, err := e.client.IsInstalled(ctx, "acme/app"); err != nil || !ok {
		t.Fatalf("app = %v, %v", ok, err)
	}
	if ok, err := e.client.IsInstalled(ctx, "acme/none"); err != nil || ok {
		t.Fatalf("none = %v, %v", ok, err)
	}
	if _, err := e.client.IsInstalled(ctx, "acme/boom"); err == nil {
		t.Fatal("want error")
	}
	_, err := e.client.IsInstalled(ctx, "nonsense")
	wantErr(t, err, github.ErrInvalid)
}

func TestInstallationToken(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	tok, err := e.client.InstallationToken(ctx, "acme/app", github.AccessWrite)
	noErr(t, err)
	if tok.Value != "ghs_1" || !tok.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("token = %+v", tok)
	}
	body := e.tokenBodies[0]
	repos, _ := body["repositories"].([]any)
	perms, _ := body["permissions"].(map[string]any)
	if len(repos) != 1 || repos[0] != "app" || len(perms) != 1 || perms["contents"] != "write" {
		t.Fatalf("body = %v", body)
	}

	// Job tokens are never served from a cache, however recent the last one is.
	for i, access := range []github.TokenAccess{github.AccessWrite, github.AccessWrite, github.AccessRead, github.AccessRead} {
		e.clock.Advance(time.Minute)
		got, err := e.client.InstallationToken(ctx, "ACME/app", access)
		noErr(t, err)
		if want := fmt.Sprintf("ghs_%d", i+2); got.Value != want || !got.ExpiresAt.Equal(e.clock.Now().Add(time.Hour)) {
			t.Fatalf("%s token %d = %+v, want %s with a full hour", access, i, got, want)
		}
		if perms := e.tokenBodies[i+1]["permissions"].(map[string]any); perms["contents"] != string(access) {
			t.Fatalf("mint %d permissions = %v, want %s", i+1, perms, access)
		}
	}
	if len(e.tokenBodies) != 5 {
		t.Fatalf("mints = %d, want 5", len(e.tokenBodies))
	}
	if e.lookups != 1 {
		t.Fatalf("installation looked up %d times, want 1", e.lookups)
	}
}

func TestInstallationTokenErrors(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	_, err := e.client.InstallationToken(ctx, "acme/app", "admin")
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.InstallationToken(ctx, "bad", github.AccessRead)
	wantErr(t, err, github.ErrInvalid)
	e.serve("GET /repos/acme/none/installation", http.StatusNotFound, "error_not_found.json")
	_, err = e.client.InstallationToken(ctx, "acme/none", github.AccessRead)
	wantErr(t, err, github.ErrNotInstalled)

	// Minting answers 404: the installation was removed.
	e.handle("GET /repos/acme/gone/installation", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id": 77}`)) })
	e.serve("POST /app/installations/77/access_tokens", http.StatusNotFound, "error_not_found.json")
	_, err = e.client.InstallationToken(ctx, "acme/gone", github.AccessRead)
	wantErr(t, err, github.ErrNotInstalled)

	e.handle("GET /repos/acme/forbidden/installation", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id": 78}`)) })
	e.handle("POST /app/installations/78/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusUnprocessableEntity)
	})
	_, err = e.client.InstallationToken(ctx, "acme/forbidden", github.AccessRead)
	wantErr(t, err, github.ErrInvalid)
}
