package testkit

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// TrackerStoreSuite checks the contract of the tracker repositories on st,
// the fake and the Postgres store alike. user must be a user of the account.
func TrackerStoreSuite(t *testing.T, st store.Store, account, project, user uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	conn := domain.TrackerConnection{AccountID: account, Name: "issues", Kind: "fake", Config: map[string]any{"x": "y"}, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	mustT(t, st.TrackerConnections().Create(ctx, &conn))
	trackerConnections(ctx, t, st, conn)
	route := trackerRoutes(ctx, t, st, conn, project)
	trackerIdentities(ctx, t, st, conn, user)
	trackerCursors(ctx, t, st, conn)
	link := ticketLinks(ctx, t, st, conn, project, route)
	trackerOutbox(ctx, t, st, link)
	deleteConnection(ctx, t, st, conn, link)
}

// deleteConnection checks that a connection goes with what hangs on it, and
// that its ended links stay.
func deleteConnection(ctx context.Context, t *testing.T, st store.Store, conn domain.TrackerConnection, live domain.TicketLink) {
	t.Helper()
	acc, repo := conn.AccountID, st.TrackerConnections()
	if err := repo.Delete(ctx, acc, conn.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Delete with a live ticket: %v", err)
	}
	live.Status = domain.LinkDismissed
	mustT(t, st.TicketLinks().Update(ctx, &live))
	mustT(t, st.TrackerCursors().Set(ctx, acc, conn.ID, "c3"))
	mustT(t, repo.Delete(ctx, acc, conn.ID))
	if err := repo.Delete(ctx, acc, conn.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second Delete: %v", err)
	}
	if got, err := st.TrackerRoutes().List(ctx, acc, conn.ID); err != nil || len(got) != 0 {
		t.Errorf("routes = %+v, %v", got, err)
	}
	if got, err := st.TrackerCursors().Get(ctx, acc, conn.ID); err != nil || got != "" {
		t.Errorf("cursor = %q, %v", got, err)
	}
	got, err := st.TicketLinks().Get(ctx, acc, live.ID)
	if err != nil || got.ConnectionID != uuid.Nil() || got.Key != live.Key {
		t.Errorf("ended link = %+v, %v", got, err)
	}
	if err := repo.Delete(ctx, uuid.NewV7(), conn.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete from another account: %v", err)
	}
}

func trackerConnections(ctx context.Context, t *testing.T, st store.Store, conn domain.TrackerConnection) {
	t.Helper()
	repo := st.TrackerConnections()
	same := domain.TrackerConnection{AccountID: conn.AccountID, Name: conn.Name, Kind: "fake", Triggers: conn.Triggers}
	if err := repo.Create(ctx, &same); !errors.Is(err, store.ErrConflict) {
		t.Errorf("same name: %v", err)
	}
	if err := repo.Create(ctx, &domain.TrackerConnection{AccountID: conn.AccountID, Name: "no-trigger", Kind: "fake"}); err == nil {
		t.Error("a connection without a trigger was stored")
	}
	got, err := repo.Get(ctx, conn.AccountID, conn.ID)
	if err != nil || got.Config["x"] != "y" || got.Triggers.Label != "forecrew" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if got, err := repo.GetByName(ctx, conn.AccountID, conn.Name); err != nil || got.ID != conn.ID {
		t.Errorf("GetByName = %+v, %v", got, err)
	}
	if got, err := repo.Find(ctx, conn.ID); err != nil || got.AccountID != conn.AccountID {
		t.Errorf("Find = %+v, %v", got, err)
	}
	if got, err := repo.List(ctx, conn.AccountID); err != nil || len(got) != 1 {
		t.Errorf("List = %+v, %v", got, err)
	}
	if got, err := repo.ListByKind(ctx, "fake"); err != nil || len(got) != 1 {
		t.Errorf("ListByKind = %+v, %v", got, err)
	}
	if _, err := repo.Get(ctx, uuid.NewV7(), conn.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get from another account: %v", err)
	}
	changed := got
	changed.Name, changed.Kind, changed.Config, changed.Triggers = "renamed", "other", map[string]any{"x": "z"}, domain.TrackerTriggers{State: "Ready"}
	mustT(t, repo.Update(ctx, &changed))
	if got, err := repo.Get(ctx, conn.AccountID, conn.ID); err != nil || got.Name != conn.Name || got.Kind != "fake" || got.Config["x"] != "z" || got.Triggers.State != "Ready" {
		t.Errorf("updated connection = %+v, %v", got, err)
	}
	changed.Config, changed.Triggers = conn.Config, conn.Triggers
	mustT(t, repo.Update(ctx, &changed))
	missing := changed
	missing.ID = uuid.NewV7()
	if err := repo.Update(ctx, &missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update of nothing: %v", err)
	}
}

func trackerRoutes(ctx context.Context, t *testing.T, st store.Store, conn domain.TrackerConnection, project uuid.UUID) domain.TrackerRoute {
	t.Helper()
	repo := st.TrackerRoutes()
	first := domain.TrackerRoute{
		AccountID: conn.AccountID, ConnectionID: conn.ID, ProjectID: project, Match: domain.TrackerMatch{Labels: []string{"a"}},
		GoalKind: domain.GoalBug, Playbook: "goal", States: map[domain.TicketState]string{domain.TicketDone: "closed"},
	}
	second := domain.TrackerRoute{AccountID: conn.AccountID, ConnectionID: conn.ID, ProjectID: project}
	mustT(t, repo.Create(ctx, &first))
	mustT(t, repo.Create(ctx, &second))
	if first.Position != 1 || second.Position != 2 {
		t.Errorf("positions = %d, %d", first.Position, second.Position)
	}
	if err := repo.Create(ctx, &domain.TrackerRoute{AccountID: conn.AccountID, ConnectionID: conn.ID, ProjectID: project, GoalKind: "epic"}); err == nil {
		t.Error("a route with an unknown goal kind was stored")
	}
	got, err := repo.List(ctx, conn.AccountID, conn.ID)
	if err != nil || len(got) != 2 || got[0].ID != first.ID || got[0].States[domain.TicketDone] != "closed" || got[0].Match.Labels[0] != "a" || got[0].GoalKind != domain.GoalBug {
		t.Errorf("List = %+v, %v", got, err)
	}
	if all, err := repo.List(ctx, conn.AccountID, uuid.Nil()); err != nil || len(all) != 2 {
		t.Errorf("List of every connection = %+v, %v", all, err)
	}
	taken := domain.TrackerRoute{AccountID: conn.AccountID, ConnectionID: conn.ID, ProjectID: project, Position: 2}
	if err := repo.Create(ctx, &taken); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a taken position: %v", err)
	}
	second.Match, second.Playbook, second.Position = domain.TrackerMatch{Project: "ENG"}, "goal", 9
	mustT(t, repo.Update(ctx, &second))
	mustT(t, repo.SetWarning(ctx, conn.AccountID, second.ID, "no transition reaches Done"))
	if got, err := repo.Get(ctx, conn.AccountID, second.ID); err != nil || got.Match.Project != "ENG" || got.Playbook != "goal" || got.Position != 2 ||
		got.Warning != "no transition reaches Done" {
		t.Errorf("updated route = %+v, %v", got, err)
	}
	mustT(t, repo.Update(ctx, &second))
	if got, err := repo.Get(ctx, conn.AccountID, second.ID); err != nil || got.Warning != "" || second.Warning != "" {
		t.Errorf("an edited route kept its warning: %+v, %v", got, err)
	}
	if err := repo.SetWarning(ctx, conn.AccountID, uuid.NewV7(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetWarning of nothing: %v", err)
	}
	if err := repo.Update(ctx, &domain.TrackerRoute{AccountID: conn.AccountID, ID: uuid.NewV7(), ProjectID: project}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update of nothing: %v", err)
	}
	mustT(t, repo.Delete(ctx, conn.AccountID, second.ID))
	if err := repo.Delete(ctx, conn.AccountID, second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second Delete: %v", err)
	}
	if got, err := repo.Get(ctx, conn.AccountID, first.ID); err != nil || got.Playbook != "goal" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	return first
}

func trackerIdentities(ctx context.Context, t *testing.T, st store.Store, conn domain.TrackerConnection, user uuid.UUID) {
	t.Helper()
	repo := st.TrackerIdentities()
	i := domain.TrackerIdentity{AccountID: conn.AccountID, ConnectionID: conn.ID, ExternalID: "42", ExternalName: "ann", UserID: user, CreatedBy: "user:x"}
	mustT(t, repo.Create(ctx, &i))
	twin := i
	twin.ID = uuid.Nil()
	if err := repo.Create(ctx, &twin); !errors.Is(err, store.ErrConflict) {
		t.Errorf("same external user: %v", err)
	}
	if got, err := repo.Find(ctx, conn.AccountID, conn.ID, "42"); err != nil || got.UserID != user {
		t.Errorf("Find = %+v, %v", got, err)
	}
	if _, err := repo.Find(ctx, conn.AccountID, conn.ID, "43"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Find of a stranger: %v", err)
	}
	if got, err := repo.Get(ctx, conn.AccountID, i.ID); err != nil || got.ExternalName != "ann" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if got, err := repo.List(ctx, conn.AccountID, conn.ID); err != nil || len(got) != 1 {
		t.Errorf("List = %+v, %v", got, err)
	}
	mustT(t, repo.Delete(ctx, conn.AccountID, i.ID))
	if err := repo.Delete(ctx, conn.AccountID, i.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second Delete: %v", err)
	}
}

func trackerCursors(ctx context.Context, t *testing.T, st store.Store, conn domain.TrackerConnection) {
	t.Helper()
	repo := st.TrackerCursors()
	if got, err := repo.Get(ctx, conn.AccountID, conn.ID); err != nil || got != "" {
		t.Errorf("first Get = %q, %v", got, err)
	}
	mustT(t, repo.Set(ctx, conn.AccountID, conn.ID, "c1"))
	mustT(t, repo.Set(ctx, conn.AccountID, conn.ID, "c2"))
	if got, err := repo.Get(ctx, conn.AccountID, conn.ID); err != nil || got != "c2" {
		t.Errorf("Get = %q, %v", got, err)
	}
}

func ticketLinks(ctx context.Context, t *testing.T, st store.Store, conn domain.TrackerConnection, project uuid.UUID, route domain.TrackerRoute) domain.TicketLink {
	t.Helper()
	repo := st.TicketLinks()
	acc := conn.AccountID
	l := domain.TicketLink{AccountID: acc, ConnectionID: conn.ID, Key: "T-1", Title: "One", Status: domain.LinkWaiting, Reason: domain.WaitNoRoute}
	mustT(t, repo.Create(ctx, &l))
	twin := domain.TicketLink{AccountID: acc, ConnectionID: conn.ID, Key: "T-1", Status: domain.LinkWaiting}
	if err := repo.Create(ctx, &twin); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second live link: %v", err)
	}
	if got, err := repo.GetLive(ctx, acc, conn.ID, "T-1"); err != nil || got.ID != l.ID || got.Reason != domain.WaitNoRoute {
		t.Errorf("GetLive = %+v, %v", got, err)
	}
	if _, err := repo.LastEnded(ctx, acc, conn.ID, "T-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LastEnded of a live ticket: %v", err)
	}
	g := domain.Goal{AccountID: acc, ProjectID: project, Title: "One", Status: domain.GoalDraft, CreatedBy: "user:x", IssueNumber: 7}
	mustT(t, st.Goals().Create(ctx, &g))
	goalKinds(ctx, t, st, g)
	l.Status, l.Reason, l.ProjectID, l.RouteID, l.GoalID, l.IssueNumber = domain.LinkActive, "", project, route.ID, g.ID, 7
	l.State, l.ContentHash, l.CommentCursor, l.HandedBy = domain.TicketInProgress, "hash", "c9", "ann"
	mustT(t, repo.Update(ctx, &l))
	activeLink(ctx, t, repo, l, g, route)
	other := domain.TicketLink{AccountID: acc, ConnectionID: conn.ID, Key: "T-2", Status: domain.LinkDismissed, GoalID: g.ID}
	if err := repo.Create(ctx, &other); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second link of a goal: %v", err)
	}
	l.Status = domain.LinkDone
	mustT(t, repo.Update(ctx, &l))
	if _, err := repo.GetLive(ctx, acc, conn.ID, "T-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetLive of an ended ticket: %v", err)
	}
	if got, err := repo.LastEnded(ctx, acc, conn.ID, "T-1"); err != nil || got.ID != l.ID {
		t.Errorf("LastEnded = %+v, %v", got, err)
	}
	if got, err := repo.List(ctx, acc, store.TicketLinkFilter{Live: true}); err != nil || len(got) != 0 {
		t.Errorf("List of live links = %+v, %v", got, err)
	}
	again := domain.TicketLink{AccountID: acc, ConnectionID: conn.ID, Key: "T-1", Status: domain.LinkWaiting}
	mustT(t, repo.Create(ctx, &again)) // a new handover after the old link ended
	return again
}

