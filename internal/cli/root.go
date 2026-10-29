// Package cli implements the forecrew admin CLI commands. The command tree
// was scaffolded with cobra-cli and reshaped to avoid package globals.
package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// NewRootCommand returns the forecrew command tree.
func NewRootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "forecrew",
		Short:         "Administer a Forecrew installation",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("forecrew {{.Version}}\n")
	root.AddCommand(newVersionCommand(version))
	return root
}

// Run executes the CLI with args and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	root := NewRootCommand(version)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		root.PrintErrln("forecrew:", err)
		return 1
	}
	return 0
}
