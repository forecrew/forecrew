package playbooks_test

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

// startRegistered starts a goal whose jobs are registered as the job service
// does, and whose gates tell the register of the decisions humans make.
func startRegistered(t *testing.T) *goalRun {
	t.Helper()
	v := newEnv(t, domain.ProjectOnboarded)
	regs := registers.New(v.st, v.bus, v.clock.Now)
	v.gates.WithRecorder(regs)
	r := startGoalOn(t, v, "web")
	r.register = regs
	return r
}

func (r *goalRun) assumption(t *testing.T, statement string) domain.Assumption {
	t.Helper()
	as, err := r.st.Assumptions().List(t.Context(), r.account, store.AssumptionFilter{})
	must(t, err)
	i := slices.IndexFunc(as, func(a domain.Assumption) bool { return a.Statement == statement })
	if i < 0 {
		t.Fatalf("no assumption %q in %+v", statement, as)
	}
	return as[i]
}

// reviewGate returns the pending assumption_review gate of an assumption.
func (r *goalRun) reviewGate(t *testing.T, a domain.Assumption) domain.Gate {
	t.Helper()
	gs, err := r.st.Gates().List(t.Context(), r.account, store.GateFilter{Kind: domain.GateAssumptionReview, Status: domain.GatePending})
	must(t, err)
	for _, g := range gs {
		if strings.HasPrefix(g.Title, "Review "+a.Ref()+":") {
			return g
		}
	}
	t.Fatalf("no pending review of %s in %+v", a.Ref(), gs)
	return domain.Gate{}
}

func (r *goalRun) decideGate(t *testing.T, g domain.Gate, tool, feedback string) {
	t.Helper()
	_, err := r.gates.Decide(t.Context(), r.human, tool, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: feedback})
	must(t, err)
}

// notYetAssessed checks that the round has not got to its assessment, which
// is what opens the merge gate.
func (r *goalRun) notYetAssessed(t *testing.T) {
	t.Helper()
	if r.startedStep(t, build(0, 1, 1, "assess")) || r.startedStep(t, "tasks/0/delivery/1/approval") {
		t.Fatal("the round was assessed while a high-impact assumption is open")
	}
	gs, err := r.st.Gates().List(t.Context(), r.account, store.GateFilter{Kind: domain.GateMergeApproval})
	must(t, err)
	if len(gs) != 0 {
		t.Fatalf("merge gate opened: %+v", gs)
	}
}

