package playbooks_test

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/advisor"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

// councilCall is a call of the council.run action the test stands in for.
type councilCall struct {
	Kind        domain.CouncilKind
	Question    string
	Constraints []string
}

// v3Run is a run of the goal-v3 playbook. The councils have their own tests
// (council_playbook_test.go): here council.run decides at once with a brief,
// and the test looks at what the goal playbook asked.
type v3Run struct {
	*v2Run
	mu    *sync.Mutex
	calls *[]councilCall
	// advised records the advise runs the learn step starts.
	advised *testkit.Playbooks
}

// advisorStarted reports whether the learn step started the advisor for
// the project.
func (r *v3Run) advisorStarted() bool {
	return slices.ContainsFunc(r.advised.Starts(), func(s testkit.PlaybookStart) bool {
		return s.Playbook == advisor.PlaybookAdvise && s.Request.ProjectID == r.project.ID
	})
}

// councilCalls returns the councils the playbook asked for, in order.
func (r *v3Run) councilCalls() []councilCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(*r.calls)
}

// startGoalV3 starts goal-v3 for a goal of size (nil: triage sizes it). With
// registered the jobs and gates feed the registers, as in production.
func startGoalV3(t *testing.T, size *domain.GoalSize, registered ...bool) *v3Run {
	t.Helper()
	v := newEnv(t, domain.ProjectOnboarded)
	policy, err := knowledge.ParsePolicy([]byte(learnPolicy))
	must(t, err)
	v.proposals = &proposals.Service{Store: v.st, GitHub: v.tracker, Gates: v.gates, Roles: testRoles(),
		Policy: func(context.Context, domain.Project) (knowledge.Policy, error) { return policy, nil }}
	advised := &testkit.Playbooks{}
	v.advisor = advisor.New(advisor.Config{Store: v.st, Gates: v.gates, Runs: advised, Now: v.clock.Now})
	v.register()
	var regs *registers.Service
	if len(registered) > 0 && registered[0] {
		regs = registers.New(v.st, v.bus, v.clock.Now)
		v.gates.WithRecorder(regs)
	}
	var mu sync.Mutex
	var calls []councilCall
	v.actions["council.run"] = fakeCouncilRun(v, &mu, &calls)
	must(t, v.tracker.SetFile("acme/app", "head1", "README.md", []byte("# app\n")))
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	v.matrixFiles(t, "agent/TASK-1")
	for round := 1; round <= 4; round++ {
		v.matrixFiles(t, v2sha(round))
	}
	g := v.newGoal(t)
	if size != nil {
		must(t, v.st.Goals().SetSize(t.Context(), v.account, g.ID, *size))
	}
	r := startRun(t, v, "goal-v3", workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": "acme/app", "goal_id": g.ID.String(), "platforms": g.Platforms},
	})
	run := &v2Run{&goalRun{runner: r, goal: g, heads: map[string]int{}}}
	if regs != nil {
		run.register = regs
	}
	run.workerOnline(t, "linux-box", "docker", "linux")
	run.workerOnline(t, "windows-box", "windows")
	return &v3Run{v2Run: run, mu: &mu, calls: &calls, advised: advised}
}

