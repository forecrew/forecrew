package tools_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tools"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	clock     *testkit.Clock
	st        *testkit.Store
	tracker   *testkit.Tracker
	creator   *testkit.JobCreator
	playbooks *testkit.Playbooks
	gates     *gates.Keeper
	jobs      *jobs.Service
	svc       *tools.Service
	logs      *bytes.Buffer
	account   domain.Account
	project   domain.Project // onboarded, acme/app
	by        domain.Actor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	f := &fixture{clock: clock, st: st, tracker: testkit.NewTracker(clock.Now), playbooks: &testkit.Playbooks{}, logs: &bytes.Buffer{}}
	f.account = domain.Account{Name: "default"}
	must(t, st.Accounts().Create(ctx, &f.account))
	f.project = domain.Project{AccountID: f.account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &f.project))
	f.tracker.Install("acme/app")
	f.tracker.Install("acme/new")
	bus := testkit.NewSignalBus(st)
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	f.creator = &testkit.JobCreator{Store: st}
	f.gates = gates.New(st, bus, clock.Now)
	f.jobs = jobs.New(st, bus, logger, clock.Now)
	f.svc = tools.New(tools.Config{
		Store: st, Gates: f.gates, Jobs: f.jobs, Creator: f.creator, Playbooks: f.playbooks, GitHub: f.tracker, Logger: logger,
		Roles: testkit.Roles{
			"reviewer":  {Name: "reviewer"},
			"developer": {Name: "developer", WritePaths: []string{"src/**"}},
		},
	})
	f.by = domain.Actor{AccountID: f.account.ID, Kind: domain.ActorMCP, Name: "laptop"}
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

// otherAccount returns an actor of a second account with its own project,
// goal and job, to check that nothing leaks across accounts.
func (f *fixture) otherAccount(t *testing.T) (domain.Actor, domain.Goal, domain.Job) {
	t.Helper()
	ctx := t.Context()
	acc := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "other/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(ctx, &p))
	g := domain.Goal{AccountID: acc.ID, ProjectID: p.ID, Title: "secret", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(ctx, &g))
	j := domain.Job{AccountID: acc.ID, ProjectID: p.ID, GoalID: g.ID, Role: "reviewer", Runtime: "claude-code", Spec: domain.JobSpec{Repo: p.Repo}}
	must(t, f.st.Jobs().Create(ctx, &j))
	return domain.Actor{AccountID: acc.ID, Kind: domain.ActorMCP, Name: "other-laptop"}, g, j
}

func (f *fixture) auditActions(t *testing.T) []string {
	t.Helper()
	entries, err := f.st.Audit().List(t.Context(), f.account.ID, store.AuditFilter{})
	must(t, err)
	var out []string
	for _, entrie := range slices.Backward(entries) { // List is newest first
		out = append(out, entrie.Actor+" "+entrie.Action)
	}
	return out
}

func (f *fixture) newGoal(t *testing.T, title string) domain.Goal {
	t.Helper()
	g := domain.Goal{AccountID: f.account.ID, ProjectID: f.project.ID, Title: title, Status: domain.GoalDraft, CreatedBy: f.by.String()}
	must(t, f.st.Goals().Create(t.Context(), &g))
	return g
}

func (f *fixture) newJob(t *testing.T, role string, g domain.Goal, tk domain.Task, cost float64) domain.Job {
	t.Helper()
	j := domain.Job{
		AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, TaskID: tk.ID, SpecID: tk.SpecID, Role: role, Runtime: "claude-code",
		Spec: domain.JobSpec{Repo: "acme/app"}, CostUSD: cost,
	}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	return j
}

