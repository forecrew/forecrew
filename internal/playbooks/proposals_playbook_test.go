package playbooks_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

const flowPolicy = `version: 1
providers:
  wiki: {kind: confluence, instance: acme.atlassian.net}
sections:
  architecture: {paths: ["docs/adr/**"], owner: architect}
  conventions: {paths: [CONTRIBUTING.md], owner: codeowners}
  vision: {owner: human, protected: true}
  closed: {owner: architect, access: {propose: [architect]}}
  runbooks: {provider: wiki, paths: ["space:ENG"], owner: developer}
`

// proposalFlow is a job of role that proposed something, and the service
// that took it; start runs the proposal's playbook on a real engine.
type proposalFlow struct {
	*env
	svc *proposals.Service
	job domain.Job
}

func newProposalFlow(t *testing.T, role string, goal uuid.UUID) *proposalFlow {
	t.Helper()
	v := newEnv(t, domain.ProjectOnboarded)
	policy, err := knowledge.ParsePolicy([]byte(flowPolicy))
	must(t, err)
	must(t, v.tracker.SetFile("acme/app", "head1", knowledge.PolicyFile, []byte(flowPolicy)))
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	svc := &proposals.Service{
		Store: v.st, GitHub: v.tracker, Gates: v.gates, Roles: testRoles(), Now: v.clock.Now,
		Policy: func(context.Context, domain.Project) (knowledge.Policy, error) { return policy, nil },
	}
	v.proposals = svc
	v.register()
	j := domain.Job{AccountID: v.account, ProjectID: v.project.ID, Role: role, Runtime: "claude-code", GoalID: goal, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, v.st.Jobs().Create(t.Context(), &j))
	return &proposalFlow{env: v, svc: svc, job: j}
}

func (f *proposalFlow) propose(t *testing.T, section, path string) domain.KnowledgeProposal {
	t.Helper()
	k, err := f.svc.Submit(t.Context(), f.job, proposals.Input{Section: section, Path: path, Action: domain.KnowledgeAdd, Body: "# Entry\n", Reason: "We learned it."})
	must(t, err)
	return k
}

func (f *proposalFlow) start(t *testing.T, provider string) *runner {
	t.Helper()
	r := startRun(t, f.env, proposals.PlaybookProposal, workflow.StartRequest{
		AccountID: f.account, ProjectID: f.project.ID,
		Inputs: map[string]any{"repo": "acme/app", "job_id": f.job.ID, "provider": provider},
	})
	r.finishJob(t, f.job.ID, []byte(`{}`), "", 0) // the run starts when the job completes
	return r
}

func (f *proposalFlow) status(t *testing.T, k domain.KnowledgeProposal) domain.KnowledgeProposalStatus {
	t.Helper()
	got, err := f.st.KnowledgeProposals().Get(t.Context(), f.account, k.ID)
	must(t, err)
	return got.Status
}

func (f *proposalFlow) pr(t *testing.T) github.PullRequest {
	t.Helper()
	pr, err := f.tracker.FindPR(t.Context(), "acme/app", "agent/kb-"+f.job.Ref())
	must(t, err)
	return pr
}

func verdict(v string) []byte {
	out, _ := json.Marshal(map[string]any{"verdict": v, "summary": "Read it.", "comments": []any{}, "criteria_check": []any{}, "assumptions": []any{}})
	return out
}

// checksDone reports the head of the PR as checked, like the webhook does.
func checksDone(t *testing.T, r *runner, pr github.PullRequest) {
	t.Helper()
	r.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Conclusion: domain.ChecksNone})
}