func fakeCouncilRun(v *env, mu *sync.Mutex, calls *[]councilCall) workflow.Action {
	return func(ctx context.Context, call workflow.ActionCall) (any, error) {
		var with councilCall
		if err := workflow.DecodeWith(call.With, &with); err != nil {
			return nil, err
		}
		id, err := uuid.Parse(call.Key)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		*calls = append(*calls, with)
		mu.Unlock()
		c := domain.Council{ID: id, AccountID: call.Run.AccountID, ProjectID: call.Run.ProjectID, GoalID: call.Run.GoalID, Kind: with.Kind,
			Question: with.Question, Constraints: with.Constraints, Status: domain.CouncilRunning}
		if err := v.st.Councils().Create(ctx, &c); err != nil {
			return nil, err
		}
		c.Brief = &domain.CouncilBrief{Question: with.Question, Recommendation: "the simple way", Reversible: true,
			Options: []domain.BriefOption{{Name: "the simple way", Summary: "BRIEF OF " + string(with.Kind)}}}
		if err := v.st.Councils().Update(ctx, &c); err != nil {
			return nil, err
		}
		d := domain.Decision{AccountID: c.AccountID, ProjectID: c.ProjectID, GoalID: c.GoalID, Question: with.Question, Choice: "the simple way",
			Reasons: "one dependency less", DecidedBy: "user:ana", Options: []domain.DecisionOption{{Name: "the simple way"}}, Assumptions: []uuid.UUID{}}
		if err := v.st.Decisions().Create(ctx, &d); err != nil {
			return nil, err
		}
		if err := v.st.Councils().UpdateStatus(ctx, c.AccountID, c.ID, domain.CouncilRunning, domain.CouncilDecided); err != nil {
			return nil, err
		}
		sig, err := domain.NewSignal(c.AccountID, c.ProjectID, &domain.CouncilDecidedSignal{CouncilID: c.ID, Status: domain.CouncilDecided, DecisionID: d.ID}, v.clock.Now())
		if err != nil {
			return nil, err
		}
		if _, err := v.bus.Publish(ctx, sig); err != nil {
			return nil, err
		}
		return map[string]string{"council_id": c.ID.String(), "ref": c.Ref()}, nil
	}
}

func triageOutput(impact, reversibility string, areas ...string) map[string]any {
	as := make([]any, len(areas))
	for i, a := range areas {
		as[i] = a
	}
	return map[string]any{"impact": impact, "reversibility": reversibility, "areas": as, "reason": "Graded by the rubric.", "assumptions": []any{}}
}

// triaged answers the triage job.
func (r *v3Run) triaged(t *testing.T, out map[string]any) workflow.JobRequest {
	t.Helper()
	return r.answer(t, "triage/0/triage_job", out, 0.1)
}

// adrPushed answers the architect's design as a worker that also pushed the
// ADR to its branch, and returns the ADR's PR once it is open at that head.
func (r *v3Run) adrPushed(t *testing.T) github.PullRequest {
	t.Helper()
	must(t, r.tracker.SetHeadOfBranch("acme/app", "agent/adr-GOAL-1", adrHead))
	r.answer(t, "architecture/1/design", planOutput(planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"})), 0.75, adrHead)
	r.waitStep(t, "adr_review/1/adr_delivery/0/adr_pr", domain.StepSucceeded)
	pr, err := r.tracker.FindPR(t.Context(), "acme/app", "agent/adr-GOAL-1")
	must(t, err)
	must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, adrHead))
	pr.HeadSHA = adrHead
	return pr
}

// adrHead is the commit the architect pushed.
const adrHead = "adr0000000000000000000000000000000000001"

// delivered runs the one task of the plan through the delivery to its merge.
func (r *v3Run) delivered(t *testing.T) {
	t.Helper()
	r.writeTests(t, 1, testMap(nil))
	r.push(t, 1)
	r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.verify(t, passedRun, 0, nil)
	r.review(t, 0, 1, 1, approveReview())
	r.pendingGate(t, domain.GateMergeApproval)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
}

func (r *v3Run) noStep(t *testing.T, prefix string) {
	t.Helper()
	if r.startedStep(t, prefix) {
		t.Errorf("a step of %s started", prefix)
	}
}

