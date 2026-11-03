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
	secret := strings.TrimSpace(run("token", "create", "laptop"))
	worker := strings.TrimSpace(run("worker", "add", "build-1"))
	run("worker", "revoke", "build-1")
	ctl := strings.TrimSpace(run("worker", "add", "ctl-1", "--pool", "control"))
	run("worker", "move", "ctl-1", "--pool", "build")

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

func TestOpenPostgresBadURL(t *testing.T) {
	if _, err := OpenPostgres(t.Context(), "postgres://%zz"); err == nil {
		t.Error("want an error for a malformed URL")
	}
}
