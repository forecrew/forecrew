package playbooks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/advisor"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// adviseEnv is an onboarded project with an advisor; proposal and advise runs
// run on the engine, and goal runs are only recorded.
type adviseEnv struct {
	*env
	goals []workflow.StartRequest
	gate  int
}

func (a *adviseEnv) Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	if playbook == "goal" {
		a.goals = append(a.goals, req)
		return domain.WorkflowRun{ID: req.ID, Status: domain.RunRunning}, nil
	}
	return a.runs.Start(ctx, playbook, req)
}

func newAdviseEnv(t *testing.T) *adviseEnv {
	t.Helper()
	a := &adviseEnv{env: newEnv(t, domain.ProjectOnboarded)}
	a.cfg = &config.Config{Roles: map[string]config.Role{"advisor": {Runtime: "claude", Tier: config.TierBest}}}
	catalog, err := advisor.LoadCatalog(forecrew.Files)
	must(t, err)
	a.advisor = advisor.New(advisor.Config{Store: a.st, Gates: a.gates, GitHub: a.tracker, Runs: a, Catalog: catalog, Now: a.clock.Now,
		GoalPlaybook: func(domain.Project) string { return "goal" }})
	a.gates.WithRecorder(gates.Recorders{registers.New(a.st, a.bus, a.clock.Now), a.advisor})
	must(t, a.tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	// CI scans, so that the only signals are the ones a test makes.
	must(t, a.tracker.SetFile("acme/app", "main", ".github/dependabot.yml", []byte("# and gitleaks\n")))
	a.register()
	return a
}

// lacking opens n escalations that say tool is missing.
func (a *adviseEnv) lacking(t *testing.T, tool string, n int) {
	t.Helper()
	for range n {
		a.gate++
		_, err := a.gates.Open(t.Context(), gates.OpenRequest{AccountID: a.account, ProjectID: a.project.ID, Kind: domain.GateNeedsYou,
			Title: "Stuck", Summary: fmt.Sprintf("step %d: bash: %s: command not found", a.gate, tool), SubjectRepo: "acme/app"})
		must(t, err)
	}
}

// draft is a proposal as the scripted advisor writes it.
func draft(key, tool, action string) string {
	return fmt.Sprintf(`{"dedupe_key":%q,"signals":[%q],"scope":"project","category":"tooling","title":"Use %s","rationale":"Agents lack it","benefit":"Fewer stops",`+
		`"effort":"s","risk":"low","tools":[%q],"measure":"tooling","action":%s}`, key, key, tool, tool, action)
}

const setupAction = `{"goal":null,"config_pr":null,"setup":{"instructions":"Add it to the toolbox","issue":null}}`

// advise starts an advise run and answers its job with drafts; no drafts
// means no job is expected.
func (a *adviseEnv) advise(t *testing.T, first *runner, drafts ...string) *runner {
	t.Helper()
	var r *runner
	if first == nil {
		r = startRun(t, a.env, "advise", workflow.StartRequest{AccountID: a.account, ProjectID: a.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
	} else {
		run, err := a.runs.Start(t.Context(), "advise", workflow.StartRequest{AccountID: a.account, ProjectID: a.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
		must(t, err)
		c := *first
		r = &c
		r.run = run
	}
	if drafts != nil {
		s := r.waitStep(t, "advise/0/advisor", domain.StepWaiting)
		r.finishJob(t, s.JobID, []byte(`{"summary":"weighed","proposals":[`+strings.Join(drafts, ",")+`],"assumptions":[]}`), "", 0.4)
	}
	r.waitRun(t, domain.RunSucceeded)
	return r
}

func (a *adviseEnv) proposals(t *testing.T) []domain.Proposal {
	t.Helper()
	all, err := a.st.Proposals().List(t.Context(), a.account, store.ProposalFilter{ProjectID: a.project.ID})
	must(t, err)
	return all
}

// of returns a runner on the proposal's own run.
func of(r *runner, p domain.Proposal) *runner {
	c := *r
	c.run.ID = p.ID
	return &c
}

// The scripted advisor writes more than the cap allows, and one proposal for
// a tool outside the catalog: that one never becomes an action, and the cap
// holds for the rest.
func TestAdviseHoldsTheCapAndTheCatalog(t *testing.T) {
	a := newAdviseEnv(t)
	for _, tool := range []string{"trivy", "semgrep", "gitleaks", "syft", "nomad"} {
		a.lacking(t, tool, 2)
	}
	r := startRun(t, a.env, "advise", workflow.StartRequest{AccountID: a.account, ProjectID: a.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
	s := r.waitStep(t, "advise/0/advisor", domain.StepWaiting)
	reqs := r.jobs.Requests()
	if len(reqs) != 1 || reqs[0].Role != "advisor" || !strings.Contains(reqs[0].Feedback, "tooling:nomad") || !strings.Contains(reqs[0].Feedback, "Write at most 3") ||
		!strings.Contains(reqs[0].Feedback, "<untrusted-data") || !strings.Contains(reqs[0].Feedback, "gitleaks (cli, MIT)") {
		t.Fatalf("advisor request = %+v", reqs)
	}
	drafts := []string{draft("tooling:nomad", "nomad", setupAction), draft("tooling:trivy", "trivy", setupAction), draft("tooling:semgrep", "semgrep", setupAction),
		draft("tooling:gitleaks", "gitleaks", setupAction), draft("tooling:syft", "syft", setupAction)}
	r.finishJob(t, s.JobID, []byte(`{"summary":"weighed","proposals":[`+strings.Join(drafts, ",")+`],"assumptions":[]}`), "", 0.4)
	run := r.waitRun(t, domain.RunSucceeded)
	got := a.proposals(t)
	if len(got) != 3 {
		t.Fatalf("%d proposals: %+v", len(got), got)
	}
	for _, p := range got {
		if strings.Contains(p.DedupeKey, "nomad") || strings.Contains(p.Title, "nomad") {
			t.Errorf("a tool outside the catalog became an action: %+v", p)
		}
		of(r, p).waitStep(t, "review/1/decision", domain.StepWaiting)
	}
	steps, err := a.st.StepRuns().ListByRun(t.Context(), a.account, run.ID)
	must(t, err)
	i := slices.IndexFunc(steps, func(s domain.StepRun) bool { return s.StepID == "file" })
	var filed struct {
		Output advisor.Filed `json:"output"`
	}
	must(t, json.Unmarshal(steps[i].Output, &filed))
	if len(filed.Output.Created) != 3 || len(filed.Output.Dropped) != 2 {
		t.Fatalf("filed %+v", filed)
	}
}

// An open proposal gets the new evidence without a new one; a rejected one
// stays away until its evidence doubles.
func TestAdviseDedupes(t *testing.T) {
	a := newAdviseEnv(t)
	a.lacking(t, "trivy", 2)
	first := startRun(t, a.env, "advise", workflow.StartRequest{AccountID: a.account, ProjectID: a.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
	s := first.waitStep(t, "advise/0/advisor", domain.StepWaiting)
	first.finishJob(t, s.JobID, []byte(`{"summary":"weighed","proposals":[`+draft("tooling:trivy", "trivy", setupAction)+`],"assumptions":[]}`), "", 0.4)
	first.waitRun(t, domain.RunSucceeded)

	a.lacking(t, "trivy", 1)
	a.advise(t, first)
	got := a.proposals(t)
	if len(got) != 1 || got[0].EvidenceWeight != 3 || len(got[0].Evidence) != 3 {
		t.Fatalf("an open proposal: %+v", got)
	}
	if n := len(first.jobs.Requests()); n != 1 {
		t.Fatalf("%d advisor jobs: nothing new to answer", n)
	}

	// Rejected at weight 3, it needs 6 to return; the cap is out of the way.
	first.decide(t, domain.GateImprovementProposal, mcp.ToolReject, "We use another scanner")
	of(first, got[0]).waitRun(t, domain.RunSucceeded)
	decisions, err := a.st.Decisions().List(t.Context(), a.account, store.DecisionFilter{ProjectID: a.project.ID})
	must(t, err)
	if len(decisions) != 1 || decisions[0].Choice != string(domain.GateRejected) || decisions[0].Reasons != "We use another scanner" {
		t.Fatalf("decisions %+v", decisions)
	}
	if p := a.proposals(t)[0]; p.Status != domain.ProposalRejected || p.Reason != "We use another scanner" {
		t.Fatalf("rejected: %+v", p)
	}
	a.clock.Advance(8 * 24 * time.Hour)
	a.lacking(t, "trivy", 2)
	a.advise(t, first)
	if n := len(first.jobs.Requests()); n != 1 {
		t.Fatalf("below double: %d advisor jobs", n)
	}
	a.lacking(t, "trivy", 1)
	a.advise(t, first, draft("tooling:trivy", "trivy", setupAction))
	if all := a.proposals(t); len(all) != 2 || all[1].Status != domain.ProposalProposed || all[1].EvidenceWeight != 6 {
		t.Fatalf("doubled: %+v", all)
	}
}

// single files one proposal with action and returns it with its runner.
func (a *adviseEnv) single(t *testing.T, action string) (*runner, domain.Proposal) {
	t.Helper()
	a.lacking(t, "trivy", 2)
	r := startRun(t, a.env, "advise", workflow.StartRequest{AccountID: a.account, ProjectID: a.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
	s := r.waitStep(t, "advise/0/advisor", domain.StepWaiting)
	r.finishJob(t, s.JobID, []byte(`{"summary":"weighed","proposals":[`+draft("tooling:trivy", "trivy", action)+`],"assumptions":[]}`), "", 0.4)
	r.waitRun(t, domain.RunSucceeded)
	return r, a.proposals(t)[0]
}

func TestAcceptedGoalStarts(t *testing.T) {
	a := newAdviseEnv(t)
	r, p := a.single(t, `{"goal":{"title":"Scan images","body":"Add a scan","platforms":[]},"config_pr":null,"setup":null}`)
	r.decide(t, domain.GateImprovementProposal, mcp.ToolApprove, "")
	of(r, p).waitRun(t, domain.RunSucceeded)
	p = a.proposals(t)[0]
	g, err := a.st.Goals().Get(t.Context(), a.account, p.GoalID)
	must(t, err)
	if p.Status != domain.ProposalAccepted || g.Kind != domain.GoalFeature || len(a.goals) != 1 || a.goals[0].GoalID != g.ID {
		t.Fatalf("proposal %+v, goal %+v, runs %+v", p, g, a.goals)
	}
}

func TestAcceptedConfigPRIsDoneWhenMerged(t *testing.T) {
	a := newAdviseEnv(t)
	r, p := a.single(t, `{"goal":null,"config_pr":{"files":[{"path":".forecrew/profile.yaml","content":"version: 1\nname: app\nsummary: An app.\n"}]},"setup":null}`)
	r.decide(t, domain.GateImprovementProposal, mcp.ToolApprove, "")
	own := of(r, p)
	own.waitStep(t, "merged/0/pr", domain.StepWaiting)
	pr, err := a.tracker.FindPR(t.Context(), "acme/app", fmt.Sprintf("agent/prop-%d", p.Number))
	must(t, err)
	r.publish(t, &domain.PRMerged{Repo: "acme/app", Number: pr.Number, SHA: "m1"})
	own.waitRun(t, domain.RunSucceeded)
	if p = a.proposals(t)[0]; p.Status != domain.ProposalDone {
		t.Fatalf("proposal %+v", p)
	}
}

func TestChangesRequestedGoThroughTheAdvisor(t *testing.T) {
	a := newAdviseEnv(t)
	r, p := a.single(t, setupAction)
	r.decide(t, domain.GateImprovementProposal, mcp.ToolRequestChanges, "Only for images")
	own := of(r, p)
	s := own.waitStep(t, "review/1/revise/0/advisor", domain.StepWaiting)
	reqs := r.jobs.Requests()
	if last := reqs[len(reqs)-1]; last.Role != "advisor" || last.Task != "revise" || !strings.Contains(last.Feedback, "Only for images") {
		t.Fatalf("revision request = %+v", last)
	}
	r.finishJob(t, s.JobID, []byte(`{"summary":"narrowed","proposal":`+strings.Replace(draft("tooling:trivy", "trivy", setupAction), "Use trivy", "Scan images with trivy", 1)+`,"assumptions":[]}`), "", 0.1)
	g := r.pendingGate(t, domain.GateImprovementProposal)
	if g.Title != "Scan images with trivy" || g.ID == p.GateID {
		t.Fatalf("new gate %+v", g)
	}
	r.decide(t, domain.GateImprovementProposal, mcp.ToolApprove, "")
	own.waitRun(t, domain.RunSucceeded)
	if p = a.proposals(t)[0]; p.Status != domain.ProposalAccepted || p.Title != "Scan images with trivy" {
		t.Fatalf("proposal %+v", p)
	}
}

func TestProposalRunCanceledAbandonsIt(t *testing.T) {
	a := newAdviseEnv(t)
	_, p := a.single(t, setupAction)
	_, err := a.call(t, "proposal.abandon", map[string]any{"proposal_id": p.ID.String()})
	must(t, err)
	if p = a.proposals(t)[0]; p.Status != domain.ProposalSuperseded {
		t.Fatalf("proposal %+v", p)
	}
}

func TestAdviseWithoutTheRoleOnlyTends(t *testing.T) {
	a := newAdviseEnv(t)
	a.cfg.Roles = map[string]config.Role{}
	a.lacking(t, "trivy", 2)
	r := a.advise(t, nil)
	if len(r.jobs.Requests()) != 0 {
		t.Fatalf("jobs %+v", r.jobs.Requests())
	}
}

// Without an advisor the actions say so, and what only tells it does nothing.
func TestAdviseActionsNeedAnAdvisor(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarded)
	id := domain.NewID().String()
	for name, with := range map[string]map[string]any{
		"advise.prepare": {}, "advise.file": {}, "proposal.round": {"proposal_id": id}, "proposal.assess": {"proposal_id": id}, "proposal.carry": {"proposal_id": id},
		"proposal.reopen": {"proposal_id": id}, "proposal.pr_ended": {"proposal_id": id, "signal": "pr.merged"}, "proposal.abandon": {"proposal_id": id},
	} {
		_, err := v.call(t, name, with)
		wantErr(t, err, "no advisor")
	}
	out, err := v.call(t, "advise.start", map[string]any{"goal_id": id})
	must(t, err)
	if m := toMap(t, out); m["started"] != false {
		t.Fatalf("advise.start = %v", m)
	}
	// A settled proposal has nothing to reopen, advisor or not.
	if _, err := v.call(t, "proposal.reopen", map[string]any{"proposal_id": id, "settled": true}); err != nil {
		t.Fatal(err)
	}
}

func TestActionsRefuseBadInput(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarded)
	for _, name := range []string{"advise.file", "advise.start", "proposal.round", "proposal.assess", "proposal.reopen", "proposal.carry", "proposal.pr_ended", "proposal.abandon"} {
		if _, err := v.call(t, name, map[string]any{"proposal_id": 3, "goal_id": 3, "jobs": 3}); err == nil {
			t.Errorf("%s took bad input", name)
		}
	}
}

// The goal actions settle the proposal the goal carries out, and a retro sees
// what was measured.
func TestGoalEndSettlesItsProposal(t *testing.T) {
	a := newAdviseEnv(t)
	a.runs = a // no engine here: nothing else starts
	ctx := t.Context()
	p := domain.Proposal{AccountID: a.account, ProjectID: a.project.ID, Scope: domain.ScopeProject, Category: domain.CategoryCI, Title: "Add CI", Rationale: "r", Benefit: "b",
		Effort: domain.EffortS, Risk: domain.LevelLow, Status: domain.ProposalAccepted, DedupeKey: "ci:x", Action: domain.ProposalAction{Goal: &domain.GoalAction{Title: "CI"}}}
	g := domain.Goal{AccountID: a.account, ProjectID: a.project.ID, Title: "CI", Status: domain.GoalDraft, IssueNumber: 1, CreatedBy: "user:ana"}
	must(t, a.st.Goals().Create(ctx, &g))
	p.GoalID = g.ID
	must(t, a.st.Proposals().Create(ctx, &p))
	must(t, a.st.Goals().UpdateStatus(ctx, a.account, g.ID, domain.GoalDraft, domain.GoalCanceled))
	_, err := a.call(t, "goal.settle", map[string]any{"goal_id": g.ID.String()})
	if err != nil && !strings.Contains(err.Error(), "issue") {
		t.Fatal(err)
	}
	if got := a.proposals(t)[0]; got.Status != domain.ProposalSuperseded {
		t.Fatalf("canceled goal: %+v", got)
	}

	q := p
	q.ID, q.DedupeKey, q.Status, q.Outcome = domain.NewID(), "ci:y", domain.ProposalDone, &domain.ProposalOutcome{Before: 4, After: 1, MeasuredAt: a.clock.Now()}
	q.Measure = &domain.ProposalMeasure{Detector: "ci", Value: 4}
	must(t, a.st.Proposals().Create(ctx, &q))
	out, err := a.call(t, "goal.retro_context", map[string]any{"goal_id": g.ID.String()})
	must(t, err)
	if text := toMap(t, out)["text"].(string); !strings.Contains(text, "Improvements measured lately") || !strings.Contains(text, "4.0 before, 1.0 after") {
		t.Fatalf("retro:\n%s", text)
	}
}