func TestOnboardProject(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	out, err := f.svc.OnboardProject(ctx, f.by, mcp.OnboardProjectInput{Repo: "acme/new"})
	must(t, err)
	if out.Project.Name != "new" || out.Project.Repo != "acme/new" || out.Project.Status != domain.ProjectOnboarding || out.RunID == uuid.Nil() {
		t.Fatalf("out = %+v", out)
	}
	got, err := f.st.Projects().Get(ctx, f.account.ID, out.Project.ID)
	must(t, err)
	if got.Status != domain.ProjectOnboarding || got.DefaultBranch != "main" {
		t.Errorf("stored project = %+v", got)
	}
	starts := f.playbooks.Starts()
	if len(starts) != 1 || starts[0].Playbook != tools.PlaybookOnboarding || starts[0].Request.ProjectID != out.Project.ID ||
		starts[0].Request.Inputs["repo"] != "acme/new" || starts[0].Request.Inputs["project"] != "new" {
		t.Errorf("starts = %+v", starts)
	}
	want := []string{"mcp:laptop project.registered", "mcp:laptop project.onboarding"}
	if got := f.auditActions(t); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("audit = %v, want %v", got, want)
	}

	_, err = f.svc.OnboardProject(ctx, f.by, mcp.OnboardProjectInput{Repo: "ACME/New"})
	wantErr(t, err, mcp.ErrInvalidInput) // already onboarding

	f.tracker.Install("acme/branchy")
	out, err = f.svc.OnboardProject(ctx, f.by, mcp.OnboardProjectInput{Repo: "acme/branchy", DefaultBranch: " trunk "})
	must(t, err)
	if out.Project.DefaultBranch != "trunk" {
		t.Errorf("default branch = %q", out.Project.DefaultBranch)
	}
	if len(f.playbooks.Starts()) != 2 { // the refused call started nothing
		t.Errorf("%d playbooks started, want 2", len(f.playbooks.Starts()))
	}
}

func TestOnboardRegisteredProjectWithName(t *testing.T) {
	f := newFixture(t)
	raw := domain.Project{AccountID: f.account.ID, Name: "raw", Repo: "acme/raw", DefaultBranch: "trunk", Status: domain.ProjectRegistered}
	must(t, f.st.Projects().Create(t.Context(), &raw))
	f.tracker.Install("acme/raw")
	out, err := f.svc.OnboardProject(t.Context(), f.by, mcp.OnboardProjectInput{Repo: "acme/raw", Name: "ignored"})
	must(t, err)
	if out.Project.ID != raw.ID || out.Project.Name != "raw" || out.Project.DefaultBranch != "trunk" {
		t.Errorf("project = %+v", out.Project)
	}
}

type failingGitHub struct{}

func (failingGitHub) IsInstalled(context.Context, string) (bool, error) {
	return false, github.ErrRateLimited
}

func (failingGitHub) CheckStatus(context.Context, string, string) (github.CheckStatus, error) {
	return github.CheckStatus{}, github.ErrRateLimited
}

func TestOnboardProjectErrors(t *testing.T) {
	boom := errors.New("engine down")
	tests := []struct {
		name  string
		in    mcp.OnboardProjectInput
		setup func(f *fixture)
		want  error
	}{
		{name: "no slash", in: mcp.OnboardProjectInput{Repo: "app"}, want: mcp.ErrInvalidInput},
		{name: "empty owner", in: mcp.OnboardProjectInput{Repo: "/app"}, want: mcp.ErrInvalidInput},
		{name: "too many parts", in: mcp.OnboardProjectInput{Repo: "a/b/c"}, want: mcp.ErrInvalidInput},
		{name: "app not installed", in: mcp.OnboardProjectInput{Repo: "acme/other"}, want: mcp.ErrInvalidInput},
		{name: "already onboarded", in: mcp.OnboardProjectInput{Repo: "acme/app"}, want: mcp.ErrInvalidInput},
		{name: "name taken", in: mcp.OnboardProjectInput{Repo: "acme/new", Name: "app"}, want: mcp.ErrInvalidInput},
		{name: "github down", in: mcp.OnboardProjectInput{Repo: "acme/new"}, want: github.ErrRateLimited,
			setup: func(f *fixture) {
				f.svc = tools.New(tools.Config{Store: f.st, Playbooks: f.playbooks, GitHub: failingGitHub{}})
			}},
		{name: "playbook fails", in: mcp.OnboardProjectInput{Repo: "acme/new"}, want: boom,
			setup: func(f *fixture) { f.playbooks.Err = boom }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			_, err := f.svc.OnboardProject(t.Context(), f.by, tt.in)
			wantErr(t, err, tt.want)
			if errors.Is(tt.want, boom) { // the project goes back to registered
				p, err := f.st.Projects().GetByName(t.Context(), f.account.ID, "acme/new")
				must(t, err)
				if p.Status != domain.ProjectRegistered {
					t.Errorf("status = %s", p.Status)
				}
				got := f.auditActions(t)
				if len(got) == 0 || got[len(got)-1] != "mcp:laptop project.onboarding_reverted" {
					t.Errorf("audit = %v, want it to end with the revert", got)
				}
			}
		})
	}
}

