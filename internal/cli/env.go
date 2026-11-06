package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
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
	// ConfigDir is the user's config directory, where forecrew login keeps
	// its credentials; nil keeps none.
	ConfigDir func() (string, error)
	// Keyring is the OS keychain; nil, or one that fails, keeps tokens in
	// the credentials file.
	Keyring Keyring
	// OpenBrowser opens a URL in the user's browser.
	OpenBrowser func(url string) error
	// Hostname names this machine, for the token a login creates.
	Hostname func() (string, error)
	// Stdin is where a login reads a pasted code from; nil reads none.
	Stdin io.Reader
}

// OSEnv is the environment of the real process.
func OSEnv() Env {
	return Env{Getenv: os.Getenv, HTTP: http.DefaultClient, Open: OpenPostgres, ConfigDir: os.UserConfigDir, Keyring: osKeyring{},
		OpenBrowser: openBrowser, Hostname: os.Hostname, Stdin: os.Stdin}
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

// openBrowser opens url with the desktop's opener.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url) //nolint:gosec // the URL is the one the user asked to log in to
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) //nolint:gosec // as above
	default:
		cmd = exec.Command("xdg-open", url) //nolint:gosec // as above
	}
	return cmd.Start()
}
