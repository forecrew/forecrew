package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/testkit"
)

func TestNewClientRefuses(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	write(t, garbage, "not a certificate", 0o644)
	const token = "forecrew@pve!agent=11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name, url, token, ca, want string
	}{
		{"plain http", "http://pve:8006", token, "", "must be https"},
		{"not an address", "https://", token, "", "not an address"},
		{"credentials in the URL", "https://u:p@pve:8006", token, "", "not an address"},
		{"bad token", "https://pve:8006", "just-a-secret", "", "user@realm!name=secret"},
		{"empty token", "https://pve:8006", "", "", "user@realm!name=secret"},
		{"missing CA file", "https://pve:8006", token, filepath.Join(dir, "none.pem"), "ca_file"},
		{"CA file without certificates", "https://pve:8006", token, garbage, "no certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.url, tt.token, tt.ca, "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "11111111") {
				t.Errorf("the token is in the error: %v", err)
			}
		})
	}
}

// The API's certificate is checked, and nothing turns that off.
func TestTLSIsVerified(t *testing.T) {
	pve := testkit.NewFakeProxmox(t)
	ctx := context.Background()
	var out struct{}

	noCA, err := NewClient(pve.URL(), pve.Token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := noCA.do(ctx, http.MethodGet, "/version", nil, &out); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an untrusted certificate was accepted: %v", err)
	}

	ca := filepath.Join(t.TempDir(), "ca.pem")
	write(t, ca, string(pve.CAPEM()), 0o644)
	good, err := NewClient(pve.URL(), pve.Token, ca, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := good.do(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		t.Errorf("a certificate its CA vouches for: %v", err)
	}

	// The httptest certificate is for example.com; a server name that is not
	// on it is refused even with the right CA.
	wrong, err := NewClient(pve.URL(), pve.Token, ca, "pve.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.do(ctx, http.MethodGet, "/version", nil, &out); err == nil {
		t.Error("a certificate for another name was accepted")
	}
	named, err := NewClient(pve.URL(), pve.Token, ca, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := named.do(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		t.Errorf("the name on the certificate: %v", err)
	}
}

func TestConfigHasNoWayToSkipVerification(t *testing.T) {
	for _, key := range []string{"insecure", "insecure_skip_verify", "verify_tls", "skip_tls_verify"} {
		path := filepath.Join(t.TempDir(), "proxmox.yaml")
		write(t, path, "nodes: [pve]\n"+key+": true\n", 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v", key, err)
		}
	}
}

func TestErrorsHideTheToken(t *testing.T) {
	pve := testkit.NewFakeProxmox(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	write(t, ca, string(pve.CAPEM()), 0o644)
	c, err := NewClient(pve.URL(), pve.Token, ca, "")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.SplitN(pve.Token, "=", 2)[1]
	pve.FailOn = func(string) (int, string) {
		return 500, "denied for " + pve.Token + " and\nthe secret " + secret + strings.Repeat(" long", 200)
	}
	err = c.do(context.Background(), http.MethodGet, "/version", nil, nil)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "\n") || len(err.Error()) > 400 {
		t.Fatalf("err = %q", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("err = %q", err)
	}
}

func TestClientFailureShapes(t *testing.T) {
	const token = "forecrew@pve!agent=11111111-2222-3333-4444-555555555555"
	serve := func(h http.HandlerFunc) *Client {
		srv := httptest.NewTLSServer(h)
		t.Cleanup(srv.Close)
		ca := filepath.Join(t.TempDir(), "ca.pem")
		write(t, ca, string(testkitPEM(srv)), 0o644)
		c, err := NewClient(srv.URL, token, ca, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ctx := context.Background()
	var out map[string]any

	c := serve(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	if err := c.do(ctx, http.MethodGet, "/x", nil, &out); err == nil || !strings.Contains(err.Error(), "not an answer") {
		t.Errorf("html: %v", err)
	}
	c = serve(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":"text"}`)) })
	if err := c.do(ctx, http.MethodGet, "/x", nil, &out); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Errorf("wrong type: %v", err)
	}
	c = serve(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/", http.StatusFound)
	})
	err := c.do(ctx, http.MethodGet, "/x", nil, &out)
	var api *APIError
	if err == nil || !asAPI(err, &api) || api.Status != http.StatusFound {
		t.Errorf("a redirect was followed: %v", err)
	}
	c = serve(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if err := c.do(ctx, http.MethodGet, "/x", nil, &out); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("bad gateway: %v", err)
	}
	c = serve(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":"` + strings.Repeat("x", maxResponse) + `"}`))
	})
	if err := c.do(ctx, http.MethodGet, "/x", nil, &out); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("huge answer: %v", err)
	}
	if err := c.do(canceled(), http.MethodGet, "/x", nil, &out); err == nil {
		t.Error("a cancelled call succeeded")
	}
	var e error = &APIError{Status: 500}
	if e.Error() != "proxmox: 500" {
		t.Errorf("text = %q", e.Error())
	}
}

func TestTaskWaitsAndFails(t *testing.T) {
	pve := testkit.NewFakeProxmox(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	write(t, ca, string(pve.CAPEM()), 0o644)
	c, _ := NewClient(pve.URL(), pve.Token, ca, "")
	pve.FailOn = failOn("GET", "/status", "task log unreadable")
	if err := c.task(context.Background(), "pve", "UPID:pve:1", time.Millisecond); err == nil {
		t.Error("an unreadable task succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// A task that never ends: the caller's time runs out.
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
	}))
	defer slow.Close()
	write(t, ca, string(testkitPEM(slow)), 0o644)
	c2, _ := NewClient(slow.URL, pve.Token, ca, "example.com")
	if err := c2.task(ctx, "pve", "UPID:pve:1", time.Millisecond); err == nil {
		t.Error("an endless task ended")
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	if _, err := readToken(filepath.Join(dir, "none")); err == nil || !strings.Contains(err.Error(), "API token") {
		t.Errorf("missing: %v", err)
	}
	open := filepath.Join(dir, "open")
	write(t, open, "forecrew@pve!a=11111111\n", 0o644)
	if _, err := readToken(open); err == nil || !strings.Contains(err.Error(), "owner alone") {
		t.Errorf("readable by others: %v", err)
	}
	closed := filepath.Join(dir, "closed")
	write(t, closed, " forecrew@pve!a=11111111 \n", 0o600)
	if got, err := readToken(closed); err != nil || got != "forecrew@pve!a=11111111" {
		t.Errorf("got %q, %v", got, err)
	}
	unreadable := filepath.Join(dir, "unreadable")
	write(t, unreadable, "x", 0o000)
	if os.Geteuid() != 0 {
		if _, err := readToken(unreadable); err == nil {
			t.Error("an unreadable file was read")
		}
	}
}
