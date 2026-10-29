//go:build integration

package workerapi

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// newBackend returns a Postgres store on a fresh database of
// FORECREW_TEST_DATABASE_URL.
func newBackend(t *testing.T) backend {
	t.Helper()
	pool := testkit.NewDatabase(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(t.Context())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("default account: %v, %v", accounts, err)
	}
	return backend{
		st: st, clock: testkit.NewClock(time.Now()), account: accounts[0].ID,
		expireLeases: func(t *testing.T) {
			t.Helper()
			if _, err := pool.Exec(t.Context(), `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE status = 'running'`); err != nil {
				t.Fatal(err)
			}
		},
	}
}
