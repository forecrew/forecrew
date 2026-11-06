package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fixture is a store with one account and one project.
type fixture struct {
	clock   *Clock
	st      *Store
	account uuid.UUID
	project domain.Project
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	clock := NewClock(t0)
	st := NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, st.Projects().Create(t.Context(), &p))
	return fixture{clock: clock, st: st, account: acc.ID, project: p}
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

func TestStoreAccounts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a, err := f.st.Accounts().Get(ctx, f.account)
	must(t, err)
	if a.Name != "default" || !a.CreatedAt.Equal(t0) {
		t.Fatalf("account = %+v", a)
	}
	wantErr(t, f.st.Accounts().Create(ctx, &a), store.ErrConflict)
	list, err := f.st.Accounts().List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	_, err = f.st.Accounts().Get(ctx, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func TestStoreProjects(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Projects()
	dup := domain.Project{AccountID: f.account, Name: "other", Repo: "ACME/app"}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)

	other := domain.Project{AccountID: domain.NewID(), Name: "app", Repo: "acme/app"}
	must(t, repo.Create(ctx, &other))
	byRepo, err := repo.ListByRepo(ctx, "Acme/App")
	if err != nil || len(byRepo) != 2 {
		t.Fatalf("ListByRepo = %v, %v", byRepo, err)
	}
	for _, name := range []string{"APP", "acme/app"} {
		p, err := repo.GetByName(ctx, f.account, name)
		if err != nil || p.ID != f.project.ID {
			t.Fatalf("GetByName(%s) = %v, %v", name, p.ID, err)
		}
	}
	list, _ := repo.List(ctx, f.account)
	if len(list) != 1 {
		t.Fatalf("List = %v", list)
	}
	_, err = repo.Get(ctx, other.AccountID, f.project.ID)
	wantErr(t, err, store.ErrNotFound)

	must(t, repo.UpdateStatus(ctx, f.account, f.project.ID, domain.ProjectRegistered, domain.ProjectOnboarding))
	wantErr(t, repo.UpdateStatus(ctx, f.account, f.project.ID, domain.ProjectRegistered, domain.ProjectOnboarding), store.ErrConflict)
	wantErr(t, repo.UpdateStatus(ctx, f.account, f.project.ID, domain.ProjectOnboarded, domain.ProjectOnboarding), domain.ErrInvalidTransition)
	must(t, repo.SetProfile(ctx, f.account, f.project.ID, json.RawMessage(`{"name":"app"}`)))
	p, _ := repo.Get(ctx, f.account, f.project.ID)
	if p.Status != domain.ProjectOnboarding || string(p.Profile) != `{"name":"app"}` {
		t.Fatalf("project = %+v", p)
	}
	wantErr(t, repo.SetProfile(ctx, f.account, domain.NewID(), nil), store.ErrNotFound)
}

func TestStoreGoalsAndSpecs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Login", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(ctx, &g))
	g2 := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Logout", Status: domain.GoalDraft, Platforms: []string{"Web", "ios", "web"}}
	must(t, f.st.Goals().Create(ctx, &g2))
	if !slices.Equal(g2.Platforms, []string{"ios", "web"}) {
		t.Fatalf("platforms = %v", g2.Platforms)
	}
	if g.Number != 1 || g2.Number != 2 || g.Ref() != "GOAL-1" {
		t.Fatalf("numbers = %d, %d", g.Number, g2.Number)
	}
	g.Title, g.IssueNumber = "Login v2", 7
	must(t, f.st.Goals().Update(ctx, &g))
	got, err := f.st.Goals().GetByNumber(ctx, f.account, 1)
	if err != nil || got.Title != "Login v2" || got.IssueNumber != 7 {
		t.Fatalf("goal = %+v, %v", got, err)
	}
	must(t, f.st.Goals().UpdateStatus(ctx, f.account, g.ID, domain.GoalDraft, domain.GoalSpecReview))
	list, _ := f.st.Goals().List(ctx, f.account, store.GoalFilter{Status: domain.GoalDraft})
	if len(list) != 1 || list[0].ID != g2.ID {
		t.Fatalf("draft goals = %v", list)
	}
	if got, _ := f.st.Goals().Get(ctx, f.account, g.ID); got.Status != domain.GoalSpecReview {
		t.Fatalf("status = %s", got.Status)
	}

	_, err = f.st.Specs().Latest(ctx, f.account, g.ID)
	wantErr(t, err, store.ErrNotFound)
	for rev := 2; rev >= 1; rev-- {
		sp := domain.Spec{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, Revision: rev}
		must(t, f.st.Specs().Create(ctx, &sp))
	}
	wantErr(t, f.st.Specs().Create(ctx, &domain.Spec{AccountID: f.account, GoalID: g.ID, Revision: 1}), store.ErrConflict)
	latest, err := f.st.Specs().Latest(ctx, f.account, g.ID)
	if err != nil || latest.Revision != 2 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	if sp, err := f.st.Specs().Get(ctx, f.account, latest.ID); err != nil || sp.Number != 1 {
		t.Fatalf("spec = %+v, %v", sp, err)
	}

}

func TestStoreTasks(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := domain.NewID()
	for _, seq := range []int{2, 1} {
		tk := domain.Task{AccountID: f.account, ProjectID: f.project.ID, GoalID: g, Seq: seq, Status: domain.TaskOpen}
		must(t, f.st.Tasks().Create(ctx, &tk))
	}
	tasks, _ := f.st.Tasks().List(ctx, f.account, store.TaskFilter{GoalID: g})
	if len(tasks) != 2 || tasks[0].Seq != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if limited, _ := f.st.Tasks().List(ctx, f.account, store.TaskFilter{Limit: 1}); len(limited) != 1 {
		t.Fatalf("limited = %v", limited)
	}
	tk := tasks[0]
	tk.IssueNumber, tk.Branch = 9, "agent/task-2"
	must(t, f.st.Tasks().Update(ctx, &tk))
	byIssue, err := f.st.Tasks().GetByIssue(ctx, f.account, f.project.ID, 9)
	if err != nil || byIssue.ID != tk.ID || byIssue.Branch != "agent/task-2" {
		t.Fatalf("GetByIssue = %+v, %v", byIssue, err)
	}
	must(t, f.st.Tasks().UpdateStatus(ctx, f.account, tk.ID, domain.TaskOpen, domain.TaskInProgress))
	if got, _ := f.st.Tasks().GetByNumber(ctx, f.account, tk.Number); got.Status != domain.TaskInProgress {
		t.Fatalf("task = %+v", got)
	}
	if got, err := f.st.Tasks().Get(ctx, f.account, tk.ID); err != nil || got.ID != tk.ID {
		t.Fatalf("Get = %v", err)
	}
}

func TestStoreChanges(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := domain.NewID()
	c := domain.Change{AccountID: f.account, ProjectID: f.project.ID, TaskID: task, Repo: "acme/app", Number: 5, HeadSHA: "a", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &c))
	f.clock.Advance(time.Minute)
	c2 := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "ACME/app", Number: 5, HeadSHA: "b", State: domain.ChangeMerged}
	must(t, f.st.Changes().Upsert(ctx, &c2))
	if c2.ID != c.ID || c2.TaskID != task || !c2.CreatedAt.Equal(t0) || !c2.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("upserted = %+v", c2)
	}
	got, err := f.st.Changes().GetByPR(ctx, f.account, "acme/app", 5)
	if err != nil || got.HeadSHA != "b" {
		t.Fatalf("GetByPR = %+v, %v", got, err)
	}
	if got, err := f.st.Changes().Get(ctx, f.account, c.ID); err != nil || got.State != domain.ChangeMerged {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	open, _ := f.st.Changes().List(ctx, f.account, store.ChangeFilter{State: domain.ChangeOpen})
	merged, _ := f.st.Changes().List(ctx, f.account, store.ChangeFilter{TaskID: task, State: domain.ChangeMerged})
	if len(open) != 0 || len(merged) != 1 {
		t.Fatalf("open = %v, merged = %v", open, merged)
	}
}

