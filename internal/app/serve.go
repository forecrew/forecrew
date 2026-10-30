package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// servers returns the servers to run: the app on the listen address.
func (a *App) servers(ctx context.Context) ([]server, error) {
	ln := a.d.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", a.d.Config.Server.Listen); err != nil {
			return nil, fmt.Errorf("listen: %w", err)
		}
	}
	srv := &http.Server{
		Handler: a.handler, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	return []server{{srv: srv, ln: ln}}, nil
}

// server is an http.Server and the listener it serves.
type server struct {
	srv *http.Server
	ln  net.Listener
}

func (s server) serve() error { return s.srv.Serve(s.ln) }

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
	a.d.Logger.Info("forecrewd started", "version", a.d.Version, "listen", servers[0].ln.Addr().String())
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
