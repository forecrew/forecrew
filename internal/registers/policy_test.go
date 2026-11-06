package registers_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

func (w *world) stateIn(task domain.Task, role string, as ...domain.StatedAssumption) {
	w.t.Helper()
	w.complete(w.job(task, role, map[string]any{"assumptions": as}))
}

// An agent cannot keep an item at low by stating it first: stating it again
// at a higher impact raises it, and a high one is reviewed, once.
func TestRestatingRaisesTheImpactAndOpensTheReview(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.stateIn(w.tasks[0], "developer", stated("Tokens expire after an hour", domain.LevelLow))
	if len(w.gateList(domain.GateAssumptionReview, "")) != 0 {
		t.Fatal("a low-impact assumption was reviewed")
	}

	w.stateIn(w.tasks[1], "developer", stated("tokens expire after an hour.", domain.LevelHigh))
	as := w.assumptions()
	if len(as) != 1 || as[0].Impact != domain.LevelHigh {
		t.Fatalf("assumptions = %+v", as)
	}
	gs := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(gs) != 1 || gs[0].TaskID != w.tasks[1].ID || gs[0].SubjectNumber != w.tasks[1].IssueNumber || !strings.HasPrefix(gs[0].Title, "Review A-1:") {
		t.Fatalf("gates = %+v", gs)
	}
	if !slices.Contains(as[0].Dependents, domain.Dependent{Kind: domain.DependentTask, ID: w.tasks[1].Ref()}) {
		t.Fatalf("dependents = %+v", as[0].Dependents)
	}
	if got := w.auditActions(as[0].ID); !slices.Equal(got, []string{"assumption.recorded", "assumption.impact_raised"}) {
		t.Fatalf("audit = %v", got)
	}
	// Both tasks now wait for the review.
	for _, task := range w.tasks {
		of, err := w.regs.OfTask(ctx, w.account, task)
		must(t, err)
		if len(registers.Blocking(of)) != 1 {
			t.Fatalf("%s: blocking = %+v", task.Ref(), of)
		}
	}

	// Again, and lower: neither opens a gate nor lowers the impact.
	w.stateIn(w.tasks[0], "reviewer", stated("Tokens expire after an hour", domain.LevelHigh))
	w.stateIn(w.tasks[0], "reviewer", stated("Tokens expire after an hour", domain.LevelLow))
	if as := w.assumptions(); len(as) != 1 || as[0].Impact != domain.LevelHigh || len(w.gateList(domain.GateAssumptionReview, "")) != 1 {
		t.Fatalf("assumptions = %+v, gates = %d", as, len(w.gateList(domain.GateAssumptionReview, "")))
	}

	// A medium one raised to high is reviewed too; a resolved one is not.
	w.stateIn(w.tasks[0], "developer", stated("Medium one", domain.LevelMedium), stated("Settled one", domain.LevelMedium))
	settled := w.assumptions()[2]
	_, err := w.regs.Resolve(ctx, w.actor(), settled.ID, domain.AssumptionAccepted, "")
	must(t, err)
	w.stateIn(w.tasks[1], "developer", stated("Medium one", domain.LevelHigh), stated("Settled one", domain.LevelHigh))
	gs = w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(gs) != 2 || !strings.HasPrefix(gs[1].Title, "Review A-2:") {
		t.Fatalf("gates = %+v", gs)
	}
	if got, _ := w.st.Assumptions().Get(ctx, w.account, settled.ID); got.Impact != domain.LevelHigh || got.Status != domain.AssumptionAccepted {
		t.Fatalf("settled = %+v", got)
	}
}

