package tools_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tools"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
	"github.com/forecrew/forecrew/internal/workflow"
)

type ticketRuns struct{ started int }

func (r *ticketRuns) Start(_ context.Context, _ string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	r.started++
	return domain.WorkflowRun{ID: req.ID}, nil
}

func (r *ticketRuns) CancelRun(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }

type ticketWorld struct {
	*fixture
	fake  *trackertest.Fake
	core  *tracker.Service
	runs  *ticketRuns
	owner domain.Actor
	conn  domain.TrackerConnection
}

func newTicketWorld(t *testing.T) *ticketWorld {
	t.Helper()
	w := &ticketWorld{fake: trackertest.NewFake("s"), runs: &ticketRuns{}}
	registry := tracker.NewRegistry()
	registry.Register(trackertest.Kind, w.fake.Factory())
	w.fixture = newFixture(t, func(c *tools.Config) {
		c.TestingProjects = []string{"app"}
		w.core = tracker.New(tracker.Config{
			Store: c.Store, Registry: registry, Gates: c.Gates, Authz: authz.New(c.Store, nil), Runs: w.runs,
			Playbook: func(domain.Project, domain.GoalKind) string { return tools.PlaybookGoal },
		})
		c.Trackers = w.core
	})
	w.owner, _ = w.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	var err error
	w.conn, err = w.svc.CreateTrackerConnection(t.Context(), w.owner, mcp.CreateTrackerConnectionInput{Name: "board", Kind: trackertest.Kind, Triggers: domain.TrackerTriggers{Label: "forecrew"}})
	must(t, err)
	return w
}

// waiting puts a stranger's ticket in the Inbox and returns it.
func (w *ticketWorld) waiting(t *testing.T, key string) domain.TicketLink {
	t.Helper()
	w.fake.Put(tracker.Ticket{
		Ref: tracker.Ref{Key: key}, Title: "Title of " + key, Body: "Body", Labels: []string{"forecrew"}, URL: "https://t.test/" + key,
		Handovers: []tracker.Handover{{Trigger: tracker.TriggerLabel, Value: "forecrew", By: tracker.User{ID: "99", Name: "stranger"}, At: w.clock.Now()}},
	})
	must(t, w.core.Hint(t.Context(), w.conn, tracker.Event{Key: key}))
	l, err := w.st.TicketLinks().GetLive(t.Context(), w.account.ID, w.conn.ID, key)
	must(t, err)
	return l
}

func (w *ticketWorld) route(t *testing.T) mcp.TrackerRouteInfo {
	t.Helper()
	r, err := w.svc.CreateTrackerRoute(t.Context(), w.owner, mcp.CreateTrackerRouteInput{Connection: "board", Project: "app"})
	must(t, err)
	return r
}

// issueTicket hands over a stranger's ticket that is issue 7 of the project.
func (w *ticketWorld) issueTicket(t *testing.T, key string) domain.TicketLink {
	t.Helper()
	w.fake.Put(tracker.Ticket{
		Ref: tracker.Ref{Key: key}, Title: "crash", Body: "Body", Labels: []string{"forecrew"}, URL: "https://t.test/" + key,
		Issue:     &tracker.RepoIssue{Repo: w.project.Repo, Number: 7},
		Handovers: []tracker.Handover{{Trigger: tracker.TriggerLabel, Value: "forecrew", By: tracker.User{ID: "99", Name: "stranger"}, At: w.clock.Now()}},
	})
	must(t, w.core.Hint(t.Context(), w.conn, tracker.Event{Key: key}))
	l, err := w.st.TicketLinks().GetLive(t.Context(), w.account.ID, w.conn.ID, key)
	must(t, err)
	return l
}

func (w *ticketWorld) goalsOnIssue(t *testing.T, number int) int {
	t.Helper()
	goals, err := w.st.Goals().List(t.Context(), w.account.ID, store.GoalFilter{ProjectID: w.project.ID})
	must(t, err)
	n := 0
	for _, g := range goals {
		if g.IssueNumber == number {
			n++
		}
	}
	return n
}

