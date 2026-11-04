package tools_test

import (
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/tools"
)

// registerWorld is a goal with two tasks and three assumptions: a high one
// both tasks rest on, a medium one and a low one of the first.
type registerWorld struct {
	*fixture
	goal  domain.Goal
	tasks []domain.Task
	job   domain.Job
	as    []domain.Assumption
}

func newRegisterWorld(t *testing.T) *registerWorld {
	t.Helper()
	f := newFixture(t)
	ctx := t.Context()
	w := &registerWorld{fixture: f, goal: f.newGoal(t, "Login")}
	spec := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: w.goal.ID, Revision: 1, Body: "s"}
	must(t, f.st.Specs().Create(ctx, &spec))
	for i := range 2 {
		tk := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: w.goal.ID, SpecID: spec.ID, Seq: i + 1, Title: "t", Status: domain.TaskOpen}
		must(t, f.st.Tasks().Create(ctx, &tk))
		w.tasks = append(w.tasks, tk)
	}
	w.job = f.newJob(t, "developer", w.goal, w.tasks[0], 0)
	stated := func(s string, impact domain.Level, by domain.VerifyBy) domain.StatedAssumption {
		return domain.StatedAssumption{Statement: s, Confidence: domain.LevelLow, Impact: impact, VerifyBy: by}
	}
	record := func(src registers.Source, st domain.StatedAssumption) {
		got, err := f.regs.Record(ctx, f.account.ID, f.project.ID, src, []domain.StatedAssumption{st})
		must(t, err)
		w.as = append(w.as, got[0])
	}
	record(registers.Source{MadeBy: "developer", GoalID: w.goal.ID, TaskID: w.tasks[0].ID, JobID: w.job.ID}, stated("Single instance", domain.LevelHigh, domain.VerifyByTest))
	record(registers.Source{MadeBy: "developer", GoalID: w.goal.ID, TaskID: w.tasks[1].ID}, stated("Single instance", domain.LevelHigh, domain.VerifyByTest))
	record(registers.Source{MadeBy: "developer", GoalID: w.goal.ID, TaskID: w.tasks[0].ID}, stated("Email is reliable", domain.LevelMedium, domain.VerifyByAskHuman))
	record(registers.Source{MadeBy: "architect", GoalID: w.goal.ID}, stated("UTF-8", domain.LevelLow, domain.VerifyByResearch))
	return w
}

func refsOf(infos []mcp.AssumptionInfo) []string {
	var out []string
	for _, i := range infos {
		out = append(out, i.Ref)
	}
	return out
}

