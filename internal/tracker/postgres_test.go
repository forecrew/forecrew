//go:build integration

package tracker_test

import (
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestScenariosOnPostgres runs every scenario on the real store: one database,
// one organization per scenario.
func TestScenariosOnPostgres(t *testing.T) {
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	st := postgres.New(pool)
	for _, name := range names() {
		t.Run(name, func(t *testing.T) {
			// Sync and Poll cover every organization: each scenario has only
			// its own connection.
			_, err := pool.Exec(t.Context(), `TRUNCATE tracker_connections CASCADE`)
			must(t, err)
			acc := domain.Account{Name: name}
			must(t, st.Accounts().Create(t.Context(), &acc))
			scenarios()[name](t, newWorld(t, st, acc.ID))
		})
	}
}