// goalKinds checks that a goal keeps its kind, and that none is a feature.
func goalKinds(ctx context.Context, t *testing.T, st store.Store, plain domain.Goal) {
	t.Helper()
	bug := domain.Goal{AccountID: plain.AccountID, ProjectID: plain.ProjectID, Kind: domain.GoalBug, Title: "Bug", Status: domain.GoalDraft, CreatedBy: "user:x"}
	mustT(t, st.Goals().Create(ctx, &bug))
	if got, err := st.Goals().Get(ctx, bug.AccountID, bug.ID); err != nil || got.Kind != domain.GoalBug {
		t.Errorf("a bug goal = %+v, %v", got, err)
	}
	if got, err := st.Goals().Get(ctx, plain.AccountID, plain.ID); err != nil || got.Kind != domain.GoalFeature {
		t.Errorf("a goal without a kind = %+v, %v", got, err)
	}
}

// activeLink checks every way to find the link l of goal g.
func activeLink(ctx context.Context, t *testing.T, repo store.TicketLinkRepo, l domain.TicketLink, g domain.Goal, route domain.TrackerRoute) {
	t.Helper()
	acc, project := l.AccountID, l.ProjectID
	got, err := repo.GetByGoal(ctx, acc, g.ID)
	if err != nil || got.ID != l.ID || got.State != domain.TicketInProgress || got.ContentHash != "hash" || got.CommentCursor != "c9" || got.HandedBy != "ann" || got.RouteID != route.ID {
		t.Errorf("GetByGoal = %+v, %v", got, err)
	}
	if got, err := repo.GetByIssue(ctx, acc, project, 7); err != nil || got.ID != l.ID {
		t.Errorf("GetByIssue = %+v, %v", got, err)
	}
	if n, err := repo.CountActive(ctx, acc, project); err != nil || n != 1 {
		t.Errorf("CountActive = %d, %v", n, err)
	}
	if got, err := repo.List(ctx, acc, store.TicketLinkFilter{Status: domain.LinkActive, ProjectID: project, ConnectionID: l.ConnectionID, Live: true, Limit: 1}); err != nil || len(got) != 1 {
		t.Errorf("List = %+v, %v", got, err)
	}
}

