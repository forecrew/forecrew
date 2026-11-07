// Command forecrew-worker is the Forecrew worker daemon.
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
	"strings"
	"syscall"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/worker"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: forecrew-worker <command> [flags]

commands:
  run      poll forecrewd for jobs and run them (--config worker.yaml)
  check    print the detected labels and verify git, the runtime CLIs and the server (--config worker.yaml)
  status   list the agent CLIs and where each comes from: image, installed or system (--config worker.yaml)
  version  print the version
  guest-init  be PID 1 of a microVM (the kernel starts it; docs/images.md)
`

func main() {
	if err := worker.ProtectProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "forecrew-worker: warning: cannot protect the process:", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Environ(), version)
	stop()
	os.Exit(code)
}

// run executes a command and returns the process exit code. environ is the
// process environment; the config reads the worker token from it.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, environ []string, version string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "version", "--version", "-version":
		fmt.Fprintf(stdout, "forecrew-worker %s\n", version)
		return 0
	case "guest-init":
		return guestInit(stderr, version)
	case "run", "check", "status":
		return runWithConfig(ctx, cmd, rest, stdout, stderr, environ, version)
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "forecrew-worker: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// runWithConfig parses the flags of run and check, loads worker.yaml and
// runs the command.
func runWithConfig(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer, environ []string, version string) int {
	fs := flag.NewFlagSet("forecrew-worker "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "worker.yaml", "path to worker.yaml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := config.LoadWorker(*path, getenv(environ))
	if err != nil {
		logger.Error("load config", "error", err)
		return 1
	}
	if cmd == "status" {
		worker.Status(cfg, environ, stdout)
		return 0
	}
	if cmd == "check" {
		if err := worker.Check(ctx, cfg, environ, version, stdout); err != nil {
			fmt.Fprintf(stderr, "forecrew-worker: %v\n", err)
			return 1
		}
		return 0
	}
	if err := worker.Run(ctx, cfg, environ, version, logger); err != nil {
		logger.Error("forecrew-worker stopped", "error", err)
		return 1
	}
	return 0
}

// getenv returns a lookup over environ.
func getenv(environ []string) func(string) string {
	return func(key string) string {
		for _, kv := range environ {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				return v
			}
		}
		return ""
	}
}