// A high-impact assumption of the task keeps the merge gate closed until a
// human accepts it.
func TestHighImpactAssumptionBlocksTheMergeGateUntilAccepted(t *testing.T) {
	r := startRegistered(t)
	r.oneTask(t)
	// The spec approval is the first decision of the register.
	if ds, _ := r.st.Decisions().List(t.Context(), r.account, store.DecisionFilter{}); len(ds) != 1 || ds[0].Choice != "approved" || ds[0].DecidedBy != "user:ana" ||
		!strings.HasPrefix(ds[0].Question, "Approve the spec") {
		t.Fatalf("decisions = %+v", ds)
	}

	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())

	// The developer's and the reviewer's high-impact assumptions each have a
	// gate on the task's issue, and the spec's has one on the goal's: the
	// round waits for all three, the goal's first.
	cents, cards, stripe := r.assumption(t, "Amounts are in cents"), r.assumption(t, "Cards only"), r.assumption(t, "Stripe is the processor")
	wait := func(i int) string { return build(0, 1, 1, "gated/0/reviewed/1/waiting/"+itoa(i)+"/assumption_wait") }
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/hold"), domain.StepSucceeded)
	r.waitStep(t, wait(0), domain.StepWaiting)
	r.notYetAssessed(t)
	task := r.tasks(t)[0]
	g := r.reviewGate(t, cents)
	if g.TaskID != task.ID || g.SubjectNumber != task.IssueNumber || cents.Status != domain.AssumptionUnverified {
		t.Fatalf("review gate = %+v, assumption %+v", g, cents)
	}
	// What the architect or the product owner assumed is the goal's: every
	// task of the goal rests on it and waits for it.
	if sg := r.reviewGate(t, stripe); sg.TaskID != uuid.Nil() || slices.ContainsFunc(stripe.Dependents, func(d domain.Dependent) bool { return d.Kind == domain.DependentTask }) ||
		!slices.Contains(stripe.Dependents, registers.ChangeDependent("acme/app", r.pr(t, 0, 1, 1).Number)) || !registers.RestsOn(stripe, task) {
		t.Fatalf("spec assumption = %+v, gate %+v", stripe, sg)
	}

	r.decideGate(t, r.reviewGate(t, stripe), mcp.ToolApprove, "Stripe it is.")
	r.waitStep(t, wait(1), domain.StepWaiting)
	r.notYetAssessed(t)
	r.decideGate(t, g, mcp.ToolApprove, "Cents it is.")
	if got := r.assumption(t, "Amounts are in cents"); got.Status != domain.AssumptionAccepted || got.ResolvedBy != "user:ana" {
		t.Fatalf("accepted = %+v", got)
	}
	r.waitStep(t, wait(2), domain.StepWaiting)
	r.notYetAssessed(t)

	r.decideGate(t, r.reviewGate(t, cards), mcp.ToolApprove, "")
	mg := r.pendingGate(t, domain.GateMergeApproval)
	for _, want := range []string{"## High-impact assumptions", "- " + cents.Ref() + " accepted (impact high): Amounts are in cents", "- " + cards.Ref() + " accepted (impact high): Cards only", "- " + stripe.Ref() + " accepted (impact high): Stripe is the processor"} {
		if !strings.Contains(mg.Summary, want) {
			t.Errorf("merge gate lacks %q:\n%s", want, mg.Summary)
		}
	}
	// The PR text lists the task's assumptions from the register.
	pr := r.pr(t, 0, 1, 1)
	if !strings.Contains(pr.Body, cents.Ref()+" unverified (impact high): Amounts are in cents") {
		t.Errorf("pr body:\n%s", pr.Body)
	}
	// The pull request depends on the assumptions too.
	if got := r.assumption(t, "Amounts are in cents").Dependents; !slices.Contains(got, registers.ChangeDependent("acme/app", pr.Number)) {
		t.Errorf("dependents = %+v", got)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)

	// Every human decision is in the register, the merge decision resting
	// on the task's assumptions.
	ds, err := r.st.Decisions().List(t.Context(), r.account, store.DecisionFilter{})
	must(t, err)
	last := ds[len(ds)-1]
	if len(ds) != 5 || last.GateID != mg.ID || len(last.Assumptions) != 4 || !strings.HasPrefix(last.Question, "Merge TASK-1") {
		t.Fatalf("decisions = %+v", ds)
	}
}

// An assumption a human rejects is invalidated: the round is sent back to
// the developer with the evidence.
func TestRejectedAssumptionSendsTheRoundBack(t *testing.T) {
	r := startRegistered(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())
	cents, cards, stripe := r.assumption(t, "Amounts are in cents"), r.assumption(t, "Cards only"), r.assumption(t, "Stripe is the processor")
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/waiting/0/assumption_wait"), domain.StepWaiting)

	r.clock.Advance(time.Minute) // after the developer job finished
	r.decideGate(t, r.reviewGate(t, stripe), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, cards), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, cents), mcp.ToolReject, "Prices are in euros with decimals.")

	r.waitStep(t, build(0, 1, 1, "assess"), domain.StepSucceeded)
	if got := r.assumption(t, "Amounts are in cents"); got.Status != domain.AssumptionInvalidated || got.Evidence != "Prices are in euros with decimals." {
		t.Fatalf("invalidated = %+v", got)
	}
	// The task is back in progress while the developer reworks it. Once the
	// developer answers, the engine opens the PR and moves it to in review
	// again at once, so this is the only time to look.
	r.waitStep(t, build(0, 1, 2, "dev/1/develop"), domain.StepWaiting)
	if got := r.taskStatus(t, 0); got != domain.TaskInProgress {
		t.Errorf("task = %s while the developer reworks it", got)
	}
	dev := r.develop(t, 0, 1, 2)
	for _, want := range []string{
		cents.Ref() + " was invalidated. Evidence:\n<untrusted-data source=\"evidence " + cents.Ref() + "\"", "Prices are in euros with decimals.",
		"<untrusted-data source=\"assumption " + cents.Ref() + "\"", "Amounts are in cents",
	} {
		if !strings.Contains(dev.Feedback, want) {
			t.Errorf("rework feedback lacks %q:\n%s", want, dev.Feedback)
		}
	}
}

