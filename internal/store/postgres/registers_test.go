//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func TestAssumptions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Assumptions()
	g := f.newGoal(t)
	job := f.newJob(t)
	mk := func(statement string, impact domain.Level, goal uuid.UUID, jobID uuid.UUID) domain.Assumption {
		a := domain.Assumption{AccountID: f.account, ProjectID: f.project.ID, GoalID: goal, JobID: jobID, MadeBy: "architect",
			StatedAssumption: domain.StatedAssumption{Statement: statement, Confidence: domain.LevelMedium, Impact: impact, VerifyBy: domain.VerifyByTest},
			Dependents:       []domain.Dependent{{Kind: domain.DependentGoal, ID: g.Ref()}}}
		must(t, repo.Create(ctx, &a))
		return a
	}
	a1 := mk("single instance", domain.LevelHigh, g.ID, job.ID)
	a2 := mk("utf-8 everywhere", domain.LevelLow, uuid.Nil(), uuid.Nil())
	if a1.Number != 1 || a2.Number != 2 || a1.Ref() != "A-1" || a1.Status != domain.AssumptionUnverified {
		t.Fatalf("created = %+v %+v", a1, a2)
	}
	got, err := repo.Get(ctx, f.account, a1.ID)
	if err != nil || got.GoalID != g.ID || got.JobID != job.ID || got.SpecID != uuid.Nil() || len(got.Dependents) != 1 || !got.ResolvedAt.IsZero() || got.ResolvedBy != "" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if got, err := repo.GetByNumber(ctx, f.account, 2); err != nil || got.ID != a2.ID || got.GoalID != uuid.Nil() {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	_, err = repo.GetByNumber(ctx, f.account, 9)
	wantErr(t, err, store.ErrNotFound)

	for name, c := range map[string]struct {
		filter store.AssumptionFilter
		want   []uuid.UUID
	}{
		"all":      {store.AssumptionFilter{ProjectID: f.project.ID}, []uuid.UUID{a1.ID, a2.ID}},
		"goal":     {store.AssumptionFilter{GoalID: g.ID}, []uuid.UUID{a1.ID}},
		"job":      {store.AssumptionFilter{JobID: job.ID}, []uuid.UUID{a1.ID}},
		"impact":   {store.AssumptionFilter{Impact: domain.LevelLow}, []uuid.UUID{a2.ID}},
		"verify":   {store.AssumptionFilter{VerifyBy: domain.VerifyByTest, Limit: 1}, []uuid.UUID{a1.ID}},
		"resolved": {store.AssumptionFilter{Status: domain.AssumptionAccepted}, []uuid.UUID{}},
	} {
		list, err := repo.List(ctx, f.account, c.filter)
		if err != nil || len(list) != len(c.want) {
			t.Fatalf("%s: list = %+v, %v", name, list, err)
		}
		for i, a := range list {
			if a.ID != c.want[i] {
				t.Fatalf("%s: list[%d] = %s", name, i, a.Ref())
			}
		}
	}

	// Dependents are appended once.
	must(t, repo.AddDependents(ctx, f.account, a1.ID, []domain.Dependent{{Kind: domain.DependentTask, ID: "TASK-1"}, {Kind: domain.DependentGoal, ID: g.Ref()}}))
	must(t, repo.AddDependents(ctx, f.account, a1.ID, []domain.Dependent{{Kind: domain.DependentTask, ID: "TASK-1"}}))
	if got, _ := repo.Get(ctx, f.account, a1.ID); len(got.Dependents) != 2 || got.Dependents[1].ID != "TASK-1" {
		t.Fatalf("dependents = %+v", got.Dependents)
	}
	wantErr(t, repo.AddDependents(ctx, f.account, domain.NewID(), nil), store.ErrNotFound)

	// Resolve is a compare-and-set.
	by := domain.Actor{Kind: domain.ActorUser, Name: "gian"}
	now := time.Now().UTC().Truncate(time.Microsecond)
	cur, _ := repo.Get(ctx, f.account, a1.ID)
	stale := cur
	must(t, cur.Resolve(by, domain.AssumptionAccepted, "", now))
	must(t, repo.Resolve(ctx, &cur, domain.AssumptionUnverified))
	got, _ = repo.Get(ctx, f.account, a1.ID)
	if got.Status != domain.AssumptionAccepted || got.ResolvedBy != "user:gian" || !got.ResolvedAt.Equal(now) {
		t.Fatalf("resolved = %+v", got)
	}
	must(t, stale.Resolve(by, domain.AssumptionVerified, "a test", now))
	wantErr(t, repo.Resolve(ctx, &stale, domain.AssumptionUnverified), store.ErrConflict)
	bad := got
	bad.Status = domain.AssumptionUnverified
	wantErr(t, repo.Resolve(ctx, &bad, domain.AssumptionAccepted), domain.ErrInvalidTransition)
	ghost := got
	ghost.ID, ghost.Status = domain.NewID(), domain.AssumptionVerified
	wantErr(t, repo.Resolve(ctx, &ghost, domain.AssumptionAccepted), store.ErrNotFound)

	// Rows of another account are not reachable.
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	_, err = repo.Get(ctx, other.ID, a1.ID)
	wantErr(t, err, store.ErrNotFound)
	foreign := domain.Assumption{AccountID: other.ID, ProjectID: f.project.ID, MadeBy: "x", StatedAssumption: a1.StatedAssumption}
	wantErr(t, repo.Create(ctx, &foreign), store.ErrNotFound)
}