func TestProtectedProposalWaitsForAHuman(t *testing.T) {
	f := newProposalFlow(t, "architect", uuid.Nil())
	k := f.propose(t, "vision", "knowledge/vision/why.md")
	r := f.start(t, "repo")

	g := r.pendingGate(t, domain.GateKnowledgeProposal)
	pr := f.pr(t)
	if g.SubjectNumber != pr.Number || !strings.Contains(g.Title, k.Ref()) {
		t.Errorf("gate = %+v", g)
	}
	if pr.Merged || f.status(t, k) != domain.KnowledgeOpen {
		t.Errorf("before the decision: %+v, %s", pr, f.status(t, k))
	}
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	r.waitStep(t, "merged/0/checks", domain.StepWaiting)
	checksDone(t, r, pr)
	r.waitRun(t, domain.RunSucceeded)
	if got, _ := f.tracker.GetPR(t.Context(), "acme/app", pr.Number); !got.Merged {
		t.Errorf("pr = %+v", got)
	}
	if f.status(t, k) != domain.KnowledgeAccepted {
		t.Errorf("status = %s", f.status(t, k))
	}
}

func TestRejectedProposalClosesItsPR(t *testing.T) {
	f := newProposalFlow(t, "architect", uuid.Nil())
	k := f.propose(t, "vision", "knowledge/vision/why.md")
	r := f.start(t, "repo")
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolReject, "Not this.")
	r.waitRun(t, domain.RunSucceeded)
	if f.status(t, k) != domain.KnowledgeRejected {
		t.Errorf("status = %s", f.status(t, k))
	}
	if _, err := f.tracker.FindPR(t.Context(), "acme/app", "agent/kb-"+f.job.Ref()); err == nil {
		t.Error("the PR is still open")
	}
}

func TestRoleOwnerReviewsThenForecrewMerges(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	k := f.propose(t, "architecture", "docs/adr/2.md")
	r := f.start(t, "repo")

	w := r.waitStep(t, "reviews/0/review", domain.StepWaiting)
	req := r.request(w.JobID)
	if req.Role != "architect" || !req.ProposalReview || req.Branch != "agent/kb-"+f.job.Ref() || !strings.Contains(req.Feedback, "<untrusted-data") {
		t.Errorf("review job = %+v", req)
	}
	r.finishJob(t, w.JobID, verdict("approve"), "", 0)
	r.waitStep(t, "merged/0/checks", domain.StepWaiting)
	if gs, _ := r.gates.ListPending(t.Context(), f.account, f.project.ID); len(gs) != 0 {
		t.Errorf("an approved owner review opened gates: %+v", gs)
	}
	checksDone(t, r, f.pr(t))
	r.waitRun(t, domain.RunSucceeded)
	if f.status(t, k) != domain.KnowledgeAccepted {
		t.Errorf("status = %s", f.status(t, k))
	}
	if body, err := f.tracker.GetFile(t.Context(), "acme/app", "main", "docs/adr/2.md"); err != nil || !strings.Contains(string(body), "author_role: developer") {
		t.Errorf("merged file = %q, %v", body, err)
	}
}

func TestDisputedOwnerReviewGoesToAHuman(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	k := f.propose(t, "architecture", "docs/adr/2.md")
	r := f.start(t, "repo")
	w := r.waitStep(t, "reviews/0/review", domain.StepWaiting)
	r.finishJob(t, w.JobID, verdict("request_changes"), "", 0)

	g := r.pendingGate(t, domain.GateKnowledgeProposal)
	if !strings.Contains(g.Summary, "request_changes") || !strings.Contains(g.Summary, "Read it.") {
		t.Errorf("summary = %q", g.Summary)
	}
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolRequestChanges, "Say it shorter.")
	r.waitRun(t, domain.RunSucceeded)
	if f.status(t, k) != domain.KnowledgeRejected {
		t.Errorf("status = %s", f.status(t, k))
	}
}

func TestCodeownersReviewOnGitHub(t *testing.T) {
	f := newProposalFlow(t, "reviewer", uuid.Nil())
	k := f.propose(t, "conventions", "CONTRIBUTING.md")
	r := f.start(t, "repo")
	r.waitStep(t, "followed/0/pr", domain.StepWaiting)
	if gs, _ := r.gates.ListPending(t.Context(), f.account, f.project.ID); len(gs) != 0 {
		t.Errorf("Forecrew has no gate of its own here: %+v", gs)
	}
	if r.jobs.Requests() != nil {
		t.Errorf("jobs = %+v", r.jobs.Requests())
	}
	pr := f.pr(t)
	if pr.Merged {
		t.Fatal("Forecrew must not merge it")
	}
	sha, err := f.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	r.publish(t, &domain.PRMerged{Repo: "acme/app", Number: pr.Number, SHA: sha})
	r.waitRun(t, domain.RunSucceeded)
	if f.status(t, k) != domain.KnowledgeAccepted {
		t.Errorf("status = %s", f.status(t, k))
	}
}