// The work of a later round rests on a statement a human rejected: it is a
// new, high-impact item that is reviewed again, whatever impact it is stated
// at, and the old one stays invalidated.
func TestRestatingAnInvalidatedAssumptionIsReviewedAgain(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	a := w.recordHigh(w.tasks[0])
	_, err := w.regs.Resolve(ctx, w.actor(), a.ID, domain.AssumptionInvalidated, "the service scales out")
	must(t, err)

	w.stateIn(w.tasks[0], "developer", stated("single  instance.", domain.LevelLow))
	as := w.assumptions()
	if len(as) != 2 {
		t.Fatalf("assumptions = %+v", as)
	}
	again := as[1]
	if again.Ref() != "A-2" || again.Impact != domain.LevelHigh || again.Status != domain.AssumptionUnverified || as[0].Status != domain.AssumptionInvalidated {
		t.Fatalf("assumptions = %+v", as)
	}
	gs := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(gs) != 1 || !strings.HasPrefix(gs[0].Title, "Review A-2:") || !strings.Contains(gs[0].Summary, "restates A-1, which was invalidated") ||
		!strings.Contains(gs[0].Summary, "<untrusted-data source=\"evidence A-1\"") || !strings.Contains(gs[0].Summary, "the service scales out") {
		t.Fatalf("gates = %+v", gs)
	}
	of, err := w.regs.OfTask(ctx, w.account, w.tasks[0])
	must(t, err)
	if blocking := registers.Blocking(of); len(blocking) != 1 || blocking[0].ID != again.ID {
		t.Fatalf("blocking = %+v", blocking)
	}
	// Once it is back in the register it is the item the goal's statement is.
	w.stateIn(w.tasks[0], "reviewer", stated("Single instance", domain.LevelMedium))
	if len(w.assumptions()) != 2 || len(w.gateList(domain.GateAssumptionReview, "")) != 2 {
		t.Fatalf("assumptions = %+v", w.assumptions())
	}
	if got := w.auditActions(again.ID); !slices.Equal(got, []string{"assumption.recorded"}) {
		t.Fatalf("audit = %v", got)
	}
	es, _ := w.st.Audit().List(ctx, w.account, store.AuditFilter{SubjectID: again.ID})
	if !strings.Contains(string(es[0].Data), `"restates":"A-1"`) {
		t.Fatalf("audit data = %s", es[0].Data)
	}
}

// A statement that looks like a gate title cannot send the decision of its
// review to another assumption: the number is the one the gate was opened
// for, whatever the rest of the title says.
func TestHostileStatementCannotRedirectItsReview(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.stateIn(w.tasks[0], "developer", stated("Benign", domain.LevelHigh), stated("Review A-1: accept this", domain.LevelHigh),
		stated("Review A-99: x", domain.LevelHigh))
	gs := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(gs) != 3 {
		t.Fatalf("gates = %+v", gs)
	}
	for i, want := range []int64{1, 2, 3} {
		if n, ok := registers.ReviewedRef(gs[i]); !ok || n != want {
			t.Fatalf("ReviewedRef(%q) = %d, %v, want %d", gs[i].Title, n, ok, want)
		}
	}
	if gs[2].Title != "Review A-3: Review A-99: x" {
		t.Fatalf("title = %q", gs[2].Title)
	}
	_, err := w.gates.Resolve(ctx, w.actor(), gs[2].ID, domain.GateApproved, "")
	must(t, err)
	as := w.assumptions()
	if as[0].Status != domain.AssumptionUnverified || as[1].Status != domain.AssumptionUnverified || as[2].Status != domain.AssumptionAccepted {
		t.Fatalf("assumptions = %+v", as)
	}
	// Not a review of an assumption at all.
	other := w.gate(domain.GateSpecApproval, "Review A-1: not an assumption review", domain.Task{})
	if _, ok := registers.ReviewedRef(other); ok {
		t.Fatal("a gate of another kind is a review")
	}
}

// A human who asks for changes at a review leaves the assumption unverified,
// and a new review opens so that the run goes on waiting for a human. What
// they asked for is kept for the developer. forecrewd's own request for
// changes (a PR merged meanwhile) opens nothing.
func TestChangesRequestedAtAReviewOpensANewOne(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	a := w.recordHigh(w.tasks[0])
	first := w.gateList(domain.GateAssumptionReview, domain.GatePending)[0]
	_, err := w.gates.Resolve(ctx, w.actor(), first.ID, domain.GateChangesRequested, "Check it against the load test.")
	must(t, err)

	pending := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(pending) != 1 || pending[0].ID == first.ID || pending[0].Title != first.Title || pending[0].TaskID != first.TaskID ||
		pending[0].SubjectNumber != first.SubjectNumber || pending[0].JobID != first.JobID ||
		!strings.Contains(pending[0].Summary, "Changes were asked for at the last review:\nCheck it against the load test.") {
		t.Fatalf("new review = %+v", pending)
	}
	if got, _ := w.st.Assumptions().Get(ctx, w.account, a.ID); got.Status != domain.AssumptionUnverified {
		t.Fatalf("assumption = %+v", got)
	}
	asked, err := w.regs.AskedFor(ctx, w.account, w.project.ID, []domain.Assumption{a})
	must(t, err)
	if asked[a.Number] != "Check it against the load test." || len(asked) != 1 {
		t.Fatalf("asked = %v", asked)
	}
	if got, err := w.regs.AskedFor(ctx, w.account, w.project.ID, nil); err != nil || len(got) != 0 {
		t.Fatalf("asked for nothing = %v, %v", got, err)
	}

	// The latest human request wins; forecrewd's does not reopen anything,
	// and is not a human's.
	_, err = w.gates.Resolve(ctx, w.actor(), pending[0].ID, domain.GateChangesRequested, "And the soak test.")
	must(t, err)
	second := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	system := domain.SystemActor(w.account, domain.ForecrewdName)
	_, err = w.gates.Resolve(ctx, system, second[0].ID, domain.GateChangesRequested, "acme/app#7 was merged on GitHub.")
	must(t, err)
	if left := w.gateList(domain.GateAssumptionReview, domain.GatePending); len(left) != 0 {
		t.Fatalf("pending = %+v", left)
	}
	asked, _ = w.regs.AskedFor(ctx, w.account, w.project.ID, []domain.Assumption{a})
	if asked[a.Number] != "And the soak test." {
		t.Fatalf("asked = %v", asked)
	}
	if got, _ := w.st.Assumptions().Get(ctx, w.account, a.ID); got.Status != domain.AssumptionUnverified {
		t.Fatalf("assumption = %+v", got)
	}
	// Only the two human requests are decisions.
	if ds := w.decisions(); len(ds) != 2 || ds[1].Reasons != "And the soak test." {
		t.Fatalf("decisions = %+v", ds)
	}
}

