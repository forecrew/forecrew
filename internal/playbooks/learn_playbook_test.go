package playbooks_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"uuid"

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

const learnPolicy = `version: 1
providers:
  wiki: {kind: confluence, instance: acme.atlassian.net}
sections:
  architecture: {paths: ["docs/adr/**"], owner: architect}
  lessons: {paths: ["knowledge/lessons/**"], owner: knowledge-curator}
  conventions: {paths: [CONTRIBUTING.md], owner: codeowners}
  vision: {paths: ["knowledge/vision/**"], owner: human, protected: true}
  runbooks: {provider: wiki, paths: ["space:ENG"], owner: developer}
`

// learnFlow is a goal that finished with two assumptions and one decision,
// and the learn playbook for it.
type learnFlow struct {
	*runner
	svc  *proposals.Service
	goal domain.Goal
	// policy is what the service reads; tests change it.
	policy *knowledge.Policy
}

func newLearnFlow(t *testing.T, seed ...func(v *env, goal domain.Goal)) *learnFlow {
	t.Helper()
	v := newEnv(t, domain.ProjectOnboarded)
	policy, err := knowledge.ParsePolicy([]byte(learnPolicy))
	must(t, err)
	svc := &proposals.Service{
		Store: v.st, GitHub: v.tracker, Gates: v.gates, Roles: testRoles(),
		Policy: func(context.Context, domain.Project) (knowledge.Policy, error) { return policy, nil },
	}
	v.proposals = svc
	v.register()
	must(t, v.tracker.SetFile("acme/app", "head1", "README.md", []byte("# app\n")))
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "head1"))

	g := v.newGoal(t)
	regs := registers.New(v.st, v.bus, v.clock.Now)
	stated := func(s string) domain.StatedAssumption {
		return domain.StatedAssumption{Statement: s, Confidence: domain.LevelMedium, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest}
	}
	as, err := regs.Record(t.Context(), v.account, v.project.ID, registers.Source{MadeBy: "architect", GoalID: g.ID}, []domain.StatedAssumption{stated("One database"), stated("UTC everywhere")})
	must(t, err)
	d := domain.Decision{AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID, Question: "Which queue?", Choice: "postgres", Reasons: "one dependency less",
		DecidedBy: "user:ana", Options: []domain.DecisionOption{{Name: "postgres"}, {Name: "nats"}}, Assumptions: []uuid.UUID{as[0].ID}}
	must(t, v.st.Decisions().Create(t.Context(), &d))

	for _, f := range seed { // before the run reads the history
		f(v, g)
	}
	r := startRun(t, v, proposals.PlaybookLearn, workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": "acme/app", "goal_id": g.ID.String()},
	})
	return &learnFlow{runner: r, svc: svc, goal: g, policy: &policy}
}

// retro waits for the retro job and returns it, as the job service would
// give it to a worker.
func (f *learnFlow) retro(t *testing.T) domain.Job {
	t.Helper()
	s := f.waitStep(t, "retro", domain.StepWaiting)
	j, err := f.st.Jobs().Get(t.Context(), f.account, s.JobID)
	must(t, err)
	return testkit.Lease(t, f.st, j) // proposals are made by a job that runs
}

func (f *learnFlow) propose(t *testing.T, j domain.Job, section, path, reason string) domain.KnowledgeProposal {
	t.Helper()
	k, err := f.svc.Submit(t.Context(), j, proposals.Input{Section: section, Path: path, Action: domain.KnowledgeAdd, Body: "# " + path + "\n\nText.\n", Reason: reason})
	must(t, err)
	return k
}

func (f *learnFlow) proposal(t *testing.T, k domain.KnowledgeProposal) domain.KnowledgeProposal {
	t.Helper()
	got, err := f.st.KnowledgeProposals().Get(t.Context(), f.account, k.ID)
	must(t, err)
	return got
}

func (f *learnFlow) pr(t *testing.T) github.PullRequest {
	t.Helper()
	pr, err := f.tracker.FindPR(t.Context(), "acme/app", proposals.LearnBranch(f.goal))
	must(t, err)
	return pr
}

func (f *learnFlow) file(t *testing.T, path string) string {
	t.Helper()
	b, err := f.tracker.GetFile(t.Context(), "acme/app", proposals.LearnBranch(f.goal), path)
	must(t, err)
	return string(b)
}