// A small, reversible goal is sized by its triage and gets no council: the
// spec job runs at once, the architect stays read-only, and the learn step
// opens its PR before the goal closes.
func TestGoalV3SmallGoalSkipsBothCouncils(t *testing.T) {
	r := startGoalV3(t, nil)
	triage := r.triaged(t, triageOutput("low", "easy", "docs"))
	if triage.Role != "triage" || triage.GoalID != r.goal.ID || triage.Repo != "acme/app" {
		t.Errorf("triage request = %+v", triage)
	}
	r.spec(t, 1, "A payment succeeds.", "A declined card shows an error.")
	if got := r.goalNow(t).Size; got == nil || len(got.Councils) != 0 || got.Impact != domain.LevelLow {
		t.Errorf("size = %+v", got)
	}
	r.decide(t, domain.GateSpecApproval, mcp.ToolApprove, "")
	arch := r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
	if arch.Pool != "" || arch.Branch != "" || len(arch.WritePaths) != 0 {
		t.Errorf("a goal without a technical council gets a writing architect: %+v", arch)
	}
	if got := r.councilCalls(); len(got) != 0 {
		t.Errorf("councils = %+v", got)
	}
	r.noStep(t, "adr_review/1/adr_delivery/0")

	r.delivered(t)
	// The learn step runs once every task is merged and before the goal
	// closes: its PR is open and the goal still is too.
	d := domain.Decision{AccountID: r.account, ProjectID: r.project.ID, GoalID: r.goal.ID, Question: "Which queue?", Choice: "postgres", DecidedBy: "user:ana", Reasons: "r"}
	must(t, r.st.Decisions().Create(t.Context(), &d))
	hist := r.waitStep(t, "learning/1/learn_retro", domain.StepWaiting)
	if r.goalNow(t).Status == domain.GoalDone {
		t.Fatal("the goal closed before its learn step")
	}
	r.finishJob(t, hist.JobID, []byte(`{"summary":"Nothing surprising.","proposals":[],"assumptions":[]}`), "", 0.2)
	g := r.pendingGate(t, domain.GateKnowledgeProposal)
	pr, err := r.tracker.FindPR(t.Context(), "acme/app", proposals.LearnBranch(r.goal))
	must(t, err)
	if g.SubjectNumber != pr.Number || pr.Merged || r.goalNow(t).Status == domain.GoalDone || r.goalIssue(t).State != "open" {
		t.Fatalf("gate %+v, pr %+v, goal %s", g, pr, r.goalNow(t).Status)
	}
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	r.waitStep(t, "learning/1/learn_merged/0/learn_checks", domain.StepWaiting)
	checksDone(t, r.runner, pr)
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, pr.Number).Merged || r.goalNow(t).Status != domain.GoalDone || r.goalIssue(t).State != "closed" {
		t.Errorf("learn pr merged = %v, goal %s", r.prNow(t, pr.Number).Merged, r.goalNow(t).Status)
	}
	// The approved learn step ends by starting the advisor.
	r.waitStep(t, "learning/1/learn_advise", domain.StepSucceeded)
	if !r.advisorStarted() {
		t.Errorf("no advise run after the learn step: %+v", r.advised.Starts())
	}
}

// A large, hard to reverse goal gets both councils: the product council before
// the spec, the technical one after its approval. The brief of the first is
// linked in the spec gate, the second makes the architect write the ADR, and a
// human merges it before the tasks start.
func TestGoalV3LargeGoalRunsBothCouncils(t *testing.T) {
	r := startGoalV3(t, nil)
	r.triaged(t, triageOutput("high", "hard", "payments", "checkout"))
	r.spec(t, 1, "A payment succeeds.", "A declined card shows an error.")

	calls := r.councilCalls()
	if len(calls) != 1 || calls[0].Kind != domain.CouncilProduct || calls[0].Question != "Should we build this, and what exactly?" ||
		len(calls[0].Constraints) != 0 {
		t.Fatalf("councils before the spec = %+v", calls)
	}
	if got := r.goalNow(t).Size; got == nil || !slices.Equal(got.Councils, []domain.CouncilKind{domain.CouncilProduct, domain.CouncilTechnical}) {
		t.Errorf("size = %+v", got)
	}
	g := r.pendingGate(t, domain.GateSpecApproval)
	if !strings.Contains(g.Summary, "COUNCIL-1") || !strings.Contains(g.Summary, "the product council") || !strings.Contains(g.Summary, "the simple way") {
		t.Errorf("the spec gate does not link the brief:\n%s", g.Summary)
	}
	if n := len(r.councilCalls()); n != 1 {
		t.Fatalf("the technical council started before the spec was approved: %d councils", n)
	}
	r.decide(t, domain.GateSpecApproval, mcp.ToolApprove, "")
	arch := r.request(r.waitStep(t, "architecture/1/design", domain.StepWaiting).JobID)
	pr := r.adrPushed(t)
	calls = r.councilCalls()
	if len(calls) != 2 || calls[1].Kind != domain.CouncilTechnical || calls[1].Question != "How should we build it?" {
		t.Fatalf("councils = %+v", calls)
	}
	if arch.Role != "architect" || arch.Pool != domain.PoolBuild || arch.Branch != "agent/adr-GOAL-1" || !slices.Equal(arch.WritePaths, []string{"docs/adr/**"}) ||
		!strings.Contains(arch.Prompt, "docs/adr/") {
		t.Errorf("architect request = %+v", arch)
	}

	// The ADR is a PR a human approves before anything else is built.
	r.waitStep(t, "adr_review/1/adr_delivery/0/adr_checks", domain.StepWaiting)
	if !strings.Contains(pr.Title, "ADR of GOAL-1") {
		t.Errorf("adr pr = %+v", pr)
	}
	checksDone(t, r.runner, pr)
	r.pendingGate(t, domain.GateMergeApproval)
	r.noStep(t, "tasks/")
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitStep(t, "tasks/0/tests/1/write_tests", domain.StepWaiting)
	if !r.prNow(t, pr.Number).Merged {
		t.Errorf("the ADR was not merged: %+v", r.prNow(t, pr.Number))
	}
}

