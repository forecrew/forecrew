package app

import (
	"context"
	"crypto/tls"
	"net/http"
	"slices"
	"testing"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func tlsFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	dir := t.TempDir()
	f := newFixture(t, func(d *Deps) {
		d.Config.Server.PublicBaseURL = "https://forecrew.example.com"
		d.Config.Server.TLSDomain = "forecrew.example.com"
		d.Config.Server.CertCacheDir = dir
	})
	return f, dir
}

func TestCertManager(t *testing.T) {
	f, dir := tlsFixture(t)
	m := f.app.certs
	if m == nil {
		t.Fatal("no certificate manager with tls_domain set")
	}
	if c, ok := m.Cache.(autocert.DirCache); !ok || string(c) != dir {
		t.Errorf("cache = %v, want the cert_cache_dir %s", m.Cache, dir)
	}
	if err := m.HostPolicy(context.Background(), "forecrew.example.com"); err != nil {
		t.Errorf("own domain: %v", err)
	}
	if err := m.HostPolicy(context.Background(), "evil.example.com"); err == nil {
		t.Error("another host must be refused")
	}
	// The TLS-ALPN-01 challenge is answered on the HTTPS port.
	if !slices.Contains(m.TLSConfig().NextProtos, acme.ALPNProto) {
		t.Errorf("ALPN protocols = %v", m.TLSConfig().NextProtos)
	}
	// A certificate for another host is not requested.
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Error("certificate issued for another host")
	}
}

func TestPlainWithoutTLSDomain(t *testing.T) {
	f := newFixture(t)
	if f.app.certs != nil {
		t.Fatal("certificate manager without tls_domain")
	}
	servers, err := f.app.servers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = servers[0].ln.Close() }()
	if len(servers) != 1 || servers[0].tls {
		t.Fatalf("servers = %+v, want one plain HTTP server", servers)
	}
}

func TestTLSServers(t *testing.T) {
	f, _ := tlsFixture(t)
	f.app.httpAddr = "127.0.0.1:0"
	servers, err := f.app.servers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		defer func() { _ = s.ln.Close() }()
	}
	if len(servers) != 2 || !servers[0].tls || servers[1].tls {
		t.Fatalf("servers = %+v, want HTTPS and plain HTTP", servers)
	}
	if servers[0].srv.TLSConfig.GetCertificate == nil {
		t.Error("HTTPS server has no certificate source")
	}

	// :80 redirects everything but the ACME challenge to HTTPS.
	plain := servers[1].srv.Handler
	rec := get(t, plain, http.MethodGet, "/api/v1/me?x=1")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.com/api/v1/me?x=1" {
		t.Errorf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = get(t, plain, http.MethodGet, "/.well-known/acme-challenge/token")
	// The manager handles it (here it refuses the unknown host); it is not redirected.
	if rec.Code != http.StatusForbidden {
		t.Errorf("ACME challenge: %d, want the manager's answer", rec.Code)
	}
}

func TestTLSListenErrors(t *testing.T) {
	f, _ := tlsFixture(t)
	busy := listen(t)
	f.app.httpAddr = busy.Addr().String()
	f.app.d.Listener = listen(t)
	if _, err := f.app.servers(t.Context()); err == nil {
		t.Error("busy :80 must fail")
	}
}
