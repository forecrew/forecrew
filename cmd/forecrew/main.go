// Command forecrew is the Forecrew admin CLI.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, version))
}

const usage = `usage: forecrew <command>

commands:
  version   print the version
`

// run executes the CLI with args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Fprintf(stdout, "forecrew %s\n", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "forecrew: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
