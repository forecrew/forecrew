//go:build integration

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestPostgresAdminCommands runs the admin commands against a real database.
func TestPostgresAdminCommands(t *testing.T) {
	pool := testkit.NewDatabase(t)
	c := pool.Config().ConnConfig
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", c.User, c.Password, c.Host, c.Port, c.Database)
	env := OSEnv()
	env.Getenv = func(k string) string {
		if k == config.EnvDatabaseURL {
			return url
		}
		return ""
	}
	run := func(args ...string) string {
		t.Helper()
		var out, errOut strings.Builder
		if code := RunWith(t.Context(), args, &out, &errOut, "test", env); code != 0 {
			t.Fatalf("%v: code %d, %s", args, code, errOut.String())
		}
		return out.String()
	}
	run("migrate", "up")
	run("migrate", "up") // again: nothing to do
	secret := strings.TrimSpace(run("token", "create", "laptop"))
	worker := strings.TrimSpace(run("worker", "add", "build-1"))
	run("worker", "revoke", "build-1")

	db, err := OpenPostgres(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tok, err := db.Store().MCPTokens().GetByTokenHash(t.Context(), mcp.HashToken(secret))
	if err != nil || tok.Name != "laptop" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	w, err := db.Store().Workers().GetByTokenHash(t.Context(), mcp.HashToken(worker))
	if err != nil || !w.Revoked() {
		t.Fatalf("worker = %+v, %v", w, err)
	}
}

func TestOpenPostgresBadURL(t *testing.T) {
	if _, err := OpenPostgres(t.Context(), "postgres://%zz"); err == nil {
		t.Error("want an error for a malformed URL")
	}
}
