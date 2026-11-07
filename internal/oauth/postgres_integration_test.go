//go:build integration

package oauth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/oauth"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestOnPostgres runs the flow on the real store: its transactions decide
// the single use of codes and refresh tokens.
func TestOnPostgres(t *testing.T) {
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	e := newEnvOn(t, postgres.New(pool))
	fullFlow(t, e)

	client := e.register(t, callback)
	code := e.code(t, e.params(client), e.a.Slug)
	status, out := e.exchange(t, client, code)
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %v", status, out)
	}
	status, out = e.exchange(t, client, code)
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")

	e.now = e.now.Add(oauth.UnusedClientTTL + 31*24*time.Hour)
	must(t, e.server.Sweep(t.Context()))
}