// An assumption invalidated while the merge gate waits sends the change back
// through the gate, as a request for changes.
func TestInvalidatingAnAssumptionAtTheMergeGateSendsItBack(t *testing.T) {
	r := startRegistered(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/waiting/0/assumption_wait"), domain.StepWaiting)
	cents := r.assumption(t, "Amounts are in cents")
	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Stripe is the processor")), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, cents), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Cards only")), mcp.ToolApprove, "")
	mg := r.pendingGate(t, domain.GateMergeApproval)

	_, err := r.register.Resolve(t.Context(), r.human, cents.ID, domain.AssumptionInvalidated, "Prices have decimals.")
	must(t, err)

	got, err := r.gates.Get(t.Context(), r.account, mg.ID)
	must(t, err)
	if got.Status != domain.GateChangesRequested || !strings.Contains(got.Feedback, "Prices have decimals.") {
		t.Fatalf("merge gate = %+v", got)
	}
	// The next delivery starts with the invalidation as the human's feedback.
	dev := r.develop(t, 0, 2, 1)
	if !strings.Contains(dev.Feedback, cents.Ref()+" was invalidated") {
		t.Errorf("feedback = %q", dev.Feedback)
	}
}

// pendingAssumption returns the assumption with the statement that is still
// unverified.
func (r *goalRun) pendingAssumption(t *testing.T, statement string) domain.Assumption {
	t.Helper()
	as, err := r.st.Assumptions().List(t.Context(), r.account, store.AssumptionFilter{Status: domain.AssumptionUnverified})
	must(t, err)
	i := slices.IndexFunc(as, func(a domain.Assumption) bool { return a.Statement == statement })
	if i < 0 {
		t.Fatalf("no unverified assumption %q in %+v", statement, as)
	}
	return as[i]
}

// A human who asks for changes at a review does not send the run back to the
// developer: a new review opens, with what they asked for, and the run waits
// for it.
func TestChangesRequestedAtAReviewKeepsTheRunWaitingForAHuman(t *testing.T) {
	r := startRegistered(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())
	cents := r.assumption(t, "Amounts are in cents")
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/waiting/0/assumption_wait"), domain.StepWaiting)

	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Stripe is the processor")), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Cards only")), mcp.ToolApprove, "")
	first := r.reviewGate(t, cents)
	r.decideGate(t, first, mcp.ToolRequestChanges, "Check the rounding.")

	// The second pass of the loop waits for the new review.
	wait := build(0, 1, 1, "gated/0/reviewed/2/waiting/0/assumption_wait")
	r.waitStep(t, wait, domain.StepWaiting)
	r.notYetAssessed(t)
	again := r.reviewGate(t, cents)
	if again.ID == first.ID || !strings.Contains(again.Summary, "Check the rounding.") || r.startedStep(t, build(0, 1, 2, "")) {
		t.Fatalf("new review = %+v", again)
	}
	if got := r.assumption(t, "Amounts are in cents"); got.Status != domain.AssumptionUnverified {
		t.Fatalf("assumption = %+v", got)
	}

	r.decideGate(t, again, mcp.ToolApprove, "Rounding is fine.")
	mg := r.pendingGate(t, domain.GateMergeApproval)
	if r.startedStep(t, build(0, 1, 2, "")) || !strings.Contains(mg.Summary, cents.Ref()+" accepted (impact high)") {
		t.Fatalf("a rework round was spent, or the merge gate lacks the assumption:\n%s", mg.Summary)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
}

