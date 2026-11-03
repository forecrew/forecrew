package tools_test

import (
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tools"
)

func (f *fixture) openGate(t *testing.T, kind domain.GateKind, g domain.Goal, tk domain.Task) domain.Gate {
	t.Helper()
	gate, err := f.gates.Open(t.Context(), gates.OpenRequest{
		AccountID: f.account.ID, ProjectID: f.project.ID, Kind: kind, Title: "check", Summary: "please look",
		SubjectRepo: "acme/app", SubjectNumber: 12, GoalID: g.ID, TaskID: tk.ID,
	})
	must(t, err)
	return gate
}

func TestGateTools(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	spec := f.openGate(t, domain.GateSpecApproval, domain.Goal{}, domain.Task{})
	stuck := f.openGate(t, domain.GateNeedsYou, domain.Goal{}, domain.Task{})
	third := f.openGate(t, domain.GateMergeApproval, domain.Goal{}, domain.Task{})

	g, err := f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{Gate: spec.Ref(), Feedback: "ship it"})
	must(t, err)
	if g.Status != domain.GateApproved || g.ResolvedBy != "mcp:laptop" || g.Feedback != "ship it" {
		t.Errorf("approved = %+v", g)
	}
	g, err = f.svc.Reject(ctx, f.by, mcp.GateDecisionInput{Gate: stuck.ID.String()})
	must(t, err)
	if g.Status != domain.GateRejected {
		t.Errorf("rejected = %+v", g)
	}
	g, err = f.svc.RequestChanges(ctx, f.by, mcp.GateDecisionInput{Gate: third.Ref(), Feedback: "tests"})
	must(t, err)
	if g.Status != domain.GateChangesRequested {
		t.Errorf("changes = %+v", g)
	}
	sigs, err := testkit.NewSignalBus(f.st).Lookup(ctx, f.account.ID, domain.GateKey(spec.ID))
	must(t, err)
	if len(sigs) != 1 {
		t.Errorf("%d gate.resolved signals, want 1", len(sigs))
	}
	if got := f.auditActions(t); len(got) != 6 || got[3] != "mcp:laptop gate.resolved" {
		t.Errorf("audit = %v", got)
	}

	fresh := f.openGate(t, domain.GateNeedsYou, domain.Goal{}, domain.Task{})
	_, err = f.svc.RequestChanges(ctx, f.by, mcp.GateDecisionInput{Gate: fresh.Ref(), Feedback: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.RequestChanges(ctx, f.by, mcp.GateDecisionInput{Gate: fresh.Ref()})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{Gate: "later"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{Gate: "GATE-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{Gate: spec.Ref()})
	wantErr(t, err, domain.ErrInvalidTransition) // already decided

	other, _, _ := f.otherAccount(t)
	_, err = f.svc.Approve(ctx, other, mcp.GateDecisionInput{Gate: fresh.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
}

func TestListPendingGates(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	goal := f.newGoal(t, "Login")
	spec := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: goal.ID, Revision: 1}
	must(t, f.st.Specs().Create(ctx, &spec))
	tk := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: goal.ID, SpecID: spec.ID, Seq: 1, Title: "t", Status: domain.TaskInReview}
	must(t, f.st.Tasks().Create(ctx, &tk))
	merge := f.openGate(t, domain.GateMergeApproval, goal, tk)
	done := f.openGate(t, domain.GateSpecApproval, goal, domain.Task{})
	_, err := f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{Gate: done.Ref()})
	must(t, err)
	second := domain.Project{AccountID: f.account.ID, Name: "second", Repo: "acme/second", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(ctx, &second))
	_, err = f.gates.Open(ctx, gates.OpenRequest{AccountID: f.account.ID, ProjectID: second.ID, Kind: domain.GateNeedsYou, Title: "stuck", SubjectRepo: "acme/second"})
	must(t, err)

	all, err := f.svc.ListPendingGates(ctx, f.by, mcp.ProjectFilterInput{})
	must(t, err)
	if len(all) != 2 {
		t.Fatalf("pending = %+v", all)
	}
	g := all[0]
	if g.Ref != merge.Ref() || g.Kind != domain.GateMergeApproval || g.Project != "app" || g.GoalRef != goal.Ref() || g.TaskRef != tk.Ref() ||
		g.URL != "https://github.com/acme/app/issues/12" || g.Summary != "please look" || g.CreatedAt.IsZero() {
		t.Errorf("gate info = %+v", g)
	}
	if all[1].URL != "" || all[1].Project != "second" || all[1].GoalRef != "" {
		t.Errorf("second = %+v", all[1])
	}
	one, err := f.svc.ListPendingGates(ctx, f.by, mcp.ProjectFilterInput{Project: "acme/app"})
	must(t, err)
	if len(one) != 1 || one[0].Ref != merge.Ref() {
		t.Errorf("filtered = %+v", one)
	}
	_, err = f.svc.ListPendingGates(ctx, f.by, mcp.ProjectFilterInput{Project: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	other, _, _ := f.otherAccount(t)
	if got, err := f.svc.ListPendingGates(ctx, other, mcp.ProjectFilterInput{}); err != nil || len(got) != 0 {
		t.Errorf("other account sees %+v, %v", got, err)
	}
}

func TestJobTools(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	system := domain.Actor{AccountID: f.account.ID, Kind: domain.ActorSystem, Name: "test"}
	create := func(needs ...string) domain.Job {
		j, err := f.jobs.Create(ctx, system, domain.Job{
			AccountID: f.account.ID, ProjectID: f.project.ID, Role: "reviewer", Runtime: "claude-code", Needs: needs, Spec: domain.JobSpec{Repo: "acme/app"},
		})
		must(t, err)
		return j
	}
	queued := create("ios")
	info, err := f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: queued.Ref()})
	must(t, err)
	if info.Ref != "JOB-1" || info.Status != domain.JobQueued || info.Reason != "no online worker in pool build with labels ios, runtime:claude-code" {
		t.Errorf("queued = %+v", info)
	}

	w := domain.Worker{AccountID: f.account.ID, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, f.st.Workers().Create(ctx, &w))
	must(t, f.st.Workers().Seen(ctx, f.account.ID, w.ID, []string{"runtime:claude-code", "linux"}, "v1.2"))
	running := create()
	leased, err := f.st.Jobs().LeaseNext(ctx, f.account.ID, w.ID, []string{"runtime:claude-code", "linux"}, domain.LeaseDuration)
	must(t, err)
	if leased.ID != running.ID {
		t.Fatalf("leased %s, want %s (the other job needs ios)", leased.Ref(), running.Ref())
	}
	info, err = f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: running.ID.String()})
	must(t, err)
	if info.Status != domain.JobRunning || info.WorkerName != "box" {
		t.Errorf("running = %+v", info)
	}

	workers, err := f.svc.ListWorkers(ctx, f.by)
	must(t, err)
	if len(workers) != 1 || !workers[0].Online || workers[0].Version != "v1.2" || !slices.Equal(workers[0].RunningJobs, []string{running.Ref()}) {
		t.Errorf("workers = %+v", workers)
	}

	_, err = f.st.JobEvents().Append(ctx, f.account.ID, running.ID, []domain.JobEvent{{Type: domain.EventLog}, {Type: domain.EventLog}, {Type: domain.EventLog}})
	must(t, err)
	logs, err := f.svc.JobLogs(ctx, f.by, mcp.JobLogsInput{Job: running.Ref(), AfterSeq: 1, Limit: 5})
	must(t, err)
	if len(logs.Events) != 2 || logs.NextSeq != logs.Events[1].Seq {
		t.Errorf("logs = %+v", logs)
	}

	for _, ref := range []string{"JOB-99", "task-1", "nope", ""} {
		_, err = f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: ref})
		if err == nil {
			t.Errorf("JobStatus(%q) succeeded", ref)
		}
	}
	_, err = f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: "JOB-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: "nope"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.JobLogs(ctx, f.by, mcp.JobLogsInput{Job: "JOB-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.JobLogs(ctx, f.by, mcp.JobLogsInput{Job: running.Ref(), AfterSeq: -1})
	wantErr(t, err, mcp.ErrInvalidInput)

	other, _, otherJob := f.otherAccount(t)
	_, err = f.svc.JobStatus(ctx, other, mcp.JobRefInput{Job: running.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.JobLogs(ctx, other, mcp.JobLogsInput{Job: running.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	if got, err := f.svc.ListWorkers(ctx, other); err != nil || len(got) != 0 {
		t.Errorf("other account sees workers %+v, %v", got, err)
	}
	if _, err := f.svc.JobStatus(ctx, other, mcp.JobRefInput{Job: otherJob.Ref()}); err != nil {
		t.Errorf("own job: %v", err)
	}
}

func TestListJobs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g1, g2 := f.newGoal(t, "one"), f.newGoal(t, "two")
	f.newJob(t, "reviewer", g1, domain.Task{}, 0)
	f.newJob(t, "reviewer", g2, domain.Task{}, 0)
	f.newJob(t, "reviewer", g2, domain.Task{}, 0)
	second := domain.Project{AccountID: f.account.ID, Name: "second", Repo: "acme/second", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(ctx, &second))
	j := domain.Job{AccountID: f.account.ID, ProjectID: second.ID, Role: "reviewer", Runtime: "codex", Spec: domain.JobSpec{Repo: "acme/second"}}
	must(t, f.st.Jobs().Create(ctx, &j))

	tests := []struct {
		name string
		in   mcp.ListJobsInput
		want int
	}{
		{"all", mcp.ListJobsInput{}, 4},
		{"project", mcp.ListJobsInput{Project: "second"}, 1},
		{"goal ref", mcp.ListJobsInput{Goal: g2.Ref()}, 2},
		{"goal uuid", mcp.ListJobsInput{Goal: g1.ID.String()}, 1},
		{"status", mcp.ListJobsInput{Status: domain.JobQueued}, 4},
		{"no running", mcp.ListJobsInput{Status: domain.JobRunning}, 0},
		{"limit", mcp.ListJobsInput{Limit: 3}, 3},
	}
	for _, tt := range tests {
		got, err := f.svc.ListJobs(ctx, f.by, tt.in)
		if err != nil || len(got) != tt.want {
			t.Errorf("%s: %d jobs, %v; want %d", tt.name, len(got), err, tt.want)
		}
	}
	newest, _ := f.svc.ListJobs(ctx, f.by, mcp.ListJobsInput{Limit: 1})
	if len(newest) != 1 || newest[0].ID != j.ID {
		t.Errorf("newest = %+v", newest)
	}
	errs := []struct {
		in   mcp.ListJobsInput
		want error
	}{
		{mcp.ListJobsInput{Status: "sleeping"}, mcp.ErrInvalidInput},
		{mcp.ListJobsInput{Limit: -1}, mcp.ErrInvalidInput},
		{mcp.ListJobsInput{Project: "nope"}, mcp.ErrNotFound},
		{mcp.ListJobsInput{Goal: "GOAL-99"}, mcp.ErrNotFound},
	}
	for _, tt := range errs {
		_, err := f.svc.ListJobs(ctx, f.by, tt.in)
		wantErr(t, err, tt.want)
	}
	other, _, _ := f.otherAccount(t)
	if got, _ := f.svc.ListJobs(ctx, other, mcp.ListJobsInput{}); len(got) != 1 {
		t.Errorf("other account sees %d jobs, want its own 1", len(got))
	}
}

func TestProjectsAndGoals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	must(t, f.st.Projects().SetProfile(ctx, f.account.ID, f.project.ID, []byte(`{"name":"app"}`)))
	second := domain.Project{AccountID: f.account.ID, Name: "second", Repo: "acme/second", Status: domain.ProjectRegistered}
	must(t, f.st.Projects().Create(ctx, &second))

	list, err := f.svc.ListProjects(ctx, f.by)
	if err != nil || len(list) != 2 {
		t.Fatalf("projects = %+v, %v", list, err)
	}
	for _, ref := range []string{"app", "ACME/APP"} {
		p, err := f.svc.GetProject(ctx, f.by, mcp.ProjectInput{Project: ref})
		if err != nil || p.ID != f.project.ID || string(p.Profile) != `{"name":"app"}` {
			t.Errorf("GetProject(%q) = %+v, %v", ref, p, err)
		}
	}
	_, err = f.svc.GetProject(ctx, f.by, mcp.ProjectInput{Project: "nope"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.GetProject(ctx, f.by, mcp.ProjectInput{})
	wantErr(t, err, mcp.ErrInvalidInput)

	g1 := f.newGoal(t, "one")
	g2 := domain.Goal{AccountID: f.account.ID, ProjectID: second.ID, Title: "two", Status: domain.GoalPlanned}
	must(t, f.st.Goals().Create(ctx, &g2))
	goals := func(in mcp.ListGoalsInput) []domain.Goal {
		t.Helper()
		got, err := f.svc.ListGoals(ctx, f.by, in)
		must(t, err)
		return got
	}
	if got := goals(mcp.ListGoalsInput{}); len(got) != 2 {
		t.Errorf("all goals = %+v", got)
	}
	if got := goals(mcp.ListGoalsInput{Project: "app"}); len(got) != 1 || got[0].ID != g1.ID {
		t.Errorf("by project = %+v", got)
	}
	if got := goals(mcp.ListGoalsInput{Status: domain.GoalPlanned}); len(got) != 1 || got[0].ID != g2.ID {
		t.Errorf("by status = %+v", got)
	}
	_, err = f.svc.ListGoals(ctx, f.by, mcp.ListGoalsInput{Status: "someday"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.ListGoals(ctx, f.by, mcp.ListGoalsInput{Project: "nope"})
	wantErr(t, err, mcp.ErrNotFound)

	other, otherGoal, _ := f.otherAccount(t)
	if got, _ := f.svc.ListProjects(ctx, other); len(got) != 1 || got[0].Repo != "other/app" {
		t.Errorf("other account projects = %+v", got)
	}
	_, err = f.svc.GetProject(ctx, other, mcp.ProjectInput{Project: "acme/app"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: otherGoal.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
}

func TestGetGoal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t, "Login")
	g.IssueNumber = 5
	must(t, f.st.Goals().Update(ctx, &g))
	empty, err := f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: g.Ref()})
	must(t, err)
	if empty.Spec != nil || len(empty.Tasks) != 0 || len(empty.Gates) != 0 || empty.CostUSD != 0 || empty.URL != "https://github.com/acme/app/issues/5" {
		t.Errorf("empty goal = %+v", empty)
	}

	spec1 := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 1, Body: "v1"}
	spec2 := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 2, Body: "v2"}
	must(t, f.st.Specs().Create(ctx, &spec1))
	must(t, f.st.Specs().Create(ctx, &spec2))
	newTask := func(seq, issue int, status domain.TaskStatus) domain.Task {
		tk := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, SpecID: spec2.ID, Seq: seq, Title: "task", Status: status, IssueNumber: issue}
		must(t, f.st.Tasks().Create(ctx, &tk))
		return tk
	}
	t1, t2 := newTask(1, 6, domain.TaskInReview), newTask(2, 0, domain.TaskOpen)
	f.newJob(t, "developer", g, t1, 0.5)
	f.newJob(t, "reviewer", g, t1, 0.25)
	f.newJob(t, "product-owner", g, domain.Task{}, 1)
	open := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: t1.ID, Repo: "acme/app", Number: 9, Branch: "agent/task-1", HeadSHA: "abc", State: domain.ChangeOpen, URL: "https://github.com/acme/app/pull/9"}
	merged := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: t1.ID, Repo: "acme/app", Number: 8, Branch: "agent/task-1-old", HeadSHA: "old", State: domain.ChangeMerged, URL: "https://github.com/acme/app/pull/8"}
	must(t, f.st.Changes().Upsert(ctx, &open))
	must(t, f.st.Changes().Upsert(ctx, &merged))
	must(t, f.tracker.SetCheckStatus("acme/app", "abc", github.ChecksFailure, 2))
	pending := f.openGate(t, domain.GateMergeApproval, g, t1)
	decided := f.openGate(t, domain.GateSpecApproval, g, domain.Task{})
	_, err = f.svc.Approve(ctx, f.by, mcp.GateDecisionInput{Gate: decided.Ref(), Feedback: "good"})
	must(t, err)
	f.openGate(t, domain.GateSpecApproval, domain.Goal{}, domain.Task{}) // another goal's gate

	d, err := f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: g.Ref()})
	must(t, err)
	if d.Ref != "GOAL-1" || d.Project != "app" || d.Spec == nil || d.Spec.Body != "v2" || d.CostUSD != 1.75 {
		t.Errorf("goal = %+v", d)
	}
	if len(d.Tasks) != 2 {
		t.Fatalf("tasks = %+v", d.Tasks)
	}
	a, b := d.Tasks[0], d.Tasks[1]
	if a.Ref != t1.Ref() || a.Status != domain.TaskInReview || a.URL != "https://github.com/acme/app/issues/6" || a.Iterations != 2 || a.CostUSD != 0.75 ||
		!slices.Equal(a.Needs, []string{pending.Ref()}) || len(a.Changes) != 2 {
		t.Errorf("task 1 = %+v", a)
	}
	checkChanges(t, a.Changes, open.URL)
	if b.Ref != t2.Ref() || b.URL != "" || b.Iterations != 0 || len(b.Needs) != 0 || b.Changes == nil || len(b.Changes) != 0 {
		t.Errorf("task 2 = %+v", b)
	}
	checkGates(t, d.Gates, decided.Ref())

	for _, ref := range []string{"GOAL-99", "nope"} {
		if _, err := f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: ref}); err == nil {
			t.Errorf("GetGoal(%q) succeeded", ref)
		}
	}
}

