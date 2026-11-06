package registers_test

import (
	"context"
	"encoding/json"
	"flag"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// world is a project with a goal, a spec, two tasks and a finished
// developer job, a register and a gate service that tells it of resolved gates.
type world struct {
	t       *testing.T
	clock   *testkit.Clock
	st      store.Store
	regs    *registers.Service
	gates   *gates.Keeper
	account uuid.UUID
	project domain.Project
	goal    domain.Goal
	spec    domain.Spec
	tasks   []domain.Task
}

func newWorld(t *testing.T) *world {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	ctx := t.Context()
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(ctx, &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &p))
	g := domain.Goal{AccountID: acc.ID, ProjectID: p.ID, Title: "Ship it", Status: domain.GoalInProgress, IssueNumber: 5, CreatedBy: "user:me"}
	must(t, st.Goals().Create(ctx, &g))
	sp := domain.Spec{AccountID: acc.ID, ProjectID: p.ID, GoalID: g.ID, Revision: 1, Body: "spec"}
	must(t, st.Specs().Create(ctx, &sp))
	w := &world{t: t, clock: clock, st: st, account: acc.ID, project: p, goal: g, spec: sp}
	bus := testkit.NewSignalBus(st)
	w.regs = registers.New(st, bus, clock.Now)
	w.gates = gates.New(st, bus, clock.Now).WithRecorder(w.regs)
	for i, issue := range []int{6, 7} {
		task := domain.Task{AccountID: acc.ID, ProjectID: p.ID, GoalID: g.ID, SpecID: sp.ID, Seq: i + 1, Title: "Task", Status: domain.TaskOpen, IssueNumber: issue}
		must(t, st.Tasks().Create(ctx, &task))
		w.tasks = append(w.tasks, task)
	}
	return w
}

func (w *world) actor() domain.Actor {
	return domain.Actor{AccountID: w.account, Kind: domain.ActorUser, Name: "gian"}
}

// job stores a succeeded developer job of the task with the given output.
func (w *world) job(task domain.Task, role string, output any) domain.Job {
	w.t.Helper()
	raw, err := json.Marshal(output)
	must(w.t, err)
	j := domain.Job{AccountID: w.account, ProjectID: w.project.ID, Role: role, Runtime: "claude-code", GoalID: w.goal.ID, SpecID: w.spec.ID,
		TaskID: task.ID, Status: domain.JobSucceeded, Output: raw, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(w.t, w.st.Jobs().Create(w.t.Context(), &j))
	j.Status, j.Output = domain.JobSucceeded, raw
	return j
}

// running stores a developer job of the task that is running under a worker,
// as a job is while it calls the job MCP tools.
func (w *world) running(task domain.Task, role string) domain.Job {
	w.t.Helper()
	j := domain.Job{AccountID: w.account, ProjectID: w.project.ID, Role: role, Runtime: "claude-code", GoalID: w.goal.ID, SpecID: w.spec.ID,
		TaskID: task.ID, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(w.t, w.st.Jobs().Create(w.t.Context(), &j))
	return testkit.Lease(w.t, w.st, j)
}

// complete registers the assumptions of j's output, as a finishing job does.
func (w *world) complete(j domain.Job) {
	w.t.Helper()
	must(w.t, w.st.WithTx(w.t.Context(), func(ctx context.Context, tx store.Store) error { return w.regs.JobCompleted(ctx, tx, j) }))
}

func stated(statement string, impact domain.Level) domain.StatedAssumption {
	return domain.StatedAssumption{Statement: statement, Confidence: domain.LevelMedium, Impact: impact, VerifyBy: domain.VerifyByTest}
}

func (w *world) assumptions() []domain.Assumption {
	w.t.Helper()
	as, err := w.st.Assumptions().List(w.t.Context(), w.account, store.AssumptionFilter{})
	must(w.t, err)
	return as
}

func (w *world) gateList(kind domain.GateKind, status domain.GateStatus) []domain.Gate {
	w.t.Helper()
	gs, err := w.st.Gates().List(w.t.Context(), w.account, store.GateFilter{Kind: kind, Status: status})
	must(w.t, err)
	return gs
}

func (w *world) auditActions(subject uuid.UUID) []string {
	w.t.Helper()
	es, err := w.st.Audit().List(w.t.Context(), w.account, store.AuditFilter{SubjectID: subject})
	must(w.t, err)
	var out []string
	for _, e := range slices.Backward(es) {
		out = append(out, e.Action)
	}
	return out
}
