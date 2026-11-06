// Package cli implements the forecrew CLI. The admin commands (migrate,
// config, token, worker, user, org, session) use the database directly; the others call /api/v1.
// The command tree was scaffolded with cobra-cli and reshaped to avoid
// package globals.
package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// NewRootCommand returns the forecrew command tree.
func NewRootCommand(version string, env Env) *cobra.Command {
	root := &cobra.Command{
		Use:           "forecrew",
		Short:         "Administer a Forecrew installation",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("forecrew {{.Version}}\n")
	root.AddCommand(newVersionCommand(version))
	r := newRemote(env)
	r.bind(root)
	cfg := newConfigCommand(env)
	cfg.Short = "Work with forecrew.yaml and the configuration resources"
	cfg.AddCommand(r.configCommands()...)
	root.AddCommand(newMigrateCommand(env), cfg, newTokenCommand(env), newWorkerAdminCommand(env),
		newUserCommand(env), newOrgCommand(env, r), newSessionCommand(env))
	root.AddCommand(r.commands()...)
	root.AddCommand(r.assistantCommand())
	return root
}

// Run executes the CLI with args in the environment of the process and
// returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	return RunWith(ctx, args, stdout, stderr, version, OSEnv())
}

// RunWith executes the CLI in env and returns the process exit code.
func RunWith(ctx context.Context, args []string, stdout, stderr io.Writer, version string, env Env) int {
	root := NewRootCommand(version, env)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		root.PrintErrln("forecrew:", err)
		return 1
	}
	return 0
}
