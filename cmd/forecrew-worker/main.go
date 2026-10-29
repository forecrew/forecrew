// Command forecrew-worker is the Forecrew worker daemon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, version))
}

// run parses args, executes the command and returns the process exit code.
func run(args []string, stdout, stderr io.Writer, version string) int {
	fs := flag.NewFlagSet("forecrew-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.String("config", "worker.yaml", "path to worker.yaml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "forecrew-worker %s\n", version)
		return 0
	}
	fmt.Fprintln(stderr, "forecrew-worker: not implemented yet")
	return 1
}