// The retro job gets the goal's history, read-only; what it proposes waits for
// the learn step, which writes one PR with the registers and the proposals and
// asks a human. Approving merges it after the checks.
func TestLearnWritesOnePRAndAsksAHuman(t *testing.T) {
	var kept domain.KnowledgeEntry
	f := newLearnFlow(t, func(v *env, g domain.Goal) {
		// A note of the goal that the retro cites.
		kept = domain.KnowledgeEntry{AccountID: v.account, ProjectID: v.project.ID, Provider: domain.ProviderInternal, Path: "notes/a", Section: domain.SectionNotes, Title: "Disk trick",
			Body: "Spool on tmpfs.", Scope: domain.NoteScopeGoal, GoalID: g.ID, Provenance: domain.Provenance{AuthorRole: "developer"}}
		must(t, v.st.Knowledge().Upsert(t.Context(), &kept))
	})
	ctx := t.Context()
	j := f.retro(t)
	reqs := f.jobs.Requests()
	if len(reqs) != 1 || reqs[0].Role != "knowledge-curator" || reqs[0].Task != "retro" || reqs[0].GoalID != f.goal.ID ||
		!strings.Contains(reqs[0].Feedback, "Add checkout") || !strings.Contains(reqs[0].Feedback, "note:"+kept.ID.String()) {
		t.Fatalf("retro request = %+v", reqs)
	}
	lesson := f.propose(t, j, "lessons", "knowledge/lessons/spool.md", "Cost two reruns, JOB-3; note:"+kept.ID.String())
	arch := f.propose(t, j, "architecture", "docs/adr/9-queue.md", "Decided at DEC-1")
	if lesson.Batch != domain.BatchPerGoal || lesson.Status != domain.KnowledgeQueued {
		t.Fatalf("proposal = %+v", lesson)
	}
	f.finishJob(t, j.ID, []byte(`{}`), "", 0.4)

	g := f.pendingGate(t, domain.GateKnowledgeProposal)
	pr := f.pr(t)
	if g.SubjectNumber != pr.Number || g.GoalID != f.goal.ID || !strings.Contains(g.Title, f.goal.Ref()) ||
		!strings.Contains(g.Summary, lesson.Ref()) || !strings.Contains(g.Summary, ".forecrew/assumptions/A-1.yaml") || !strings.Contains(g.Summary, "<untrusted-data") {
		t.Errorf("gate = %+v", g)
	}
	f.checkPRFiles(t)
	for _, k := range []domain.KnowledgeProposal{lesson, arch} {
		if got := f.proposal(t, k); got.Status != domain.KnowledgeOpen || got.GateID != g.ID || got.ChangeID == uuid.Nil() {
			t.Errorf("%s = %+v", k.Ref(), got)
		}
	}
	if pr.Merged {
		t.Fatalf("merged before the decision: %+v", pr)
	}
	if n, _ := f.st.Knowledge().Get(ctx, f.account, kept.ID); n.PromotedAt.IsZero() {
		t.Error("the cited note was not promoted")
	}

	f.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	f.waitStep(t, "merged/0/checks", domain.StepWaiting)
	checksDone(t, f.runner, pr)
	f.waitRun(t, domain.RunSucceeded)
	if got, _ := f.tracker.GetPR(ctx, "acme/app", pr.Number); !got.Merged {
		t.Errorf("pr = %+v", got)
	}
	for _, k := range []domain.KnowledgeProposal{lesson, arch} {
		if got := f.proposal(t, k); got.Status != domain.KnowledgeAccepted {
			t.Errorf("%s = %s", k.Ref(), got.Status)
		}
	}
}

// checkPRFiles checks the learn PR has the register files and the entries.
func (f *learnFlow) checkPRFiles(t *testing.T) {
	t.Helper()
	for _, path := range []string{".forecrew/assumptions/A-1.yaml", ".forecrew/assumptions/A-2.yaml", "docs/decisions/DEC-1.md", "knowledge/lessons/spool.md", "docs/adr/9-queue.md"} {
		if got := f.file(t, path); got == "" {
			t.Errorf("%s is not in the PR", path)
		}
	}
	if !strings.Contains(f.file(t, "docs/decisions/DEC-1.md"), "Which queue?") || !strings.Contains(f.file(t, "knowledge/lessons/spool.md"), "source: "+f.goal.Ref()) {
		t.Error("the files are not what the registers and the proposal say")
	}
}

func (f *learnFlow) pendingGates(t *testing.T) []domain.Gate {
	t.Helper()
	gs, err := f.gates.ListPending(t.Context(), f.account, f.project.ID)
	must(t, err)
	return slices.DeleteFunc(gs, func(g domain.Gate) bool { return g.Kind != domain.GateKnowledgeProposal })
}

