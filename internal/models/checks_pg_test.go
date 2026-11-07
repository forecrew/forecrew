//go:build integration

package models_test

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestOneTestAtATimeOnPostgres: callers that ask at once for a test of the
// same model on the same connection get one test between them.
func TestOneTestAtATimeOnPostgres(t *testing.T) {
	ctx := t.Context()
	pool := testkit.NewDatabase(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	account := accounts[0].ID
	p := domain.Project{AccountID: account, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	if err := st.Projects().Create(ctx, &p); err != nil {
		t.Fatal(err)
	}
	c := conn("cloud", config.AdapterClaudeCode, "anthropic")
	c.AccountID, c.UpdatedBy = account, "user:ana"
	if err := st.ModelConnections().Put(ctx, &c); err != nil {
		t.Fatal(err)
	}
	js := jobs.New(st, testkit.NewSignalBus(st), nil, slog.New(slog.DiscardHandler), time.Now)
	checker := models.NewChecker(st, js, nil)
	by := domain.Actor{AccountID: account, Kind: domain.ActorUser, Name: "ana"}

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() { _, errs[i] = checker.Test(ctx, by, "cloud", "claude-sonnet-4-5") })
	}
	wg.Wait()
	started := 0
	for _, err := range errs {
		switch {
		case err == nil:
			started++
		case !errors.Is(err, models.ErrTestRunning):
			t.Fatalf("err = %v", err)
		}
	}
	if started != 1 {
		t.Fatalf("%d tests started", started)
	}
}
