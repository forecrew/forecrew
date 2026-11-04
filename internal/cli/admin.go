package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"uuid"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/actionsprovider"
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
	list := &cobra.Command{
		Use:   "list",
		Short: "List the tokens; the secrets are not stored",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				accountID, err := account(ctx, db.Store(), accountFlag)
				if err != nil {
					return err
				}
				tokens, err := db.Store().MCPTokens().List(ctx, accountID)
				if err != nil {
					return fmt.Errorf("list tokens: %w", err)
				}
				infos := make([]mcp.TokenInfo, len(tokens))
				for i, t := range tokens {
					infos[i] = mcp.NewTokenInfo(t, false)
				}
				printTokens(cmd.OutOrStdout(), infos)
				return nil
			})
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <name|id>",
		Short: "Revoke a token; it stops working at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				accountID, err := account(ctx, db.Store(), accountFlag)
				if err != nil {
					return err
				}
				if err := revokeToken(ctx, db.Store(), accountID, args[0]); err != nil {
					return err
				}
				cmd.Printf("token %s revoked\n", args[0])
				return nil
			})
		},
	}
	token.PersistentFlags().StringVar(&accountFlag, "account", "", "account ID; defaults to the only account")
	token.AddCommand(create, list, revoke)
	return token
}

// revokeToken revokes the active token with the given name or ID.
func revokeToken(ctx context.Context, st store.Store, accountID uuid.UUID, nameOrID string) error {
	tokens, err := st.MCPTokens().List(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	return revokeByNameOrID("token", nameOrID, tokens,
		func(t domain.MCPToken) (uuid.UUID, string, bool) { return t.ID, t.Name, !t.RevokedAt.IsZero() },
		func(id uuid.UUID) error { return st.MCPTokens().Revoke(ctx, accountID, id) })
}

// revokeByNameOrID revokes the first item that is not revoked yet and whose
// name or ID is key; what names the kind of item in errors.
func revokeByNameOrID[T any](what, key string, items []T, info func(T) (id uuid.UUID, name string, revoked bool), revoke func(uuid.UUID) error) error {
	it, err := store.FindActive(key, items, info)
	if err != nil {
		return fmt.Errorf("%s %q does not exist or is already revoked", what, key)
	}
	id, _, _ := info(it)
	if err := revoke(id); err != nil {
		return fmt.Errorf("revoke %s %s: %w", what, key, err)
	}
	return nil
}

func newWorkerAdminCommand(env Env) *cobra.Command {
	worker := &cobra.Command{Use: "worker", Short: "Register, move and revoke workers"}
	var accountFlag, addPool, configPath string
	add := issueCommand(env, &accountFlag, "add <name>", "Register a worker in a pool; its token is shown once", prefixWorkerToken, "worker",
		func(ctx context.Context, st store.Store, accountID uuid.UUID, name string, hash []byte) error {
			if name == actionsprovider.WorkerName {
				return reservedWorkerError(name)
			}
			if err := checkWorkerPool(env, configPath, addPool); err != nil {
				return err
			}
			return st.Workers().Create(ctx, &domain.Worker{AccountID: accountID, Name: name, Pool: addPool, TokenHash: hash})
		})
	add.Flags().StringVar(&addPool, "pool", domain.PoolBuild, "pool the worker leases jobs from")
	var movePool string
	move := &cobra.Command{
		Use:   "move <name|id>",
		Short: "Move a worker to another pool; it takes effect at its next poll and running jobs finish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkWorkerPool(env, configPath, movePool); err != nil {
				return err
			}
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				accountID, err := account(ctx, db.Store(), accountFlag)
				if err != nil {
					return err
				}
				by := domain.Actor{AccountID: accountID, Kind: domain.ActorUser, Name: adminName(env)}
				if err := moveWorker(ctx, db.Store(), by, args[0], movePool); err != nil {
					return err
				}
				cmd.Printf("worker %s moved to pool %s\n", args[0], movePool)
				return nil
			})
		},
	}
	move.Flags().StringVar(&movePool, "pool", "", "pool to move the worker to (required)")
	_ = move.MarkFlagRequired("pool")
	revoke := &cobra.Command{
		Use:   "revoke <name|id>",
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
	worker.PersistentFlags().StringVar(&configPath, "config", "forecrew.yaml",
		"path to forecrew.yaml, read for pools other than "+domain.PoolBuild+" and "+domain.PoolControl)
	worker.AddCommand(add, move, revoke)
	return worker
}

