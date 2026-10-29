//go:build integration

package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew/internal/domain"
)

// newPool migrates a fresh database and returns a pool on it.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("FORECREW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FORECREW_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "fc_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// fixture is a migrated database with the default account and a project.
type fixture struct {
	pool    *pgxpool.Pool
	st      *Store
	account uuid.UUID
	project domain.Project
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := newPool(t)
	st := New(pool)
	accounts, err := st.Accounts().List(t.Context())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("default account: %v, %v", accounts, err)
	}
	f := &fixture{pool: pool, st: st, account: accounts[0].ID}
	f.project = f.newProject(t, f.account, "forecrew")
	return f
}

func (f *fixture) newProject(t *testing.T, account uuid.UUID, name string) domain.Project {
	t.Helper()
	p := domain.Project{AccountID: account, Name: name, Repo: "Acme/" + name}
	must(t, f.st.Projects().Create(t.Context(), &p))
	return p
}

func (f *fixture) newGoal(t *testing.T) domain.Goal {
	t.Helper()
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Ship it", CreatedBy: "user:me"}
	must(t, f.st.Goals().Create(t.Context(), &g))
	return g
}

func (f *fixture) newSpec(t *testing.T, g domain.Goal, revision int) domain.Spec {
	t.Helper()
	s := domain.Spec{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, Revision: revision, Body: "spec"}
	must(t, f.st.Specs().Create(t.Context(), &s))
	return s
}

func (f *fixture) newTask(t *testing.T, g domain.Goal, s domain.Spec, seq int) domain.Task {
	t.Helper()
	task := domain.Task{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, SpecID: s.ID, Seq: seq, Title: "Do it"}
	must(t, f.st.Tasks().Create(t.Context(), &task))
	return task
}

func (f *fixture) newJob(t *testing.T, needs ...string) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code", Needs: needs,
		Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "go", Env: map[string]string{"SECRET": "x"}, RepoToken: "tok"}}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	return j
}

func (f *fixture) newWorker(t *testing.T, name string, labels ...string) domain.Worker {
	t.Helper()
	w := domain.Worker{AccountID: f.account, Name: name, TokenHash: []byte("hash-" + name), Labels: labels}
	must(t, f.st.Workers().Create(t.Context(), &w))
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