// A rejected ADR stops the run and is not merged; an architect that wrote no
// ADR leaves nothing to merge.
func TestGoalV3ADROnlyWhenWritten(t *testing.T) {
	t.Run("not written", func(t *testing.T) {
		r := startGoalV3(t, &domain.GoalSize{Impact: domain.LevelMedium, Reversibility: domain.ReversibleEasy, Councils: []domain.CouncilKind{domain.CouncilTechnical}, Reason: "A human chose."})
		r.noStep(t, "triage")
		r.approveSpec(t)
		arch := r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
		if len(arch.WritePaths) != 1 || arch.Branch != "agent/adr-GOAL-1" {
			t.Errorf("architect request = %+v", arch)
		}
		r.waitStep(t, "tasks/0/tests/1/write_tests", domain.StepWaiting)
		r.noStep(t, "adr_review/1/adr_delivery/0/")
		if _, err := r.tracker.FindPR(t.Context(), "acme/app", "agent/adr-GOAL-1"); err == nil {
			t.Error("a PR for an ADR nobody wrote")
		}
	})
	t.Run("rejected", func(t *testing.T) {
		r := startGoalV3(t, &domain.GoalSize{Impact: domain.LevelMedium, Reversibility: domain.ReversibleEasy, Councils: []domain.CouncilKind{domain.CouncilTechnical}, Reason: "A human chose."})
		r.approveSpec(t)
		pr := r.adrPushed(t)
		r.waitStep(t, "adr_review/1/adr_delivery/0/adr_checks", domain.StepWaiting)
		checksDone(t, r.runner, pr)
		r.decide(t, domain.GateMergeApproval, mcp.ToolReject, "not now")
		r.waitRun(t, domain.RunCanceled)
		if r.prNow(t, pr.Number).Merged {
			t.Error("a rejected ADR was merged")
		}
	})
}

// Changes requested on the ADR send the architect back to it with the
// feedback, not out of the run; the new head is reviewed again and merged.
func TestGoalV3ADRChangesRequested(t *testing.T) {
	r := startGoalV3(t, &domain.GoalSize{Impact: domain.LevelMedium, Reversibility: domain.ReversibleEasy, Councils: []domain.CouncilKind{domain.CouncilTechnical}, Reason: "A human chose."})
	r.approveSpec(t)
	pr := r.adrPushed(t)
	r.waitStep(t, "adr_review/1/adr_delivery/0/adr_checks", domain.StepWaiting)
	checksDone(t, r.runner, pr)
	g := r.pendingGate(t, domain.GateMergeApproval)
	if !strings.Contains(g.Summary, "request changes") {
		t.Errorf("the gate does not say what changes requested do: %s", g.Summary)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolRequestChanges, "say more about the options")

	const second = "adr0000000000000000000000000000000000002"
	redo := r.request(r.waitStep(t, "adr_review/2/adr_rework/0/adr_redo", domain.StepWaiting).JobID)
	if redo.Role != "architect" || !strings.Contains(redo.Feedback, "say more about the options") || redo.Pool != domain.PoolBuild ||
		redo.Branch != "agent/adr-GOAL-1" || !slices.Equal(redo.WritePaths, []string{"docs/adr/**"}) {
		t.Errorf("rework request = %+v", redo)
	}
	must(t, r.tracker.SetHeadOfBranch("acme/app", "agent/adr-GOAL-1", second))
	r.answer(t, "adr_review/2/adr_rework/0/adr_redo", planOutput(planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"})), 0.5, second)
	r.waitStep(t, "adr_review/2/adr_delivery/0/adr_pr", domain.StepSucceeded)
	must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, second))
	pr.HeadSHA = second
	r.waitStep(t, "adr_review/2/adr_delivery/0/adr_checks", domain.StepWaiting)
	checksDone(t, r.runner, pr)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitStep(t, "tasks/0/tests/1/write_tests", domain.StepWaiting)
	if !r.prNow(t, pr.Number).Merged {
		t.Errorf("the ADR was not merged: %+v", r.prNow(t, pr.Number))
	}
}

