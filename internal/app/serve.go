package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// server is an http.Server and the listener it serves.
type server struct {
	srv *http.Server
	ln  net.Listener
	tls bool
}

func (s server) serve() error {
	if s.tls {
		return s.srv.ServeTLS(s.ln, "", "")
	}
	return s.srv.Serve(s.ln)
}

// certManager issues certificates for one domain with Let's Encrypt.
type certManager = autocert.Manager

// newCertManager returns a manager that accepts only domain and caches
// certificates in cacheDir.
func newCertManager(domain, cacheDir string) *certManager {
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domain),
		Cache:      autocert.DirCache(cacheDir),
	}
}

// servers returns the servers to run: the app on the listen address, plus,
// with TLS, the plain HTTP server of the ACME challenge and the redirect.
func (a *App) servers(ctx context.Context) ([]server, error) {
	base := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler: h, ReadHeaderTimeout: 10 * time.Second,
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
	}
	ln := a.d.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", a.d.Config.Server.Listen); err != nil {
			return nil, fmt.Errorf("listen: %w", err)
		}
	}
	main := base(a.handler)
	if a.certs == nil {
		return []server{{srv: main, ln: ln}}, nil
	}
	main.TLSConfig = a.certs.TLSConfig()
	plainLn, err := net.Listen("tcp", a.httpAddr)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("listen: %w", err)
	}
	// With a nil fallback the manager answers ACME challenges and
	// redirects every other request to HTTPS.
	return []server{{srv: main, ln: ln, tls: true}, {srv: base(a.certs.HTTPHandler(nil)), ln: plainLn}}, nil
}

// serve runs the servers until ctx ends or one fails, then shuts them down.
func (a *App) serve(ctx context.Context) error {
	servers, err := a.servers(ctx)
	if err != nil {
		return err
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errc <- s.serve() }()
	}
	a.d.Logger.Info("forecrewd started", "version", a.d.Version, "listen", servers[0].ln.Addr().String(), "tls", a.certs != nil)
	var failed error
	select {
	case err := <-errc:
		failed = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	for _, s := range servers {
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			failed = errors.Join(failed, fmt.Errorf("shutdown: %w", err))
		}
	}
	if failed == nil {
		a.d.Logger.Info("forecrewd stopped")
	}
	return failed
}