func newJob(f fixture, needs ...string) domain.Job {
	return domain.Job{
		AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code", Needs: needs,
		Spec: domain.JobSpec{Repo: "acme/app", RepoToken: "secret", Env: map[string]string{"K": "v"}},
	}
}

func TestStoreJobLeasing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	jobs := f.st.Jobs()
	node := newJob(f, "node")
	goJob := newJob(f, "GO")
	must(t, jobs.Create(ctx, &node))
	must(t, jobs.Create(ctx, &goJob))
	if node.Status != domain.JobQueued || node.MaxAttempts != domain.DefaultMaxAttempts || node.Spec.RepoToken != "" || node.Spec.Env != nil || node.Spec.JobID != node.ID {
		t.Fatalf("created = %+v", node)
	}
	worker := domain.NewID()
	_, err := jobs.LeaseNext(ctx, f.account, worker, []string{"go"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	leased, err := jobs.LeaseNext(ctx, f.account, worker, []string{"go", "Runtime:Claude-Code"}, time.Minute)
	must(t, err)
	if leased.ID != goJob.ID || leased.Status != domain.JobRunning || !leased.LeaseExpiresAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("leased = %+v", leased)
	}
	f.clock.Advance(30 * time.Second)
	ext, err := jobs.ExtendLease(ctx, f.account, goJob.ID, worker, time.Minute)
	if err != nil || !ext.LeaseExpiresAt.Equal(t0.Add(90*time.Second)) {
		t.Fatalf("extended = %+v, %v", ext, err)
	}
	_, err = jobs.ExtendLease(ctx, f.account, goJob.ID, domain.NewID(), time.Minute)
	wantErr(t, err, store.ErrLeaseLost)

	expired, err := jobs.RequeueExpired(ctx, f.account)
	if err != nil || len(expired) != 0 {
		t.Fatalf("early requeue = %v, %v", expired, err)
	}
	f.clock.Advance(2 * time.Minute)
	expired, err = jobs.RequeueExpired(ctx, f.account)
	if err != nil || len(expired) != 1 || expired[0].Status != domain.JobQueued || expired[0].Attempts != 1 {
		t.Fatalf("requeued = %+v, %v", expired, err)
	}
	_, err = jobs.ExtendLease(ctx, f.account, goJob.ID, worker, time.Minute)
	wantErr(t, err, store.ErrLeaseLost)

	leased, err = jobs.LeaseNext(ctx, f.account, worker, []string{"go", "runtime:claude-code"}, time.Minute)
	must(t, err)
	released, err := jobs.Release(ctx, f.account, leased.ID, worker, "clone failed")
	if err != nil || released.Status != domain.JobQueued || released.Attempts != 2 {
		t.Fatalf("released = %+v, %v", released, err)
	}
	_, err = jobs.Release(ctx, f.account, leased.ID, worker, "again")
	wantErr(t, err, store.ErrLeaseLost)

	// Released jobs wait for their backoff.
	if want := f.clock.Now().Add(domain.RetryBackoff(2)); !released.RunAfter.Equal(want) {
		t.Fatalf("run after = %v, want %v", released.RunAfter, want)
	}
	_, err = jobs.LeaseNext(ctx, f.account, worker, []string{"go", "runtime:claude-code"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	f.clock.Advance(domain.RetryBackoff(2))
	leased, err = jobs.LeaseNext(ctx, f.account, worker, []string{"go", "runtime:claude-code"}, time.Minute)
	must(t, err)
	_, err = jobs.Complete(ctx, f.account, leased.ID, worker, store.JobResult{Status: domain.JobQueued})
	wantErr(t, err, domain.ErrInvalidTransition)
	done, err := jobs.Complete(ctx, f.account, leased.ID, worker, store.JobResult{
		Status: domain.JobSucceeded, Output: json.RawMessage(`{}`), HeadSHA: "abc", Usage: domain.Usage{InputTokens: 5}, CostUSD: 0.1,
	})
	if err != nil || done.Status != domain.JobSucceeded || done.HeadSHA != "abc" || done.Usage.InputTokens != 5 || done.WorkerID != uuid.Nil() {
		t.Fatalf("done = %+v, %v", done, err)
	}
	_, err = jobs.Complete(ctx, f.account, leased.ID, worker, store.JobResult{Status: domain.JobFailed})
	wantErr(t, err, store.ErrLeaseLost)

	canceled, err := jobs.Cancel(ctx, f.account, node.ID)
	if err != nil || canceled.Status != domain.JobCanceled {
		t.Fatalf("canceled = %+v, %v", canceled, err)
	}
	_, err = jobs.Cancel(ctx, f.account, node.ID)
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = jobs.Cancel(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func TestStoreJobQueries(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a, b := newJob(f), newJob(f)
	must(t, f.st.Jobs().Create(ctx, &a))
	must(t, f.st.Jobs().Create(ctx, &b))
	got, err := f.st.Jobs().GetByNumber(ctx, f.account, 2)
	if err != nil || got.ID != b.ID || got.Ref() != "JOB-2" {
		t.Fatalf("GetByNumber = %+v, %v", got, err)
	}
	if got, err := f.st.Jobs().Get(ctx, f.account, a.ID); err != nil || got.Number != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	list, _ := f.st.Jobs().List(ctx, f.account, store.JobFilter{ProjectID: f.project.ID, Limit: 1})
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("newest job = %+v", list)
	}

	events, err := f.st.JobEvents().Append(ctx, f.account, a.ID, []domain.JobEvent{
		{Type: domain.EventLog, Text: "one"}, {Type: domain.EventMessage, Text: "two"},
	})
	if err != nil || len(events) != 2 || events[1].Seq != 2 || events[0].JobID != a.ID {
		t.Fatalf("Append = %+v, %v", events, err)
	}
	_, err = f.st.JobEvents().Append(ctx, f.account, domain.NewID(), nil)
	wantErr(t, err, store.ErrNotFound)
	w := domain.NewID()
	_, err = f.st.JobEvents().AppendLeased(ctx, f.account, a.ID, w, events[:1])
	wantErr(t, err, store.ErrLeaseLost)
	_, err = f.st.JobEvents().AppendLeased(ctx, f.account, domain.NewID(), w, nil)
	wantErr(t, err, store.ErrNotFound)
	after, _ := f.st.JobEvents().List(ctx, f.account, a.ID, 1, 10)
	if len(after) != 1 || after[0].Text != "two" {
		t.Fatalf("List = %+v", after)
	}
}

func TestStoreGatesAndSignals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateMergeApproval, SubjectRepo: "acme/app", SubjectNumber: 5}
	must(t, f.st.Gates().Create(ctx, &g))
	if g.Status != domain.GatePending || g.Ref() != "GATE-1" {
		t.Fatalf("gate = %+v", g)
	}
	pending, _ := f.st.Gates().ListPendingBySubject(ctx, f.account, "ACME/app", 5)
	if len(pending) != 1 {
		t.Fatalf("pending = %v", pending)
	}
	must(t, g.Resolve(domain.Actor{Kind: domain.ActorUser, Name: "ana"}, domain.GateApproved, "ok", t0))
	must(t, f.st.Gates().Resolve(ctx, &g))
	wantErr(t, f.st.Gates().Resolve(ctx, &g), store.ErrConflict)
	got, err := f.st.Gates().GetByNumber(ctx, f.account, 1)
	if err != nil || got.Status != domain.GateApproved || got.ResolvedBy != "user:ana" {
		t.Fatalf("gate = %+v, %v", got, err)
	}
	if got, err := f.st.Gates().Get(ctx, f.account, g.ID); err != nil || got.Feedback != "ok" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	approved, _ := f.st.Gates().List(ctx, f.account, store.GateFilter{Status: domain.GateApproved, Kind: domain.GateMergeApproval})
	if len(approved) != 1 {
		t.Fatalf("List = %v", approved)
	}

	sig, err := domain.NewSignal(f.account, f.project.ID, &domain.PRMerged{Repo: "acme/app", Number: 5, SHA: "m"}, t0)
	must(t, err)
	dup := sig
	dup.ID = domain.NewID()
	if ok, err := f.st.Signals().Insert(ctx, &sig); !ok || err != nil {
		t.Fatalf("Insert = %v, %v", ok, err)
	}
	if ok, err := f.st.Signals().Insert(ctx, &dup); ok || err != nil {
		t.Fatalf("duplicate Insert = %v, %v", ok, err)
	}
	noTime := domain.Signal{AccountID: f.account, Key: "pr:acme/app#5", DedupeKey: "pr.closed:acme/app#5"}
	if ok, _ := f.st.Signals().Insert(ctx, &noTime); !ok || !noTime.CreatedAt.Equal(t0) {
		t.Fatalf("signal = %+v", noTime)
	}
	byKey, _ := f.st.Signals().ListByKey(ctx, f.account, "pr:acme/app#5")
	if len(byKey) != 2 {
		t.Fatalf("ListByKey = %v", byKey)
	}
}

func TestStoreWorkersAndTokens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w := domain.Worker{AccountID: f.account, Name: "w1", TokenHash: []byte("h1"), MaxJobs: 1}
	must(t, f.st.Workers().Create(ctx, &w))
	wantErr(t, f.st.Workers().Create(ctx, &domain.Worker{AccountID: f.account, Name: "w1"}), store.ErrConflict)
	must(t, f.st.Workers().Seen(ctx, f.account, w.ID, []string{"Go", "linux"}, "v1"))
	got, err := f.st.Workers().GetByTokenHash(ctx, []byte("h1"))
	if err != nil || !got.Online(t0) || got.Version != "v1" || got.Labels[0] != "go" {
		t.Fatalf("worker = %+v, %v", got, err)
	}
	must(t, f.st.Workers().SetPool(ctx, f.account, w.ID, domain.PoolControl))
	if got, _ = f.st.Workers().Get(ctx, f.account, w.ID); got.Pool != domain.PoolControl {
		t.Fatalf("pool = %q", got.Pool)
	}
	must(t, f.st.Workers().SetPool(ctx, f.account, w.ID, ""))
	if got, _ = f.st.Workers().Get(ctx, f.account, w.ID); got.Pool != domain.PoolBuild {
		t.Fatalf("pool = %q", got.Pool)
	}
	wantErr(t, f.st.Workers().SetPool(ctx, f.account, domain.NewID(), domain.PoolControl), store.ErrNotFound)
	must(t, f.st.Workers().Revoke(ctx, f.account, w.ID))
	f.clock.Advance(time.Hour)
	must(t, f.st.Workers().Revoke(ctx, f.account, w.ID))
	got, _ = f.st.Workers().Get(ctx, f.account, w.ID)
	if !got.RevokedAt.Equal(t0) {
		t.Fatalf("revoked at %v", got.RevokedAt)
	}
	if list, _ := f.st.Workers().List(ctx, f.account); len(list) != 1 {
		t.Fatalf("List = %v", list)
	}

	tok := domain.MCPToken{AccountID: f.account, Name: "laptop", TokenHash: []byte("t1")}
	must(t, f.st.MCPTokens().Create(ctx, &tok))
	wantErr(t, f.st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: f.account, Name: "laptop"}), store.ErrConflict)
	must(t, f.st.MCPTokens().Touch(ctx, f.account, tok.ID))
	must(t, f.st.MCPTokens().Revoke(ctx, f.account, tok.ID))
	must(t, f.st.MCPTokens().Revoke(ctx, f.account, tok.ID))
	gotTok, err := f.st.MCPTokens().GetByTokenHash(ctx, []byte("t1"))
	if err != nil || gotTok.LastUsedAt.IsZero() || gotTok.RevokedAt.IsZero() {
		t.Fatalf("token = %+v, %v", gotTok, err)
	}
	if list, _ := f.st.MCPTokens().List(ctx, f.account); len(list) != 1 {
		t.Fatalf("List = %v", list)
	}
	_, err = f.st.MCPTokens().GetByTokenHash(ctx, []byte("nope"))
	wantErr(t, err, store.ErrNotFound)
}