func TestCodeownersClosedPRRejects(t *testing.T) {
	f := newProposalFlow(t, "reviewer", uuid.Nil())
	k := f.propose(t, "conventions", "CONTRIBUTING.md")
	r := f.start(t, "repo")
	r.waitStep(t, "followed/0/pr", domain.StepWaiting)
	pr := f.pr(t)
	must(t, f.tracker.ClosePR("acme/app", pr.Number))
	r.publish(t, &domain.PRClosed{Repo: "acme/app", Number: pr.Number})
	r.waitRun(t, domain.RunSucceeded)
	if f.status(t, k) != domain.KnowledgeRejected {
		t.Errorf("status = %s", f.status(t, k))
	}
}

func TestProviderWithoutProposeGetsAGate(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	k := f.propose(t, "runbooks", "space:ENG/page:9")
	r := f.start(t, "wiki")
	g := r.pendingGate(t, domain.GateKnowledgeProposal)
	if g.SubjectNumber != 0 || !strings.Contains(g.Summary, "# Entry") {
		t.Errorf("gate = %+v", g)
	}
	if _, err := f.tracker.FindPR(t.Context(), "acme/app", "agent/kb-"+f.job.Ref()); err == nil {
		t.Error("no PR for a provider outside the repo")
	}
	r.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if f.status(t, k) != domain.KnowledgeAccepted {
		t.Errorf("status = %s", f.status(t, k))
	}
}

func TestDisallowedProposalIsDroppedAndAudited(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	_, err := f.svc.Submit(t.Context(), f.job, proposals.Input{Section: "closed", Path: "knowledge/closed/a.md", Action: domain.KnowledgeAdd, Body: "x", Reason: "r"})
	if err == nil || !strings.Contains(err.Error(), "may not propose") {
		t.Fatalf("err = %v", err)
	}
	all, err := f.st.Audit().List(t.Context(), f.account, store.AuditFilter{})
	must(t, err)
	found := false
	for _, a := range all {
		found = found || a.Action == "knowledge.proposal_dropped"
	}
	if !found {
		t.Error("not audited")
	}
	rows, err := f.st.KnowledgeProposals().List(t.Context(), f.account, store.KnowledgeProposalFilter{})
	if err != nil || len(rows) != 0 {
		t.Errorf("rows = %+v, %v", rows, err)
	}
}

// A goal's proposal to a section that is not protected stays per_goal: the
// job's completion starts nothing, and sending the job's batch skips it.
func TestGoalProposalWaitsForTheLearnStep(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	g := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "g", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(t.Context(), &g))
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code", GoalID: g.ID, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	f.job = j
	k := f.propose(t, "architecture", "docs/adr/2.md")
	must(t, f.st.WithTx(t.Context(), func(ctx context.Context, tx store.Store) error { return f.svc.JobCompleted(ctx, tx, j) }))
	if runs, _ := f.st.WorkflowRuns().ListActive(t.Context(), f.account); len(runs) != 0 {
		t.Errorf("runs = %+v", runs)
	}
	r := f.start(t, "repo")
	r.waitRun(t, domain.RunSucceeded)
	got, err := f.st.KnowledgeProposals().Get(t.Context(), f.account, k.ID)
	must(t, err)
	if got.Batch != domain.BatchPerGoal || got.Status != domain.KnowledgeQueued || got.ExternalRef != "" {
		t.Errorf("proposal = %+v", got)
	}
}

