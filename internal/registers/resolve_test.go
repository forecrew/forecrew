package registers_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func (w *world) recordHigh(task domain.Task) domain.Assumption {
	w.t.Helper()
	w.complete(w.job(task, "developer", map[string]any{"assumptions": []domain.StatedAssumption{stated("Single instance", domain.LevelHigh)}}))
	for _, a := range w.assumptions() {
		if a.Statement == "Single instance" {
			return a
		}
	}
	w.t.Fatal("assumption not registered")
	return domain.Assumption{}
}

func TestResolveVerifiedAndAccepted(t *testing.T) {
	w := newWorld(t)
	a := w.recordHigh(w.tasks[0])
	_, err := w.regs.Resolve(t.Context(), w.actor(), a.ID, domain.AssumptionVerified, "  ")
	if !errors.Is(err, domain.ErrInvalidRecord) {
		t.Fatalf("verified without evidence: %v", err)
	}
	got, err := w.regs.Resolve(t.Context(), w.actor(), a.ID, domain.AssumptionVerified, "test TestSingle passes")
	must(t, err)
	if got.Status != domain.AssumptionVerified || got.Evidence != "test TestSingle passes" || got.ResolvedBy != "user:gian" || !got.ResolvedAt.Equal(t0) {
		t.Fatalf("resolved = %+v", got)
	}
	stored, _ := w.st.Assumptions().Get(t.Context(), w.account, a.ID)
	if stored.Status != domain.AssumptionVerified {
		t.Fatalf("stored = %+v", stored)
	}
	_, err = w.regs.Resolve(t.Context(), w.actor(), a.ID, domain.AssumptionAccepted, "")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("verified -> accepted: %v", err)
	}
	_, err = w.regs.Resolve(t.Context(), w.actor(), domain.NewID(), domain.AssumptionAccepted, "")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if got := w.auditActions(a.ID); !slices.Equal(got, []string{"assumption.recorded", "assumption.resolved"}) {
		t.Fatalf("audit = %v", got)
	}
	// Its review gate was decided with it, as the human who resolved it,
	// so that a run waiting for the review goes on.
	if gs := w.gateList(domain.GateAssumptionReview, domain.GatePending); len(gs) != 0 {
		t.Fatalf("pending gates = %+v", gs)
	}
	gs := w.gateList(domain.GateAssumptionReview, domain.GateApproved)
	if len(gs) != 1 || gs[0].ResolvedBy != "user:gian" || gs[0].Feedback != "test TestSingle passes" {
		t.Fatalf("closed gates = %+v", gs)
	}
	if sig, _ := w.st.Signals().ListByKey(t.Context(), w.account, domain.GateKey(gs[0].ID)); len(sig) != 1 {
		t.Fatalf("gate.resolved signals = %d", len(sig))
	}
	_, err = w.gates.Resolve(t.Context(), w.actor(), gs[0].ID, domain.GateRejected, "too late")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a closed gate was decided again: %v", err)
	}
	if stored, _ := w.st.Assumptions().Get(t.Context(), w.account, a.ID); stored.Status != domain.AssumptionVerified {
		t.Fatalf("resolved assumption changed by a gate: %+v", stored)
	}
	// Resolving it directly records no decision of its own.
	if ds, _ := w.st.Decisions().List(t.Context(), w.account, store.DecisionFilter{}); len(ds) != 0 {
		t.Fatalf("decisions = %+v", ds)
	}
}