func TestStoreWorkflowRuns(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	run := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID, Playbook: "goal", Status: domain.RunRunning}
	must(t, f.st.WorkflowRuns().Create(ctx, &run))
	step := domain.StepRun{AccountID: f.account, RunID: run.ID, StepID: "spec", Attempt: 1, Status: domain.StepRunning}
	must(t, f.st.StepRuns().Create(ctx, &step))
	wantErr(t, f.st.StepRuns().Create(ctx, &domain.StepRun{AccountID: f.account, RunID: run.ID, StepID: "spec", Attempt: 1}), store.ErrConflict)
	step.Status, step.WaitKey, step.StepID = domain.StepWaiting, "job:1", "ignored"
	must(t, f.st.StepRuns().Update(ctx, &step))
	if step.StepID != "spec" {
		t.Fatalf("immutable field changed: %+v", step)
	}
	waiting, _ := f.st.StepRuns().ListWaiting(ctx, f.account, "job:1")
	byRun, _ := f.st.StepRuns().ListByRun(ctx, f.account, run.ID)
	if len(waiting) != 1 || len(byRun) != 1 {
		t.Fatalf("waiting = %v, byRun = %v", waiting, byRun)
	}
	if got, err := f.st.StepRuns().Get(ctx, f.account, step.ID); err != nil || got.WaitKey != "job:1" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if active, _ := f.st.WorkflowRuns().ListActive(ctx, f.account); len(active) != 1 {
		t.Fatalf("active = %v", active)
	}
	run.Status, run.FinishedAt = domain.RunSucceeded, t0
	must(t, f.st.WorkflowRuns().Update(ctx, &run))
	if active, _ := f.st.WorkflowRuns().ListActive(ctx, f.account); len(active) != 0 {
		t.Fatalf("active = %v", active)
	}
	if got, err := f.st.WorkflowRuns().Get(ctx, f.account, run.ID); err != nil || got.Status != domain.RunSucceeded {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

func TestStoreDeliveriesAndAudit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	d := domain.WebhookDelivery{DeliveryID: "d1", Event: "push"}
	if ok, err := f.st.WebhookDeliveries().Record(ctx, d); !ok || err != nil {
		t.Fatalf("Record = %v, %v", ok, err)
	}
	if ok, _ := f.st.WebhookDeliveries().Record(ctx, d); ok {
		t.Fatal("redelivery recorded as new")
	}
	for _, action := range []string{"job.created", "gate.resolved"} {
		must(t, f.st.Audit().Append(ctx, &domain.AuditEntry{AccountID: f.account, ProjectID: f.project.ID, Actor: "system:forecrewd", Action: action}))
	}
	entries, _ := f.st.Audit().List(ctx, f.account, store.AuditFilter{ProjectID: f.project.ID})
	if len(entries) != 2 || entries[0].Action != "gate.resolved" {
		t.Fatalf("audit = %+v", entries)
	}
}