func TestCanceledRunClosesThePR(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	k := f.propose(t, "architecture", "docs/adr/2.md")
	r := f.start(t, "repo")
	w := r.waitStep(t, "reviews/0/review", domain.StepWaiting)
	pr := f.pr(t)
	j, err := r.st.Jobs().Get(t.Context(), r.account, w.JobID)
	must(t, err)
	leased, err := r.st.Jobs().LeaseNext(t.Context(), r.account, r.worker.ID, j.RequiredLabels(), time.Hour)
	must(t, err)
	_, err = r.st.Jobs().Complete(t.Context(), r.account, leased.ID, r.worker.ID, store.JobResult{Status: domain.JobFailed, Error: "no network"})
	must(t, err)
	r.publish(t, &domain.JobCompleted{JobID: leased.ID, Status: domain.JobFailed})
	r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	r.waitRun(t, domain.RunCanceled)
	if f.status(t, k) != domain.KnowledgeRejected {
		t.Errorf("status = %s", f.status(t, k))
	}
	if got, _ := f.tracker.GetPR(t.Context(), "acme/app", pr.Number); got.State != "closed" {
		t.Errorf("pr = %+v", got)
	}
}

func TestAccessRequestToPolicyPR(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	g, err := f.svc.RequestAccess(t.Context(), f.job, proposals.AccessRequest{Section: "closed", Level: "propose", Reason: "I keep finding gaps."})
	must(t, err)
	runs, err := f.st.WorkflowRuns().ListActive(t.Context(), f.account)
	must(t, err)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	var inputs map[string]any
	must(t, json.Unmarshal(runs[0].Inputs, &inputs))
	// The stored run is the one the engine advances; here a second start
	// with the same inputs stands in for it.
	r := startRun(t, f.env, proposals.PlaybookAccess, workflow.StartRequest{ID: runs[0].ID, AccountID: f.account, ProjectID: f.project.ID, Inputs: inputs})
	r.decide(t, domain.GateAccessRequest, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)

	pr, err := f.tracker.FindPR(t.Context(), "acme/app", "agent/kb-access-"+g.Ref())
	must(t, err)
	if pr.Merged {
		t.Error("the policy PR is merged by a human")
	}
	raw, err := f.tracker.GetFile(t.Context(), "acme/app", pr.Head, knowledge.PolicyFile)
	must(t, err)
	p, err := knowledge.ParsePolicy(raw)
	must(t, err)
	if !p.Has("closed", "propose", "developer") {
		t.Errorf("policy:\n%s", raw)
	}
	if cur, _ := f.tracker.GetFile(t.Context(), "acme/app", "head1", knowledge.PolicyFile); string(cur) != flowPolicy {
		t.Error("the policy changed directly")
	}
}

func TestAccessRequestRejectedChangesNothing(t *testing.T) {
	f := newProposalFlow(t, "developer", uuid.Nil())
	g, err := f.svc.RequestAccess(t.Context(), f.job, proposals.AccessRequest{Section: "closed", Level: "propose", Reason: "r"})
	must(t, err)
	runs, err := f.st.WorkflowRuns().ListActive(t.Context(), f.account)
	must(t, err)
	var inputs map[string]any
	must(t, json.Unmarshal(runs[0].Inputs, &inputs))
	r := startRun(t, f.env, proposals.PlaybookAccess, workflow.StartRequest{ID: runs[0].ID, AccountID: f.account, ProjectID: f.project.ID, Inputs: inputs})
	r.decide(t, domain.GateAccessRequest, mcp.ToolReject, "No.")
	r.waitRun(t, domain.RunSucceeded)
	if _, err := f.tracker.FindPR(t.Context(), "acme/app", "agent/kb-access-"+g.Ref()); err == nil {
		t.Error("a rejected request opened a PR")
	}
}

func TestKnowledgeActionsNeedTheService(t *testing.T) {
	v := newEnv(t)
	for _, name := range []string{"knowledge.propose", "knowledge.assess", "knowledge.decide", "knowledge.settle"} {
		_, err := v.call(t, name, map[string]any{"job_id": domain.NewID().String(), "provider": "repo"})
		wantErr(t, err, "no knowledge base")
	}
	_, err := v.call(t, "knowledge.access_pr", map[string]any{"gate_id": domain.NewID().String()})
	wantErr(t, err, "no knowledge base")
	f := newProposalFlow(t, "developer", uuid.Nil())
	_, err = f.call(t, "knowledge.propose", map[string]any{})
	wantErr(t, err, "job_id and provider")
}
