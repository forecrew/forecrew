// Command forecrewd is the Forecrew orchestrator.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/forecrew/forecrew/internal/app"
	"github.com/forecrew/forecrew/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

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

func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) error {
	a, err := app.Open(ctx, cfg, logger, version)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}