func TestStoreWithTx(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	boom := errors.New("boom")
	err := f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID}
		must(t, tx.Goals().Create(ctx, &g))
		if list, _ := tx.Goals().List(ctx, f.account, store.GoalFilter{}); len(list) != 1 {
			t.Errorf("tx does not see its write")
		}
		return boom
	})
	wantErr(t, err, boom)
	if list, _ := f.st.Goals().List(ctx, f.account, store.GoalFilter{}); len(list) != 0 {
		t.Fatalf("rolled back goal still there: %v", list)
	}
	err = f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		return tx.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID}
			return tx.Goals().Create(ctx, &g)
		})
	})
	must(t, err)
	list, _ := f.st.Goals().List(ctx, f.account, store.GoalFilter{})
	if len(list) != 1 || list[0].Number != 1 {
		t.Fatalf("committed goals = %+v", list)
	}
}

func TestStoreRejectsForeignRows(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	o := other.ID
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "g"}
	must(t, f.st.Goals().Create(ctx, &g))
	sp := domain.Spec{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, Revision: 1}
	must(t, f.st.Specs().Create(ctx, &sp))
	tk := domain.Task{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, SpecID: sp.ID}
	must(t, f.st.Tasks().Create(ctx, &tk))
	job := newJob(f)
	must(t, f.st.Jobs().Create(ctx, &job))
	run := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID}
	must(t, f.st.WorkflowRuns().Create(ctx, &run))

	for name, err := range map[string]error{
		"goal":   f.st.Goals().Create(ctx, &domain.Goal{AccountID: o, ProjectID: f.project.ID}),
		"spec":   f.st.Specs().Create(ctx, &domain.Spec{AccountID: o, GoalID: g.ID, Revision: 1}),
		"task":   f.st.Tasks().Create(ctx, &domain.Task{AccountID: o, SpecID: sp.ID}),
		"job":    f.st.Jobs().Create(ctx, &domain.Job{AccountID: o, TaskID: tk.ID}),
		"gate":   f.st.Gates().Create(ctx, &domain.Gate{AccountID: o, JobID: job.ID}),
		"run":    f.st.WorkflowRuns().Create(ctx, &domain.WorkflowRun{AccountID: o, GoalID: g.ID}),
		"step":   f.st.StepRuns().Create(ctx, &domain.StepRun{AccountID: o, RunID: run.ID}),
		"audit":  f.st.Audit().Append(ctx, &domain.AuditEntry{AccountID: o, ProjectID: f.project.ID}),
		"change": f.st.Changes().Upsert(ctx, &domain.Change{AccountID: o, ProjectID: f.project.ID, Repo: "a/b", Number: 1}),
	} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want not found", name, err)
		}
	}
	_, err := f.st.Signals().Insert(ctx, &domain.Signal{AccountID: o, ProjectID: f.project.ID, DedupeKey: "k"})
	wantErr(t, err, store.ErrNotFound)
}

func TestStoreJobLeasingByPool(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	jobs := f.st.Jobs()
	build := newJob(f)
	must(t, jobs.Create(ctx, &build))
	cmd := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Pool: domain.PoolControl, Needs: []string{"docker"}}
	must(t, jobs.Create(ctx, &cmd))
	if build.Kind != domain.JobKindAgent || build.Pool != domain.PoolBuild {
		t.Fatalf("defaults = %s, %s", build.Kind, build.Pool)
	}
	control := domain.Worker{AccountID: f.account, Name: "ctl", Pool: domain.PoolControl}
	must(t, f.st.Workers().Create(ctx, &control))
	labels := []string{"runtime:claude-code", "docker", "pool:build"}
	leased, err := jobs.LeaseNext(ctx, f.account, control.ID, labels, time.Minute)
	if err != nil || leased.ID != cmd.ID {
		t.Fatalf("control leased %+v, %v; want the control command job", leased, err)
	}
	_, err = jobs.LeaseNext(ctx, f.account, control.ID, labels, time.Minute)
	wantErr(t, err, store.ErrNoJob)
}

func TestStoreArtifacts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer"}
	must(t, f.st.Jobs().Create(ctx, &job))
	art := func(name string) domain.Artifact {
		return domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, Name: name, Size: 1, StorageKey: "k/" + name}
	}

	b, a := art("b.txt"), art("a.txt")
	must(t, repo.Put(ctx, &b))
	f.clock.Advance(time.Hour)
	must(t, repo.Put(ctx, &a))
	list, err := repo.ListByJob(ctx, f.account, job.ID)
	if err != nil || len(list) != 2 || list[0].Name != "a.txt" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	again := art("a.txt")
	again.Size = 7
	must(t, repo.Put(ctx, &again))
	got, err := repo.Get(ctx, f.account, a.ID)
	if err != nil || again.ID != a.ID || got.Size != 7 {
		t.Fatalf("re-upload = %+v, %v", got, err)
	}
	if list, _ := repo.ListByJob(ctx, f.account, job.ID); len(list) != 2 {
		t.Fatalf("re-upload added a row: %+v", list)
	}

	// Without a job a key may not repeat; rows are never merged.
	r1 := domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, Name: "r.md", StorageKey: "r1"}
	r2 := domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, Name: "r.md", StorageKey: "r2"}
	must(t, repo.Put(ctx, &r1))
	must(t, repo.Put(ctx, &r2))
	r3 := domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, Name: "r3.md", StorageKey: "r1"}
	wantErr(t, repo.Put(ctx, &r3), store.ErrConflict)

	older, err := repo.ListOlderThan(ctx, f.account, t0.Add(30*time.Minute), 0)
	if err != nil || len(older) != 1 || older[0].Name != "b.txt" {
		t.Fatalf("older = %+v, %v", older, err)
	}
	older, _ = repo.ListOlderThan(ctx, f.account, t0.Add(2*time.Hour), 2)
	if len(older) != 2 {
		t.Fatalf("limit = %+v", older)
	}
	_, err = repo.ListOlderThan(ctx, domain.NewID(), t0.Add(time.Hour), 0)
	must(t, err)

	must(t, repo.Delete(ctx, f.account, a.ID))
	_, err = repo.Get(ctx, f.account, a.ID)
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, repo.Delete(ctx, f.account, a.ID), store.ErrNotFound)

	foreign := art("z.txt")
	foreign.AccountID = domain.NewID()
	foreign.ProjectID = f.project.ID
	wantErr(t, repo.Put(ctx, &foreign), store.ErrNotFound)
}

