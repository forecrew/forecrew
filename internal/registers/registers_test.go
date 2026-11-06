package registers_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"The service runs as a single instance.":       "the service runs as a single instance",
		"  The  service\nRUNS as a single instance ?!": "the service runs as a single instance",
		"": "",
	} {
		if got := registers.Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// A job output with three assumptions, two of them the same statement,
// registers two items, with the job as source and the goal and the task as
// dependents; the high-impact one opens one review gate.
func TestJobCompletedRegistersTheOutput(t *testing.T) {
	w := newWorld(t)
	j := w.job(w.tasks[0], "developer", map[string]any{"summary": "done", "assumptions": []domain.StatedAssumption{
		stated("The service runs as a single instance.", domain.LevelHigh),
		stated("Logs are UTF-8", domain.LevelLow),
		stated("the service runs as a  single instance", domain.LevelMedium),
	}})
	w.complete(j)

	as := w.assumptions()
	if len(as) != 2 {
		t.Fatalf("assumptions = %+v, want 2", as)
	}
	a := as[0]
	if a.Ref() != "A-1" || a.Status != domain.AssumptionUnverified || a.Impact != domain.LevelHigh || a.MadeBy != "developer" ||
		a.JobID != j.ID || a.SpecID != w.spec.ID || a.GoalID != w.goal.ID || a.ProjectID != w.project.ID {
		t.Fatalf("A-1 = %+v", a)
	}
	want := []domain.Dependent{{Kind: domain.DependentGoal, ID: w.goal.Ref()}, {Kind: domain.DependentTask, ID: w.tasks[0].Ref()}}
	if !slices.Equal(a.Dependents, want) {
		t.Fatalf("dependents = %+v, want %+v", a.Dependents, want)
	}
	if as[1].Impact != domain.LevelLow {
		t.Fatalf("A-2 = %+v", as[1])
	}
	if got := w.auditActions(a.ID); !slices.Equal(got, []string{"assumption.recorded"}) {
		t.Fatalf("audit = %v", got)
	}

	// Only the high-impact assumption has a gate, on the task's issue.
	gs := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(gs) != 1 {
		t.Fatalf("review gates = %+v", gs)
	}
	g := gs[0]
	if g.Title != "Review A-1: The service runs as a single instance." || g.SubjectRepo != "acme/app" || g.SubjectNumber != w.tasks[0].IssueNumber ||
		g.TaskID != w.tasks[0].ID || g.GoalID != w.goal.ID || g.JobID != j.ID || g.Origin != domain.GateFromPlaybook ||
		!strings.Contains(g.Summary, "<untrusted-data") {
		t.Fatalf("gate = %+v", g)
	}

	// Another job of the goal stating it again adds its task, no new item or gate.
	again := w.job(w.tasks[1], "reviewer", map[string]any{"assumptions": []domain.StatedAssumption{stated("The service runs as a single instance", domain.LevelHigh)}})
	w.complete(again)
	as = w.assumptions()
	if len(as) != 2 || len(as[0].Dependents) != 3 || as[0].Dependents[2].ID != w.tasks[1].Ref() {
		t.Fatalf("after restating: %+v", as)
	}
	if n := len(w.gateList(domain.GateAssumptionReview, "")); n != 1 {
		t.Fatalf("gates = %d, want 1", n)
	}
}

func TestJobCompletedSkipsWhatIsNotAssumptions(t *testing.T) {
	w := newWorld(t)
	failed := w.job(w.tasks[0], "developer", map[string]any{"assumptions": []domain.StatedAssumption{stated("x", domain.LevelLow)}})
	failed.Status = domain.JobFailed
	w.complete(failed)
	command := w.job(w.tasks[0], "developer", map[string]any{"assumptions": []domain.StatedAssumption{stated("x", domain.LevelLow)}})
	command.Kind = domain.JobKindCommand
	w.complete(command)
	empty := w.job(w.tasks[0], "developer", nil)
	empty.Output = nil
	w.complete(empty)
	odd := w.job(w.tasks[0], "developer", map[string]any{"assumptions": "none"})
	w.complete(odd)
	list := w.job(w.tasks[0], "developer", []int{1})
	w.complete(list)
	invalid := w.job(w.tasks[0], "developer", map[string]any{"assumptions": []map[string]string{{"statement": "", "confidence": "low", "impact": "low", "verify_by": "test"}}})
	w.complete(invalid)
	if as := w.assumptions(); len(as) != 0 {
		t.Fatalf("assumptions = %+v", as)
	}
}

// A job without a goal registers its assumptions in the project, deduplicated
// among the assumptions without a goal.
func TestRecordWithoutAGoal(t *testing.T) {
	w := newWorld(t)
	src := registers.Source{MadeBy: "onboarding-analyst"}
	first, err := w.regs.Record(t.Context(), w.account, w.project.ID, src, []domain.StatedAssumption{stated("Monorepo", domain.LevelMedium)})
	must(t, err)
	// The same statement in a goal is another item.
	inGoal, err := w.regs.Record(t.Context(), w.account, w.project.ID, registers.Source{MadeBy: "architect", GoalID: w.goal.ID}, []domain.StatedAssumption{stated("monorepo", domain.LevelMedium)})
	must(t, err)
	second, err := w.regs.Record(t.Context(), w.account, w.project.ID, registers.Source{MadeBy: "x", Extra: []domain.Dependent{{Kind: domain.DependentTest, ID: "a_test.go"}}}, []domain.StatedAssumption{stated("MONOREPO.", domain.LevelMedium)})
	must(t, err)
	if first[0].ID != second[0].ID || inGoal[0].ID == first[0].ID || len(w.assumptions()) != 2 {
		t.Fatalf("first %+v, in goal %+v, second %+v", first, inGoal, second)
	}
	if got := w.assumptions()[0].Dependents; len(got) != 1 || got[0].Kind != domain.DependentTest {
		t.Fatalf("dependents = %+v", got)
	}
	if n := len(w.gateList("", "")); n != 0 {
		t.Fatalf("gates = %d", n)
	}
	if _, err := w.regs.Record(t.Context(), w.account, w.project.ID, src, []domain.StatedAssumption{{Statement: "x"}}); err == nil {
		t.Fatal("an invalid assumption was recorded")
	}
	if _, err := w.regs.Record(t.Context(), w.account, w.project.ID, registers.Source{GoalID: domain.NewID()}, nil); err == nil {
		t.Fatal("a goal that does not exist was accepted")
	}
}

func TestRecordAssumptionOfAJobMCPCall(t *testing.T) {
	w := newWorld(t)
	j := w.running(w.tasks[0], "developer")
	first, dup, err := w.regs.RecordAssumption(t.Context(), j, stated("Tokens expire after an hour", domain.LevelMedium))
	must(t, err)
	if first.Ref() != "A-1" || dup {
		t.Fatalf("first = %+v, dup %v", first, dup)
	}
	got, dup, err := w.regs.RecordAssumption(t.Context(), j, stated("tokens expire after an hour.", domain.LevelLow))
	must(t, err)
	if got.Ref() != "A-1" || !dup || got.Impact != domain.LevelMedium {
		t.Fatalf("got %+v, dup %v", got, dup)
	}
	fresh, dup, err := w.regs.RecordAssumption(t.Context(), j, stated("Another one", domain.LevelLow))
	must(t, err)
	if fresh.Ref() != "A-2" || dup {
		t.Fatalf("fresh = %+v, dup %v", fresh, dup)
	}
	if _, _, err := w.regs.RecordAssumption(t.Context(), j, domain.StatedAssumption{}); !errors.Is(err, domain.ErrInvalidRecord) {
		t.Fatalf("an invalid assumption: %v", err)
	}
}

func TestRecordAssumptionOfAFinishedJobIsRefused(t *testing.T) {
	w := newWorld(t)
	j := w.running(w.tasks[0], "developer")
	_, err := w.st.Jobs().Complete(t.Context(), j.AccountID, j.ID, j.WorkerID, store.JobResult{Status: domain.JobSucceeded})
	must(t, err)
	if _, _, err := w.regs.RecordAssumption(t.Context(), j, stated("Too late", domain.LevelHigh)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("err = %v, want a lost lease", err)
	}
	if as := w.assumptions(); len(as) != 0 || len(w.gateList(domain.GateAssumptionReview, "")) != 0 {
		t.Fatalf("a refused record left %+v", as)
	}
}

func TestOfTaskBlockingAndReviewGates(t *testing.T) {
	w := newWorld(t)
	w.complete(w.job(w.tasks[0], "developer", map[string]any{"assumptions": []domain.StatedAssumption{
		stated("high one", domain.LevelHigh), stated("medium one", domain.LevelMedium)}}))
	w.complete(w.job(w.tasks[1], "developer", map[string]any{"assumptions": []domain.StatedAssumption{stated("other task", domain.LevelHigh)}}))

	of, err := w.regs.OfTask(t.Context(), w.account, w.tasks[0])
	must(t, err)
	if len(of) != 2 || !registers.RestsOn(of[0], w.tasks[0]) || registers.RestsOn(of[0], w.tasks[1]) {
		t.Fatalf("of task = %+v", of)
	}
	blocking := registers.Blocking(of)
	if len(blocking) != 1 || blocking[0].Statement != "high one" {
		t.Fatalf("blocking = %+v", blocking)
	}
	gs, err := w.regs.ReviewGates(t.Context(), w.account, w.project.ID, blocking)
	must(t, err)
	if len(gs) != 1 || gs[0].TaskID != w.tasks[0].ID {
		t.Fatalf("gates = %+v", gs)
	}
	if gs, err := w.regs.ReviewGates(t.Context(), w.account, w.project.ID, nil); err != nil || len(gs) != 0 {
		t.Fatalf("no assumptions: %+v, %v", gs, err)
	}

	// Accepted, it no longer blocks and its gate is no longer pending.
	_, err = w.gates.Resolve(t.Context(), w.actor(), gs[0].ID, domain.GateApproved, "fine")
	must(t, err)
	of, _ = w.regs.OfTask(t.Context(), w.account, w.tasks[0])
	if len(registers.Blocking(of)) != 0 {
		t.Fatalf("still blocking: %+v", of)
	}
	if gs, _ := w.regs.ReviewGates(t.Context(), w.account, w.project.ID, blocking); len(gs) != 0 {
		t.Fatalf("pending gates = %+v", gs)
	}
}

func TestLinks(t *testing.T) {
	w := newWorld(t)
	j := w.job(w.tasks[0], "developer", map[string]any{"assumptions": []domain.StatedAssumption{stated("one", domain.LevelLow)}})
	w.complete(j)
	must(t, w.regs.LinkChange(t.Context(), w.account, w.tasks[0], "acme/app", 9))
	must(t, w.regs.LinkChange(t.Context(), w.account, w.tasks[0], "acme/app", 9))
	must(t, w.regs.LinkSpec(t.Context(), w.account, j.ID, w.spec))
	deps := w.assumptions()[0].Dependents
	want := []domain.Dependent{{Kind: domain.DependentGoal, ID: "GOAL-1"}, {Kind: domain.DependentTask, ID: "TASK-1"},
		{Kind: domain.DependentChange, ID: "acme/app#9"}, {Kind: domain.DependentSpec, ID: "SPEC-1"}}
	if !slices.Equal(deps, want) {
		t.Fatalf("dependents = %+v", deps)
	}
	if registers.ChangeDependent("a/b", 3).ID != "a/b#3" {
		t.Fatal("change dependent")
	}
}