// A waiting ticket holds its issue against @forecrew bug.
func TestAWaitingTicketKeepsItsIssueFromABugReport(t *testing.T) {
	w := newTicketWorld(t)
	w.route(t)
	l := w.issueTicket(t, "T-1")
	if l.Status != domain.LinkWaiting || l.IssueNumber != 7 {
		t.Fatalf("link = %+v", l)
	}
	_, err := w.svc.ReportIssueAsBug(t.Context(), w.owner, w.project, github.Issue{Number: 7, Title: "crash", Body: "b"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.AcceptTicket(t.Context(), w.owner, mcp.AcceptTicketInput{Ticket: l.ID.String()})
	must(t, err)
	if n := w.goalsOnIssue(t, 7); n != 1 {
		t.Errorf("goals on the issue = %d, want 1", n)
	}
}

// Without a route a ticket cannot know its repo, so the bug report gets there
// first, and accepting the ticket later makes no second goal.
func TestAcceptingATicketWhoseIssueHasAGoalMakesNoSecond(t *testing.T) {
	w := newTicketWorld(t)
	l := w.issueTicket(t, "T-1")
	if l.ProjectID != uuid.Nil() {
		t.Fatalf("link = %+v", l)
	}
	g, err := w.svc.ReportIssueAsBug(t.Context(), w.owner, w.project, github.Issue{Number: 7, Title: "crash", Body: "b"})
	must(t, err)
	got, err := w.svc.AcceptTicket(t.Context(), w.owner, mcp.AcceptTicketInput{Ticket: l.ID.String(), Project: "app"})
	must(t, err)
	if got.Status != domain.LinkCanceled || got.GoalID != uuid.Nil() {
		t.Errorf("accepted = %+v", got)
	}
	if n := w.goalsOnIssue(t, 7); n != 1 {
		t.Errorf("goals on the issue = %d, want 1", n)
	}
	must(t, w.core.Sync(t.Context()))
	if text := w.fake.Ticket("T-1").Comments; len(text) == 0 || !strings.Contains(text[len(text)-1].Body, g.Ref()) {
		t.Errorf("comments = %+v", text)
	}
}

func TestTrackerToolsNeedTheService(t *testing.T) {
	f := newFixture(t)
	by, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	ctx := t.Context()
	_, err := f.svc.CreateTrackerConnection(ctx, by, mcp.CreateTrackerConnectionInput{Name: "x", Kind: "github"})
	wantErr(t, err, mcp.ErrUnavailable)
	_, err = f.svc.CreateTrackerRoute(ctx, by, mcp.CreateTrackerRouteInput{})
	wantErr(t, err, mcp.ErrUnavailable)
	_, err = f.svc.AcceptTicket(ctx, by, mcp.AcceptTicketInput{})
	wantErr(t, err, mcp.ErrUnavailable)
	_, err = f.svc.DismissTicket(ctx, by, mcp.DismissTicketInput{})
	wantErr(t, err, mcp.ErrUnavailable)
}

func TestCreateTrackerConnection(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	label := domain.TrackerTriggers{Label: "x"}
	for name, in := range map[string]mcp.CreateTrackerConnectionInput{
		"bad name":     {Name: "Not Valid", Kind: trackertest.Kind, Triggers: label},
		"no trigger":   {Name: "a", Kind: trackertest.Kind},
		"unknown kind": {Name: "a", Kind: "jira", Triggers: label},
		"a secret":     {Name: "a", Kind: trackertest.Kind, Triggers: label, Config: map[string]any{"api_token": "x"}},
	} {
		_, err := w.svc.CreateTrackerConnection(ctx, w.owner, in)
		if !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := w.svc.CreateTrackerConnection(ctx, w.owner, mcp.CreateTrackerConnectionInput{Name: "board", Kind: trackertest.Kind, Triggers: label})
	wantErr(t, err, mcp.ErrConflict)
	w.fake.Caps.Labels = false
	_, err = w.svc.CreateTrackerConnection(ctx, w.owner, mcp.CreateTrackerConnectionInput{Name: "b", Kind: trackertest.Kind, Triggers: label})
	wantErr(t, err, mcp.ErrInvalidInput)
	got, err := w.svc.ListTrackerConnections(ctx, w.owner)
	must(t, err)
	if len(got) != 1 || got[0].Name != "board" {
		t.Errorf("connections = %+v", got)
	}
}

func TestDeleteTrackerConnection(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	member, _ := w.member(t, orgScope, domain.RoleMember, uuid.Nil())
	_, err := w.svc.DeleteTrackerConnection(ctx, member, mcp.TrackerConnectionRefInput{Connection: "board"})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.DeleteTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	w.route(t)
	l := w.waiting(t, "T-1")
	_, err = w.svc.DeleteTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "board"})
	wantErr(t, err, mcp.ErrConflict)
	info, err := w.svc.AcceptTicket(ctx, w.owner, mcp.AcceptTicketInput{Ticket: l.ID.String()})
	must(t, err)
	goal, err := w.st.Goals().GetByNumber(ctx, w.account.ID, 1)
	must(t, err)
	_, err = w.svc.DeleteTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "board"})
	wantErr(t, err, mcp.ErrConflict) // its goal is at work
	must(t, w.st.Goals().UpdateStatus(ctx, w.account.ID, goal.ID, domain.GoalDraft, domain.GoalCanceled))
	must(t, w.core.Sync(ctx))

	c, err := w.svc.DeleteTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "board"})
	must(t, err)
	if c.Name != "board" {
		t.Errorf("deleted = %+v", c)
	}
	if got, err := w.svc.ListTrackerConnections(ctx, w.owner); err != nil || len(got) != 0 {
		t.Errorf("connections = %+v, %v", got, err)
	}
	// The goal keeps its ticket, so its text stays marked as untrusted.
	d, err := w.svc.GetGoal(ctx, w.owner, mcp.GoalRefInput{Goal: info.GoalRef, WrapUntrusted: true})
	must(t, err)
	if d.Ticket == nil || d.Ticket.Key != "T-1" || d.Ticket.Connection != "" || !strings.Contains(d.Goal.Body, "<untrusted-data") {
		t.Errorf("goal = %+v", d)
	}
}

