//go:build !integration

package workerapi

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
)

// newBackend returns the in-memory store; the integration build runs the
// same tests on Postgres.
func newBackend(t *testing.T) backend {
	t.Helper()
	clock := testkit.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	if err := st.Accounts().Create(t.Context(), &acc); err != nil {
		t.Fatal(err)
	}
	return backend{
		st: st, clock: clock, account: acc.ID,
		expireLeases: func(*testing.T) { clock.Advance(domain.LeaseDuration + time.Second) },
	}
}
