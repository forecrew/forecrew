package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// memKeyring is a keychain in memory; broken makes every call fail, as on a
// server without one.
type memKeyring struct {
	mu     sync.Mutex
	items  map[string]string
	broken bool
	// getErr fails reads only, as a locked keychain does.
	getErr error
}

func (k *memKeyring) Get(_, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.broken {
		return "", errors.New("no keychain")
	}
	if k.getErr != nil {
		return "", k.getErr
	}
	v, ok := k.items[user]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}

func (k *memKeyring) Set(_, user, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.broken {
		return errors.New("no keychain")
	}
	k.items[user] = secret
	return nil
}

func (k *memKeyring) Delete(_, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, user)
	return nil
}

// fakeServer is forecrewd's side of the login: it redeems codes with PKCE,
// knows a token and records revocations.
type fakeServer struct {
	*httptest.Server
	mu         sync.Mutex
	challenge  string
	orgAsked   string
	revoked    []string
	revokeCode int
	tokens     int
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{revokeCode: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/cli/token", func(w http.ResponseWriter, r *http.Request) {
		var in api.CLITokenInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		sum := sha256.Sum256([]byte(in.CodeVerifier))
		f.mu.Lock()
		defer f.mu.Unlock()
		if in.Code != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_input","message":"the login code is not valid"}}`))
			return
		}
		f.tokens++
		slug := "acme"
		if f.orgAsked != "" {
			slug = f.orgAsked
		}
		_ = json.NewEncoder(w).Encode(api.CLIToken{Secret: fmt.Sprintf("fcm_secret_%s_%d", slug, f.tokens), Token: mcp.TokenInfo{ID: domain.NewID(), Name: in.Name},
			Account: domain.Account{Name: strings.ToUpper(slug), Slug: slug}, User: domain.User{Name: "Ana\x1b[2J"}})
	})
	mux.HandleFunc("POST /api/v1/tokens/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revoked = append(f.revoked, r.PathValue("id")+" "+r.Header.Get("Authorization"))
		if f.revokeCode != http.StatusOK {
			w.WriteHeader(f.revokeCode)
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"nope"}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer fcm_secret_") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"cli-laptop","user":{"id":"` + domain.NewID().String() + `","name":"Ana","email":"ana@acme.test"},"account":{"name":"Acme","slug":"acme"},"roles":[{"role":"owner","scope":"organization"}]}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

type loginEnv struct {
	server  *fakeServer
	keyring *memKeyring
	dir     string
	env     Env
	opened  []string
}

func newLoginEnv(t *testing.T) *loginEnv {
	t.Helper()
	l := &loginEnv{server: newFakeServer(t), keyring: &memKeyring{items: map[string]string{}}, dir: t.TempDir()}
	l.env = Env{
		Getenv: func(string) string { return "" }, HTTP: l.server.Client(),
		ConfigDir: func() (string, error) { return l.dir, nil }, Keyring: l.keyring, Hostname: func() (string, error) { return "Laptop", nil },
		// The browser: it reads what the CLI asks, as the page would, and is sent back to the CLI.
		OpenBrowser: func(u string) error {
			l.opened = append(l.opened, u)
			parsed, err := url.Parse(u)
			if err != nil {
				return err
			}
			q := parsed.Query()
			l.server.mu.Lock()
			l.server.challenge, l.server.orgAsked = q.Get("code_challenge"), q.Get("organization")
			l.server.mu.Unlock()
			go func() {
				resp, err := http.Get(q.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(q.Get("state")))
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		},
	}
	return l
}

func (l *loginEnv) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunWith(t.Context(), args, &out, &errOut, "test", l.env)
	return out.String(), errOut.String(), code
}

func (l *loginEnv) file(t *testing.T) (stored, os.FileInfo) {
	t.Helper()
	path := filepath.Join(l.dir, "forecrew", "credentials.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, info
}

func TestLoginThroughTheLoopback(t *testing.T) {
	l := newLoginEnv(t)
	out, errOut, code := l.run(t, "login", "--server", l.server.URL)
	if code != 0 || !strings.Contains(out, "Logged in to "+l.server.URL+" as Ana [2J on ACME, with the token cli-Laptop") {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, errOut, out)
	}
	if len(l.opened) != 1 || !strings.Contains(l.opened[0], "/cli-login?") || !strings.Contains(l.opened[0], "host=Laptop") {
		t.Errorf("opened %v", l.opened)
	}
	// The secret is in the keychain, not in the file, and the file is private.
	s, info := l.file(t)
	if l.keyring.items[keychainUser(l.server.URL, "acme")] != "fcm_secret_acme_1" || s.Orgs["acme"].Token != "" || s.Current != "acme" || s.Server != l.server.URL {
		t.Errorf("stored = %+v, keychain %v", s, l.keyring.items)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}

	// Later commands use the login, and whoami says who it is.
	out, errOut, code = l.run(t, "whoami")
	for _, want := range []string{"Server:", l.server.URL, "Ana ana@acme.test", "Acme (acme)", "owner"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("whoami: code %d, stderr %q, lacks %q:\n%s", code, errOut, want, out)
		}
	}
	// The token never goes to another server.
	if _, errOut, code = l.run(t, "whoami", "--url", "https://other.example.com"); code == 0 || !strings.Contains(errOut, "you are logged in to") {
		t.Errorf("another server: code %d, %q", code, errOut)
	}
}

