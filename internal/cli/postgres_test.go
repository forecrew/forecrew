//go:build integration

package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
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
	run("org", "create", "Acme Labs")
	run("user", "add", "ana@example.com", "--org", "default", "--role", "owner")
	run("user", "add", "bea@example.com", "--org", "acme-labs")
	run("session", "revoke", "--user", "ana@example.com")
	run("user", "disable", "bea@example.com")
	secret := strings.TrimSpace(run("token", "create", "laptop", "--user", "ana@example.com", "--account", defaultAccount(t, url)))
	acc := "--account=" + defaultAccount(t, url)
	worker := strings.TrimSpace(run("worker", "add", "build-1", acc))
	run("worker", "revoke", "build-1", acc)
	ctl := strings.TrimSpace(run("worker", "add", "ctl-1", "--pool", "control", acc))
	run("worker", "move", "ctl-1", "--pool", "build", acc)

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
	moved, err := db.Store().Workers().GetByTokenHash(t.Context(), mcp.HashToken(ctl))
	if err != nil || moved.Pool != "build" {
		t.Fatalf("moved worker = %+v, %v", moved, err)
	}
	audit, err := db.Store().Audit().List(t.Context(), moved.AccountID, store.AuditFilter{SubjectID: moved.ID})
	if err != nil || len(audit) != 1 || audit[0].Action != "worker.pool_changed" || audit[0].Actor != "user:admin" {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	var data map[string]string
	if err := json.Unmarshal(audit[0].Data, &data); err != nil || !maps.Equal(data, map[string]string{"from": "control", "name": "ctl-1", "to": "build"}) {
		t.Fatalf("audit data = %s, %v", audit[0].Data, err)
	}
}

// defaultAccount is the ID of the organization created at install.
func defaultAccount(t *testing.T, url string) string {
	t.Helper()
	db, err := OpenPostgres(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.Store().Accounts().GetBySlug(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	return a.ID.String()
}

func TestOpenPostgresBadURL(t *testing.T) {
	if _, err := OpenPostgres(t.Context(), "postgres://%zz"); err == nil {
		t.Error("want an error for a malformed URL")
	}
}