func TestStoreArtifactsPutLeased(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code"}
	must(t, f.st.Jobs().Create(ctx, &job))
	w := domain.Worker{AccountID: f.account, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, f.st.Workers().Create(ctx, &w))
	art := func(name, key string) domain.Artifact {
		return domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, Name: name, StorageKey: key}
	}

	a := art("a.txt", "k1")
	_, err := repo.PutLeased(ctx, &a, w.ID)
	wantErr(t, err, store.ErrLeaseLost) // still queued
	_, err = f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	_, err = repo.PutLeased(ctx, &a, domain.NewID())
	wantErr(t, err, store.ErrLeaseLost) // another worker
	replaced, err := repo.PutLeased(ctx, &a, w.ID)
	if err != nil || replaced != "" {
		t.Fatalf("first: %q, %v", replaced, err)
	}
	again := art("a.txt", "k2")
	if replaced, err = repo.PutLeased(ctx, &again, w.ID); err != nil || replaced != "k1" || again.ID != a.ID {
		t.Fatalf("replace: %q, %v", replaced, err)
	}
	missing := art("x", "k3")
	missing.JobID = domain.NewID()
	_, err = repo.PutLeased(ctx, &missing, w.ID)
	wantErr(t, err, store.ErrNotFound)
}

func TestStoreLeasedWritesRefuseAnotherWorker(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code"}
	must(t, f.st.Jobs().Create(ctx, &j))
	w := domain.Worker{AccountID: f.account, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, f.st.Workers().Create(ctx, &w))
	_, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	_, err = f.st.JobEvents().AppendLeased(ctx, f.account, j.ID, domain.NewID(), []domain.JobEvent{{Type: domain.EventLog}})
	wantErr(t, err, store.ErrLeaseLost)
	if rows, _ := f.st.JobEvents().List(ctx, f.account, j.ID, 0, 10); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestStoreGoalSize(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Login", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(ctx, &g))
	stale := g // loaded before the size is set
	low := domain.GoalSize{Impact: domain.LevelLow, Reversibility: domain.ReversibleEasy, Councils: []domain.CouncilKind{}, Reason: "small"}
	must(t, f.st.Goals().SetSize(ctx, f.account, g.ID, low))
	got, err := f.st.Goals().Get(ctx, f.account, g.ID)
	if err != nil || got.Size == nil || got.Size.Reason != "small" || got.Size.Impact != domain.LevelLow {
		t.Fatalf("goal size = %+v, %v", got.Size, err)
	}
	// A stale goal updates its title and keeps the stored size.
	stale.Title = "Login v2"
	must(t, f.st.Goals().Update(ctx, &stale))
	if got, _ := f.st.Goals().Get(ctx, f.account, g.ID); got.Size == nil || got.Size.Reason != "small" || got.Title != "Login v2" {
		t.Errorf("after a stale update = %+v", got)
	}
	// A second SetSize keeps the first size.
	must(t, f.st.Goals().SetSize(ctx, f.account, g.ID, domain.GoalSize{Councils: []domain.CouncilKind{domain.CouncilProduct}, Reason: "other"}))
	if got, _ := f.st.Goals().Get(ctx, f.account, g.ID); got.Size == nil || got.Size.Reason != "small" {
		t.Errorf("second SetSize replaced the size: %+v", got.Size)
	}
	wantErr(t, f.st.Goals().SetSize(ctx, f.account, domain.NewID(), low), store.ErrNotFound)
}

func TestStoreArtifactsExistingKeys(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer"}
	must(t, f.st.Jobs().Create(ctx, &job))
	a := domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, Name: "a.txt", Size: 1, StorageKey: "k/a"}
	must(t, repo.Put(ctx, &a))
	got, err := repo.ExistingKeys(ctx, []string{"k/a", "k/missing"})
	if err != nil || len(got) != 1 || got[0] != "k/a" {
		t.Fatalf("ExistingKeys = %v, %v", got, err)
	}
	if got, err := repo.ExistingKeys(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("ExistingKeys(nil) = %v, %v", got, err)
	}
}

func TestStoreJobMCPTokenHash(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	jobs := f.st.Jobs()
	j, k := newJob(f), newJob(f)
	must(t, jobs.Create(ctx, &j))
	must(t, jobs.Create(ctx, &k))
	worker := domain.NewID()
	hash := make([]byte, 32)
	hash[0] = 1
	wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, j.ID, worker, 0, hash), store.ErrLeaseLost)
	leased, err := jobs.LeaseNext(ctx, f.account, worker, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	if err := jobs.SetJobMCPTokenHash(ctx, f.account, leased.ID, worker, leased.Attempts, hash[:3]); err == nil {
		t.Fatal("short hash accepted")
	}
	wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, leased.ID, domain.NewID(), leased.Attempts, hash), store.ErrLeaseLost)
	must(t, jobs.SetJobMCPTokenHash(ctx, f.account, leased.ID, worker, leased.Attempts, hash))
	second, err := jobs.LeaseNext(ctx, f.account, worker, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, second.ID, worker, second.Attempts, hash), store.ErrConflict)
	if got, err := jobs.GetByJobMCPTokenHash(ctx, hash); err != nil || got.ID != leased.ID {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	_, err = jobs.Release(ctx, f.account, leased.ID, worker, "retry")
	must(t, err)
	_, err = jobs.GetByJobMCPTokenHash(ctx, hash)
	wantErr(t, err, store.ErrNotFound)
	_, err = jobs.GetByJobMCPTokenHash(ctx, make([]byte, 32))
	wantErr(t, err, store.ErrNotFound)
}

func TestStoreGateOrigin(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := newJob(f)
	must(t, f.st.Jobs().Create(ctx, &j))
	ask := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Origin: domain.GateFromAskHuman, Title: "q", SubjectRepo: "acme/app", JobID: j.ID}
	must(t, f.st.Gates().Create(ctx, &ask))
	plain := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "q", SubjectRepo: "acme/app"}
	must(t, f.st.Gates().Create(ctx, &plain))
	if !ask.AsksHuman() || plain.Origin != domain.GateFromPlaybook {
		t.Fatalf("origins = %q %q", ask.Origin, plain.Origin)
	}
	bad := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateSpecApproval, Origin: domain.GateFromAskHuman, Title: "q", SubjectRepo: "acme/app", JobID: j.ID}
	if err := f.st.Gates().Create(ctx, &bad); err == nil {
		t.Fatal("ask_human spec gate accepted")
	}
}

