//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func TestGates(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Gates()
	g := f.newGoal(t)
	gate := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateSpecApproval, Status: domain.GateApproved,
		Title: "Review spec", Summary: "md", SubjectRepo: "Acme/Forecrew", SubjectNumber: 3, GoalID: g.ID}
	must(t, repo.Create(ctx, &gate))
	second := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "Stuck", SubjectRepo: "acme/other"}
	must(t, repo.Create(ctx, &second))
	if gate.Status != domain.GatePending || gate.Number != 1 || second.Number != 2 || gate.Ref() != "GATE-1" {
		t.Fatalf("gates = %+v %+v", gate, second)
	}

	got, err := repo.Get(ctx, f.account, gate.ID)
	if err != nil || got.GoalID != g.ID || got.SubjectNumber != 3 || !got.ResolvedAt.IsZero() || got.ResolvedBy != "" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if got, err := repo.GetByNumber(ctx, f.account, 2); err != nil || got.ID != second.ID || got.SubjectNumber != 0 {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	_, err = repo.GetByNumber(ctx, f.account, 3)
	wantErr(t, err, store.ErrNotFound)
	pending, err := repo.ListPendingBySubject(ctx, f.account, "acme/FORECREW", 3)
	if err != nil || len(pending) != 1 || pending[0].ID != gate.ID {
		t.Fatalf("pending by subject = %+v, %v", pending, err)
	}

	list, _ := repo.List(ctx, f.account, store.GateFilter{Kind: domain.GateNeedsYou, Status: domain.GatePending, ProjectID: f.project.ID})
	if len(list) != 1 || list[0].ID != second.ID {
		t.Fatalf("list = %+v", list)
	}

	// Resolve through the domain method, then store.
	actor := domain.Actor{Kind: domain.ActorUser, Name: "gian"}
	must(t, got.Resolve(actor, domain.GateApproved, "", time.Now().UTC()))
	stale := got
	got, _ = repo.Get(ctx, f.account, gate.ID)
	must(t, got.Resolve(actor, domain.GateChangesRequested, "tighten it", time.Now().UTC()))
	must(t, repo.Resolve(ctx, &got))
	again, _ := repo.Get(ctx, f.account, gate.ID)
	if again.Status != domain.GateChangesRequested || again.ResolvedBy != "user:gian" || again.Feedback != "tighten it" || again.ResolvedAt.IsZero() {
		t.Fatalf("resolved = %+v", again)
	}
	// Without a ResolvedAt the database clock is used and written back.
	untimed := second
	untimed.Status, untimed.ResolvedBy = domain.GateApproved, "user:gian"
	must(t, repo.Resolve(ctx, &untimed))
	if untimed.ResolvedAt.IsZero() || time.Since(untimed.ResolvedAt) > time.Minute {
		t.Fatalf("resolved at = %v", untimed.ResolvedAt)
	}
	if stored, _ := repo.Get(ctx, f.account, second.ID); !stored.ResolvedAt.Equal(untimed.ResolvedAt) {
		t.Fatalf("stored resolved at = %v", stored.ResolvedAt)
	}
	wantErr(t, repo.Resolve(ctx, &stale), store.ErrConflict)
	pendingGate := again
	pendingGate.Status = domain.GatePending
	wantErr(t, repo.Resolve(ctx, &pendingGate), domain.ErrInvalidTransition)
	ghost := again
	ghost.ID = domain.NewID()
	wantErr(t, repo.Resolve(ctx, &ghost), store.ErrNotFound)
	if pending, _ := repo.ListPendingBySubject(ctx, f.account, "acme/forecrew", 3); len(pending) != 0 {
		t.Fatalf("resolved gate still pending: %+v", pending)
	}
}