// checkWorkerPool fails unless workers may join pool: a default pool except
// actions, whose jobs forecrewd runs on GitHub-hosted runners, or a pool of
// the forecrew.yaml at path, which is read only for pools that are not
// default ones.
func checkWorkerPool(env Env, path, pool string) error {
	if pool == domain.PoolActions {
		return fmt.Errorf("pool %s has no workers: forecrewd runs its jobs on GitHub-hosted runners", pool)
	}
	if _, ok := config.DefaultPools()[pool]; ok {
		return nil
	}
	if !domain.ValidName(pool) {
		return fmt.Errorf("pool %q is not a valid pool name", pool)
	}
	cfg, err := config.Load(path, env.Getenv)
	if err != nil {
		return fmt.Errorf("pool %s is not a default pool and the pools of %s cannot be read: %w", pool, path, err)
	}
	if _, ok := cfg.Pools[pool]; !ok {
		return fmt.Errorf("unknown pool %s: it is not a default pool or under pools in %s", pool, path)
	}
	return nil
}

// moveWorker moves the active worker with the given name or ID to pool and
// records worker.pool_changed with the old and new pool, in one transaction:
// a move changes the worker's trust boundary.
func moveWorker(ctx context.Context, st store.Store, by domain.Actor, nameOrID, pool string) error {
	return st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		workers, err := tx.Workers().List(ctx, by.AccountID)
		if err != nil {
			return fmt.Errorf("list workers: %w", err)
		}
		w, err := store.FindActive(nameOrID, workers, workerInfo)
		if err != nil {
			return fmt.Errorf("worker %q does not exist or is revoked", nameOrID)
		}
		if w.Name == actionsprovider.WorkerName {
			return reservedWorkerError(w.Name)
		}
		if err := tx.Workers().SetPool(ctx, by.AccountID, w.ID, pool); err != nil {
			return fmt.Errorf("move worker %s: %w", nameOrID, err)
		}
		data, err := json.Marshal(map[string]string{"name": w.Name, "from": w.Pool, "to": domain.OrDefaultPool(pool)})
		if err != nil {
			return fmt.Errorf("marshal audit data: %w", err)
		}
		err = tx.Audit().Append(ctx, &domain.AuditEntry{
			AccountID: by.AccountID, Actor: by.String(), Action: "worker.pool_changed", SubjectType: "worker", SubjectID: w.ID, Data: data,
		})
		if err != nil {
			return fmt.Errorf("audit move of worker %s: %w", nameOrID, err)
		}
		return nil
	})
}

// adminName names the person running an admin command in the audit log:
// the login of $USER, or admin.
func adminName(env Env) string {
	if u := env.Getenv("USER"); u != "" {
		return u
	}
	return "admin"
}

// revokeWorker revokes the active worker with the given name or ID.
func revokeWorker(ctx context.Context, st store.Store, accountID uuid.UUID, nameOrID string) error {
	workers, err := st.Workers().List(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	return revokeByNameOrID("worker", nameOrID, workers, workerInfo,
		func(id uuid.UUID) error { return st.Workers().Revoke(ctx, accountID, id) })
}

func workerInfo(w domain.Worker) (uuid.UUID, string, bool) { return w.ID, w.Name, w.Revoked() }

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

// reservedWorkerError says why the worker of the Actions provider cannot be
// created or moved by an admin: it holds the leases of pool actions, which
// only forecrewd serves.
func reservedWorkerError(name string) error {
	return fmt.Errorf("worker name %s is reserved for the GitHub Actions provider of forecrewd and cannot be added or moved", name)
}