func TestStoreTestRuns(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.TestRuns()
	job := newJob(f)
	must(t, f.st.Jobs().Create(ctx, &job))
	change := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/app", Number: 3, HeadSHA: "abc123", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &change))

	run := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, ChangeID: change.ID, SHA: "abc123", Suite: "unit",
		Status: domain.TestRunFailed, Counts: domain.TestCounts{Passed: 1, Failed: 1}, ExitCode: 1, Attempt: 1, Cell: map[string]string{"os": "linux"}}
	results := []domain.TestResult{
		{Name: "TestB", Classname: "pkg", Status: domain.TestFailed, Message: "boom"},
		{Name: "TestA", Classname: "pkg", Status: domain.TestPassed},
		{Name: "TestC", Classname: "alpha", Status: domain.TestSkipped},
	}
	must(t, repo.Create(ctx, &run, results))
	if run.ID == uuid.Nil() || run.CreatedAt.IsZero() || results[0].ID == uuid.Nil() || results[0].RunID != run.ID || results[0].AccountID != f.account {
		t.Fatalf("Create did not fill %+v, %+v", run, results[0])
	}
	got, err := repo.Get(ctx, f.account, run.ID)
	if err != nil || got.Suite != "unit" || got.Cell["os"] != "linux" || got.Versions == nil || got.ReportArtifacts == nil || got.ChangeID != change.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	rs, err := repo.Results(ctx, f.account, run.ID)
	if err != nil || len(rs) != 3 || rs[0].Name != "TestC" || rs[1].Name != "TestA" || rs[2].Name != "TestB" || rs[2].Message != "boom" {
		t.Fatalf("Results = %+v, %v", rs, err)
	}

	dup := run
	dup.ID = uuid.Nil()
	wantErr(t, repo.Create(ctx, &dup, nil), store.ErrConflict)
	other := run
	other.ID, other.Suite = uuid.Nil(), "lint"
	f.clock.Advance(time.Minute)
	must(t, repo.Create(ctx, &other, nil))
	otherJob := newJob(f)
	must(t, f.st.Jobs().Create(ctx, &otherJob))
	later := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: otherJob.ID, SHA: "def456", Suite: "unit", Status: domain.TestRunPassed, Attempt: 1}
	f.clock.Advance(time.Minute)
	must(t, repo.Create(ctx, &later, nil))

	for _, tt := range []struct {
		name string
		f    store.TestRunFilter
		want []string
	}{
		{"all", store.TestRunFilter{}, []string{"unit", "lint", "unit"}},
		{"job", store.TestRunFilter{JobID: job.ID}, []string{"unit", "lint"}},
		{"change", store.TestRunFilter{ChangeID: change.ID}, []string{"unit", "lint"}},
		{"sha", store.TestRunFilter{SHA: "def456"}, []string{"unit"}},
		{"no change", store.TestRunFilter{NoChange: true}, []string{"unit"}},
		{"newest first", store.TestRunFilter{Newest: true}, []string{"unit", "lint", "unit"}},
		{"newest, limited", store.TestRunFilter{Newest: true, Limit: 1}, []string{"unit"}},
		{"project", store.TestRunFilter{ProjectID: f.project.ID, Limit: 2}, []string{"unit", "lint"}},
	} {
		list, err := repo.List(ctx, f.account, tt.f)
		var suites []string
		for _, r := range list {
			suites = append(suites, r.Suite)
		}
		if err != nil || !slices.Equal(suites, tt.want) {
			t.Errorf("List %s = %v, %v; want %v", tt.name, suites, err, tt.want)
		}
	}

	// Another account sees nothing, and cannot attach a run to this project.
	stranger := domain.NewID()
	if _, err := repo.Get(ctx, stranger, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("foreign Get err = %v", err)
	}
	if list, _ := repo.List(ctx, stranger, store.TestRunFilter{}); len(list) != 0 {
		t.Errorf("foreign List = %+v", list)
	}
	if rs, _ := repo.Results(ctx, stranger, run.ID); len(rs) != 0 {
		t.Errorf("foreign Results = %+v", rs)
	}
	foreign := domain.TestRun{AccountID: stranger, ProjectID: f.project.ID, JobID: job.ID, SHA: "x", Suite: "s", Status: domain.TestRunPassed, Attempt: 1}
	wantErr(t, repo.Create(ctx, &foreign, nil), store.ErrNotFound)

	// A failed transaction leaves nothing behind.
	boom := errors.New("boom")
	err = f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		r := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, SHA: "x", Suite: "rolled-back", Status: domain.TestRunPassed, Attempt: 2}
		must(t, tx.TestRuns().Create(ctx, &r, []domain.TestResult{{Name: "t"}}))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if list, _ := repo.List(ctx, f.account, store.TestRunFilter{JobID: job.ID}); len(list) != 2 {
		t.Errorf("rolled back run is visible: %+v", list)
	}

	// Changes can be found by head.
	if cs, _ := f.st.Changes().List(ctx, f.account, store.ChangeFilter{HeadSHA: "abc123"}); len(cs) != 1 {
		t.Errorf("changes by head = %+v", cs)
	}
	if cs, _ := f.st.Changes().List(ctx, f.account, store.ChangeFilter{HeadSHA: "nope"}); len(cs) != 0 {
		t.Errorf("changes by unknown head = %+v", cs)
	}
}

func TestStoreTestRunFailures(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.TestRuns()
	job := newJob(f)
	must(t, f.st.Jobs().Create(ctx, &job))
	run := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, SHA: "x", Suite: "s", Status: domain.TestRunFailed, Attempt: 1}
	must(t, repo.Create(ctx, &run, []domain.TestResult{
		{Name: "D", Status: domain.TestPassed}, {Name: "C", Status: domain.TestFailed}, {Name: "B", Status: domain.TestFailed},
		{Name: "A", Status: domain.TestErrored}, {Name: "S", Status: domain.TestSkipped},
	}))
	fails, err := repo.Failures(ctx, f.account, run.ID, 0)
	if err != nil || len(fails) != 3 || fails[0].Name != "A" || fails[2].Name != "C" {
		t.Fatalf("Failures = %+v, %v", fails, err)
	}
	if fails, err := repo.Failures(ctx, f.account, run.ID, 2); err != nil || len(fails) != 2 {
		t.Fatalf("limited Failures = %+v, %v", fails, err)
	}
	if fails, _ := repo.Failures(ctx, domain.NewID(), run.ID, 0); len(fails) != 0 {
		t.Fatalf("foreign Failures = %+v", fails)
	}
}