func TestSignals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Signals()
	jobID := domain.NewID()
	mk := func() domain.Signal {
		s, err := domain.NewSignal(f.account, f.project.ID, domain.JobCompleted{JobID: jobID, Status: domain.JobSucceeded}, time.Now())
		must(t, err)
		return s
	}
	first := mk()
	inserted, err := repo.Insert(ctx, &first)
	if err != nil || !inserted {
		t.Fatalf("insert = %v, %v", inserted, err)
	}
	dup := mk()
	inserted, err = repo.Insert(ctx, &dup)
	if err != nil || inserted || dup.ID != first.ID {
		t.Fatalf("duplicate = %v, %v, id %s vs %s", inserted, err, dup.ID, first.ID)
	}
	var n int
	must(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM signals`).Scan(&n))
	if n != 1 {
		t.Fatalf("%d signals stored", n)
	}

	got, err := repo.ListByKey(ctx, f.account, domain.JobKey(jobID))
	if err != nil || len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("by key = %+v, %v", got, err)
	}
	if p, err := got[0].Decode(); err != nil || p.SignalKey() != domain.JobKey(jobID) {
		t.Fatalf("payload = %v, %v", p, err)
	}
	if none, err := repo.ListByKey(ctx, f.account, "job:other"); err != nil || len(none) != 0 {
		t.Fatalf("other key = %+v, %v", none, err)
	}

	// The same dedupe key in another account is a different signal.
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	otherProject := f.newProject(t, other.ID, "forecrew")
	foreign := mk()
	foreign.AccountID, foreign.ProjectID = other.ID, otherProject.ID
	if inserted, err := repo.Insert(ctx, &foreign); err != nil || !inserted {
		t.Fatalf("foreign insert = %v, %v", inserted, err)
	}
	bad := mk()
	bad.DedupeKey, bad.ProjectID = "x", domain.NewID()
	_, err = repo.Insert(ctx, &bad)
	wantErr(t, err, store.ErrNotFound)
}

func TestWorkers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Workers()
	hash := sha256.Sum256([]byte("fcw_secret"))
	w := domain.Worker{AccountID: f.account, Name: "laptop", TokenHash: hash[:], Labels: []string{"Linux", "runtime:claude-code", "linux"}, Version: "1.0"}
	must(t, repo.Create(ctx, &w))
	if w.MaxJobs != 1 || len(w.Labels) != 2 || w.Labels[0] != "linux" {
		t.Fatalf("created = %+v", w)
	}
	dup := domain.Worker{AccountID: f.account, Name: "laptop", TokenHash: []byte("other")}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)
	dup = domain.Worker{AccountID: f.account, Name: "desktop", TokenHash: hash[:]}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)

	got, err := repo.GetByTokenHash(ctx, hash[:])
	if err != nil || got.ID != w.ID || !got.LastSeenAt.IsZero() || got.Revoked() {
		t.Fatalf("by hash = %+v, %v", got, err)
	}
	_, err = repo.GetByTokenHash(ctx, []byte("nope"))
	wantErr(t, err, store.ErrNotFound)

	must(t, repo.Seen(ctx, f.account, w.ID, []string{"DOCKER", "linux"}, "1.1"))
	got, _ = repo.Get(ctx, f.account, w.ID)
	if got.LastSeenAt.IsZero() || got.Version != "1.1" || len(got.Labels) != 2 || got.Labels[0] != "docker" || !got.Online(time.Now()) {
		t.Fatalf("seen = %+v", got)
	}
	wantErr(t, repo.Seen(ctx, f.account, domain.NewID(), nil, ""), store.ErrNotFound)

	must(t, repo.Revoke(ctx, f.account, w.ID))
	first, _ := repo.Get(ctx, f.account, w.ID)
	must(t, repo.Revoke(ctx, f.account, w.ID))
	second, _ := repo.Get(ctx, f.account, w.ID)
	if !first.Revoked() || !second.RevokedAt.Equal(first.RevokedAt) || second.Online(time.Now()) {
		t.Fatalf("revoke = %+v then %+v", first, second)
	}
	wantErr(t, repo.Revoke(ctx, f.account, domain.NewID()), store.ErrNotFound)
	if list, err := repo.List(ctx, f.account); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	_, err = repo.Get(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func TestWorkflowRunsAndSteps(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	runs, steps := f.st.WorkflowRuns(), f.st.StepRuns()
	g := f.newGoal(t)
	run := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID, Playbook: "goal-to-spec", GoalID: g.ID}
	must(t, runs.Create(ctx, &run))
	done := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID, Playbook: "x", Status: domain.RunSucceeded, Inputs: []byte(`{"a": 1}`)}
	must(t, runs.Create(ctx, &done))
	if run.Status != domain.RunRunning {
		t.Fatalf("status = %q", run.Status)
	}
	got, err := runs.Get(ctx, f.account, done.ID)
	if err != nil || string(got.Inputs) != `{"a": 1}` || string(got.State) != `{}` || got.GoalID != uuid.Nil() {
		t.Fatalf("get = %+v, %v", got, err)
	}
	active, err := runs.ListActive(ctx, f.account)
	if err != nil || len(active) != 1 || active[0].ID != run.ID || active[0].GoalID != g.ID {
		t.Fatalf("active = %+v, %v", active, err)
	}
	run.Status, run.State, run.Error, run.FinishedAt = domain.RunFailed, []byte(`{"step": 2}`), "boom", time.Now()
	must(t, runs.Update(ctx, &run))
	got, _ = runs.Get(ctx, f.account, run.ID)
	if got.Status != domain.RunFailed || string(got.State) != `{"step": 2}` || got.Error != "boom" || got.FinishedAt.IsZero() {
		t.Fatalf("updated = %+v", got)
	}
	if active, _ := runs.ListActive(ctx, f.account); len(active) != 0 {
		t.Fatalf("active = %+v", active)
	}
	ghost := domain.WorkflowRun{AccountID: f.account, ID: domain.NewID()}
	wantErr(t, runs.Update(ctx, &ghost), store.ErrNotFound)
	_, err = runs.Get(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)

	step := domain.StepRun{AccountID: f.account, RunID: run.ID, StepID: "spec", Status: domain.StepWaiting, WaitKey: "job:1"}
	must(t, steps.Create(ctx, &step))
	plain := domain.StepRun{AccountID: f.account, RunID: run.ID, StepID: "plan"}
	must(t, steps.Create(ctx, &plain))
	if step.Attempt != 1 || plain.Status != domain.StepPending {
		t.Fatalf("defaults = %+v %+v", step, plain)
	}
	dup := domain.StepRun{AccountID: f.account, RunID: run.ID, StepID: "spec"}
	wantErr(t, steps.Create(ctx, &dup), store.ErrConflict)

	waiting, err := steps.ListWaiting(ctx, f.account, "job:1")
	if err != nil || len(waiting) != 1 || waiting[0].ID != step.ID || waiting[0].JobID != uuid.Nil() {
		t.Fatalf("waiting = %+v, %v", waiting, err)
	}
	job := f.newJob(t)
	step.Status, step.WaitKey, step.JobID, step.Output, step.StartedAt, step.FinishedAt = domain.StepSucceeded, "", job.ID, []byte(`{"ok": 1}`), time.Now(), time.Now()
	must(t, steps.Update(ctx, &step))
	got2, err := steps.Get(ctx, f.account, step.ID)
	if err != nil || got2.Status != domain.StepSucceeded || got2.JobID != job.ID || got2.WaitKey != "" || string(got2.Output) != `{"ok": 1}` || got2.StartedAt.IsZero() {
		t.Fatalf("step = %+v, %v", got2, err)
	}
	list, err := steps.ListByRun(ctx, f.account, run.ID)
	if err != nil || len(list) != 2 || list[0].ID != step.ID {
		t.Fatalf("by run = %+v, %v", list, err)
	}
	wantErr(t, steps.Update(ctx, &domain.StepRun{AccountID: f.account, ID: domain.NewID()}), store.ErrNotFound)
	_, err = steps.Get(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func TestWebhookDeliveries(t *testing.T) {
	f := newFixture(t)
	repo := f.st.WebhookDeliveries()
	d := domain.WebhookDelivery{DeliveryID: "abc-123", Event: "pull_request"}
	for i, want := range []bool{true, false, false} {
		got, err := repo.Record(t.Context(), d)
		if err != nil || got != want {
			t.Fatalf("record %d = %v, %v; want %v", i, got, err, want)
		}
	}
	if got, err := repo.Record(t.Context(), domain.WebhookDelivery{DeliveryID: "other", Event: "push"}); err != nil || !got {
		t.Fatalf("other delivery = %v, %v", got, err)
	}
}

func TestMCPTokens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.MCPTokens()
	tok := domain.MCPToken{AccountID: f.account, Name: "claude", TokenHash: []byte("h1")}
	must(t, repo.Create(ctx, &tok))
	dup := domain.MCPToken{AccountID: f.account, Name: "claude", TokenHash: []byte("h2")}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)

	got, err := repo.GetByTokenHash(ctx, []byte("h1"))
	if err != nil || got.ID != tok.ID || !got.LastUsedAt.IsZero() {
		t.Fatalf("by hash = %+v, %v", got, err)
	}
	_, err = repo.GetByTokenHash(ctx, []byte("zz"))
	wantErr(t, err, store.ErrNotFound)
	must(t, repo.Touch(ctx, f.account, tok.ID))
	must(t, repo.Revoke(ctx, f.account, tok.ID))
	must(t, repo.Revoke(ctx, f.account, tok.ID))
	list, err := repo.List(ctx, f.account)
	if err != nil || len(list) != 1 || list[0].LastUsedAt.IsZero() || list[0].RevokedAt.IsZero() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	wantErr(t, repo.Touch(ctx, f.account, domain.NewID()), store.ErrNotFound)
	wantErr(t, repo.Revoke(ctx, f.account, domain.NewID()), store.ErrNotFound)
}

func TestAuditIsAppendOnly(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Audit()
	e1 := domain.AuditEntry{AccountID: f.account, ProjectID: f.project.ID, Actor: "user:gian", Action: "gate.resolved", SubjectType: "gate", SubjectID: domain.NewID(), Data: []byte(`{"x": 1}`)}
	e2 := domain.AuditEntry{AccountID: f.account, Actor: "system:reaper", Action: "job.requeued", SubjectType: "job"}
	must(t, repo.Append(ctx, &e1))
	must(t, repo.Append(ctx, &e2))
	if e1.CreatedAt.IsZero() {
		t.Fatal("created_at not set")
	}
	list, err := repo.List(ctx, f.account, store.AuditFilter{})
	if err != nil || len(list) != 2 || list[0].ID != e2.ID || string(list[1].Data) != `{"x": 1}` || list[0].ProjectID != uuid.Nil() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	list, _ = repo.List(ctx, f.account, store.AuditFilter{SubjectID: e1.SubjectID, ProjectID: f.project.ID, Limit: 5})
	if len(list) != 1 || list[0].ID != e1.ID {
		t.Fatalf("filtered = %+v", list)
	}
	for name, flt := range map[string]store.AuditFilter{
		"actor": {Actor: "user:gian"}, "action": {Action: "gate.resolved"}, "type": {SubjectType: "gate"}, "until": {Until: e2.CreatedAt},
	} {
		if list, err = repo.List(ctx, f.account, flt); err != nil || len(list) != 1 || list[0].ID != e1.ID {
			t.Fatalf("filter %s = %+v, %v", name, list, err)
		}
	}
	if list, _ = repo.List(ctx, f.account, store.AuditFilter{Since: e2.CreatedAt}); len(list) != 1 || list[0].ID != e2.ID {
		t.Fatalf("since the second entry: %+v", list)
	}
	orphan := domain.AuditEntry{AccountID: domain.NewID(), Actor: "a", Action: "b", SubjectType: "c"}
	wantErr(t, repo.Append(ctx, &orphan), store.ErrNotFound)

	for _, q := range []string{
		`UPDATE audit_log SET actor = 'evil'`,
		`UPDATE audit_log SET actor = 'evil' WHERE false`,
		`DELETE FROM audit_log`,
		`DELETE FROM audit_log WHERE false`,
		`TRUNCATE audit_log`,
	} {
		_, err := f.pool.Exec(ctx, q)
		if pg, ok := err.(*pgconn.PgError); !ok || pg.Message != "audit_log is append-only" {
			t.Errorf("%s: error = %v", q, err)
		}
	}
	list, _ = repo.List(ctx, f.account, store.AuditFilter{})
	if len(list) != 2 {
		t.Fatalf("%d entries left", len(list))
	}
}

func TestAccountIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t)
	s := f.newSpec(t, g, 1)
	task := f.newTask(t, g, s, 1)
	job := f.newJob(t)
	gate := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "t", SubjectRepo: "acme/forecrew", SubjectNumber: 1}
	must(t, f.st.Gates().Create(ctx, &gate))
	w := f.newWorker(t, "w")
	run := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID, Playbook: "p"}
	must(t, f.st.WorkflowRuns().Create(ctx, &run))
	step := domain.StepRun{AccountID: f.account, RunID: run.ID, StepID: "s", Status: domain.StepWaiting, WaitKey: "job:x"}
	must(t, f.st.StepRuns().Create(ctx, &step))
	_, err := f.st.JobEvents().Append(ctx, f.account, job.ID, []domain.JobEvent{{Type: domain.EventLog, Text: "hi"}})
	must(t, err)
	ch := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/forecrew", Number: 1, Branch: "b", BaseRef: "main", HeadSHA: "s", State: domain.ChangeOpen, URL: "u"}
	must(t, f.st.Changes().Upsert(ctx, &ch))
	sig, err := domain.NewSignal(f.account, f.project.ID, domain.JobCompleted{JobID: job.ID, Status: domain.JobFailed}, time.Now())
	must(t, err)
	_, err = f.st.Signals().Insert(ctx, &sig)
	must(t, err)
	tok := domain.MCPToken{AccountID: f.account, Name: "t", TokenHash: []byte("x")}
	must(t, f.st.MCPTokens().Create(ctx, &tok))
	entry := domain.AuditEntry{AccountID: f.account, Actor: "a", Action: "b", SubjectType: "c"}
	must(t, f.st.Audit().Append(ctx, &entry))

	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	o := other.ID
	st := f.st
	notFound := map[string]error{}
	check := func(name string, err error) { notFound[name] = err }
	_, err = st.Projects().Get(ctx, o, f.project.ID)
	check("project", err)
	_, err = st.Projects().GetByName(ctx, o, "forecrew")
	check("project by name", err)
	_, err = st.Goals().Get(ctx, o, g.ID)
	check("goal", err)
	_, err = st.Goals().GetByNumber(ctx, o, g.Number)
	check("goal number", err)
	_, err = st.Specs().Get(ctx, o, s.ID)
	check("spec", err)
	_, err = st.Specs().Latest(ctx, o, g.ID)
	check("latest spec", err)
	_, err = st.Tasks().Get(ctx, o, task.ID)
	check("task", err)
	_, err = st.Tasks().GetByNumber(ctx, o, task.Number)
	check("task number", err)
	_, err = st.Changes().Get(ctx, o, ch.ID)
	check("change", err)
	_, err = st.Changes().GetByPR(ctx, o, "acme/forecrew", 1)
	check("change by PR", err)
	_, err = st.Jobs().Get(ctx, o, job.ID)
	check("job", err)
	_, err = st.Jobs().GetByNumber(ctx, o, job.Number)
	check("job number", err)
	_, err = st.Gates().Get(ctx, o, gate.ID)
	check("gate", err)
	_, err = st.Gates().GetByNumber(ctx, o, gate.Number)
	check("gate number", err)
	_, err = st.Workers().Get(ctx, o, w.ID)
	check("worker", err)
	_, err = st.WorkflowRuns().Get(ctx, o, run.ID)
	check("run", err)
	check("project status", st.Projects().UpdateStatus(ctx, o, f.project.ID, domain.ProjectRegistered, domain.ProjectOnboarding))
	check("goal status", st.Goals().UpdateStatus(ctx, o, g.ID, domain.GoalDraft, domain.GoalSpecReview))
	check("worker revoke", st.Workers().Revoke(ctx, o, w.ID))
	check("token revoke", st.MCPTokens().Revoke(ctx, o, tok.ID))
	_, err = st.Jobs().Cancel(ctx, o, job.ID)
	check("job cancel", err)
	_, err = st.Jobs().ExtendLease(ctx, o, job.ID, w.ID, time.Minute)
	check("job extend", err)
	_, err = st.JobEvents().Append(ctx, o, job.ID, []domain.JobEvent{{Type: domain.EventLog}})
	check("job events", err)
	_, err = st.StepRuns().Get(ctx, o, step.ID)
	check("step run", err)
	for name, err := range notFound {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s of another account: error = %v, want not found", name, err)
		}
	}

	empty := func(name string, n int, err error) {
		t.Helper()
		if err != nil || n != 0 {
			t.Errorf("%s of another account: %d rows, %v", name, n, err)
		}
	}
	projects, err := st.Projects().List(ctx, o)
	empty("projects", len(projects), err)
	goals, err := st.Goals().List(ctx, o, store.GoalFilter{})
	empty("goals", len(goals), err)
	specs, err := st.Specs().ListByGoal(ctx, o, g.ID)
	empty("specs", len(specs), err)
	tasks, err := st.Tasks().List(ctx, o, store.TaskFilter{})
	empty("tasks", len(tasks), err)
	changes, err := st.Changes().List(ctx, o, store.ChangeFilter{})
	empty("changes", len(changes), err)
	jobs, err := st.Jobs().List(ctx, o, store.JobFilter{})
	empty("jobs", len(jobs), err)
	gates, err := st.Gates().List(ctx, o, store.GateFilter{})
	empty("gates", len(gates), err)
	pending, err := st.Gates().ListPendingBySubject(ctx, o, "acme/forecrew", 1)
	empty("pending gates", len(pending), err)
	signals, err := st.Signals().ListByKey(ctx, o, sig.Key)
	empty("signals", len(signals), err)
	workers, err := st.Workers().List(ctx, o)
	empty("workers", len(workers), err)
	runs, err := st.WorkflowRuns().ListActive(ctx, o)
	empty("runs", len(runs), err)
	events, err := st.JobEvents().List(ctx, o, job.ID, 0, 0)
	empty("job events", len(events), err)
	steps, err := st.StepRuns().ListByRun(ctx, o, run.ID)
	empty("steps", len(steps), err)
	waiting, err := st.StepRuns().ListWaiting(ctx, o, "job:x")
	empty("waiting steps", len(waiting), err)
	tokens, err := st.MCPTokens().List(ctx, o)
	empty("tokens", len(tokens), err)
	entries, err := st.Audit().List(ctx, o, store.AuditFilter{})
	empty("audit", len(entries), err)
	_, err = st.Jobs().LeaseNext(ctx, o, w.ID, []string{"runtime:claude-code"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	expired, err := st.Jobs().RequeueExpired(ctx, o)
	empty("expired", len(expired), err)

	// Nothing of the first account changed.
	if cur, _ := st.Jobs().Get(ctx, f.account, job.ID); cur.Status != domain.JobQueued {
		t.Fatalf("job = %+v", cur)
	}
}

func TestRefsAreGaplessUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	otherProject := f.newProject(t, other.ID, "forecrew")

	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			createNumbered(t, f)
			if i == 0 {
				og := domain.Goal{AccountID: other.ID, ProjectID: otherProject.ID, Title: "o", CreatedBy: "user:me"}
				if err := f.st.Goals().Create(ctx, &og); err != nil || og.Number != 1 {
					t.Errorf("other account goal = %+v, %v", og, err)
				}
			}
		})
	}
	wg.Wait()
	for _, table := range []string{"goals", "jobs"} {
		var count, maxNum int
		must(t, f.pool.QueryRow(ctx, `SELECT count(*), max(number) FROM `+table+` WHERE account_id = $1`, f.account).Scan(&count, &maxNum))
		if count != n || maxNum != n {
			t.Errorf("%s: count %d, max number %d, want %d", table, count, maxNum, n)
		}
	}
}

func TestWithTx(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	boom := store.ErrConflict

	err := f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "rolled back", CreatedBy: "user:me"}
		if err := tx.Goals().Create(ctx, &g); err != nil {
			return err
		}
		if _, err := tx.Goals().Get(ctx, f.account, g.ID); err != nil {
			return err
		}
		return boom
	})
	wantErr(t, err, boom)
	if goals, _ := f.st.Goals().List(ctx, f.account, store.GoalFilter{}); len(goals) != 0 {
		t.Fatalf("rolled back goal is visible: %+v", goals)
	}

	var first, second domain.Goal
	err = f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		first = domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "a", CreatedBy: "user:me"}
		if err := tx.Goals().Create(ctx, &first); err != nil {
			return err
		}
		// A nested failing transaction rolls back to its savepoint only.
		_ = tx.WithTx(ctx, func(ctx context.Context, inner store.Store) error {
			bad := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "inner", CreatedBy: "user:me"}
			if err := inner.Goals().Create(ctx, &bad); err != nil {
				return err
			}
			return boom
		})
		second = domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "b", CreatedBy: "user:me"}
		return tx.Goals().Create(ctx, &second)
	})
	must(t, err)
	goals, _ := f.st.Goals().List(ctx, f.account, store.GoalFilter{})
	if len(goals) != 2 || goals[0].ID != first.ID || goals[1].ID != second.ID || second.Number != first.Number+1 {
		t.Fatalf("goals = %+v", goals)
	}
}

// createNumbered creates a goal and a job, and a failing goal that must not
// burn a number.
func createNumbered(t *testing.T, f *fixture) {
	ctx := t.Context()
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "g", CreatedBy: "user:me"}
	if err := f.st.Goals().Create(ctx, &g); err != nil {
		t.Error(err)
	}
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "dev", Runtime: "codex"}
	if err := f.st.Jobs().Create(ctx, &j); err != nil {
		t.Error(err)
	}
	bad := domain.Goal{AccountID: f.account, ProjectID: domain.NewID(), Title: "bad"}
	if err := f.st.Goals().Create(ctx, &bad); err == nil {
		t.Error("goal with unknown project created")
	}
}

// TestWritesRejectForeignRows checks that an account cannot attach rows to
// another account's goals, jobs and so on.
func TestWritesRejectForeignRows(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t)
	s := f.newSpec(t, g, 1)
	task := f.newTask(t, g, s, 1)
	job := f.newJob(t)
	gate := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "t", SubjectRepo: "a/b"}
	must(t, f.st.Gates().Create(ctx, &gate))
	run := domain.WorkflowRun{AccountID: f.account, ProjectID: f.project.ID, Playbook: "p"}
	must(t, f.st.WorkflowRuns().Create(ctx, &run))
	sig, err := domain.NewSignal(f.account, f.project.ID, domain.JobCompleted{JobID: job.ID, Status: domain.JobFailed}, time.Now())
	must(t, err)
	_, err = f.st.Signals().Insert(ctx, &sig)
	must(t, err)

	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	o, op := other.ID, f.newProject(t, other.ID, "mine")
	st := f.st

	// Every case uses the other account with at least one of the first account's rows.
	var errs = map[string]error{}
	add := func(name string, err error) { errs[name] = err }
	add("goal", st.Goals().Create(ctx, &domain.Goal{AccountID: o, ProjectID: f.project.ID, Title: "x"}))
	add("spec on foreign goal", st.Specs().Create(ctx, &domain.Spec{AccountID: o, ProjectID: op.ID, GoalID: g.ID, Revision: 1, Body: "x"}))
	add("task", st.Tasks().Create(ctx, &domain.Task{AccountID: o, ProjectID: op.ID, GoalID: g.ID, SpecID: s.ID, Title: "x"}))
	add("change", st.Changes().Upsert(ctx, &domain.Change{AccountID: o, ProjectID: op.ID, TaskID: task.ID, Repo: "a/b", Number: 1, State: domain.ChangeOpen}))
	add("job project", st.Jobs().Create(ctx, &domain.Job{AccountID: o, ProjectID: f.project.ID, Role: "x", Runtime: "codex"}))
	add("job goal", st.Jobs().Create(ctx, &domain.Job{AccountID: o, ProjectID: op.ID, GoalID: g.ID, Role: "x", Runtime: "codex"}))
	add("job spec", st.Jobs().Create(ctx, &domain.Job{AccountID: o, ProjectID: op.ID, SpecID: s.ID, Role: "x", Runtime: "codex"}))
	add("job task", st.Jobs().Create(ctx, &domain.Job{AccountID: o, ProjectID: op.ID, TaskID: task.ID, Role: "x", Runtime: "codex"}))
	add("gate", st.Gates().Create(ctx, &domain.Gate{AccountID: o, ProjectID: op.ID, JobID: job.ID, Kind: domain.GateNeedsYou, Title: "x", SubjectRepo: "a/b"}))
	foreignSig := domain.Signal{AccountID: o, ProjectID: f.project.ID, Kind: domain.SignalJobCompleted, Key: "k", DedupeKey: "d", Payload: []byte(`{}`)}
	_, err = st.Signals().Insert(ctx, &foreignSig)
	add("signal", err)
	add("run", st.WorkflowRuns().Create(ctx, &domain.WorkflowRun{AccountID: o, ProjectID: op.ID, Playbook: "p", GoalID: g.ID}))
	add("step run", st.StepRuns().Create(ctx, &domain.StepRun{AccountID: o, RunID: run.ID, StepID: "s"}))
	add("audit", st.Audit().Append(ctx, &domain.AuditEntry{AccountID: o, ProjectID: f.project.ID, Actor: "a", Action: "b", SubjectType: "c"}))

	ownRun := domain.WorkflowRun{AccountID: o, ProjectID: op.ID, Playbook: "p"}
	must(t, st.WorkflowRuns().Create(ctx, &ownRun))
	step := domain.StepRun{AccountID: o, RunID: ownRun.ID, StepID: "s"}
	must(t, st.StepRuns().Create(ctx, &step))
	step.JobID = job.ID
	add("step run update", st.StepRuns().Update(ctx, &step))

	for name, err := range errs {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: error = %v, want not found", name, err)
		}
	}
	var goals int
	must(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM goals WHERE account_id = $1`, o).Scan(&goals))
	if goals != 0 {
		t.Fatalf("%d goals written for the other account", goals)
	}

	// A worker of another account leases nothing.
	otherWorker := domain.Worker{AccountID: o, Name: "w", TokenHash: []byte("h")}
	must(t, st.Workers().Create(ctx, &otherWorker))
	_, err = st.Jobs().LeaseNext(ctx, f.account, otherWorker.ID, []string{"runtime:claude-code"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	if cur, _ := st.Jobs().Get(ctx, f.account, job.ID); cur.Status != domain.JobQueued {
		t.Fatalf("job = %+v", cur)
	}
}

func TestJobEventsAppendLeased(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	job := f.newJob(t)
	w := f.newWorker(t, "box")
	other := f.newWorker(t, "other")
	evs := []domain.JobEvent{{Type: domain.EventLog, Text: "hi"}}
	repo := f.st.JobEvents()

	_, err := repo.AppendLeased(ctx, f.account, job.ID, w.ID, evs)
	wantErr(t, err, store.ErrLeaseLost)
	_, err = f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	_, err = repo.AppendLeased(ctx, f.account, job.ID, other.ID, evs)
	wantErr(t, err, store.ErrLeaseLost)
	_, err = repo.AppendLeased(ctx, f.account, domain.NewID(), w.ID, evs)
	wantErr(t, err, store.ErrNotFound)
	got, err := repo.AppendLeased(ctx, f.account, job.ID, w.ID, evs)
	if err != nil || len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("AppendLeased = %+v, %v", got, err)
	}
	if rows, _ := repo.List(ctx, f.account, job.ID, 0, 10); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

// The lease ends in a transaction that holds the job lock: the events call
// waits for it, then sees the lease gone and writes nothing.
func TestJobEventsAppendLeasedWaitsForTheJobLock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	job := f.newJob(t)
	w := f.newWorker(t, "box")
	_, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)

	tx, err := f.pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'canceled', worker_id = NULL, lease_expires_at = NULL, finished_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.st.JobEvents().AppendLeased(ctx, f.account, job.ID, w.ID, []domain.JobEvent{{Type: domain.EventLog, Text: "late"}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("AppendLeased did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	must(t, tx.Commit(ctx))
	wantErr(t, <-done, store.ErrLeaseLost)
	if rows, _ := f.st.JobEvents().List(ctx, f.account, job.ID, 0, 10); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}