// A human who chose the councils when creating the goal skips triage, also
// when the choice is none.
func TestGoalV3OverrideSkipsTriage(t *testing.T) {
	size := &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."}
	r := startGoalV3(t, size)
	req := r.spec(t, 1, "A payment succeeds.", "A declined card shows an error.")
	if req.Role != "product-owner" {
		t.Errorf("request = %+v", req)
	}
	jobs, err := r.st.Jobs().List(t.Context(), r.account, store.JobFilter{Role: "triage"})
	must(t, err)
	if len(jobs) != 0 || len(r.councilCalls()) != 0 {
		t.Errorf("triage jobs %d, councils %+v", len(jobs), r.councilCalls())
	}
	if got := r.goalNow(t).Size; got == nil || len(got.Councils) != 0 || got.Reason != size.Reason {
		t.Errorf("size = %+v", got)
	}
}

// A learn step that fails does not fail the goal: a needs_you gate asks, and
// approving it skips the step. The proposals nobody sent are rejected.
func TestGoalV3FailedLearnCanBeSkipped(t *testing.T) {
	r := startGoalV3(t, &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."})
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
	r.delivered(t)

	j := domain.Job{AccountID: r.account, ProjectID: r.project.ID, Role: "knowledge-curator", Runtime: "claude-code", GoalID: r.goal.ID, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, r.st.Jobs().Create(t.Context(), &j))
	r.failJob(t, "learning/1/learn_retro", "the retro broke")
	g := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "learn_retro") || !strings.Contains(g.Summary, "skip it") || r.goalNow(t).Status == domain.GoalDone {
		t.Errorf("gate = %+v, goal %s", g, r.goalNow(t).Status)
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if r.goalNow(t).Status != domain.GoalDone || r.goalIssue(t).State != "closed" {
		t.Errorf("goal %s, issue %s", r.goalNow(t).Status, r.goalIssue(t).State)
	}
	if _, err := r.tracker.FindPR(t.Context(), "acme/app", proposals.LearnBranch(r.goal)); err == nil {
		t.Error("a learn PR for a skipped step")
	}
	// A skipped learn step starts no advisor.
	r.noStep(t, "learning/1/learn_advise")
	if r.advisorStarted() {
		t.Errorf("an advise run after a skipped learn step: %+v", r.advised.Starts())
	}
}

// A learn step that fails after its PR was opened (the head moved before the
// merge) is skipped the same way: the PR is closed and the proposals are
// rejected, and the goal closes.
func TestGoalV3SkipAfterTheLearnPROpened(t *testing.T) {
	ctx := t.Context()
	r := startGoalV3(t, &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."})
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
	r.delivered(t)
	hist := r.waitStep(t, "learning/1/learn_retro", domain.StepWaiting)
	j, err := r.st.Jobs().Get(ctx, r.account, hist.JobID)
	must(t, err)
	j = testkit.Lease(t, r.st, j)
	k, err := r.proposals.Submit(ctx, j, proposals.Input{Section: "lessons", Path: "knowledge/lessons/a.md", Action: domain.KnowledgeAdd, Body: "# A\n", Reason: "JOB-1"})
	must(t, err)
	r.finishJob(t, hist.JobID, []byte(`{"summary":"s","proposals":[],"assumptions":[]}`), "", 0.2)
	r.pendingGate(t, domain.GateKnowledgeProposal)
	pr, err := r.tracker.FindPR(ctx, "acme/app", proposals.LearnBranch(r.goal))
	must(t, err)
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	r.waitStep(t, "learning/1/learn_merged/0/learn_checks", domain.StepWaiting)
	must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, "feedfacefeedfacefeedfacefeedfacefeedface"))
	checksDone(t, r.runner, pr)
	g := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "learn_merge") || !strings.Contains(g.Summary, "skip it") {
		t.Errorf("gate = %+v", g)
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	got, err := r.st.KnowledgeProposals().Get(ctx, r.account, k.ID)
	must(t, err)
	if got.Status != domain.KnowledgeRejected || r.prNow(t, pr.Number).State != "closed" || r.goalNow(t).Status != domain.GoalDone {
		t.Errorf("proposal %s, pr %+v, goal %s", got.Status, r.prNow(t, pr.Number), r.goalNow(t).Status)
	}
}