func TestGetGoalChecksUnknownWhenGitHubFails(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t, "Login")
	spec := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 1}
	must(t, f.st.Specs().Create(ctx, &spec))
	tk := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, SpecID: spec.ID, Seq: 1, Title: "t", Status: domain.TaskInReview}
	must(t, f.st.Tasks().Create(ctx, &tk))
	c := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: tk.ID, Repo: "acme/app", Number: 9, HeadSHA: "abc", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &c))
	svc := tools.New(tools.Config{Store: f.st, Gates: f.gates, Jobs: f.jobs, GitHub: failingGitHub{}, Logger: nil})
	d, err := svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: g.Ref()})
	must(t, err)
	if got := d.Tasks[0].Changes[0].Checks; got != mcp.ChecksUnknown {
		t.Errorf("checks = %q", got)
	}
}

func checkChanges(t *testing.T, changes []mcp.ChangeInfo, openURL string) {
	t.Helper()
	for _, c := range changes {
		switch c.Number {
		case 9:
			if c.Checks != "failure" || c.URL != openURL {
				t.Errorf("open change = %+v", c)
			}
		case 8:
			if c.Checks != "" || c.State != domain.ChangeMerged {
				t.Errorf("merged change = %+v", c)
			}
		}
	}
}

func checkGates(t *testing.T, gates []mcp.GateInfo, decidedRef string) {
	t.Helper()
	if len(gates) != 2 {
		t.Fatalf("gates = %+v", gates)
	}
	for _, gi := range gates {
		if gi.Ref == decidedRef && (gi.Status != domain.GateApproved || gi.Feedback != "good" || gi.ResolvedBy != "mcp:laptop") {
			t.Errorf("decided gate = %+v", gi)
		}
	}
}
