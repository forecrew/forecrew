package testkit

import (
	"context"
	"encoding/json"
	"errors"
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
	g2 := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Logout", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(ctx, &g2))
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