func TestLoginKeepsTheTokenInAPrivateFileWithoutAKeychain(t *testing.T) {
	l := newLoginEnv(t)
	l.keyring.broken = true
	out, errOut, code := l.run(t, "login", "--server", l.server.URL)
	if code != 0 || !strings.Contains(errOut, "OS keychain could not be used") || !strings.Contains(errOut, "only you can read") {
		t.Fatalf("code %d, stderr %q, output %s", code, errOut, out)
	}
	s, info := l.file(t)
	if s.Orgs["acme"].Token != "fcm_secret_acme_1" || info.Mode().Perm() != 0o600 {
		t.Errorf("stored = %+v, mode %v", s, info.Mode())
	}
	if out, errOut, code := l.run(t, "whoami"); code != 0 || !strings.Contains(out, "Ana") {
		t.Errorf("whoami: code %d, %q, %s", code, errOut, out)
	}
}

func TestLoginRefusals(t *testing.T) {
	l := newLoginEnv(t)
	if _, errOut, code := l.run(t, "login", "--server", "http://forecrew.example.com"); code == 0 || !strings.Contains(errOut, "plain http") {
		t.Errorf("plain http: code %d, %q", code, errOut)
	}
	if _, errOut, code := l.run(t, "login", "--server", "nonsense"); code == 0 || !strings.Contains(errOut, "bad server URL") {
		t.Errorf("bad URL: code %d, %q", code, errOut)
	}
	// A wrong code is refused by the server and keeps nothing.
	l.env.OpenBrowser = func(u string) error {
		parsed, _ := url.Parse(u)
		q := parsed.Query()
		go func() {
			resp, err := http.Get(q.Get("redirect_uri") + "?code=wrong&state=" + url.QueryEscape(q.Get("state")))
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code == 0 || !strings.Contains(errOut, "not valid") {
		t.Errorf("wrong code: code %d, %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(l.dir, "forecrew", "credentials.json")); err == nil {
		t.Error("a failed login kept something")
	}
	// A redirect with another state is not the login in progress.
	l.env.OpenBrowser = func(u string) error {
		parsed, _ := url.Parse(u)
		q := parsed.Query()
		go func() {
			if resp, err := http.Get(q.Get("redirect_uri") + "?code=the-code&state=forged"); err == nil {
				_ = resp.Body.Close()
			}
			l.server.mu.Lock()
			l.server.challenge = q.Get("code_challenge")
			l.server.mu.Unlock()
			if resp, err := http.Get(q.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(q.Get("state"))); err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	if out, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Errorf("forged state then the real one: code %d, %q, %s", code, errOut, out)
	}
}

func TestLoginWithAPastedCode(t *testing.T) {
	l := newLoginEnv(t)
	l.env.OpenBrowser = nil
	// The page shows the code; the verifier is the CLI's, so take the challenge from the address it prints.
	var out, errOut syncBuffer
	done := make(chan int, 1)
	pr, pw := newPipe()
	l.env.Stdin = pr
	go func() {
		done <- RunWith(t.Context(), []string{"login", "--server", l.server.URL, "--no-browser"}, &out, &errOut, "test", l.env)
	}()
	for i := 0; i < 500 && !strings.Contains(out.String(), "code_challenge"); i++ {
		sleepBriefly()
	}
	printed := out.String()
	line := printed[strings.Index(printed, l.server.URL+"/cli-login"):]
	u, err := url.Parse(strings.SplitN(line, "\n", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	l.server.mu.Lock()
	l.server.challenge = u.Query().Get("code_challenge")
	l.server.mu.Unlock()
	_, _ = pw.Write([]byte("the-code\n"))
	if code := <-done; code != 0 || !strings.Contains(out.String(), "Logged in") {
		t.Fatalf("code %d, stderr %q, output %s", code, errOut.String(), out.String())
	}
}

func TestLogoutRevokesTheToken(t *testing.T) {
	l := newLoginEnv(t)
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	out, errOut, code := l.run(t, "logout")
	if code != 0 || !strings.Contains(out, "Logged out of acme") {
		t.Fatalf("logout: code %d, %q, %s", code, errOut, out)
	}
	if len(l.server.revoked) != 1 || !strings.HasSuffix(l.server.revoked[0], "Bearer fcm_secret_acme_1") {
		t.Errorf("revoked = %v", l.server.revoked)
	}
	s, _ := l.file(t)
	if len(s.Orgs) != 0 || s.Current != "" || len(l.keyring.items) != 0 {
		t.Errorf("stored = %+v, keychain %v", s, l.keyring.items)
	}
	if _, errOut, code := l.run(t, "whoami"); code == 0 || !strings.Contains(errOut, "no token") {
		t.Errorf("whoami after logout: code %d, %q", code, errOut)
	}
	if _, errOut, code := l.run(t, "logout"); code == 0 || !strings.Contains(errOut, "not logged in") {
		t.Errorf("logout again: code %d, %q", code, errOut)
	}
}

func TestLogoutWhenTheServerCannotRevoke(t *testing.T) {
	l := newLoginEnv(t)
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	// An error from the server: forgotten here, and said so.
	l.server.revokeCode = http.StatusInternalServerError
	if _, errOut, code := l.run(t, "logout"); code == 0 || !strings.Contains(errOut, "not revoked on the server") {
		t.Fatalf("code %d, %q", code, errOut)
	}
	if s, _ := l.file(t); len(s.Orgs) != 0 {
		t.Errorf("stored = %+v", s)
	}
	// A token the server does not know any more is revoked already.
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	l.server.revokeCode = http.StatusUnauthorized
	if _, errOut, code := l.run(t, "logout", "--all"); code != 0 {
		t.Errorf("logout --all: code %d, %q", code, errOut)
	}
}

func TestOrgSwitch(t *testing.T) {
	l := newLoginEnv(t)
	if _, _, code := l.run(t, "org", "switch", "beta"); code == 0 {
		t.Error("switch without a login")
	}
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	// Another organization is a login to it, asked for by slug.
	if out, errOut, code := l.run(t, "org", "switch", "beta"); code != 0 || !strings.Contains(out, "on BETA") {
		t.Fatalf("switch: code %d, %q, %s", code, errOut, out)
	}
	if s, _ := l.file(t); s.Current != "beta" || len(s.Orgs) != 2 {
		t.Fatalf("stored = %+v", s)
	}
	if !strings.Contains(l.opened[len(l.opened)-1], "organization=beta") {
		t.Errorf("opened %v", l.opened)
	}
	// An organization already logged in to only changes the current one.
	before := len(l.opened)
	if out, _, code := l.run(t, "org", "switch", "acme"); code != 0 || !strings.Contains(out, "Working in acme") || len(l.opened) != before {
		t.Errorf("switch back: code %d, %s, opened %v", code, out, l.opened)
	}
	if s, _ := l.file(t); s.Current != "acme" {
		t.Errorf("current = %s", s.Current)
	}
	// Logging out of the current one makes the other current.
	if _, errOut, code := l.run(t, "logout"); code != 0 {
		t.Fatalf("logout: %d %s", code, errOut)
	}
	if s, _ := l.file(t); s.Current != "beta" {
		t.Errorf("current after logout = %s", s.Current)
	}
}

func newPipe() (*pipeReader, *pipeWriter) {
	ch := make(chan []byte, 8)
	return &pipeReader{ch: ch}, &pipeWriter{ch: ch}
}

// pipeReader blocks until something is written, like a terminal.
type pipeReader struct {
	ch  chan []byte
	buf []byte
}

func (p *pipeReader) Read(b []byte) (int, error) {
	if len(p.buf) == 0 {
		p.buf = <-p.ch
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

type pipeWriter struct{ ch chan []byte }

func (p *pipeWriter) Write(b []byte) (int, error) {
	p.ch <- append([]byte(nil), b...)
	return len(b), nil
}

func sleepBriefly() { time.Sleep(10 * time.Millisecond) }

// syncBuffer is a buffer that a test reads while a command writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRepeatedLoginsRevokeThePreviousToken(t *testing.T) {
	l := newLoginEnv(t)
	for i := 0; i < 3; i++ {
		if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
			t.Fatalf("login %d: %d %s", i, code, errOut)
		}
	}
	// Each login replaced the one before: two revocations, each with the token it ends.
	if len(l.server.revoked) != 2 || !strings.HasSuffix(l.server.revoked[0], "Bearer fcm_secret_acme_1") || !strings.HasSuffix(l.server.revoked[1], "Bearer fcm_secret_acme_2") {
		t.Errorf("revoked = %v", l.server.revoked)
	}
	if got := l.keyring.items[keychainUser(l.server.URL, "acme")]; got != "fcm_secret_acme_3" {
		t.Errorf("kept %q", got)
	}
	// A revocation that fails is a warning: the new login stands.
	l.server.revokeCode = http.StatusInternalServerError
	out, errOut, code := l.run(t, "login", "--server", l.server.URL)
	if code != 0 || !strings.Contains(errOut, "was not revoked on the server") || !strings.Contains(out, "Logged in") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
	if got := l.keyring.items[keychainUser(l.server.URL, "acme")]; got != "fcm_secret_acme_4" {
		t.Errorf("kept %q", got)
	}
}

func TestLoginToAnotherServerRevokesTheTokensOfTheFirst(t *testing.T) {
	l := newLoginEnv(t)
	first := l.server
	if _, errOut, code := l.run(t, "login", "--server", first.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	l.server = newFakeServer(t)
	l.env.HTTP = http.DefaultClient
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("second login: %d %s", code, errOut)
	}
	if len(first.revoked) != 1 || !strings.HasSuffix(first.revoked[0], "Bearer fcm_secret_acme_1") {
		t.Errorf("revoked at the first server = %v", first.revoked)
	}
	if s, _ := l.file(t); s.Server != l.server.URL || len(s.Orgs) != 1 {
		t.Errorf("stored = %+v", s)
	}
}

func TestLogoutReportsAKeychainThatCannotBeRead(t *testing.T) {
	l := newLoginEnv(t)
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	// A locked keychain is not "nothing to revoke": the token lives on and the user is told.
	l.keyring.getErr = errors.New("keychain is locked")
	_, errOut, code := l.run(t, "logout")
	if code == 0 || !strings.Contains(errOut, "keychain is locked") || !strings.Contains(errOut, "not revoked on the server") || len(l.server.revoked) != 0 {
		t.Errorf("code %d, stderr %q, revoked %v", code, errOut, l.server.revoked)
	}
	// A secret that is simply gone is nothing to revoke.
	if _, errOut, code := l.run(t, "login", "--server", l.server.URL); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	l.keyring.getErr = nil
	delete(l.keyring.items, keychainUser(l.server.URL, "acme"))
	if _, errOut, code := l.run(t, "logout"); code != 0 {
		t.Errorf("logout with the secret gone: code %d, %q", code, errOut)
	}
}

func TestKeychainCallsGiveUp(t *testing.T) {
	was := keychainTimeout
	keychainTimeout = 20 * time.Millisecond
	t.Cleanup(func() { keychainTimeout = was })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	if _, err := within(func() (string, error) { <-block; return "late", nil }); !errors.Is(err, errKeychainTimeout) {
		t.Errorf("a call that hangs: %v", err)
	}
	if v, err := within(func() (string, error) { return "ok", nil }); v != "ok" || err != nil {
		t.Errorf("a call that answers: %q, %v", v, err)
	}
}