// dropTransitions checks that only unsent transitions of the link go.
func dropTransitions(ctx context.Context, t *testing.T, repo store.TrackerOutboxRepo, link domain.TicketLink, now time.Time) {
	t.Helper()
	write := func(key string, kind domain.TrackerWriteKind) *domain.TrackerWrite {
		w := domain.TrackerWrite{AccountID: link.AccountID, ConnectionID: link.ConnectionID, LinkID: link.ID, Key: link.Key, DedupeKey: key, Kind: kind, Value: "v", NextAttempt: now}
		if _, err := repo.Enqueue(ctx, &w); err != nil {
			t.Fatal(err)
		}
		return &w
	}
	sent := write("state:sent", domain.WriteTransition)
	mustT(t, repo.MarkSent(ctx, link.AccountID, sent.ID, "", now))
	write("state:stale", domain.WriteTransition)
	comment := write("comment:kept", domain.WriteComment)
	mustT(t, repo.DropTransitions(ctx, link.AccountID, link.ID))
	due, err := repo.Due(ctx, link.AccountID, now.Add(time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].ID != comment.ID {
		t.Errorf("Due after DropTransitions = %+v, %v", due, err)
	}
	mustT(t, repo.MarkSent(ctx, link.AccountID, comment.ID, "", now))
}

func trackerOutbox(ctx context.Context, t *testing.T, st store.Store, link domain.TicketLink) {
	t.Helper()
	repo := st.TrackerOutbox()
	acc := link.AccountID
	now := time.Now().UTC()
	w := domain.TrackerWrite{AccountID: acc, ConnectionID: link.ConnectionID, LinkID: link.ID, Key: link.Key, DedupeKey: "goal", Kind: domain.WriteComment, Value: "hello", NextAttempt: now.Add(-time.Minute)}
	if isNew, err := repo.Enqueue(ctx, &w); err != nil || !isNew {
		t.Fatalf("Enqueue = %v, %v", isNew, err)
	}
	twin := w
	twin.ID = uuid.Nil()
	if isNew, err := repo.Enqueue(ctx, &twin); err != nil || isNew {
		t.Errorf("second Enqueue = %v, %v", isNew, err)
	}
	due, err := repo.Due(ctx, acc, now, 10)
	if err != nil || len(due) != 1 || due[0].Value != "hello" || due[0].Key != link.Key || due[0].Kind != domain.WriteComment {
		t.Fatalf("Due = %+v, %v", due, err)
	}
	mustT(t, repo.MarkFailed(ctx, acc, w.ID, "down", now.Add(time.Hour)))
	if due, err := repo.Due(ctx, acc, now, 10); err != nil || len(due) != 0 {
		t.Errorf("Due while backing off = %+v, %v", due, err)
	}
	due, err = repo.Due(ctx, acc, now.Add(2*time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].Attempts != 1 || due[0].LastError != "down" {
		t.Fatalf("Due after the wait = %+v, %v", due, err)
	}
	mustT(t, repo.MarkSent(ctx, acc, w.ID, "c-1", now))
	if due, err := repo.Due(ctx, acc, now.Add(2*time.Hour), 10); err != nil || len(due) != 0 {
		t.Errorf("Due after sending = %+v, %v", due, err)
	}
	if ok, err := repo.Sent(ctx, acc, link.ID, "c-1"); err != nil || !ok {
		t.Errorf("Sent = %v, %v", ok, err)
	}
	if ok, err := repo.Sent(ctx, acc, link.ID, "c-2"); err != nil || ok {
		t.Errorf("Sent of a person's comment = %v, %v", ok, err)
	}
	if err := repo.MarkSent(ctx, acc, uuid.NewV7(), "", now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("MarkSent of nothing: %v", err)
	}
	dropTransitions(ctx, t, repo, link, now)
}
