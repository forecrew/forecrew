package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/tracker"
)

type ticketSink struct {
	kinds []string
	err   error
}

func (s *ticketSink) Deliver(_ context.Context, kind string, r tracker.Request) error {
	s.kinds = append(s.kinds, kind+":"+r.Header.Get("X-GitHub-Event"))
	return s.err
}

// Issue events reach the trackers once, with what the adapter needs to check
// the signature; other events do not.
func TestIssueEventsReachTheTrackers(t *testing.T) {
	e := newEnv(t)
	sink := &ticketSink{}
	e.h.e.Tickets = sink
	issue := []byte(`{"action":"labeled","issue":{"number":3},"repository":{"full_name":"acme/app"},"sender":{"login":"ann"}}`)
	for _, id := range []string{"d1", "d1"} { // redelivered
		if code := e.deliver(id, "issues", issue); code != 202 {
			t.Fatalf("status = %d", code)
		}
	}
	e.process()
	if code := e.deliver("d2", "status", []byte(`{"state":"success","sha":"a","repository":{"full_name":"acme/app"}}`)); code != 202 {
		t.Fatalf("status = %d", code)
	}
	e.process()
	if len(sink.kinds) != 1 || sink.kinds[0] != "github:issues" {
		t.Errorf("deliveries = %v", sink.kinds)
	}
	sink.err = errors.New("down")
	if code := e.deliver("d3", "issues", issue); code != 202 {
		t.Fatalf("status = %d", code)
	}
	e.process() // logged: polling recovers it
}

// A comment never approves a gate of a ticket, even from who could approve it
// in Forecrew. Elsewhere commands work as before, the allowlist included.
func TestApprovalsOfATicketStayInForecrew(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	g := domain.Goal{AccountID: e.account, ProjectID: e.project.ID, Title: "t", Status: domain.GoalSpecReview, CreatedBy: "user:x", IssueNumber: 1}
	must(t, e.st.Goals().Create(ctx, &g))
	conn := domain.TrackerConnection{AccountID: e.account, Name: "issues", Kind: "github", Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	must(t, e.st.TrackerConnections().Create(ctx, &conn))
	must(t, e.st.TicketLinks().Create(ctx, &domain.TicketLink{AccountID: e.account, ConnectionID: conn.ID, Key: "acme/app#1", Status: domain.LinkActive, ProjectID: e.project.ID, GoalID: g.ID, IssueNumber: 1}))
	_, err := e.tracker.CreateIssue(ctx, repo, github.IssueInput{Title: "ticket"})
	must(t, err)
	spec := e.openGate(domain.GateSpecApproval, 1)
	e.comment("@forecrew approve")
	got, err := e.gates.Get(ctx, e.account, spec.ID)
	must(t, err)
	if got.Status != domain.GatePending {
		t.Fatalf("a comment approved %+v", got)
	}
	e.comment("@forecrew changes do it differently")
	if got, _ = e.gates.Get(ctx, e.account, spec.ID); got.Status != domain.GatePending {
		t.Fatalf("a comment sent back %+v", got)
	}
	comments, err := e.tracker.ListComments(ctx, repo, 1, time.Time{})
	must(t, err)
	if len(comments) != 2 || !strings.Contains(comments[0].Body, "stay in Forecrew") {
		t.Errorf("replies = %+v", comments)
	}
	e.comment("@forecrew status") // reading is fine
	if comments, _ = e.tracker.ListComments(ctx, repo, 1, time.Time{}); !strings.Contains(comments[2].Body, spec.Ref()) {
		t.Errorf("status = %+v", comments[2])
	}

	// The link ended: the issue is an issue again.
	l, err := e.st.TicketLinks().GetByGoal(ctx, e.account, g.ID)
	must(t, err)
	l.Status = domain.LinkDone
	must(t, e.st.TicketLinks().Update(ctx, &l))
	e.comment("@forecrew approve")
	if got, _ = e.gates.Get(ctx, e.account, spec.ID); got.Status != domain.GateApproved {
		t.Errorf("the allowlist stopped working: %+v", got)
	}
}

// A card event of a project board reaches the trackers of boards, and only
// those; it needs no repo.
func TestProjectItemEventsReachTheBoardTrackers(t *testing.T) {
	e := newEnv(t)
	sink := &ticketSink{}
	e.h.e.Tickets = sink
	card := []byte(`{"action":"edited","projects_v2_item":{"node_id":"PVTI_1","content_type":"Issue"},"organization":{"login":"acme"},"sender":{"login":"ann"}}`)
	for _, id := range []string{"c1", "c1"} { // redelivered
		if code := e.deliver(id, "projects_v2_item", card); code != 202 {
			t.Fatalf("status = %d", code)
		}
	}
	e.process()
	if len(sink.kinds) != 1 || sink.kinds[0] != "github_project:projects_v2_item" {
		t.Errorf("deliveries = %v", sink.kinds)
	}
	if code := e.deliver("c2", "projects_v2_item", []byte("nope")); code != 400 {
		t.Errorf("an unreadable payload: status %d", code)
	}
}