// A learn step whose gate a human rejects closes its PR, the goal closes, and
// no advisor starts on what the human turned down.
func TestGoalV3RejectedLearnStartsNoAdvisor(t *testing.T) {
	r := startGoalV3(t, &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."})
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
	r.delivered(t)
	// A decision of the goal gives the learn step a file to write, so a gate.
	d := domain.Decision{AccountID: r.account, ProjectID: r.project.ID, GoalID: r.goal.ID, Question: "Which queue?", Choice: "postgres", DecidedBy: "user:ana", Reasons: "r"}
	must(t, r.st.Decisions().Create(t.Context(), &d))
	hist := r.waitStep(t, "learning/1/learn_retro", domain.StepWaiting)
	r.finishJob(t, hist.JobID, []byte(`{"summary":"s","proposals":[],"assumptions":[]}`), "", 0.2)
	r.pendingGate(t, domain.GateKnowledgeProposal)
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolReject, "not these")
	r.waitRun(t, domain.RunSucceeded)
	s := r.waitStep(t, "learning/1/learn_advise", domain.StepSucceeded)
	if !strings.Contains(string(s.Output), `"started":false`) || r.advisorStarted() || r.goalNow(t).Status != domain.GoalDone {
		t.Errorf("advise %s, starts %+v, goal %s", s.Output, r.advised.Starts(), r.goalNow(t).Status)
	}
}

