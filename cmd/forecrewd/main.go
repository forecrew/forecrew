// Command forecrewd is the Forecrew orchestrator.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/workerapi"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// shutdownTimeout bounds how long in-flight requests may take on shutdown.
const shutdownTimeout = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv, version)
	stop()
	os.Exit(code)
}

// run parses args, serves until ctx ends and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, version string) int {
	if len(args) > 0 && args[0] == "healthcheck" {
		return runHealthcheck(ctx, args[1:], stderr, getenv)
	}
	fs := flag.NewFlagSet("forecrewd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	path := fs.String("config", "forecrew.yaml", "path to forecrew.yaml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "forecrewd %s\n", version)
		return 0
	}
	logger := newLogger(stderr)
	cfg, err := config.Load(*path, getenv)
	if err != nil {
		logger.Error("load config", "error", err)
		return 1
	}
	if err := serve(ctx, cfg, logger, version); err != nil {
		logger.Error("forecrewd stopped", "error", err)
		return 1
	}
	return 0
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// openStore connects to Postgres and migrates the schema.
func openStore(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func newGitHub(cfg *config.Config) (*github.Client, error) {
	return github.New(github.Config{AppID: cfg.GitHub.AppID, PrivateKey: []byte(cfg.GitHub.PrivateKey)})
}

// signalStore publishes signals by storing them. It is the publisher until
// the workflow engine's signal bus is wired in.
type signalStore struct{ signals store.SignalRepo }

func (p signalStore) Publish(ctx context.Context, s domain.Signal) (bool, error) {
	return p.signals.Insert(ctx, &s)
}

// pinger reports whether the database is reachable.
type pinger interface {
	Ping(ctx context.Context) error
}

// newMux routes /healthz and the worker API.
func newMux(db pinger, workers http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/worker/v1/", workers)
	return mux
}

func now() time.Time { return time.Now().UTC() }

// serve runs the HTTP server and the lease reaper until ctx ends.
func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) error {
	pool, err := openStore(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	gh, err := newGitHub(cfg)
	if err != nil {
		return err
	}
	st := postgres.New(pool)
	svc := jobs.New(st, signalStore{st.Signals()}, logger, now)
	api := workerapi.New(workerapi.Config{
		Store: st, Jobs: svc, Tokens: gh, Env: workerapi.RuntimeEnv(cfg), Logger: logger,
	})
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           newMux(pool, api.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	return listen(ctx, srv, logger, version, func(ctx context.Context) { svc.RunReaper(ctx, jobs.ReapInterval) })
}

// listen serves srv and runs the loops until ctx ends, then shuts down.
func listen(ctx context.Context, srv *http.Server, logger *slog.Logger, version string, loops ...func(context.Context)) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	loopCtx, stopLoops := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, loop := range loops {
		wg.Go(func() { loop(loopCtx) })
	}
	defer func() { stopLoops(); wg.Wait() }()

	logger.Info("forecrewd started", "version", version, "listen", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("forecrewd stopped")
	return nil
}