func TestListAssumptions(t *testing.T) {
	w := newRegisterWorld(t)
	ctx := t.Context()
	other, _, _ := w.otherAccount(t)
	for _, tt := range []struct {
		name string
		in   mcp.ListAssumptionsInput
		want []string
	}{
		{"all", mcp.ListAssumptionsInput{Project: "app"}, []string{"A-1", "A-2", "A-3"}},
		{"by goal", mcp.ListAssumptionsInput{Project: "app", Goal: w.goal.Ref()}, []string{"A-1", "A-2", "A-3"}},
		// The goal's own assumption (A-3, the architect's) is one every task rests on.
		{"by task", mcp.ListAssumptionsInput{Project: "acme/app", Task: w.tasks[0].Ref()}, []string{"A-1", "A-2", "A-3"}},
		{"by task and goal", mcp.ListAssumptionsInput{Project: "app", Task: w.tasks[1].Ref(), Goal: w.goal.Ref()}, []string{"A-1", "A-3"}},
		{"high", mcp.ListAssumptionsInput{Project: "app", Impact: domain.LevelHigh}, []string{"A-1"}},
		{"to test", mcp.ListAssumptionsInput{Project: "app", Task: w.tasks[0].ID.String(), VerifyBy: domain.VerifyByTest}, []string{"A-1"}},
		{"accepted", mcp.ListAssumptionsInput{Project: "app", Status: domain.AssumptionAccepted}, nil},
	} {
		got, err := w.svc.ListAssumptions(ctx, w.by, tt.in)
		must(t, err)
		if !slices.Equal(refsOf(got), tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, refsOf(got), tt.want)
		}
	}
	got, _ := w.svc.ListAssumptions(ctx, w.by, mcp.ListAssumptionsInput{Project: "app"})
	if got[0].Source != w.job.Ref() || got[0].Statement != "Single instance" || len(got[0].Dependents) != 3 {
		t.Fatalf("A-1 = %+v", got[0])
	}
	if got[1].Source != "" {
		t.Fatalf("source of a goal-level assumption: %q", got[1].Source)
	}

	bad := []mcp.ListAssumptionsInput{
		{}, {Project: "app", Status: "maybe"}, {Project: "app", Impact: "huge"}, {Project: "app", VerifyBy: "magic"},
		{Project: "app", Goal: "nonsense"}, {Project: "app", Task: "nonsense"},
	}
	for _, in := range bad {
		if _, err := w.svc.ListAssumptions(ctx, w.by, in); err == nil {
			t.Errorf("%+v was accepted", in)
		}
	}
	_, err := w.svc.ListAssumptions(ctx, w.by, mcp.ListAssumptionsInput{Project: "app", Goal: "GOAL-99"})
	wantErr(t, err, mcp.ErrNotFound)
	// A task of another goal does not fit the goal filter.
	g2 := w.newGoal(t, "Other")
	_, err = w.svc.ListAssumptions(ctx, w.by, mcp.ListAssumptionsInput{Project: "app", Goal: g2.Ref(), Task: w.tasks[0].Ref()})
	wantErr(t, err, mcp.ErrInvalidInput)
	// A task of another project does not fit the project.
	p2 := domain.Project{AccountID: w.account.ID, Name: "second", Repo: "acme/second", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, w.st.Projects().Create(ctx, &p2))
	_, err = w.svc.ListAssumptions(ctx, w.by, mcp.ListAssumptionsInput{Project: "second", Task: w.tasks[0].Ref()})
	wantErr(t, err, mcp.ErrInvalidInput)
	// Another account sees nothing of it.
	if _, err := w.svc.ListAssumptions(ctx, other, mcp.ListAssumptionsInput{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	got, _ = w.svc.ListAssumptions(ctx, other, mcp.ListAssumptionsInput{Project: "app"})
	if len(got) != 0 {
		t.Fatalf("another account sees %+v", got)
	}
}

func TestResolveAssumption(t *testing.T) {
	w := newRegisterWorld(t)
	ctx := t.Context()
	other, _, _ := w.otherAccount(t)

	for _, in := range []mcp.ResolveAssumptionInput{
		{Assumption: "A-1", Status: "unverified"}, {Assumption: "A-1", Status: "maybe"}, {Assumption: "nonsense", Status: domain.AssumptionAccepted},
		{Assumption: "A-1", Status: domain.AssumptionVerified}, {Assumption: "A-1", Status: domain.AssumptionInvalidated, Evidence: " "},
	} {
		if _, err := w.svc.ResolveAssumption(ctx, w.by, in); !errorsIsInvalid(err) {
			t.Errorf("%+v: err = %v, want invalid input", in, err)
		}
	}
	_, err := w.svc.ResolveAssumption(ctx, w.by, mcp.ResolveAssumptionInput{Assumption: "A-99", Status: domain.AssumptionAccepted})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = w.svc.ResolveAssumption(ctx, other, mcp.ResolveAssumptionInput{Assumption: "A-1", Status: domain.AssumptionAccepted})
	wantErr(t, err, mcp.ErrNotFound)

	got, err := w.svc.ResolveAssumption(ctx, w.by, mcp.ResolveAssumptionInput{Assumption: "A-1", Status: domain.AssumptionVerified, Evidence: "TestSingle passes"})
	must(t, err)
	if got.Ref != "A-1" || got.Status != domain.AssumptionVerified || got.ResolvedBy != "mcp:laptop" || got.Source != w.job.Ref() {
		t.Fatalf("resolved = %+v", got)
	}
	_, err = w.svc.ResolveAssumption(ctx, w.by, mcp.ResolveAssumptionInput{Assumption: w.as[0].ID.String(), Status: domain.AssumptionAccepted})
	wantErr(t, err, domain.ErrInvalidTransition)
}

func errorsIsInvalid(err error) bool { return err != nil && strings.Contains(err.Error(), "invalid") }

func TestListDecisions(t *testing.T) {
	w := newRegisterWorld(t)
	ctx := t.Context()
	council := uuid.MustParse("0192f5a0-0000-7000-8000-00000000000c")
	d1 := domain.Decision{AccountID: w.account.ID, ProjectID: w.project.ID, GoalID: w.goal.ID, Question: "Spec", Choice: "approved", DecidedBy: "user:me", Assumptions: []uuid.UUID{w.as[0].ID, w.as[2].ID}}
	d2 := domain.Decision{AccountID: w.account.ID, ProjectID: w.project.ID, Question: "Q", Choice: "x", DecidedBy: "user:me"}
	must(t, w.st.Decisions().Create(ctx, &d1))
	must(t, w.st.Decisions().Create(ctx, &d2))

	got, err := w.svc.ListDecisions(ctx, w.by, mcp.ListDecisionsInput{Project: "app"})
	must(t, err)
	if len(got) != 2 || got[0].Ref != "DEC-1" || !slices.Equal(got[0].Assumptions, []string{"A-1", "A-2"}) || got[1].Assumptions == nil || len(got[1].Assumptions) != 0 {
		t.Fatalf("decisions = %+v", got)
	}
	if got, _ := w.svc.ListDecisions(ctx, w.by, mcp.ListDecisionsInput{Project: "app", Goal: w.goal.Ref()}); len(got) != 1 {
		t.Fatalf("by goal = %+v", got)
	}
	if got, _ := w.svc.ListDecisions(ctx, w.by, mcp.ListDecisionsInput{Project: "app", Council: council.String()}); len(got) != 0 {
		t.Fatalf("by council = %+v", got)
	}
	for _, in := range []mcp.ListDecisionsInput{{}, {Project: "app", Goal: "nonsense"}, {Project: "app", Council: "COUNCIL-1"}} {
		if _, err := w.svc.ListDecisions(ctx, w.by, in); err == nil {
			t.Errorf("%+v was accepted", in)
		}
	}
}

func TestGetGoalListsAssumptions(t *testing.T) {
	w := newRegisterWorld(t)
	d, err := w.svc.GetGoal(t.Context(), w.by, mcp.GoalRefInput{Goal: w.goal.Ref()})
	must(t, err)
	if !slices.Equal(refsOf(d.Assumptions), []string{"A-1", "A-2", "A-3"}) {
		t.Fatalf("assumptions = %+v", d.Assumptions)
	}
	empty := w.newGoal(t, "Nothing yet")
	d, err = w.svc.GetGoal(t.Context(), w.by, mcp.GoalRefInput{Goal: empty.Ref()})
	must(t, err)
	if d.Assumptions == nil || len(d.Assumptions) != 0 {
		t.Fatalf("assumptions = %#v", d.Assumptions)
	}
}

func TestRegisterToolsNeedARegister(t *testing.T) {
	f := newFixture(t)
	svc := tools.New(tools.Config{Store: f.st, Gates: f.gates, Jobs: f.jobs, Creator: f.creator, GitHub: f.tracker})
	ctx := t.Context()
	_, err := svc.ListAssumptions(ctx, f.by, mcp.ListAssumptionsInput{Project: "app"})
	wantErr(t, err, mcp.ErrUnavailable)
	_, err = svc.ResolveAssumption(ctx, f.by, mcp.ResolveAssumptionInput{Assumption: "A-1", Status: domain.AssumptionAccepted})
	wantErr(t, err, mcp.ErrUnavailable)
	_, err = svc.ListDecisions(ctx, f.by, mcp.ListDecisionsInput{Project: "app"})
	wantErr(t, err, mcp.ErrUnavailable)
	g := f.newGoal(t, "g")
	d, err := svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: g.Ref()})
	if err != nil || d.Assumptions == nil || len(d.Assumptions) != 0 {
		t.Fatalf("get goal: %#v, %v", d.Assumptions, err)
	}
}