// The developer of a rework round states again what a human rejected: the
// merge gate stays closed until a human reviews it again.
func TestRestatingARejectedAssumptionIsReviewedAgain(t *testing.T) {
	r := startRegistered(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())
	cents := r.assumption(t, "Amounts are in cents")
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/waiting/0/assumption_wait"), domain.StepWaiting)
	r.clock.Advance(time.Minute)
	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Stripe is the processor")), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Cards only")), mcp.ToolApprove, "")
	r.decideGate(t, r.reviewGate(t, cents), mcp.ToolReject, "Prices are in euros with decimals.")
	r.waitStep(t, build(0, 1, 1, "assess"), domain.StepSucceeded)

	// Round 2 states "Amounts are in cents" again (the fake developer always
	// does): it is not the rejected item, it is a new one under review.
	r.clock.Advance(time.Minute)
	r.develop(t, 0, 1, 2)
	r.checks(t, 0, 1, 2, domain.ChecksSuccess)
	r.review(t, 0, 1, 2, approveReview())
	r.waitStep(t, build(0, 1, 2, "gated/0/reviewed/1/waiting/0/assumption_wait"), domain.StepWaiting)
	r.notYetAssessed2(t)
	again := r.pendingAssumption(t, "Amounts are in cents")
	if again.ID == cents.ID || again.Impact != domain.LevelHigh || again.Number <= cents.Number {
		t.Fatalf("restated = %+v, rejected = %+v", again, cents)
	}
	g := r.reviewGate(t, again)
	if !strings.Contains(g.Summary, "restates "+cents.Ref()+", which was invalidated") || !strings.Contains(g.Summary, "Prices are in euros with decimals.") {
		t.Fatalf("review = %s", g.Summary)
	}

	r.decideGate(t, g, mcp.ToolApprove, "Fine this time.")
	r.pendingGate(t, domain.GateMergeApproval)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
}

// notYetAssessed2 is notYetAssessed for the second round of the first delivery.
func (r *goalRun) notYetAssessed2(t *testing.T) {
	t.Helper()
	if r.startedStep(t, build(0, 1, 2, "assess")) {
		t.Fatal("the round was assessed while a high-impact assumption is open")
	}
	gs, err := r.st.Gates().List(t.Context(), r.account, store.GateFilter{Kind: domain.GateMergeApproval})
	must(t, err)
	if len(gs) != 0 {
		t.Fatalf("merge gate opened: %+v", gs)
	}
}

// A PR merged on GitHub while the run waits for the reviews of its task: the
// webhook sends the reviews back, the wait ends, and the run goes on to the
// merged task without a rework round or a merge gate.
func TestAPRMergedOnGitHubEndsTheWaitForReviews(t *testing.T) {
	r := startRegistered(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/waiting/0/assumption_wait"), domain.StepWaiting)
	r.decideGate(t, r.reviewGate(t, r.assumption(t, "Stripe is the processor")), mcp.ToolApprove, "")
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/waiting/1/assumption_wait"), domain.StepWaiting)

	pr := r.pr(t, 0, 1, 1)
	_, err := r.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	forecrewd := domain.SystemActor(r.account, domain.ForecrewdName)
	for _, name := range []string{"Amounts are in cents", "Cards only"} {
		g := r.reviewGate(t, r.assumption(t, name))
		_, err := r.gates.Resolve(t.Context(), forecrewd, g.ID, domain.GateChangesRequested, "merged on GitHub")
		must(t, err)
	}

	r.waitRun(t, domain.RunSucceeded)
	if r.startedStep(t, build(0, 1, 2, "")) || r.startedStep(t, "tasks/0/delivery/1/approval/") {
		t.Fatal("a rework round or the merge gate followed a merge on GitHub")
	}
	if r.taskStatus(t, 0) != domain.TaskMerged {
		t.Fatalf("task = %s", r.taskStatus(t, 0))
	}
	for _, name := range []string{"Amounts are in cents", "Cards only"} {
		if got := r.assumption(t, name); got.Status != domain.AssumptionUnverified {
			t.Fatalf("%s = %+v", name, got)
		}
	}
}