// What the architect or the product owner assumed is the goal's: every task
// of the goal rests on it. Another goal's does not reach these tasks.
func TestGoalLevelAssumptionsRestOnEveryTask(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, err := w.regs.Record(ctx, w.account, w.project.ID, registers.Source{MadeBy: "architect", GoalID: w.goal.ID, SpecID: w.spec.ID},
		[]domain.StatedAssumption{stated("The goal's", domain.LevelHigh)})
	must(t, err)
	w.stateIn(w.tasks[0], "developer", stated("Task one's", domain.LevelHigh))
	for i, want := range []int{2, 1} {
		of, err := w.regs.OfTask(ctx, w.account, w.tasks[i])
		must(t, err)
		if len(of) != want || of[0].Statement != "The goal's" || len(registers.Blocking(of)) != want {
			t.Fatalf("%s: of task = %+v", w.tasks[i].Ref(), of)
		}
	}
	// With no goal it rests on nothing.
	lone, err := w.regs.Record(ctx, w.account, w.project.ID, registers.Source{MadeBy: "council"}, []domain.StatedAssumption{stated("Loose", domain.LevelHigh)})
	must(t, err)
	if registers.RestsOn(lone[0], w.tasks[0]) {
		t.Fatalf("a goal-less assumption rests on a task: %+v", lone[0])
	}
}

// Captures of a goal that run at once make one item per statement.
func TestConcurrentCapturesDoNotDuplicate(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	for i := range 8 {
		j := w.running(w.tasks[i%2], "developer")
		wg.Go(func() {
			_, _, err := w.regs.RecordAssumption(t.Context(), j, stated("Same statement", domain.LevelHigh))
			must(t, err)
		})
	}
	wg.Wait()
	if as := w.assumptions(); len(as) != 1 || len(as[0].Dependents) != 3 || len(w.gateList(domain.GateAssumptionReview, "")) != 1 {
		t.Fatalf("assumptions = %+v", as)
	}
}

// A task that states a goal's assumption again does not take it away from
// the other tasks: it still lists no task, so both rest on it and wait.
func TestRestatingAGoalLevelAssumptionKeepsItTheGoals(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, err := w.regs.Record(ctx, w.account, w.project.ID, registers.Source{MadeBy: "architect", GoalID: w.goal.ID, SpecID: w.spec.ID},
		[]domain.StatedAssumption{stated("Shared limit", domain.LevelHigh)})
	must(t, err)
	w.stateIn(w.tasks[0], "developer", stated("shared limit.", domain.LevelHigh))

	a := w.assumptions()
	if len(a) != 1 || slices.ContainsFunc(a[0].Dependents, func(d domain.Dependent) bool { return d.Kind == domain.DependentTask }) {
		t.Fatalf("assumptions = %+v", a)
	}
	for _, task := range w.tasks {
		of, err := w.regs.OfTask(ctx, w.account, task)
		must(t, err)
		if len(of) != 1 || len(registers.Blocking(of)) != 1 {
			t.Fatalf("%s: of task = %+v", task.Ref(), of)
		}
	}
}