// Rejecting the gate of a failed learn step stops the run: the goal is
// canceled like a goal that fails anywhere else.
func TestGoalV3FailedLearnRejected(t *testing.T) {
	r := startGoalV3(t, &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."})
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
	r.delivered(t)
	r.failJob(t, "learning/1/learn_retro", "the retro broke")
	r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	r.waitRun(t, domain.RunCanceled)
	if r.goalNow(t).Status != domain.GoalCanceled {
		t.Errorf("goal %s", r.goalNow(t).Status)
	}
}

// A goal whose run ends before the learn step rejects what its jobs proposed
// for the knowledge base.
func TestGoalV3CanceledGoalRejectsItsProposals(t *testing.T) {
	r := startGoalV3(t, &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."})
	j := domain.Job{AccountID: r.account, ProjectID: r.project.ID, Role: "knowledge-curator", Runtime: "claude-code", GoalID: r.goal.ID, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, r.st.Jobs().Create(t.Context(), &j))
	r.waitStep(t, "spec/1/write", domain.StepWaiting)
	k := domain.KnowledgeProposal{AccountID: r.account, ProjectID: r.project.ID, GoalID: r.goal.ID, Batch: domain.BatchPerGoal, Status: domain.KnowledgeQueued,
		Section: "lessons", Path: "knowledge/lessons/a.md", Action: domain.KnowledgeAdd, Body: "# A\n", Reason: "JOB-1", Provider: "repo", JobID: j.ID}
	must(t, r.st.KnowledgeProposals().Create(t.Context(), &k))
	r.failJob(t, "spec/1/write", "the product owner broke")
	r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	r.waitRun(t, domain.RunCanceled)
	got, err := r.st.KnowledgeProposals().Get(t.Context(), r.account, k.ID)
	must(t, err)
	if got.Status != domain.KnowledgeRejected {
		t.Errorf("proposal = %s", got.Status)
	}
}

// A high-impact assumption that is not accepted keeps the merge gate of the
// task closed, in this playbook too.
func TestGoalV3HighImpactAssumptionBlocksTheMergeGate(t *testing.T) {
	r := startGoalV3(t, &domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "Chosen by the person who created the goal."}, true)
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
	r.writeTests(t, 1, testMap(nil))
	r.push(t, 1)
	r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.verify(t, passedRun, 0, nil)
	r.review(t, 0, 1, 1, approveReview())
	r.waitStep(t, build(0, 1, 1, "gated/0/reviewed/1/hold"), domain.StepSucceeded)
	r.notYetAssessed(t)
	for _, statement := range []string{"Stripe is the processor", "Amounts are in cents", "Cards only"} {
		r.notYetAssessed(t)
		r.decideGate(t, r.reviewGate(t, r.assumption(t, statement)), mcp.ToolApprove, "")
	}
	r.pendingGate(t, domain.GateMergeApproval)
}

// goal-v3 keeps what makes a merge safe in goal-v2: the verification report
// gates assess and the merge, the merge is of the head that was approved, and
// the developer works with the tests locked.
func TestGoalV3KeepsTheMergeGate(t *testing.T) {
	books, err := workflow.LoadPlaybooks(forecrew.Files, "packs/playbooks")
	must(t, err)
	var assess, merges, developers []workflow.Step
	var others []string
	walkSteps(books["goal-v3"].Steps, func(s workflow.Step) {
		switch {
		case s.Action == "goal.assess":
			assess = append(assess, s)
		case s.Action == "github.merge_pr" && s.With["require_verification"] != nil:
			merges = append(merges, s)
		case s.Action == "github.merge_pr":
			others = append(others, s.ID)
		case s.Type == workflow.StepJob && s.With["role"] == "developer":
			developers = append(developers, s)
		}
	})
	if len(assess) != 1 || len(merges) != 1 || len(developers) != 1 {
		t.Fatalf("assess %d, merges %d, developers %d", len(assess), len(merges), len(developers))
	}
	for _, s := range append(assess, merges...) {
		if s.With["require_verification"] != true || s.With["sha"] == nil {
			t.Errorf("%s lacks the verification or the head: %+v", s.ID, s.With)
		}
	}
	if developers[0].With["locked_paths"] == nil {
		t.Errorf("the developer is not locked: %+v", developers[0].With)
	}
	// Every other merge of the playbook is a docs PR a human approved: the
	// ADR and the learn PR.
	if !slices.Equal(others, []string{"adr_merge", "learn_merge"}) {
		t.Errorf("merges without the report: %v", others)
	}
}

// walkSteps calls f on every step of steps, nested ones included.
func walkSteps(steps []workflow.Step, f func(workflow.Step)) {
	for _, s := range steps {
		f(s)
		walkSteps(s.Steps, f)
	}
}

// The learn step of goal-v3 is the steps of learn.yaml with the ids prefixed
// and the failure handling of the loop around them: a file cannot include
// another, so the copy is kept equal by this test.
func TestGoalV3LearnIsTheLearnPlaybooks(t *testing.T) {
	learn, err := os.ReadFile("../../packs/playbooks/learn.yaml")
	must(t, err)
	v3, err := os.ReadFile("../../packs/playbooks/goal-v3.yaml")
	must(t, err)
	body, _, _ := strings.Cut(strings.SplitN(string(learn), "steps:\n", 2)[1], "finally:")
	var lines []string
	for l := range strings.SplitSeq(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(l) != "on_failure: escalate" {
			lines = append(lines, l)
		}
	}
	body = strings.Join(lines, "\n")
	for _, id := range []string{"history", "retro", "learn", "human", "decide", "merged", "followed", "settle", "decision", "checks", "merge", "pr", "advise"} {
		body = regexp.MustCompile(`\bid: `+id+`\b`).ReplaceAllString(body, "id: learn_"+id)
		body = regexp.MustCompile(`steps\.`+id+`\.`).ReplaceAllString(body, "steps.learn_"+id+".")
	}
	var want []string
	for l := range strings.SplitSeq(body, "\n") {
		if l != "" {
			l = "    " + l
		}
		want = append(want, l)
	}
	if !strings.Contains(string(v3), strings.Join(want, "\n")) {
		t.Errorf("the learn step of goal-v3.yaml differs from learn.yaml: change both")
	}
}
