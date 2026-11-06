package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/scheduler"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// engine stores the runs it is asked to start, like workflow.Engine does:
// starting an ID that exists returns that run.
type engine struct {
	st    *testkit.Store
	mu    sync.Mutex
	calls []workflow.StartRequest
	fail  map[uuid.UUID]error // by project
}

func (e *engine) Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail[req.ProjectID]; err != nil {
		return domain.WorkflowRun{}, err
	}
	e.calls = append(e.calls, req)
	run := domain.WorkflowRun{ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Playbook: playbook, Status: domain.RunRunning}
	return run, e.st.WorkflowRuns().Create(ctx, &run)
}

func (e *engine) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

type env struct {
	st      *testkit.Store
	clock   *testkit.Clock
	engine  *engine
	account domain.Account
	project domain.Project
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// A Thursday.
	clock := testkit.NewClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	st := testkit.NewStore(clock.Now)
	e := &env{st: st, clock: clock, engine: &engine{st: st}, account: domain.Account{Name: "default"}}
	must(t, st.Accounts().Create(t.Context(), &e.account))
	e.project = e.newProject(t, e.account, "app", "acme/app", domain.ProjectOnboarded)
	return e
}

func (e *env) newProject(t *testing.T, a domain.Account, name, repo string, status domain.ProjectStatus) domain.Project {
	t.Helper()
	p := domain.Project{AccountID: a.ID, Name: name, Repo: repo, DefaultBranch: "main", Status: status}
	must(t, e.st.Projects().Create(t.Context(), &p))
	return p
}

// scheduler returns a scheduler, as forecrewd builds one at every start.
func (e *env) scheduler(t *testing.T, schedules ...config.Schedule) *scheduler.Scheduler {
	t.Helper()
	s, err := scheduler.New(schedules, e.st, e.engine)
	must(t, err)
	s.Now = e.clock.Now
	return s
}

var weekly = config.Schedule{Playbook: "curate", Project: "acme/app", Cron: "0 3 * * 1"} // Mondays at 03:00 UTC

func (e *env) tick(t *testing.T, s *scheduler.Scheduler, want int) {
	t.Helper()
	n, err := s.Tick(t.Context())
	if err != nil || n != want {
		t.Fatalf("Tick at %s = %d, %v; want %d", e.clock.Now().Format("Mon 2006-01-02 15:04"), n, err, want)
	}
}

func TestSlotsStartOnceEach(t *testing.T) {
	e := newEnv(t)
	s := e.scheduler(t, weekly)

	e.tick(t, s, 1) // the Monday before, never run: once
	e.tick(t, s, 0) // the same slot again
	first := e.engine.calls[0]
	if first.Inputs["repo"] != "acme/app" || first.ProjectID != e.project.ID || first.AccountID != e.account.ID {
		t.Errorf("start = %+v", first)
	}
	if want := scheduler.RunID("curate", e.project.ID, time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)); first.ID != want {
		t.Errorf("run id = %s, want %s", first.ID, want)
	}

	e.clock.Advance(4*24*time.Hour + 17*time.Hour) // Monday 2026-11-09 03:00
	e.tick(t, s, 1)
	e.clock.Advance(time.Hour)
	e.tick(t, s, 0)
}

// Slots missed while forecrewd is down are not run one by one: a new process
// starts the latest, once, and one that finds it started does nothing.
func TestMissedSlotsRunOnceAfterARestart(t *testing.T) {
	e := newEnv(t)
	e.tick(t, e.scheduler(t, weekly), 1)

	e.tick(t, e.scheduler(t, weekly), 0) // restart with no slot missed

	e.clock.Advance(21 * 24 * time.Hour) // three Mondays go by, Thursday 2026-11-26
	restarted := e.scheduler(t, weekly)
	e.tick(t, restarted, 1)
	e.tick(t, restarted, 0)
	e.tick(t, e.scheduler(t, weekly), 0) // and a second restart
	if e.engine.count() != 2 {
		t.Fatalf("%d runs, want 2", e.engine.count())
	}
	if want := scheduler.RunID("curate", e.project.ID, time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)); e.engine.calls[1].ID != want {
		t.Errorf("the run of the missed slots is %s, want that of 2026-11-23", e.engine.calls[1].ID)
	}
}

func TestEveryProjectOfTheRepoAndNoOthers(t *testing.T) {
	e := newEnv(t)
	other := domain.Account{Name: "other"}
	must(t, e.st.Accounts().Create(t.Context(), &other))
	e.newProject(t, other, "theirs", "Acme/App", domain.ProjectOnboarded) // the repo's case does not matter
	e.newProject(t, other, "fresh", "acme/app2", domain.ProjectOnboarded)
	registered := domain.Account{Name: "third"}
	must(t, e.st.Accounts().Create(t.Context(), &registered))
	e.newProject(t, registered, "new", "acme/app", domain.ProjectRegistered) // not onboarded: no profile to work with
	e.tick(t, e.scheduler(t, weekly, config.Schedule{Playbook: "curate", Project: "acme/none", Cron: "@daily"}), 2)
}

// A project that cannot be started does not keep the others from starting,
// and is tried again at the next tick.
func TestAFailedStartIsRetried(t *testing.T) {
	e := newEnv(t)
	other := domain.Account{Name: "other"}
	must(t, e.st.Accounts().Create(t.Context(), &other))
	theirs := e.newProject(t, other, "theirs", "acme/app", domain.ProjectOnboarded)
	e.engine.fail = map[uuid.UUID]error{e.project.ID: errors.New("database is down")}
	s := e.scheduler(t, weekly)

	n, err := s.Tick(t.Context())
	if n != 1 || err == nil {
		t.Fatalf("Tick = %d, %v; want 1 started and the error of the other", n, err)
	}
	if got := e.engine.calls[0].ProjectID; got != theirs.ID {
		t.Errorf("started project %s, want theirs", got)
	}
	e.engine.fail = nil
	e.tick(t, s, 1)
}

func TestNewRefusesABadCron(t *testing.T) {
	e := newEnv(t)
	if _, err := scheduler.New([]config.Schedule{{Playbook: "curate", Project: "acme/app", Cron: "soon"}}, e.st, e.engine); err == nil {
		t.Error("want an error")
	}
	if id := scheduler.RunID("a", e.project.ID, e.clock.Now()); id == scheduler.RunID("b", e.project.ID, e.clock.Now()) || id == scheduler.RunID("a", uuid.New(), e.clock.Now()) {
		t.Error("run ids of different schedules collide")
	}
}

func TestRunTicksAtOnceAndUntilDone(t *testing.T) {
	e := newEnv(t)
	s := e.scheduler(t, config.Schedule{Playbook: "curate", Project: "acme/app", Cron: "* * * * *"}) // every minute
	s.Every = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, func() bool { return e.engine.count() == 1 })
	e.clock.Advance(time.Minute)
	waitFor(t, func() bool { return e.engine.count() == 2 })
	cancel()
	<-done
}

func TestRunWithoutSchedulesReturns(t *testing.T) {
	e := newEnv(t)
	e.scheduler(t).Run(t.Context()) // would block for ever otherwise
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 400 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}