func TestCreateGoal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g, err := f.svc.CreateGoal(ctx, f.by, mcp.CreateGoalInput{Project: "acme/app", Title: " Login ", Body: "Add login", Platforms: []string{"Web", "ios", "web"}})
	must(t, err)
	if g.Title != "Login" || g.Status != domain.GoalDraft || g.CreatedBy != "mcp:laptop" || g.Ref() != "GOAL-1" || len(g.Platforms) != 2 || g.Platforms[0] != "ios" {
		t.Fatalf("goal = %+v", g)
	}
	starts := f.playbooks.Starts()
	if len(starts) != 1 || starts[0].Playbook != tools.PlaybookGoal || starts[0].Request.GoalID != g.ID ||
		starts[0].Request.Inputs["goal_id"] != g.ID.String() || starts[0].Request.Inputs["repo"] != "acme/app" {
		t.Errorf("starts = %+v", starts)
	}
	if got := f.auditActions(t); len(got) != 1 || got[0] != "mcp:laptop goal.created" {
		t.Errorf("audit = %v", got)
	}
}

func TestCreateGoalErrors(t *testing.T) {
	tests := []struct {
		name  string
		in    mcp.CreateGoalInput
		setup func(t *testing.T, f *fixture)
		want  error
	}{
		{name: "no title", in: mcp.CreateGoalInput{Project: "app", Title: "  "}, want: mcp.ErrInvalidInput},
		{name: "no project", in: mcp.CreateGoalInput{Title: "x"}, want: mcp.ErrInvalidInput},
		{name: "unknown project", in: mcp.CreateGoalInput{Project: "nope", Title: "x"}, want: mcp.ErrNotFound},
		{name: "not onboarded", in: mcp.CreateGoalInput{Project: "raw", Title: "x"}, want: mcp.ErrInvalidInput,
			setup: func(t *testing.T, f *fixture) {
				p := domain.Project{AccountID: f.account.ID, Name: "raw", Repo: "acme/raw", Status: domain.ProjectRegistered}
				must(t, f.st.Projects().Create(t.Context(), &p))
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			_, err := f.svc.CreateGoal(t.Context(), f.by, tt.in)
			wantErr(t, err, tt.want)
			if len(f.playbooks.Starts()) != 0 {
				t.Error("a playbook was started")
			}
		})
	}
}

func TestCreateGoalCancelsGoalWhenPlaybookFails(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("engine down")
	f.playbooks.Err = boom
	_, err := f.svc.CreateGoal(t.Context(), f.by, mcp.CreateGoalInput{Project: "app", Title: "x"})
	wantErr(t, err, boom)
	goals, err := f.st.Goals().List(t.Context(), f.account.ID, store.GoalFilter{})
	must(t, err)
	if len(goals) != 1 || goals[0].Status != domain.GoalCanceled {
		t.Errorf("goals = %+v", goals)
	}
}

func TestDispatchJob(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t, "Login")
	spec := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 1, Body: "s"}
	must(t, f.st.Specs().Create(ctx, &spec))
	tk := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, SpecID: spec.ID, Seq: 1, Title: "t", Status: domain.TaskOpen}
	must(t, f.st.Tasks().Create(ctx, &tk))

	j, err := f.svc.DispatchJob(ctx, f.by, mcp.DispatchJobInput{
		Project: "app", Role: "reviewer", Branch: "agent/task-1", Needs: []string{"linux"}, Prompt: "review it", Task: tk.Ref(), IncludeRepoFacts: true,
	})
	must(t, err)
	if j.Role != "reviewer" || j.Status != domain.JobQueued || j.TaskID != tk.ID {
		t.Fatalf("job = %+v", j)
	}
	reqs := f.creator.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	r := reqs[0]
	if r.Prompt != "review it" || r.Repo != "acme/app" || r.Branch != "agent/task-1" || r.GoalID != g.ID || r.TaskID != tk.ID || r.SpecID != spec.ID ||
		r.AccountID != f.account.ID || len(r.Needs) != 1 || !r.IncludeRepoFacts {
		t.Errorf("request = %+v", r)
	}
	if got := f.auditActions(t); len(got) != 1 || got[0] != "mcp:laptop job.dispatched" {
		t.Errorf("audit = %v", got)
	}

	j, err = f.svc.DispatchJob(ctx, f.by, mcp.DispatchJobInput{Project: "app", Role: "reviewer", Goal: g.ID.String()})
	must(t, err)
	if j.GoalID != g.ID {
		t.Errorf("goal = %v", j.GoalID)
	}
}

func TestDispatchJobErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, otherGoal, _ := f.otherAccount(t)
	second := domain.Project{AccountID: f.account.ID, Name: "second", Repo: "acme/second", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(ctx, &second))
	secondGoal := domain.Goal{AccountID: f.account.ID, ProjectID: second.ID, Title: "g", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(ctx, &secondGoal))
	tk := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: f.newGoal(t, "x").ID, Seq: 1, Title: "t", Status: domain.TaskOpen}
	spec := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: tk.GoalID, Revision: 1}
	must(t, f.st.Specs().Create(ctx, &spec))
	tk.SpecID = spec.ID
	must(t, f.st.Tasks().Create(ctx, &tk))
	secondSpec := domain.Spec{AccountID: f.account.ID, ProjectID: second.ID, GoalID: secondGoal.ID, Revision: 1}
	must(t, f.st.Specs().Create(ctx, &secondSpec))
	secondTask := domain.Task{AccountID: f.account.ID, ProjectID: second.ID, GoalID: secondGoal.ID, SpecID: secondSpec.ID, Seq: 1, Title: "t", Status: domain.TaskOpen}
	must(t, f.st.Tasks().Create(ctx, &secondTask))

	tests := []struct {
		name string
		in   mcp.DispatchJobInput
		want error
	}{
		{"no role", mcp.DispatchJobInput{Project: "app"}, mcp.ErrInvalidInput},
		{"no project", mcp.DispatchJobInput{Role: "reviewer"}, mcp.ErrInvalidInput},
		{"unknown project", mcp.DispatchJobInput{Project: "nope", Role: "reviewer"}, mcp.ErrNotFound},
		{"unknown role", mcp.DispatchJobInput{Project: "app", Role: "wizard"}, mcp.ErrInvalidInput},
		{"writing role", mcp.DispatchJobInput{Project: "app", Role: "developer"}, mcp.ErrInvalidInput},
		{"bad goal ref", mcp.DispatchJobInput{Project: "app", Role: "reviewer", Goal: "soon"}, mcp.ErrInvalidInput},
		{"missing goal", mcp.DispatchJobInput{Project: "app", Role: "reviewer", Goal: "GOAL-99"}, mcp.ErrNotFound},
		{"goal of another project", mcp.DispatchJobInput{Project: "app", Role: "reviewer", Goal: secondGoal.Ref()}, mcp.ErrInvalidInput},
		{"goal of another account", mcp.DispatchJobInput{Project: "app", Role: "reviewer", Goal: otherGoal.ID.String()}, mcp.ErrNotFound},
		{"missing task", mcp.DispatchJobInput{Project: "app", Role: "reviewer", Task: "TASK-99"}, mcp.ErrNotFound},
		{"task of another project", mcp.DispatchJobInput{Project: "app", Role: "reviewer", Task: secondTask.Ref()}, mcp.ErrInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.DispatchJob(ctx, f.by, tt.in)
			wantErr(t, err, tt.want)
		})
	}
	if n := len(f.creator.Requests()); n != 0 {
		t.Errorf("%d jobs created", n)
	}

	boom := errors.New("pack failed")
	f.creator.Err = boom
	_, err := f.svc.DispatchJob(ctx, f.by, mcp.DispatchJobInput{Project: "app", Role: "reviewer"})
	wantErr(t, err, boom)

	broken := tools.New(tools.Config{Store: f.st, Creator: &testkit.JobCreator{}, Roles: failingRoles{boom}})
	_, err = broken.DispatchJob(ctx, f.by, mcp.DispatchJobInput{Project: "app", Role: "reviewer"})
	wantErr(t, err, boom)
	// A creator without a store returns an ID that is not a job.
	broken = tools.New(tools.Config{Store: f.st, Creator: &testkit.JobCreator{}, Roles: testkit.Roles{"reviewer": {}}})
	_, err = broken.DispatchJob(ctx, f.by, mcp.DispatchJobInput{Project: "app", Role: "reviewer"})
	if err == nil {
		t.Error("want an error for a job that was not stored")
	}
}

type failingRoles struct{ err error }

func (r failingRoles) Resolve(context.Context, uuid.UUID, uuid.UUID, string) (roles.Role, error) {
	return roles.Role{}, r.err
}
