//go:build integration

package testkit

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvTestDatabaseURL names the Postgres URL integration tests use.
const EnvTestDatabaseURL = "FORECREW_TEST_DATABASE_URL"

// NewDatabase creates an empty database on the server of
// FORECREW_TEST_DATABASE_URL and returns a pool on it; the database is
// dropped when the test ends. The test is skipped when the variable is not
// set. Callers migrate the schema themselves (postgres.Migrate).
func NewDatabase(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(EnvTestDatabaseURL)
	if url == "" {
		t.Skip(EnvTestDatabaseURL + " is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := "fc_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
