package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
)

// Environment variables of the CLI.
const (
	EnvURL   = "FORECREW_URL"   // base URL of forecrewd, for the commands that call /api/v1
	EnvToken = "FORECREW_TOKEN" // API token, for the same commands
)

// DefaultURL is the base URL when neither --url nor FORECREW_URL is set.
const DefaultURL = "http://localhost:8080"

// Database is an open connection of the admin commands.
type Database interface {
	Store() store.Store
	// Migrate applies the schema migrations.
	Migrate(ctx context.Context) error
	Close()
}

// Env is what the commands depend on, so tests can replace it.
type Env struct {
	Getenv func(string) string
	// HTTP calls /api/v1; defaults to http.DefaultClient.
	HTTP *http.Client
	// Open connects to the database named by url.
	Open func(ctx context.Context, url string) (Database, error)
	// GitHubAPI is GitHub's API, which turns a login into its stable user
	// ID; it defaults to api.github.com.
	GitHubAPI string
}

// OSEnv is the environment of the real process.
func OSEnv() Env {
	return Env{Getenv: os.Getenv, HTTP: http.DefaultClient, Open: OpenPostgres}
}

// openDatabase opens the database of FORECREW_DATABASE_URL.
func (e Env) openDatabase(ctx context.Context) (Database, error) {
	url := e.Getenv(config.EnvDatabaseURL)
	if url == "" {
		return nil, fmt.Errorf("%s is not set", config.EnvDatabaseURL)
	}
	return e.Open(ctx, url)
}

// OpenPostgres connects to Postgres.
func OpenPostgres(ctx context.Context, url string) (Database, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pgDatabase{pool}, nil
}

type pgDatabase struct{ pool *pgxpool.Pool }

func (d pgDatabase) Store() store.Store                { return postgres.New(d.pool) }
func (d pgDatabase) Migrate(ctx context.Context) error { return postgres.Migrate(ctx, d.pool) }
func (d pgDatabase) Close()                            { d.pool.Close() }

// account returns the account the admin commands work on: the one named by
// the --account flag, or the only account of the installation.
func account(ctx context.Context, st store.Store, flag string) (uuid.UUID, error) {
	if flag != "" {
		id, err := uuid.Parse(flag)
		if err != nil {
			return uuid.Nil(), fmt.Errorf("--account %q is not a UUID", flag)
		}
		a, err := st.Accounts().Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return uuid.Nil(), fmt.Errorf("account %s does not exist", id)
		}
		return a.ID, err
	}
	accounts, err := st.Accounts().List(ctx)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("list accounts: %w", err)
	}
	switch len(accounts) {
	case 0:
		return uuid.Nil(), errors.New("there is no account yet: run forecrew migrate up")
	case 1:
		return accounts[0].ID, nil
	}
	return uuid.Nil(), errors.New("there are several accounts: pass --account")
}
