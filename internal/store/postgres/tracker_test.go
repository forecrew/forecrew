//go:build integration

package postgres

import (
	"slices"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestTrackers(t *testing.T) {
	f := newFixture(t)
	var user uuid.UUID
	must(t, f.pool.QueryRow(t.Context(), `INSERT INTO users (name, email, email_verified) VALUES ('Ann', 'ann@example.com', true) RETURNING id`).Scan(&user))
	testkit.TrackerStoreSuite(t, f.st, f.account, f.project.ID, user)
}

func TestRequestCancel(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	run := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID, Playbook: "goal", Status: domain.RunRunning}
	must(t, f.st.WorkflowRuns().Create(ctx, &run))
	must(t, f.st.WorkflowRuns().RequestCancel(ctx, f.account, run.ID, "the ticket was closed"))
	got, err := f.st.WorkflowRuns().Get(ctx, f.account, run.ID)
	must(t, err)
	if got.CancelReason != "the ticket was closed" {
		t.Errorf("cancel reason = %q", got.CancelReason)
	}
	got.Status = domain.RunSucceeded
	must(t, f.st.WorkflowRuns().Update(ctx, &got))
	must(t, f.st.WorkflowRuns().RequestCancel(ctx, f.account, run.ID, "too late"))
	if got, _ = f.st.WorkflowRuns().Get(ctx, f.account, run.ID); got.CancelReason != "the ticket was closed" {
		t.Errorf("a finished run took a cancel: %q", got.CancelReason)
	}
}

// 00011 rolls back with tickets and their gates in place.
func TestTrackerMigrationRollsBack(t *testing.T) {
	pool := newPoolAt(t, 10)
	ctx := t.Context()
	p := provider(t, pool)
	_, err := p.UpTo(ctx, 11)
	must(t, err)
	st := New(pool)
	var account uuid.UUID
	must(t, pool.QueryRow(ctx, `SELECT id FROM accounts`).Scan(&account))
	project := domain.Project{AccountID: account, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &project))
	conn := domain.TrackerConnection{AccountID: account, Name: "board", Kind: "fake", Triggers: domain.TrackerTriggers{Label: "x"}}
	must(t, st.TrackerConnections().Create(ctx, &conn))
	gate := domain.Gate{AccountID: account, ProjectID: project.ID, Kind: domain.GateTicketIntake, Title: "t", SubjectRepo: "acme/app"}
	must(t, st.Gates().Create(ctx, &gate))
	must(t, st.TicketLinks().Create(ctx, &domain.TicketLink{AccountID: account, ConnectionID: conn.ID, Key: "T-1", Status: domain.LinkWaiting, ProjectID: project.ID, GateID: gate.ID}))
	_, err = p.DownTo(ctx, 10)
	must(t, err)
	var perms []string
	must(t, pool.QueryRow(ctx, `SELECT permissions FROM roles WHERE built_in AND name = 'owner'`).Scan(&perms))
	if slices.Contains(perms, "trackers.read") {
		t.Errorf("owner still has trackers.read: %v", perms)
	}
	_, err = p.Up(ctx)
	must(t, err)
}

// 00012 gives the routes of a connection one position each, and rolls back.
func TestRoutePositionsMigration(t *testing.T) {
	pool := newPoolAt(t, 11)
	ctx := t.Context()
	p := provider(t, pool)
	st := New(pool)
	var account uuid.UUID
	must(t, pool.QueryRow(ctx, `SELECT id FROM accounts`).Scan(&account))
	project := domain.Project{AccountID: account, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &project))
	conn := domain.TrackerConnection{AccountID: account, Name: "board", Kind: "fake", Triggers: domain.TrackerTriggers{Label: "x"}}
	must(t, st.TrackerConnections().Create(ctx, &conn))
	for range 2 {
		_, err := pool.Exec(ctx, `INSERT INTO tracker_routes (account_id, connection_id, position, project_id) VALUES ($1, $2, 5, $3)`, account, conn.ID, project.ID)
		must(t, err)
	}
	_, err := p.UpTo(ctx, 12)
	must(t, err)
	routes, err := st.TrackerRoutes().List(ctx, account, conn.ID)
	must(t, err)
	if len(routes) != 2 || routes[0].Position != 1 || routes[1].Position != 2 || routes[0].Warning != "" {
		t.Errorf("routes = %+v", routes)
	}
	_, err = p.DownTo(ctx, 11)
	must(t, err)
	_, err = p.Up(ctx)
	must(t, err)
}