func TestStoreRegisters(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Ship", CreatedBy: "user:me"}
	must(t, f.st.Goals().Create(ctx, &g))
	repo := f.st.Assumptions()
	a := domain.Assumption{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, MadeBy: "architect",
		Statement: "s", Confidence: domain.LevelLow, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest}
	must(t, repo.Create(ctx, &a))
	b := a
	b.ID, b.GoalID, b.Impact = uuid.Nil(), uuid.Nil(), domain.LevelLow
	must(t, repo.Create(ctx, &b))
	if a.Number != 1 || b.Number != 2 || a.Status != domain.AssumptionUnverified || a.Dependents == nil {
		t.Fatalf("created = %+v %+v", a, b)
	}
	if got, err := repo.GetByNumber(ctx, f.account, 2); err != nil || got.ID != b.ID {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	if got, err := repo.Get(ctx, f.account, a.ID); err != nil || got.Number != 1 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	list, err := repo.List(ctx, f.account, store.AssumptionFilter{GoalID: g.ID, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest})
	if err != nil || len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	dep := []domain.Dependent{{Kind: domain.DependentTask, ID: "TASK-1"}}
	must(t, repo.AddDependents(ctx, f.account, a.ID, dep))
	must(t, repo.AddDependents(ctx, f.account, a.ID, dep))
	if got, _ := repo.Get(ctx, f.account, a.ID); len(got.Dependents) != 1 {
		t.Fatalf("dependents = %+v", got.Dependents)
	}
	wantErr(t, repo.AddDependents(ctx, f.account, domain.NewID(), dep), store.ErrNotFound)

	by := domain.Actor{Kind: domain.ActorUser, Name: "me"}
	cur, _ := repo.Get(ctx, f.account, a.ID)
	stale := cur
	must(t, cur.Resolve(by, domain.AssumptionAccepted, "", t0))
	must(t, repo.Resolve(ctx, &cur, domain.AssumptionUnverified))
	must(t, stale.Resolve(by, domain.AssumptionVerified, "test", t0))
	wantErr(t, repo.Resolve(ctx, &stale, domain.AssumptionUnverified), store.ErrConflict)
	bad := cur
	bad.Status = domain.AssumptionUnverified
	wantErr(t, repo.Resolve(ctx, &bad, domain.AssumptionAccepted), domain.ErrInvalidTransition)
	if got, _ := repo.Get(ctx, f.account, a.ID); got.Status != domain.AssumptionAccepted || got.ResolvedBy != "user:me" {
		t.Fatalf("resolved = %+v", got)
	}

	dec := f.st.Decisions()
	d := domain.Decision{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, Question: "Q", Choice: "c", DecidedBy: "user:me", Assumptions: []uuid.UUID{a.ID}}
	must(t, dec.Create(ctx, &d))
	e := domain.Decision{AccountID: f.account, ProjectID: f.project.ID, Question: "Q2", Choice: "c", DecidedBy: "user:me"}
	must(t, dec.Create(ctx, &e))
	if d.Number != 1 || e.Number != 2 || e.Options == nil || e.Assumptions == nil {
		t.Fatalf("decisions = %+v %+v", d, e)
	}
	if got, err := dec.GetByNumber(ctx, f.account, 1); err != nil || got.ID != d.ID {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	if got, err := dec.Get(ctx, f.account, e.ID); err != nil || got.Question != "Q2" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if l, err := dec.List(ctx, f.account, store.DecisionFilter{GoalID: g.ID}); err != nil || len(l) != 1 {
		t.Fatalf("list = %+v, %v", l, err)
	}
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	_, err = dec.Get(ctx, other.ID, d.ID)
	wantErr(t, err, store.ErrNotFound)
	foreign := domain.Decision{AccountID: other.ID, ProjectID: f.project.ID, Question: "Q", Choice: "c", DecidedBy: "user:x"}
	wantErr(t, dec.Create(ctx, &foreign), store.ErrNotFound)
	foreignA := domain.Assumption{AccountID: other.ID, ProjectID: f.project.ID, MadeBy: "x", StatedAssumption: a.StatedAssumption}
	wantErr(t, repo.Create(ctx, &foreignA), store.ErrNotFound)
}

func TestStoreAssumptionRaiseImpactAndLock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Assumptions()
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Ship", CreatedBy: "user:me"}
	must(t, f.st.Goals().Create(ctx, &g))
	a := domain.Assumption{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, MadeBy: "architect",
		Statement: "s", Confidence: domain.LevelLow, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest}
	must(t, repo.Create(ctx, &a))
	lo := a
	lo.ID, lo.Impact = uuid.Nil(), domain.LevelLow
	must(t, repo.Create(ctx, &lo))

	// The impact only goes up.
	must(t, repo.RaiseImpact(ctx, f.account, a.ID, domain.LevelLow))
	if got, _ := repo.Get(ctx, f.account, a.ID); got.Impact != domain.LevelHigh {
		t.Fatalf("impact lowered: %+v", got)
	}
	for _, to := range []domain.Level{domain.LevelMedium, domain.LevelMedium, domain.LevelHigh} {
		must(t, repo.RaiseImpact(ctx, f.account, lo.ID, to))
		if got, _ := repo.Get(ctx, f.account, lo.ID); got.Impact != to {
			t.Fatalf("impact = %+v, want %s", got, to)
		}
	}
	wantErr(t, repo.RaiseImpact(ctx, f.account, domain.NewID(), domain.LevelHigh), store.ErrNotFound)
	must(t, repo.Lock(ctx, f.account, f.project.ID, g.ID))
	must(t, repo.Lock(ctx, f.account, f.project.ID, uuid.Nil()))
}

func TestAccountSlugs(t *testing.T) {
	st := NewStore(time.Now)
	ctx := t.Context()
	var got []string
	for range 3 {
		a := domain.Account{Name: "Acme Labs"}
		if err := st.Accounts().Create(ctx, &a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a.Slug)
	}
	if !slices.Equal(got, []string{"acme-labs", "acme-labs-2", "acme-labs-3"}) {
		t.Fatalf("slugs = %v", got)
	}
	given := domain.Account{Name: "other", Slug: "acme-labs"}
	if err := st.Accounts().Create(ctx, &given); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("taken slug: %v", err)
	}
	first := domain.Account{Name: "first"}
	must(t, st.Accounts().Create(ctx, &first))
	again := domain.Account{ID: first.ID, Name: "second"}
	if err := st.Accounts().Create(ctx, &again); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("taken ID: %v", err)
	}
}

func TestAccountTokenCap(t *testing.T) {
	st := NewStore(time.Now)
	a := domain.Account{Name: "acme"}
	if err := st.Accounts().Create(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	if err := st.Accounts().SetMaxTokenTTL(t.Context(), a.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := st.Accounts().Get(t.Context(), a.ID)
	if err != nil || time.Duration(got.MaxTokenTTL) != time.Hour {
		t.Fatalf("account = %+v, %v", got, err)
	}
	if err := st.Accounts().SetMaxTokenTTL(t.Context(), domain.NewID(), time.Hour); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}

}

func TestKnowledgeStore(t *testing.T) {
	// The real clock: the suite checks that the newest note comes first.
	st := NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, st.Projects().Create(t.Context(), &p))
	KnowledgeStoreSuite(t, st, acc.ID, p.ID, true)
	KnowledgeProposalStoreSuite(t, st, acc.ID, p.ID)
}

func TestStoreCouncils(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Councils()
	c := domain.Council{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilTechnical, Question: "Which queue?"}
	must(t, repo.Create(ctx, &c))
	if c.ID == uuid.Nil() || c.Number != 1 || c.Status != domain.CouncilRunning || c.Constraints == nil {
		t.Fatalf("Create = %+v", c)
	}
	dup := domain.Council{ID: c.ID, AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)

	c.Constraints = []string{"keep Postgres"}
	c.Brief = &domain.CouncilBrief{Question: "Which queue?"}
	must(t, repo.Update(ctx, &c))
	got, err := repo.GetForUpdate(ctx, f.account, c.ID)
	if err != nil || !slices.Equal(got.Constraints, c.Constraints) || got.Brief == nil {
		t.Fatalf("GetForUpdate = %+v, %v", got, err)
	}
	got.Constraints[0] = "changed by the caller"
	if again, _ := repo.GetByNumber(ctx, f.account, c.Number); again.Constraints[0] != "keep Postgres" {
		t.Fatalf("a caller changed the stored constraints: %v", again.Constraints)
	}
	must(t, repo.UpdateStatus(ctx, f.account, c.ID, domain.CouncilRunning, domain.CouncilDecided))
	wantErr(t, repo.UpdateStatus(ctx, f.account, c.ID, domain.CouncilRunning, domain.CouncilFailed), store.ErrConflict)
	wantErr(t, repo.UpdateStatus(ctx, f.account, c.ID, domain.CouncilDecided, domain.CouncilRunning), domain.ErrInvalidTransition)

	other := domain.Council{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	must(t, repo.Create(ctx, &other))
	if list, err := repo.List(ctx, f.account, store.CouncilFilter{Status: domain.CouncilRunning}); err != nil || len(list) != 1 || list[0].ID != other.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list, _ := repo.List(ctx, f.account, store.CouncilFilter{ProjectID: f.project.ID}); len(list) != 2 || list[0].ID != other.ID {
		t.Fatalf("List is not newest first: %+v", list)
	}
	g := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateCouncilDecision, Title: "t", SubjectRepo: "acme/app"}
	must(t, f.st.Gates().Create(ctx, &g))
	other.GateID = g.ID
	must(t, repo.Update(ctx, &other))
	if list, _ := repo.List(ctx, f.account, store.CouncilFilter{GateID: g.ID}); len(list) != 1 || list[0].ID != other.ID {
		t.Fatalf("List by gate = %+v", list)
	}
}

