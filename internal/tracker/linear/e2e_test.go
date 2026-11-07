package linear

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/workflow"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// runs records the goals the core starts.
type runs struct {
	mu      sync.Mutex
	started []workflow.StartRequest
}

func (r *runs) Start(_ context.Context, _ string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, req)
	return domain.WorkflowRun{ID: req.ID}, nil
}

func (r *runs) CancelRun(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }

// A Linear workspace from connection to done, through the tracker core and
// nothing of Linear in it: a connection that tests well, a route, a ticket a
// mapped person hands over by a signed webhook, a goal that starts, and the
// ticket's state and comments following the goal. This is the third tracker
// family: the core was not changed to serve it.
func TestALinearTicketBecomesAGoalAndGetsItsStatus(t *testing.T) {
	ctx := t.Context()
	f := newFakeLinear(t)
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(ctx, &acc))
	vault, err := secrets.New(st.Secrets(), make([]byte, secrets.KeySize))
	must(t, err)
	by := domain.SystemActor(acc.ID, domain.ForecrewdName)
	must(t, vault.Put(ctx, by, acc.ID, "linear-key", []byte(f.key)))
	must(t, vault.Put(ctx, by, acc.ID, "linear-hook", []byte("hook-secret")))

	// Connect: the adapter's checks pass, then the connection is stored.
	conn := domain.TrackerConnection{
		AccountID: acc.ID, Name: "linear", Kind: Kind, Triggers: domain.TrackerTriggers{Label: "forecrew"},
		Config: map[string]any{"teams": []any{"ENG"}, "token_ref": "store:linear-key", "webhook_secret_ref": "store:linear-hook"},
	}
	registry := tracker.NewRegistry()
	registry.Register(Kind, Factory(Deps{Secrets: vault, Endpoint: f.srv.URL, PrivateAddresses: true, RootCAs: []*x509.Certificate{f.srv.Certificate()}, Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }}))
	ad, err := registry.Build(conn)
	must(t, err)
	if err := tracker.Failed(tracker.Test(ctx, ad)); err != nil {
		t.Fatal(err)
	}
	must(t, tracker.CheckTriggers(conn, ad.Capabilities()))
	must(t, st.TrackerConnections().Create(ctx, &conn))

	// A project, a person who may start goals there, and who they are on Linear.
	app := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &app))
	ann := domain.User{Name: "Marta Rossi", Email: "marta@example.com"}
	must(t, st.Users().Create(ctx, &ann))
	must(t, st.Memberships().Create(ctx, &domain.Membership{AccountID: acc.ID, UserID: ann.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	testkit.Bind(t, st, acc.ID, domain.SubjectUser, ann.ID, domain.RoleScopeOrganization, domain.RoleMember, uuid.Nil())
	testkit.Bind(t, st, acc.ID, domain.SubjectUser, ann.ID, domain.RoleScopeProject, domain.RoleContributor, app.ID)
	must(t, st.TrackerIdentities().Create(ctx, &domain.TrackerIdentity{AccountID: acc.ID, ConnectionID: conn.ID, ExternalID: "u-marta", ExternalName: "Marta Rossi", UserID: ann.ID, CreatedBy: "user:test"}))

	// A route, with the statuses the team has.
	route := domain.TrackerRoute{AccountID: acc.ID, ConnectionID: conn.ID, ProjectID: app.ID, Match: domain.TrackerMatch{Project: "ENG"},
		States: map[domain.TicketState]string{domain.TicketInProgress: "In Progress", domain.TicketInReview: "In Review", domain.TicketDone: "Done"}}
	must(t, st.TrackerRoutes().Create(ctx, &route))
	if states, err := ad.States(ctx); err != nil || len(states) != 6 {
		t.Fatalf("states = %+v, %v", states, err)
	}

	r := &runs{}
	svc := tracker.New(tracker.Config{
		Store: st, Registry: registry, Gates: testkit.NewGates(st, testkit.NewSignalBus(st), nil), Authz: authz.New(st, nil), Runs: r, MaxActiveGoals: 2, PublicURL: "https://forecrew.test",
		Playbook: func(domain.Project, domain.GoalKind) string { return "goal" },
	})

	// Hand a ticket over: Marta labels it, Linear sends a signed webhook.
	key := f.create("ENG", "Sign in with an email link", "Let people sign in without a password.")
	f.edit(key, func(i *fakeIssue) {
		i.Labels = []string{"forecrew"}
		i.History = []fakeHistory{{At: f.now, Actor: "u-marta", Label: "forecrew"}}
	})
	if err := svc.Deliver(ctx, Kind, f.issueEvent("wrong", key)); !errors.Is(err, tracker.ErrBadSignature) {
		t.Fatalf("a forged delivery: %v", err)
	}
	svc.Drain(ctx)
	if _, err := st.TicketLinks().GetLive(ctx, acc.ID, conn.ID, key); err == nil {
		t.Fatal("a forged delivery made a ticket")
	}
	must(t, svc.Deliver(ctx, Kind, f.issueEvent("hook-secret", key)))
	svc.Drain(ctx)
	link, err := st.TicketLinks().GetLive(ctx, acc.ID, conn.ID, key)
	must(t, err)
	if link.Status != domain.LinkActive || link.GoalID == uuid.Nil() || link.ProjectID != app.ID || link.URL != "https://linear.app/acme/issue/"+key {
		t.Fatalf("link = %+v", link)
	}
	goal, err := st.Goals().Get(ctx, acc.ID, link.GoalID)
	must(t, err)
	if goal.Title != "Sign in with an email link" || goal.CreatedBy != domain.UserActor(acc.ID, ann.ID, domain.Credential{}).String() || len(r.started) != 1 {
		t.Fatalf("goal = %+v, runs %d", goal, len(r.started))
	}

	// The goal advances; the ticket follows, in the team's own states, with a comment.
	for _, to := range []domain.GoalStatus{domain.GoalSpecReview, domain.GoalPlanned, domain.GoalInProgress} {
		must(t, st.Goals().UpdateStatus(ctx, acc.ID, goal.ID, goal.Status, to))
		goal.Status = to
	}
	must(t, svc.Sync(ctx))
	f.mu.Lock()
	issue := f.issues[key]
	state, comments := issue.State, append([]fakeComment(nil), issue.Comments...)
	f.mu.Unlock()
	if state != "In Progress" {
		t.Errorf("state = %q after the goal started", state)
	}
	if len(comments) == 0 || !strings.Contains(comments[0].Body, "https://forecrew.test/goals/GOAL-") {
		t.Errorf("comments = %+v", comments)
	}
	must(t, st.Goals().UpdateStatus(ctx, acc.ID, goal.ID, goal.Status, domain.GoalDone))
	must(t, svc.Sync(ctx))
	f.mu.Lock()
	state, closed := issue.State, f.stateType("ENG", issue.State)
	f.mu.Unlock()
	if state != "Done" || closed != "completed" {
		t.Errorf("state = %q (%s) after the goal was done", state, closed)
	}
	// The write of Forecrew's own is no handover: polling finds the ticket changed and starts nothing.
	f.mu.Lock()
	cursor := f.now.Add(-time.Hour).Format(time.RFC3339Nano)
	f.mu.Unlock()
	events, _, err := ad.Changes(ctx, cursor)
	must(t, err)
	for _, ev := range events {
		must(t, svc.Hint(ctx, conn, ev))
	}
	if len(r.started) != 1 {
		t.Errorf("a finished ticket started %d goals", len(r.started))
	}
}