// Rejecting the gate closes the PR and rejects the proposals; the run ends
// well.
func TestLearnRejectedClosesThePR(t *testing.T) {
	f := newLearnFlow(t)
	j := f.retro(t)
	k := f.propose(t, j, "lessons", "knowledge/lessons/spool.md", "JOB-3")
	f.finishJob(t, j.ID, []byte(`{}`), "", 0)
	f.pendingGate(t, domain.GateKnowledgeProposal)
	pr := f.pr(t)
	f.decide(t, domain.GateKnowledgeProposal, mcp.ToolReject, "not now")
	f.waitRun(t, domain.RunSucceeded)
	if got, _ := f.tracker.GetPR(t.Context(), "acme/app", pr.Number); got.State != "closed" || got.Merged {
		t.Errorf("pr = %+v", got)
	}
	if got := f.proposal(t, k); got.Status != domain.KnowledgeRejected {
		t.Errorf("proposal = %s", got.Status)
	}
}

// A goal with nothing to learn opens no PR and no gate.
func TestLearnWithNothingToLearn(t *testing.T) {
	f := newLearnFlow(t)
	ctx := t.Context()
	// The registers of the goal are already in the repo, as after an earlier
	// learn step.
	files, err := registers.New(f.st, f.bus, f.clock.Now).Export(ctx, f.account, f.goal.ID)
	must(t, err)
	for path, content := range files {
		must(t, f.tracker.SetFile("acme/app", "head2", path, content))
		must(t, f.tracker.SetFile("acme/app", "main", path, content))
	}
	must(t, f.tracker.SetHeadOfBranch("acme/app", "main", "head2"))
	j := f.retro(t)
	f.finishJob(t, j.ID, []byte(`{}`), "", 0)
	f.waitRun(t, domain.RunSucceeded)
	if gs := f.pendingGates(t); len(gs) != 0 {
		t.Errorf("gates = %+v", gs)
	}
	if _, err := f.tracker.FindPR(ctx, "acme/app", proposals.LearnBranch(f.goal)); err == nil {
		t.Error("a PR was opened")
	}
}

// A proposal for a provider without a review flow goes to the same gate, which
// shows the text; approving says it was copied across. The PR holds the repo's.
func TestLearnGatesProposalsOfProvidersWithoutAReviewFlow(t *testing.T) {
	f := newLearnFlow(t)
	j := f.retro(t)
	wiki := f.propose(t, j, "runbooks", "space:ENG/page:7", "JOB-3")
	repo := f.propose(t, j, "lessons", "knowledge/lessons/x.md", "JOB-3")
	f.finishJob(t, j.ID, []byte(`{}`), "", 0)
	g := f.pendingGate(t, domain.GateKnowledgeProposal)
	if !strings.Contains(g.Summary, "no review flow") || !strings.Contains(g.Summary, "space:ENG/page:7") {
		t.Errorf("summary = %s", g.Summary)
	}
	if got := f.proposal(t, wiki); got.Status != domain.KnowledgeOpen || got.GateID != g.ID || got.ChangeID != uuid.Nil() {
		t.Errorf("wiki proposal = %+v", got)
	}
	pr := f.pr(t)
	f.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	f.waitStep(t, "merged/0/checks", domain.StepWaiting)
	checksDone(t, f.runner, pr)
	f.waitRun(t, domain.RunSucceeded)
	for _, k := range []domain.KnowledgeProposal{wiki, repo} {
		if got := f.proposal(t, k); got.Status != domain.KnowledgeAccepted {
			t.Errorf("%s = %s", k.Ref(), got.Status)
		}
	}
}