func TestStoreCouncilContributions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Councils()
	c := domain.Council{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	must(t, repo.Create(ctx, &c))
	j := newJob(f)
	must(t, f.st.Jobs().Create(ctx, &j))
	p := domain.CouncilContribution{AccountID: f.account, CouncilID: c.ID, Role: "architect", Runtime: "codex", Stage: domain.StageProposal, Round: 1, JobID: j.ID, Content: json.RawMessage(`{}`)}
	must(t, repo.AddContribution(ctx, &p))
	same := p
	same.ID, same.Runtime = uuid.Nil(), "claude-code"
	wantErr(t, repo.AddContribution(ctx, &same), store.ErrConflict) // same job
	same.JobID, same.Runtime = uuid.Nil(), "codex"
	wantErr(t, repo.AddContribution(ctx, &same), store.ErrConflict) // same member, stage and round
	same.Stage = domain.StageCritique
	must(t, repo.AddContribution(ctx, &same))

	must(t, repo.SetEvidenceCheck(ctx, f.account, p.ID, nil))
	all, err := repo.Contributions(ctx, f.account, c.ID, "")
	if err != nil || len(all) != 2 || all[0].EvidenceCheck == nil || len(all[0].EvidenceCheck) != 0 || all[1].EvidenceCheck != nil {
		t.Fatalf("Contributions = %+v, %v", all, err)
	}
	if got, _ := repo.Contributions(ctx, f.account, c.ID, domain.StageCritique); len(got) != 1 || got[0].ID != same.ID {
		t.Fatalf("critiques = %+v", got)
	}
	wantErr(t, repo.SetEvidenceCheck(ctx, f.account, domain.NewID(), nil), store.ErrNotFound)
}

func TestIdentityStoreSuite(t *testing.T) {
	st := NewStore(nil)
	account := domain.Account{Name: "Acme"}
	if err := st.Accounts().Create(t.Context(), &account); err != nil {
		t.Fatal(err)
	}
	IdentityStoreSuite(t, st, account.ID)
}

func TestStoreVerificationReports(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.VerificationReports()
	change := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/app", Number: 3, HeadSHA: "abc123", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &change))
	art := domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, Name: "verification/abc123.md", ContentType: "text/markdown", StorageKey: "k1"}
	must(t, f.st.Artifacts().Put(ctx, &art))

	r := domain.VerificationReport{AccountID: f.account, ProjectID: f.project.ID, ChangeID: change.ID, SHA: "abc123",
		Status: domain.VerificationIncomplete, Report: []byte(`{"version":1}`), ArtifactID: art.ID}
	must(t, repo.Upsert(ctx, &r))
	if r.ID == uuid.Nil() || r.CreatedAt.IsZero() || !r.UpdatedAt.Equal(r.CreatedAt) {
		t.Fatalf("Upsert did not fill %+v", r)
	}
	f.clock.Advance(time.Minute)
	again := domain.VerificationReport{AccountID: f.account, ProjectID: f.project.ID, ChangeID: change.ID, SHA: "abc123",
		Status: domain.VerificationPassed, Report: []byte(`{"version":1,"x":1}`)}
	must(t, repo.Upsert(ctx, &again))
	if again.ID != r.ID || !again.CreatedAt.Equal(r.CreatedAt) || !again.UpdatedAt.After(r.UpdatedAt) {
		t.Fatalf("rebuild = %+v, first %+v", again, r)
	}
	got, err := repo.Get(ctx, f.account, r.ID)
	if err != nil || got.Status != domain.VerificationPassed || string(got.Report) != `{"version":1,"x":1}` || got.ArtifactID != uuid.Nil() {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	other := domain.VerificationReport{AccountID: f.account, ProjectID: f.project.ID, ChangeID: change.ID, SHA: "def456",
		Status: domain.VerificationFailed, Report: []byte(`{}`), ArtifactID: art.ID}
	f.clock.Advance(time.Minute)
	must(t, repo.Upsert(ctx, &other))
	if got, err := repo.GetBySHA(ctx, f.account, change.ID, "abc123"); err != nil || got.ID != r.ID {
		t.Fatalf("GetBySHA = %+v, %v", got, err)
	}
	if got, err := repo.Latest(ctx, f.account, change.ID); err != nil || got.ID != other.ID {
		t.Fatalf("Latest = %+v, %v", got, err)
	}
	_, err = repo.GetBySHA(ctx, f.account, change.ID, "zzz")
	wantErr(t, err, store.ErrNotFound)
	_, err = repo.Latest(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
	_, err = repo.Get(ctx, domain.NewID(), r.ID)
	wantErr(t, err, store.ErrNotFound)

	// Deleting the artifact keeps the report and drops the link.
	must(t, f.st.Artifacts().Delete(ctx, f.account, art.ID))
	if got, err := repo.Get(ctx, f.account, other.ID); err != nil || got.ArtifactID != uuid.Nil() || got.Status != domain.VerificationFailed {
		t.Fatalf("after delete = %+v, %v", got, err)
	}
}

// A task search lists the matching tasks, best match first, within the limit.
func TestStoreTaskTextSearch(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := domain.NewID()
	for i, c := range []struct{ title, body string }{
		{"Add token store", "Hash reset tokens in Postgres."},
		{"Rename colours", "Cosmetic change."},
		{"Token store tests", "Test the token store."},
	} {
		tk := domain.Task{AccountID: f.account, ProjectID: f.project.ID, GoalID: g, Seq: i + 1, Title: c.title, Body: c.body, Status: domain.TaskOpen}
		must(t, f.st.Tasks().Create(ctx, &tk))
		if i != 0 {
			must(t, f.st.Tasks().UpdateStatus(ctx, f.account, tk.ID, domain.TaskOpen, domain.TaskInProgress))
		}
	}
	titles := func(q store.TaskFilter) []string {
		q.ProjectID = f.project.ID
		list, err := f.st.Tasks().List(ctx, f.account, q)
		must(t, err)
		var out []string
		for _, tk := range list {
			out = append(out, tk.Title)
		}
		return out
	}
	if got := titles(store.TaskFilter{Text: "token or store"}); fmt.Sprint(got) != "[Token store tests Add token store]" {
		t.Errorf("search = %v, want the better match first", got)
	}
	if got := titles(store.TaskFilter{Text: "token", Status: domain.TaskInProgress}); fmt.Sprint(got) != "[Token store tests]" {
		t.Errorf("search by status = %v", got)
	}
	if got := titles(store.TaskFilter{Text: "token store", Limit: 1}); len(got) != 1 {
		t.Errorf("limited search = %v", got)
	}
	if got := titles(store.TaskFilter{Text: "nothingmatches"}); len(got) != 0 {
		t.Errorf("search = %v", got)
	}
	if got := titles(store.TaskFilter{Text: "  "}); len(got) != 3 {
		t.Errorf("a blank text lists every task: %v", got)
	}
}

// fakeAccessSeed seeds the fake.
type fakeAccessSeed struct{ st *Store }

func (s fakeAccessSeed) Member(_ *testing.T, accountID uuid.UUID) uuid.UUID {
	return s.st.AddMember(accountID, domain.User{Name: "someone"}, "").ID
}

func (s fakeAccessSeed) SetStatus(_ *testing.T, accountID, userID uuid.UUID, status domain.MembershipStatus) {
	s.st.SetMemberStatus(accountID, userID, status)
}

func (s fakeAccessSeed) Link(_ *testing.T, _, userID uuid.UUID, id int64, login string) {
	s.st.LinkGitHub(userID, id, login)
}

func TestAccessStore(t *testing.T) {
	f := newFixture(t)
	AccessStoreSuite(t, f.st, fakeAccessSeed{f.st}, f.account, f.project.ID)
}