func TestDecisions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Decisions()
	g := f.newGoal(t)
	a := domain.Assumption{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, MadeBy: "architect",
		StatedAssumption: domain.StatedAssumption{Statement: "s", Confidence: domain.LevelLow, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByResearch}}
	must(t, f.st.Assumptions().Create(ctx, &a))
	gate := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateSpecApproval, Title: "Spec", SubjectRepo: "acme/forecrew", GoalID: g.ID}
	must(t, f.st.Gates().Create(ctx, &gate))

	d1 := domain.Decision{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, Question: "Spec", Choice: "approved", Reasons: "ok",
		DecidedBy: "user:gian", GateID: gate.ID, Assumptions: []uuid.UUID{a.ID},
		Options: []domain.DecisionOption{{Name: "approved", Summary: "go"}}}
	d2 := domain.Decision{AccountID: f.account, ProjectID: f.project.ID, Question: "Q", Choice: "c", DecidedBy: "user:gian"}
	must(t, repo.Create(ctx, &d1))
	must(t, repo.Create(ctx, &d2))
	if d1.Number != 1 || d2.Number != 2 || d1.Ref() != "DEC-1" {
		t.Fatalf("numbers = %d %d", d1.Number, d2.Number)
	}
	got, err := repo.Get(ctx, f.account, d1.ID)
	if err != nil || got.GateID != gate.ID || len(got.Assumptions) != 1 || got.Assumptions[0] != a.ID || got.Options[0].Name != "approved" || got.GoalID != g.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if got, err := repo.GetByNumber(ctx, f.account, 2); err != nil || got.ID != d2.ID || got.GoalID != uuid.Nil() || got.GateID != uuid.Nil() || len(got.Options) != 0 || len(got.Assumptions) != 0 {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	_, err = repo.GetByNumber(ctx, f.account, 3)
	wantErr(t, err, store.ErrNotFound)
	if list, err := repo.List(ctx, f.account, store.DecisionFilter{GoalID: g.ID}); err != nil || len(list) != 1 || list[0].ID != d1.ID {
		t.Fatalf("by goal = %+v, %v", list, err)
	}
	if list, err := repo.List(ctx, f.account, store.DecisionFilter{ProjectID: f.project.ID, Limit: 5}); err != nil || len(list) != 2 {
		t.Fatalf("by project = %+v, %v", list, err)
	}
	if list, err := repo.List(ctx, f.account, store.DecisionFilter{CouncilID: domain.NewID()}); err != nil || len(list) != 0 {
		t.Fatalf("by council = %+v, %v", list, err)
	}

	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	_, err = repo.Get(ctx, other.ID, d1.ID)
	wantErr(t, err, store.ErrNotFound)
	foreign := domain.Decision{AccountID: other.ID, ProjectID: f.project.ID, Question: "Q", Choice: "c", DecidedBy: "user:x"}
	wantErr(t, repo.Create(ctx, &foreign), store.ErrNotFound)
}

func TestAssumptionsRaiseImpact(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Assumptions()
	g := f.newGoal(t)
	mk := func(impact domain.Level) domain.Assumption {
		a := domain.Assumption{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, MadeBy: "architect",
			StatedAssumption: domain.StatedAssumption{Statement: "s", Confidence: domain.LevelLow, Impact: impact, VerifyBy: domain.VerifyByTest}}
		must(t, repo.Create(ctx, &a))
		return a
	}
	high, low := mk(domain.LevelHigh), mk(domain.LevelLow)
	// The impact only goes up.
	must(t, repo.RaiseImpact(ctx, f.account, high.ID, domain.LevelLow))
	if got, _ := repo.Get(ctx, f.account, high.ID); got.Impact != domain.LevelHigh {
		t.Fatalf("impact lowered: %+v", got)
	}
	for _, to := range []domain.Level{domain.LevelMedium, domain.LevelMedium, domain.LevelHigh} {
		must(t, repo.RaiseImpact(ctx, f.account, low.ID, to))
		if got, _ := repo.Get(ctx, f.account, low.ID); got.Impact != to {
			t.Fatalf("impact = %+v, want %s", got, to)
		}
	}
	wantErr(t, repo.RaiseImpact(ctx, f.account, domain.NewID(), domain.LevelHigh), store.ErrNotFound)
	must(t, repo.Lock(ctx, f.account, f.project.ID, g.ID))
	must(t, repo.Lock(ctx, f.account, f.project.ID, uuid.Nil()))
}

// Lock holds the registrations of one goal, and only that goal's, until the
// transaction that took it ends.
func TestAssumptionsLock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g, other := f.newGoal(t), f.newGoal(t)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			if err := tx.Assumptions().Lock(ctx, f.account, f.project.ID, g.ID); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	got := make(chan struct{})
	go func() {
		done <- f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			err := tx.Assumptions().Lock(ctx, f.account, f.project.ID, g.ID)
			close(got)
			return err
		})
	}()
	// Another goal is not held up.
	must(t, f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		return tx.Assumptions().Lock(ctx, f.account, f.project.ID, other.ID)
	}))
	select {
	case <-got:
		t.Fatal("the second transaction got the lock while the first held it")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the second transaction never got the lock")
	}
	for range 2 {
		must(t, <-done)
	}
}
