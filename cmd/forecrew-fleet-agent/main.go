// Command forecrew-fleet-agent runs where machines live, dials out to
// forecrewd and creates machines there, so credentials never leave that place.
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

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleetagent"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: forecrew-fleet-agent <command> [flags]

commands:
  run      connect to forecrewd and keep the machines here as it asks (--config agent.yaml)
  doctor   say what is missing for a kind of location (--kind microvm)
  detect   print the kind of location this machine looks like
  version  print the version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "version", "--version", "-version":
		fmt.Fprintf(stdout, "forecrew-fleet-agent %s\n", version)
		return 0
	case "detect":
		kind := fleetagent.DetectKind()
		if kind == "" {
			fmt.Fprintln(stderr, "forecrew-fleet-agent: this machine fits no kind of location")
			return 1
		}
		fmt.Fprintln(stdout, kind)
		return 0
	case "doctor":
		return doctor(ctx, rest, stdout, stderr)
	case "run":
		return serve(ctx, rest, stderr, getenv)
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "forecrew-fleet-agent: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func doctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("kind", "", "kind of location; defaults to what this machine looks like")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	k := domain.ProviderKind(*kind)
	if k == "" {
		k = fleetagent.DetectKind()
	}
	if !fleetagent.Doctor(ctx, stdout, k, version) {
		return 1
	}
	return 0
}

// serve runs the agent. Being rejected ends it with status 0: the service
// managers restart what fails, and no restart brings a revoked token back. The
// log says why it stopped.
func serve(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "agent.yaml", "path to agent.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := config.LoadFleetAgent(*path, getenv)
	if err != nil {
		log.Error("read configuration", "error", err)
		return 1
	}
	err = fleetagent.Serve(ctx, cfg, version, log)
	switch {
	case errors.Is(err, fleetagent.ErrRejected):
		log.Error("forecrewd rejected this agent: its token is revoked or wrong, or its join code unusable; stopping", "error", err)
		return 0
	case err != nil && ctx.Err() == nil:
		log.Error("fleet agent stopped", "error", err)
		return 1
	}
	return 0
}
