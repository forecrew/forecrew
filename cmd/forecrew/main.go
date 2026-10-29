// Command forecrew is the Forecrew admin CLI.
package main

import (
	"context"
	"os"

	"github.com/forecrew/forecrew/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, version))
}
