package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"uuid"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Token prefixes (docs/contracts/conventions.md).
const (
	prefixAPIToken    = "fcm_"
	prefixWorkerToken = "fcw_"
)

// newToken returns a random token: 32 bytes, base64url without padding.
func newToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// withDatabase runs fn on the database of FORECREW_DATABASE_URL.
func withDatabase(env Env, cmd *cobra.Command, fn func(ctx context.Context, db Database) error) error {
	db, err := env.openDatabase(cmd.Context())
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(cmd.Context(), db)
}

func newMigrateCommand(env Env) *cobra.Command {
	migrate := &cobra.Command{Use: "migrate", Short: "Manage the database schema"}
	migrate.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Apply the schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				if err := db.Migrate(ctx); err != nil {
					return err
				}
				cmd.Println("database is up to date")
				return nil
			})
		},
	})
	return migrate
}

func newConfigCommand(env Env) *cobra.Command {
	cfg := &cobra.Command{Use: "config", Short: "Work with forecrew.yaml"}
	var path string
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check forecrew.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.Load(path, env.Getenv); err != nil {
				return err
			}
			cmd.Println(path, "is valid")
			return nil
		},
	}
	validate.Flags().StringVar(&path, "config", "forecrew.yaml", "path to forecrew.yaml")
	cfg.AddCommand(validate)
	return cfg
}

// issueCommand returns a command that creates a secret for a name with
// create, which stores the hash, and prints the secret once.
func issueCommand(env Env, accountFlag *string, use, short, prefix, what string,
	create func(ctx context.Context, st store.Store, accountID uuid.UUID, name string, hash []byte) error,
) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				secret, err := newToken(prefix)
				if err != nil {
					return err
				}
				accountID, err := account(ctx, db.Store(), *accountFlag)
				if err != nil {
					return err
				}
				if err := create(ctx, db.Store(), accountID, args[0], mcp.HashToken(secret)); err != nil {
					return createError(what, args[0], err)
				}
				printSecret(cmd.OutOrStdout(), cmd.ErrOrStderr(), secret, what, args[0])
				return nil
			})
		},
	}
}

func newTokenCommand(env Env) *cobra.Command {
	token := &cobra.Command{Use: "token", Short: "Manage API and MCP tokens"}
	var accountFlag string
	create := issueCommand(env, &accountFlag, "create <name>", "Create an API and MCP token; it is shown once", prefixAPIToken, "token",
		func(ctx context.Context, st store.Store, accountID uuid.UUID, name string, hash []byte) error {
			return st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: accountID, Name: name, TokenHash: hash})
		})
	create.Flags().StringVar(&accountFlag, "account", "", "account ID; defaults to the only account")
	token.AddCommand(create)
	return token
}

func newWorkerAdminCommand(env Env) *cobra.Command {
	worker := &cobra.Command{Use: "worker", Short: "Register and revoke workers"}
	var accountFlag string
	add := issueCommand(env, &accountFlag, "add <name>", "Register a worker; its token is shown once", prefixWorkerToken, "worker",
		func(ctx context.Context, st store.Store, accountID uuid.UUID, name string, hash []byte) error {
			return st.Workers().Create(ctx, &domain.Worker{AccountID: accountID, Name: name, TokenHash: hash})
		})
	revoke := &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a worker's token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				accountID, err := account(ctx, db.Store(), accountFlag)
				if err != nil {
					return err
				}
				if err := revokeWorker(ctx, db.Store(), accountID, args[0]); err != nil {
					return err
				}
				cmd.Printf("worker %s revoked\n", args[0])
				return nil
			})
		},
	}
	worker.PersistentFlags().StringVar(&accountFlag, "account", "", "account ID; defaults to the only account")
	worker.AddCommand(add, revoke)
	return worker
}

func revokeWorker(ctx context.Context, st store.Store, accountID uuid.UUID, name string) error {
	workers, err := st.Workers().List(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	for _, w := range workers {
		if w.Name == name {
			if err := st.Workers().Revoke(ctx, accountID, w.ID); err != nil {
				return fmt.Errorf("revoke worker %s: %w", name, err)
			}
			return nil
		}
	}
	return fmt.Errorf("worker %q does not exist", name)
}

// createError explains why a token or worker could not be created.
func createError(what, name string, err error) error {
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("a %s named %q already exists", what, name)
	}
	return fmt.Errorf("create %s %s: %w", what, name, err)
}

// printSecret prints a secret to stdout, alone on its line so scripts can
// capture it, and says on stderr that it is shown once.
func printSecret(stdout, stderr io.Writer, secret, what, name string) {
	fmt.Fprintf(stderr, "Created %s %s. Copy it now: it is not stored and cannot be shown again.\n", what, name)
	fmt.Fprintln(stdout, secret)
}
