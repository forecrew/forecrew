package testkit

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// pendingCode is an authorization code the fakes handed out.
type pendingCode struct {
	challenge string
	nonce     string
	used      bool
}

// codes is the authorization endpoint both fakes share: it hands out one
// code per sign-in and redeems each once, with its PKCE verifier.
type codes struct {
	mu   sync.Mutex
	next int
	all  map[string]*pendingCode
}

func (c *codes) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c.mu.Lock()
	c.next++
	code := "code-" + strconv.Itoa(c.next)
	if c.all == nil {
		c.all = map[string]*pendingCode{}
	}
	c.all[code] = &pendingCode{challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	c.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound) // #nosec G710 -- a fake provider of tests
}

// redeem spends a code and returns it; nil for a code that is unknown,
// used already, or sent with the wrong verifier.
func (c *codes) redeem(code, verifier string) *pendingCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.all[code]
	sum := sha256.Sum256([]byte(verifier))
	if p == nil || p.used || p.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		return nil
	}
	p.used = true
	return p
}

// FakeOIDC is an OpenID Connect provider over HTTPS: discovery, JWKS,
// authorization code with PKCE, and ID tokens with real RS256 signatures.
type FakeOIDC struct {
	*httptest.Server
	// Claims are the ID token's on top of the standard ones, which they
	// replace: set "nonce", "aud", "exp" or "iss" to forge a token.
	Claims   map[string]any
	ClientID string
	// IDTokens holds every ID token issued.
	IDTokens []string
	Now      func() time.Time
	key      *rsa.PrivateKey
	codes    codes
}

// oidcKey is the signing key of every fake: generating one is slow.
var oidcKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

// NewFakeOIDC starts a provider for clientID; it stops with the test.
func NewFakeOIDC(t testing.TB, clientID string) *FakeOIDC {
	t.Helper()
	key, err := oidcKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeOIDC{ClientID: clientID, key: key, Claims: map[string]any{}, Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token", "jwks_uri": f.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /authorize", f.codes.authorize)
	mux.HandleFunc("POST /token", f.token)
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Close)
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *FakeOIDC) token(w http.ResponseWriter, r *http.Request) {
	p := f.codes.redeem(r.FormValue("code"), r.FormValue("code_verifier"))
	if p == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	now := f.Now()
	claims := map[string]any{"iss": f.URL, "aud": f.ClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": p.nonce}
	maps.Copy(claims, f.Claims)
	id, err := f.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.IDTokens = append(f.IDTokens, id)
	writeJSON(w, http.StatusOK, map[string]any{"access_token": "at-" + r.FormValue("code"), "token_type": "Bearer", "id_token": id})
}

func (f *FakeOIDC) sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signed := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + b64(sig), nil
}

// FakeGitHub is GitHub's user authorization and the API of the signed-in
// user.
type FakeGitHub struct {
	*httptest.Server
	// ID, Login and Name are the user who signs in; Email is their primary
	// address, verified with EmailVerified. NoEmails answers /user/emails
	// with 403, as without the app's "Email addresses" permission.
	ID            int64
	Login, Name   string
	Email         string
	EmailVerified bool
	NoEmails      bool
	// AccessTokens holds every access token issued.
	AccessTokens []string
	codes        codes
}

// NewFakeGitHub starts a fake GitHub; it stops with the test.
func NewFakeGitHub(t testing.TB) *FakeGitHub {
	t.Helper()
	f := &FakeGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.codes.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if f.codes.redeem(r.FormValue("code"), r.FormValue("code_verifier")) == nil {
			writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
			return
		}
		tok := "gho_" + r.FormValue("code")
		f.AccessTokens = append(f.AccessTokens, tok)
		writeJSON(w, http.StatusOK, map[string]string{"access_token": tok, "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": f.ID, "login": f.Login, "name": f.Name})
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, _ *http.Request) {
		if f.NoEmails {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{{"email": "other@example.com", "primary": false, "verified": true}, {"email": f.Email, "primary": true, "verified": f.EmailVerified}})
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("login") != f.Login {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": f.ID, "login": f.Login})
	})
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Close)
	return f
}

// String names the fake in test output.
func (f *FakeGitHub) String() string { return fmt.Sprintf("github %s", f.URL) }