// The policy is read again when the learn step sends: a proposal it no longer
// allows is dropped and audited, and one to a section that became protected
// is left to a human's own proposal.
func TestLearnChecksThePolicyAgain(t *testing.T) {
	f := newLearnFlow(t)
	ctx := t.Context()
	j := f.retro(t)
	gone := f.propose(t, j, "architecture", "docs/adr/9-a.md", "JOB-3")
	protected := f.propose(t, j, "lessons", "knowledge/lessons/b.md", "JOB-3")
	fine := f.propose(t, j, "runbooks", "space:ENG/page:7", "JOB-3")
	changed := strings.NewReplacer(
		`architecture: {paths: ["docs/adr/**"], owner: architect}`, `architecture: {paths: ["docs/adr/**"], owner: architect, access: {propose: [nobody]}}`,
		`lessons: {paths: ["knowledge/lessons/**"], owner: knowledge-curator}`, `lessons: {paths: ["knowledge/lessons/**"], owner: knowledge-curator, protected: true}`,
	).Replace(learnPolicy)
	next, err := knowledge.ParsePolicy([]byte(changed))
	must(t, err)
	*f.policy = next
	f.finishJob(t, j.ID, []byte(`{}`), "", 0)

	g := f.pendingGate(t, domain.GateKnowledgeProposal)
	if strings.Contains(g.Summary, gone.Ref()) || strings.Contains(g.Summary, protected.Ref()) || !strings.Contains(g.Summary, fine.Ref()) {
		t.Errorf("summary = %s", g.Summary)
	}
	for _, k := range []domain.KnowledgeProposal{gone, protected} {
		if got := f.proposal(t, k); got.Status != domain.KnowledgeRejected {
			t.Errorf("%s = %s, want rejected", k.Ref(), got.Status)
		}
	}
	if got := f.proposal(t, fine); got.Status != domain.KnowledgeOpen {
		t.Errorf("fine = %s", got.Status)
	}
	rows, err := f.st.Audit().List(ctx, f.account, store.AuditFilter{})
	must(t, err)
	if n := len(slices.DeleteFunc(rows, func(a domain.AuditEntry) bool { return a.Action != "knowledge.proposal_dropped" })); n != 2 {
		t.Errorf("%d proposals dropped in the audit log, want 2", n)
	}
}

// A learn run that fails closes its PR and rejects what is in review: here a
// push after the approval, which is not what the human approved, makes the
// merge escalate, and rejecting that stops the run.
func TestLearnStoppedClosesThePR(t *testing.T) {
	f := newLearnFlow(t)
	j := f.retro(t)
	k := f.propose(t, j, "lessons", "knowledge/lessons/spool.md", "JOB-3")
	f.finishJob(t, j.ID, []byte(`{}`), "", 0)
	f.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	f.waitStep(t, "merged/0/checks", domain.StepWaiting)
	pr := f.pr(t)
	must(t, f.tracker.SetHeadSHA("acme/app", pr.Number, "pushed-later"))
	checksDone(t, f.runner, pr)
	g := f.pendingGate(t, domain.GateNeedsYou)
	_, err := f.gates.Decide(t.Context(), f.human, mcp.ToolReject, mcp.GateDecisionInput{Gate: g.Ref()})
	must(t, err)
	f.waitRun(t, domain.RunCanceled)
	if got, _ := f.tracker.GetPR(t.Context(), "acme/app", pr.Number); got.Merged || got.State != "closed" {
		t.Errorf("pr = %+v", got)
	}
	if got := f.proposal(t, k); got.Status != domain.KnowledgeRejected {
		t.Errorf("proposal = %s", got.Status)
	}
}

// A goal that proposed nothing still writes its registers: one PR with the
// files of the two assumptions and the decision, and a gate.
func TestLearnWritesTheRegistersWhenNothingWasProposed(t *testing.T) {
	f := newLearnFlow(t)
	j := f.retro(t)
	f.finishJob(t, j.ID, []byte(`{}`), "", 0)
	g := f.pendingGate(t, domain.GateKnowledgeProposal)
	pr := f.pr(t)
	if g.SubjectNumber != pr.Number || strings.Contains(g.Summary, "### KB-") || !strings.Contains(g.Summary, "docs/decisions/DEC-1.md") {
		t.Errorf("gate = %+v", g)
	}
	for _, path := range []string{".forecrew/assumptions/A-1.yaml", ".forecrew/assumptions/A-2.yaml", "docs/decisions/DEC-1.md"} {
		if f.file(t, path) == "" {
			t.Errorf("%s is not in the PR", path)
		}
	}
	if _, err := f.tracker.GetFile(t.Context(), "acme/app", proposals.LearnBranch(f.goal), "knowledge/lessons/spool.md"); err == nil {
		t.Error("an entry nobody proposed is in the PR")
	}
	f.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	f.waitStep(t, "merged/0/checks", domain.StepWaiting)
	checksDone(t, f.runner, pr)
	f.waitRun(t, domain.RunSucceeded)
	if got, _ := f.tracker.GetPR(t.Context(), "acme/app", pr.Number); !got.Merged {
		t.Errorf("pr = %+v", got)
	}
}
