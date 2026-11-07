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

// ProposalStoreSuite checks the contract of store.ProposalRepo on st, the
// fake and the Postgres store alike.
func ProposalStoreSuite(t *testing.T, st store.Store, account, project uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	repo := st.Proposals()
	mk := func(key string) *domain.Proposal {
		return &domain.Proposal{AccountID: account, ProjectID: project, Scope: domain.ScopeProject, Category: domain.CategoryWorkers, Title: "Add a worker",
			Rationale: "Jobs wait", Benefit: "No waiting", Effort: domain.EffortS, Risk: domain.LevelLow, Status: domain.ProposalProposed, DedupeKey: key,
			EvidenceWeight: 2, Action: domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "Add it"}},
			Evidence: []domain.ProposalEvidence{{Kind: "workers", Ref: "JOB-1", Detail: "waited"}}, Measure: &domain.ProposalMeasure{Detector: "workers", Value: 2}}
	}
	p := mk("suite:a")
	mustT(t, repo.Create(ctx, p))
	proposalReads(t, repo, p, account, project)
	if err := repo.Create(ctx, mk("suite:a")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second open proposal: %v", err)
	}
	proposalMoves(t, repo, p, account)
	// A final proposal frees its key.
	q := mk("suite:a")
	q.Category = domain.CategoryCost
	mustT(t, repo.Create(ctx, q))
	proposalLists(t, repo, p, q, account, project)
	q.Status = domain.ProposalSuperseded
	mustT(t, repo.Update(ctx, q, domain.ProposalProposed))
}

// proposalReads checks the lookups of p, freshly created.
func proposalReads(t *testing.T, repo store.ProposalRepo, p *domain.Proposal, account, project uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if p.ID == uuid.Nil() || p.Number == 0 || p.CreatedAt.IsZero() {
		t.Fatalf("create filled %+v", p)
	}
	got, err := repo.Get(ctx, account, p.ID)
	if err != nil || got.Ref() != p.Ref() || got.Action.Setup == nil || got.Measure == nil || got.Measure.Value != 2 || len(got.Evidence) != 1 || got.Outcome != nil || !got.SnoozedUntil.IsZero() {
		t.Fatalf("get: %+v, %v", got, err)
	}
	if got, err = repo.GetByNumber(ctx, account, p.Number); err != nil || got.ID != p.ID {
		t.Fatalf("by number: %+v, %v", got, err)
	}
	if got, err = repo.GetOpen(ctx, account, project, "suite:a"); err != nil || got.ID != p.ID {
		t.Fatalf("open: %+v, %v", got, err)
	}
	if _, err := repo.GetOpen(ctx, account, project, "suite:none"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open of nothing: %v", err)
	}
	if _, err := repo.Get(ctx, uuid.New(), p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another account: %v", err)
	}
}

// proposalMoves takes p, proposed, through to done with an outcome.
func proposalMoves(t *testing.T, repo store.ProposalRepo, p *domain.Proposal, account uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	until := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	mustT(t, p.Decide(domain.Actor{Kind: domain.ActorUser, Name: "ann"}, domain.ProposalSnooze, "after the release", until, until.Add(-time.Hour)))
	mustT(t, repo.Update(ctx, p, domain.ProposalProposed))
	got, _ := repo.Get(ctx, account, p.ID)
	if got.Status != domain.ProposalSnoozed || !got.SnoozedUntil.Equal(until) || got.Reason != "after the release" || got.DecidedBy == "" {
		t.Fatalf("snoozed: %+v", got)
	}
	if err := repo.Update(ctx, p, domain.ProposalProposed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a stale status: %v", err)
	}
	p.Status = domain.ProposalDone
	if err := repo.Update(ctx, p, domain.ProposalSnoozed); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a refused move: %v", err)
	}
	p.Status, p.SnoozedUntil = domain.ProposalAccepted, time.Time{}
	mustT(t, repo.Update(ctx, p, domain.ProposalSnoozed))
	p.Status = domain.ProposalDone
	mustT(t, repo.Update(ctx, p, domain.ProposalAccepted))
	// A final proposal keeps its status while its outcome is written.
	p.Outcome = &domain.ProposalOutcome{Before: 2, After: 0, MeasuredAt: until}
	mustT(t, repo.Update(ctx, p, domain.ProposalDone))
	if got, _ = repo.Get(ctx, account, p.ID); got.Outcome == nil || got.Outcome.Before != 2 || !got.Outcome.MeasuredAt.Equal(until) {
		t.Fatalf("outcome: %+v", got)
	}
}

// proposalLists checks the filters on p, done, and q, open, of one key.
func proposalLists(t *testing.T, repo store.ProposalRepo, p, q *domain.Proposal, account, project uuid.UUID) {
	t.Helper()
	list := func(f store.ProposalFilter) []domain.Proposal {
		t.Helper()
		f.ProjectID = project
		out, err := repo.List(context.Background(), account, f)
		mustT(t, err)
		return out
	}
	if n := len(list(store.ProposalFilter{DedupeKey: "suite:a"})); n != 2 {
		t.Fatalf("by key: %d", n)
	}
	if got := list(store.ProposalFilter{Status: domain.ProposalDone, DedupeKey: "suite:a"}); len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("by status: %+v", got)
	}
	if got := list(store.ProposalFilter{Category: domain.CategoryCost, DedupeKey: "suite:a"}); len(got) != 1 || got[0].ID != q.ID {
		t.Fatalf("by category: %+v", got)
	}
	if n := len(list(store.ProposalFilter{GateID: uuid.New()})) + len(list(store.ProposalFilter{GoalID: uuid.New()})); n != 0 {
		t.Fatalf("by an unknown link: %d", n)
	}
}