func TestTrackerRoutes(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	r, err := w.svc.CreateTrackerRoute(ctx, w.owner, mcp.CreateTrackerRouteInput{
		Connection: w.conn.ID.String(), Project: "app", Match: domain.TrackerMatch{Labels: []string{"a"}}, GoalKind: domain.GoalBug, Playbook: tools.PlaybookGoalTesting,
		States: map[domain.TicketState]string{domain.TicketDone: "closed"},
	})
	must(t, err)
	if r.Connection != "board" || r.Project != "app" || r.Position != 1 {
		t.Errorf("route = %+v", r)
	}
	for name, in := range map[string]mcp.CreateTrackerRouteInput{
		"playbook":   {Connection: "board", Project: "app", Playbook: "onboarding"},
		"position":   {Connection: "board", Project: "app", Position: -1},
		"goal kind":  {Connection: "board", Project: "app", GoalKind: "epic"},
		"state":      {Connection: "board", Project: "app", States: map[domain.TicketState]string{domain.TicketDone: "nowhere"}},
		"logical":    {Connection: "board", Project: "app", States: map[domain.TicketState]string{"later": "open"}},
		"closes":     {Connection: "board", Project: "app", States: map[domain.TicketState]string{domain.TicketInProgress: "closed"}},
		"connection": {Project: "app"},
	} {
		if _, err := w.svc.CreateTrackerRoute(ctx, w.owner, in); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = w.svc.CreateTrackerRoute(ctx, w.owner, mcp.CreateTrackerRouteInput{Connection: "nope", Project: "app"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.CreateTrackerRoute(ctx, w.owner, mcp.CreateTrackerRouteInput{Connection: "board", Project: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	w.fake.Caps.Transitions = false
	_, err = w.svc.CreateTrackerRoute(ctx, w.owner, mcp.CreateTrackerRouteInput{Connection: "board", Project: "app", States: map[domain.TicketState]string{domain.TicketDone: "closed"}})
	wantErr(t, err, mcp.ErrInvalidInput)
	got, err := w.svc.ListTrackerRoutes(ctx, w.owner, mcp.TrackerConnectionFilterInput{Connection: "board"})
	must(t, err)
	if len(got) != 1 || got[0].ID != r.ID {
		t.Errorf("routes = %+v", got)
	}
	_, err = w.svc.ListTrackerRoutes(ctx, w.owner, mcp.TrackerConnectionFilterInput{Connection: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.DeleteTrackerRoute(ctx, w.owner, mcp.TrackerRouteRefInput{Route: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.DeleteTrackerRoute(ctx, w.owner, mcp.TrackerRouteRefInput{Route: domain.NewID().String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.DeleteTrackerRoute(ctx, w.owner, mcp.TrackerRouteRefInput{Route: r.ID.String()})
	must(t, err)
}

func TestGoalPlaybook(t *testing.T) {
	tested, plain, councils := domain.Project{Name: "app"}, domain.Project{Name: "other"}, domain.Project{Name: "v3"}
	for _, tt := range []struct {
		p    domain.Project
		kind domain.GoalKind
		want string
	}{
		{tested, domain.GoalBug, tools.PlaybookBugfix},
		{tested, domain.GoalFeature, tools.PlaybookGoalTesting},
		{plain, domain.GoalBug, tools.PlaybookGoal}, // no testing system, no regression test first
		{plain, domain.GoalFeature, tools.PlaybookGoal},
		{councils, domain.GoalBug, tools.PlaybookBugfix}, // goal-v3 has the testing system
		{councils, domain.GoalFeature, tools.PlaybookGoalCouncils},
	} {
		if got := tools.GoalPlaybook([]string{"app"}, []string{"v3"}, tt.p, tt.kind); got != tt.want {
			t.Errorf("%s %s = %s, want %s", tt.p.Name, tt.kind, got, tt.want)
		}
	}
}

// The playbooks of the testing system need it on the route's project.
func TestRoutePlaybookNeedsTheTestingSystem(t *testing.T) {
	w := newTicketWorld(t)
	second := w.secondProject(t)
	_, err := w.svc.CreateTrackerRoute(t.Context(), w.owner, mcp.CreateTrackerRouteInput{Connection: "board", Project: second.Name, Playbook: tools.PlaybookBugfix})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.CreateTrackerRoute(t.Context(), w.owner, mcp.CreateTrackerRouteInput{Connection: "board", Project: "app", Playbook: tools.PlaybookBugfix})
	must(t, err)
}

func TestTrackerToolsNeedTheirPermission(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	viewer, _ := w.member(t, projectScope, domain.RoleViewer, w.project.ID)
	member, _ := w.member(t, orgScope, domain.RoleMember, uuid.Nil())
	// A project viewer reads the project's tickets, not the organization's setup.
	_, err := w.svc.ListTrackerConnections(ctx, viewer)
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.ListTrackerRoutes(ctx, viewer, mcp.TrackerConnectionFilterInput{})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.ListTrackerIdentities(ctx, viewer, mcp.TrackerConnectionFilterInput{})
	wantErr(t, err, authz.ErrDenied)
	// A member reads, and manages nothing.
	_, err = w.svc.ListTrackerConnections(ctx, member)
	must(t, err)
	_, err = w.svc.CreateTrackerConnection(ctx, member, mcp.CreateTrackerConnectionInput{Name: "x", Kind: trackertest.Kind})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.CreateTrackerRoute(ctx, member, mcp.CreateTrackerRouteInput{})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.DeleteTrackerRoute(ctx, member, mcp.TrackerRouteRefInput{})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.MapTrackerIdentity(ctx, member, mcp.MapTrackerIdentityInput{})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.UnmapTrackerIdentity(ctx, member, mcp.TrackerIdentityRefInput{})
	wantErr(t, err, authz.ErrDenied)
}

// A mapping lets whoever is that person on the tracker act as the user, so it
// needs everything the user holds.
func TestMapTrackerIdentityIsNoEscalation(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	admin, _ := w.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	_, ownerID := w.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	_, memberID := w.member(t, orgScope, domain.RoleMember, uuid.Nil())
	_, err := w.svc.MapTrackerIdentity(ctx, admin, mcp.MapTrackerIdentityInput{Connection: "board", ExternalID: "1", User: ownerID.String()})
	wantErr(t, err, authz.ErrDenied)
	i, err := w.svc.MapTrackerIdentity(ctx, admin, mcp.MapTrackerIdentityInput{Connection: "board", ExternalID: "2", ExternalName: "ann", User: memberID.String()})
	must(t, err)
	_, err = w.svc.MapTrackerIdentity(ctx, admin, mcp.MapTrackerIdentityInput{Connection: "board", ExternalID: "2", User: memberID.String()})
	wantErr(t, err, mcp.ErrConflict)
	_, err = w.svc.MapTrackerIdentity(ctx, admin, mcp.MapTrackerIdentityInput{Connection: "board", ExternalID: " ", User: memberID.String()})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.MapTrackerIdentity(ctx, admin, mcp.MapTrackerIdentityInput{Connection: "board", ExternalID: "3", User: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.MapTrackerIdentity(ctx, admin, mcp.MapTrackerIdentityInput{Connection: "board", ExternalID: "3", User: domain.NewID().String()})
	wantErr(t, err, mcp.ErrNotFound)
	got, err := w.svc.ListTrackerIdentities(ctx, admin, mcp.TrackerConnectionFilterInput{Connection: "board"})
	must(t, err)
	if len(got) != 1 || got[0].ID != i.ID {
		t.Errorf("identities = %+v", got)
	}
	_, err = w.svc.ListTrackerIdentities(ctx, admin, mcp.TrackerConnectionFilterInput{Connection: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.UnmapTrackerIdentity(ctx, admin, mcp.TrackerIdentityRefInput{Identity: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.UnmapTrackerIdentity(ctx, admin, mcp.TrackerIdentityRefInput{Identity: domain.NewID().String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.UnmapTrackerIdentity(ctx, admin, mcp.TrackerIdentityRefInput{Identity: i.ID.String()})
	must(t, err)
}

func TestListTicketsWaiting(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	loose := w.waiting(t, "T-1") // no route yet: no project
	w.route(t)
	routed := w.waiting(t, "T-2")
	second := w.secondProject(t)
	elsewhere, _ := w.member(t, projectScope, domain.RoleViewer, second.ID)
	viewer, _ := w.member(t, projectScope, domain.RoleViewer, w.project.ID)

	got, err := w.svc.ListTicketsWaiting(ctx, w.owner, mcp.ListTicketsWaitingInput{})
	must(t, err)
	if len(got) != 2 || got[0].ID != loose.ID || got[1].Project != "app" || got[1].GateRef == "" || got[1].Connection != "board" {
		t.Fatalf("owner sees %+v", got)
	}
	got, err = w.svc.ListTicketsWaiting(ctx, viewer, mcp.ListTicketsWaitingInput{})
	must(t, err)
	if len(got) != 1 || got[0].ID != routed.ID {
		t.Errorf("a project viewer sees %+v", got)
	}
	got, err = w.svc.ListTicketsWaiting(ctx, elsewhere, mcp.ListTicketsWaitingInput{})
	must(t, err)
	if len(got) != 0 {
		t.Errorf("the viewer of another project sees %+v", got)
	}
	_, err = w.svc.ListTicketsWaiting(ctx, elsewhere, mcp.ListTicketsWaitingInput{Project: "app"})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.ListTicketsWaiting(ctx, w.owner, mcp.ListTicketsWaitingInput{Project: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	got, err = w.svc.ListTicketsWaiting(ctx, w.owner, mcp.ListTicketsWaitingInput{Project: "app", WrapUntrusted: true})
	must(t, err)
	if len(got) != 1 || !strings.Contains(got[0].Title, `<untrusted-data source="ticket T-2"`) {
		t.Errorf("for an agent: %+v", got)
	}
}

func TestAcceptTicket(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	loose := w.waiting(t, "T-1")
	w.route(t)
	routed := w.waiting(t, "T-2")
	second := w.secondProject(t)
	contributor, _ := w.member(t, projectScope, domain.RoleContributor, w.project.ID)
	outsider, _ := w.member(t, projectScope, domain.RoleContributor, second.ID)
	viewer, _ := w.member(t, projectScope, domain.RoleViewer, w.project.ID)

	_, err := w.svc.AcceptTicket(ctx, contributor, mcp.AcceptTicketInput{Ticket: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.svc.AcceptTicket(ctx, contributor, mcp.AcceptTicketInput{Ticket: domain.NewID().String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.AcceptTicket(ctx, contributor, mcp.AcceptTicketInput{Ticket: loose.ID.String()})
	wantErr(t, err, mcp.ErrInvalidInput) // no project was guessed
	_, err = w.svc.AcceptTicket(ctx, contributor, mcp.AcceptTicketInput{Ticket: loose.ID.String(), Project: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.AcceptTicket(ctx, outsider, mcp.AcceptTicketInput{Ticket: routed.ID.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.AcceptTicket(ctx, viewer, mcp.AcceptTicketInput{Ticket: routed.ID.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.AcceptTicket(ctx, w.owner, mcp.AcceptTicketInput{Ticket: routed.ID.String(), Project: second.Name})
	wantErr(t, err, mcp.ErrInvalidInput)
	if w.runs.started != 0 {
		t.Fatalf("runs = %d", w.runs.started)
	}

	got, err := w.svc.AcceptTicket(ctx, contributor, mcp.AcceptTicketInput{Ticket: routed.ID.String()})
	must(t, err)
	if got.Status != domain.LinkActive || got.GoalRef == "" || w.runs.started != 1 {
		t.Fatalf("accepted = %+v", got)
	}
	_, err = w.svc.AcceptTicket(ctx, contributor, mcp.AcceptTicketInput{Ticket: routed.ID.String()})
	wantErr(t, err, mcp.ErrConflict)

	d, err := w.svc.GetGoal(ctx, contributor, mcp.GoalRefInput{Goal: got.GoalRef})
	must(t, err)
	if d.Ticket == nil || d.Ticket.Key != "T-2" || d.Ticket.URL != "https://t.test/T-2" || d.Goal.Title != "Title of T-2" {
		t.Errorf("goal = %+v", d)
	}
	d, err = w.svc.GetGoal(ctx, contributor, mcp.GoalRefInput{Goal: got.GoalRef, WrapUntrusted: true})
	must(t, err)
	if !strings.Contains(d.Goal.Title, "<untrusted-data") || !strings.Contains(d.Goal.Body, "<untrusted-data") {
		t.Errorf("for an agent: %+v", d.Goal)
	}
	goals, err := w.svc.ListGoals(ctx, contributor, mcp.ListGoalsInput{Project: "app", WrapUntrusted: true})
	must(t, err)
	if len(goals) != 1 || !strings.Contains(goals[0].Title, "<untrusted-data") {
		t.Errorf("for an agent: %+v", goals)
	}

	notReady := domain.Project{AccountID: w.account.ID, Name: "new", Repo: "acme/new", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, w.st.Projects().Create(ctx, &notReady))
	_, err = w.svc.AcceptTicket(ctx, w.owner, mcp.AcceptTicketInput{Ticket: loose.ID.String(), Project: "new"})
	wantErr(t, err, mcp.ErrInvalidInput)
	got, err = w.svc.AcceptTicket(ctx, w.owner, mcp.AcceptTicketInput{Ticket: loose.ID.String(), Project: second.Name})
	must(t, err)
	if got.Project != second.Name || got.Status != domain.LinkActive {
		t.Errorf("accepted = %+v", got)
	}
}

// Only people resolve gates, and accepting resolves the ticket's.
func TestServiceAccountCannotAcceptATicketWithAGate(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	w.route(t)
	l := w.waiting(t, "T-1")
	sa := domain.ServiceAccount{AccountID: w.account.ID, Name: "ci"}
	must(t, w.st.ServiceAccounts().Create(ctx, &sa))
	testkit.Bind(t, w.st, w.account.ID, domain.SubjectServiceAccount, sa.ID, orgScope, domain.RoleAdmin, uuid.Nil())
	token := domain.MCPToken{AccountID: w.account.ID, Name: "ci", TokenHash: mcp.HashToken("fcm_ci"), OwnerKind: domain.SubjectServiceAccount, OwnerID: sa.ID, Scopes: []domain.Scope{domain.ScopeAdmin}}
	must(t, w.st.MCPTokens().Create(ctx, &token))
	by := domain.ServiceAccountActor(w.account.ID, sa.ID, token.ID)
	_, err := w.svc.AcceptTicket(ctx, by, mcp.AcceptTicketInput{Ticket: l.ID.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.DismissTicket(ctx, by, mcp.DismissTicketInput{Ticket: l.ID.String()})
	wantErr(t, err, authz.ErrDenied)
}

func TestDismissTicket(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	loose := w.waiting(t, "T-1")
	w.route(t)
	routed := w.waiting(t, "T-2")
	second := w.secondProject(t)
	outsider, _ := w.member(t, projectScope, domain.RoleContributor, second.ID)
	contributor, _ := w.member(t, projectScope, domain.RoleContributor, w.project.ID)

	_, err := w.svc.DismissTicket(ctx, outsider, mcp.DismissTicketInput{Ticket: routed.ID.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = w.svc.DismissTicket(ctx, contributor, mcp.DismissTicketInput{Ticket: loose.ID.String()})
	wantErr(t, err, authz.ErrDenied) // no project: only the organization's
	got, err := w.svc.DismissTicket(ctx, contributor, mcp.DismissTicketInput{Ticket: routed.ID.String(), Reason: "not now"})
	must(t, err)
	if got.Status != domain.LinkDismissed {
		t.Errorf("dismissed = %+v", got)
	}
	_, err = w.svc.DismissTicket(ctx, contributor, mcp.DismissTicketInput{Ticket: routed.ID.String()})
	wantErr(t, err, mcp.ErrConflict)
	_, err = w.svc.DismissTicket(ctx, w.owner, mcp.DismissTicketInput{Ticket: loose.ID.String()})
	must(t, err)
	_, err = w.svc.DismissTicket(ctx, w.owner, mcp.DismissTicketInput{Ticket: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
}

// preparing is an adapter that reads its deployment when set up, and tests
// its connection check by check.
type preparing struct {
	*trackertest.Fake
	cfg    map[string]any
	checks []tracker.Check
	fail   error
}

func (p *preparing) Prepare(context.Context) (map[string]any, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	out := maps.Clone(p.cfg)
	out["deployment"] = "cloud"
	return out, nil
}

func (p *preparing) Derived() []string { return []string{"deployment"} }

func (p *preparing) Check(context.Context) []tracker.Check { return p.checks }

func TestPreparedConnections(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	p := &preparing{Fake: w.fake, checks: []tracker.Check{{Name: "address", OK: true}, {Name: "login", OK: true}}}
	w.core.Registry().Register("prep", func(c domain.TrackerConnection) (tracker.Adapter, error) {
		p.cfg = c.Config
		return p, nil
	})
	c, err := w.svc.CreateTrackerConnection(ctx, w.owner, mcp.CreateTrackerConnectionInput{Name: "jira", Kind: "prep", Config: map[string]any{"x": "y"}, Triggers: domain.TrackerTriggers{Label: "x"}})
	must(t, err)
	if c.Config["deployment"] != "cloud" || c.Config["x"] != "y" {
		t.Errorf("config = %+v", c.Config)
	}
	// A connection a plan stored is prepared by its first test that passes.
	c.Config = map[string]any{"x": "z"}
	must(t, w.st.TrackerConnections().Update(ctx, &c))
	p.checks[1] = tracker.Check{Name: "login", Detail: "401"}
	got, err := w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "jira"})
	must(t, err)
	if got.OK || len(got.Checks) != 3 || got.Checks[1].Detail != "401" || got.Connection != "jira" {
		t.Errorf("test = %+v", got)
	}
	if stored, _ := w.st.TrackerConnections().Get(ctx, w.account.ID, c.ID); stored.Config["deployment"] != nil {
		t.Errorf("a failed test changed the config: %+v", stored.Config)
	}
	p.checks[1].OK = true
	got, err = w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: c.ID.String()})
	must(t, err)
	if stored, _ := w.st.TrackerConnections().Get(ctx, w.account.ID, c.ID); !got.OK || stored.Config["deployment"] != "cloud" || stored.Config["x"] != "z" {
		t.Errorf("test = %+v, config %+v", got, stored.Config)
	}
	p.fail = errors.New("serverInfo: 502")
	_, err = w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "jira"})
	if err == nil {
		t.Error("a failed Prepare passed")
	}
	_, err = w.svc.CreateTrackerConnection(ctx, w.owner, mcp.CreateTrackerConnectionInput{Name: "jira2", Kind: "prep", Triggers: domain.TrackerTriggers{Label: "x"}})
	if err == nil {
		t.Error("a connection whose tracker could not be read was created")
	}
	// The fake has no checks of its own: Verify is its one check, and its
	// routes' states are checked against its states.
	got, err = w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "board"})
	must(t, err)
	if !got.OK || len(got.Checks) != 2 || got.Checks[0].Name != "connection" || got.Checks[1].Name != "routes" {
		t.Errorf("test of the fake = %+v", got)
	}
	r := domain.TrackerRoute{AccountID: w.account.ID, ConnectionID: w.conn.ID, ProjectID: w.project.ID, States: map[domain.TicketState]string{domain.TicketDone: "Shipped"}}
	must(t, w.st.TrackerRoutes().Create(ctx, &r))
	got, err = w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "board"})
	must(t, err)
	if got.OK || !strings.Contains(got.Checks[1].Detail, "Shipped") {
		t.Errorf("a route to a state the tracker lacks = %+v", got)
	}
	w.fake.Down = true
	p.checks[1].OK = false
	got, _ = w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "jira"})
	if got.Checks[2].Name != "routes" || !strings.Contains(got.Checks[2].Detail, "not checked") {
		t.Errorf("routes after a failed check = %+v", got)
	}
	w.fake.Down = false
	_, err = w.svc.TestTrackerConnection(ctx, w.owner, mcp.TrackerConnectionRefInput{Connection: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	member, _ := w.member(t, orgScope, domain.RoleMember, uuid.Nil())
	_, err = w.svc.TestTrackerConnection(ctx, member, mcp.TrackerConnectionRefInput{Connection: "jira"})
	wantErr(t, err, authz.ErrDenied)
}