// Invalidation flags the dependents: a task waiting at the merge gate goes
// back with the evidence as feedback, merged work gets a draft goal.
func TestInvalidationFlagsDependents(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	a := w.recordHigh(w.tasks[0])
	// Task 1 rests on it too, and its PR is merged; a third change is merged.
	must(t, w.st.Assumptions().AddDependents(ctx, w.account, a.ID, []domain.Dependent{
		{Kind: domain.DependentTask, ID: w.tasks[1].Ref()},
		{Kind: domain.DependentChange, ID: "acme/app#20"}, {Kind: domain.DependentChange, ID: "acme/app#21"},
		{Kind: domain.DependentChange, ID: "acme/app#22"}, {Kind: domain.DependentChange, ID: "acme/app#23"},
		{Kind: domain.DependentTask, ID: "TASK-99"}, {Kind: domain.DependentTask, ID: "nonsense"},
		{Kind: domain.DependentChange, ID: "nonsense"}, {Kind: domain.DependentSpec, ID: w.spec.Ref()},
	}))
	move := func(task domain.Task, path ...domain.TaskStatus) {
		cur := domain.TaskOpen
		for _, to := range path {
			must(t, w.st.Tasks().UpdateStatus(ctx, w.account, task.ID, cur, to))
			cur = to
		}
	}
	move(w.tasks[0], domain.TaskInProgress, domain.TaskInReview, domain.TaskMergeReady)
	move(w.tasks[1], domain.TaskInProgress, domain.TaskInReview, domain.TaskMergeReady, domain.TaskMerged)
	change := func(number int, task domain.Task, state domain.ChangeState) {
		c := domain.Change{AccountID: w.account, ProjectID: w.project.ID, TaskID: task.ID, Repo: "acme/app", Number: number, Branch: "b", BaseRef: "main", State: state}
		must(t, w.st.Changes().Upsert(ctx, &c))
	}
	change(20, domain.Task{}, domain.ChangeMerged)
	change(21, domain.Task{}, domain.ChangeClosed)
	change(22, w.tasks[0], domain.ChangeOpen)
	change(23, domain.Task{}, domain.ChangeOpen)
	merge := domain.Gate{AccountID: w.account, ProjectID: w.project.ID, Kind: domain.GateMergeApproval, Title: "Merge", SubjectRepo: "acme/app", SubjectNumber: 22, TaskID: w.tasks[0].ID}
	must(t, w.st.Gates().Create(ctx, &merge))
	other := domain.Gate{AccountID: w.account, ProjectID: w.project.ID, Kind: domain.GateMergeApproval, Title: "Merge", SubjectRepo: "acme/app", SubjectNumber: 30, TaskID: w.tasks[1].ID}
	must(t, w.st.Gates().Create(ctx, &other))

	got, err := w.regs.Resolve(ctx, w.actor(), a.ID, domain.AssumptionInvalidated, "the service scales out")
	must(t, err)
	if got.Status != domain.AssumptionInvalidated {
		t.Fatalf("resolved = %+v", got)
	}

	task, _ := w.st.Tasks().Get(ctx, w.account, w.tasks[0].ID)
	if task.Status != domain.TaskInProgress {
		t.Fatalf("task = %s, want in_progress", task.Status)
	}
	if merged, _ := w.st.Tasks().Get(ctx, w.account, w.tasks[1].ID); merged.Status != domain.TaskMerged {
		t.Fatalf("merged task moved: %s", merged.Status)
	}
	sent, _ := w.st.Gates().Get(ctx, w.account, merge.ID)
	if sent.Status != domain.GateChangesRequested || sent.ResolvedBy != "system:forecrewd" ||
		!strings.Contains(sent.Feedback, "A-1 was invalidated") || !strings.Contains(sent.Feedback, "<untrusted-data source=\"evidence A-1\"") ||
		!strings.Contains(sent.Feedback, "the service scales out") {
		t.Fatalf("merge gate = %+v", sent)
	}
	if still, _ := w.st.Gates().Get(ctx, w.account, other.ID); still.Status != domain.GatePending {
		t.Fatalf("the gate of a merged task was decided: %+v", still)
	}
	sig, _ := w.st.Signals().ListByKey(ctx, w.account, domain.GateKey(merge.ID))
	if len(sig) != 1 {
		t.Fatalf("gate.resolved signals = %d", len(sig))
	}

	// One re-evaluation goal, in draft, naming the merged work.
	goals, _ := w.st.Goals().List(ctx, w.account, store.GoalFilter{Status: domain.GoalDraft})
	if len(goals) != 1 {
		t.Fatalf("draft goals = %+v", goals)
	}
	g := goals[0]
	if g.Title != "Re-evaluate A-1" || g.CreatedBy != "system:forecrewd" || g.ProjectID != w.project.ID ||
		!strings.Contains(g.Body, "<untrusted-data source=\"assumption A-1\"") || !strings.Contains(g.Body, "<untrusted-data source=\"evidence A-1\"") ||
		!strings.Contains(g.Body, "TASK-2") || !strings.Contains(g.Body, "acme/app#20") || strings.Contains(g.Body, "acme/app#21") {
		t.Fatalf("goal = %+v", g)
	}
	// The goal is a dependent of the assumption, and its review gate, if it
	// had one pending, was rejected with it.
	if stored, _ := w.st.Assumptions().Get(ctx, w.account, a.ID); !slices.Contains(stored.Dependents, domain.Dependent{Kind: domain.DependentGoal, ID: g.Ref()}) {
		t.Fatalf("dependents = %+v", stored.Dependents)
	}
	if rejected := w.gateList(domain.GateAssumptionReview, domain.GateRejected); len(rejected) != 1 || rejected[0].Feedback != "the service scales out" {
		t.Fatalf("review gates = %+v", rejected)
	}
	actions := w.auditActions(a.ID)
	if actions[len(actions)-1] != "assumption.resolved" {
		t.Fatalf("audit = %v", actions)
	}
	es, _ := w.st.Audit().List(ctx, w.account, store.AuditFilter{SubjectID: a.ID, Limit: 1})
	if !strings.Contains(string(es[0].Data), g.Ref()) || !strings.Contains(string(es[0].Data), "TASK-2") {
		t.Fatalf("audit data = %s", es[0].Data)
	}
}

func TestInvalidationWithoutMergedWorkMakesNoGoal(t *testing.T) {
	w := newWorld(t)
	a := w.recordHigh(w.tasks[0])
	_, err := w.regs.Resolve(t.Context(), w.actor(), a.ID, domain.AssumptionInvalidated, "wrong")
	must(t, err)
	if goals, _ := w.st.Goals().List(t.Context(), w.account, store.GoalFilter{Status: domain.GoalDraft}); len(goals) != 0 {
		t.Fatalf("goals = %+v", goals)
	}
	// An accepted assumption can still be invalidated by later evidence.
	b := w.assumptions()
	if len(b) != 1 {
		t.Fatal(b)
	}
}
