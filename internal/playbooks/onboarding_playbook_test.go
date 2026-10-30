package playbooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Step paths of the onboarding playbook: the nth review round and the nth
// draft try inside it.
func analyzePath(round, try int) string {
	return fmt.Sprintf("review/%d/draft/%d/analyze", round, try)
}

// onboarding is a run of the onboarding playbook.
type onboarding struct {
	*runner
	pushes  int
	lastSHA string // head of the last analyst job
	file    []byte // the profile file as last committed
}

// startOnboarding starts the onboarding run of the env's project. The
// engine stops when the test ends.
func startOnboarding(t *testing.T, v *env) *onboarding {
	t.Helper()
	return &onboarding{runner: startRun(t, v, "onboarding", workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, Inputs: map[string]any{"repo": "acme/app", "project": "app"},
	})}
}

// analyst waits for the analyst job at path, completes it with output as the
// worker would (the profile committed on agent/onboarding, a new head if the
// PR exists) and returns its request.
func (o *onboarding) analyst(t *testing.T, path string, output map[string]any) workflow.JobRequest {
	t.Helper()
	return o.analystFile(t, path, output, nil)
}

// analystFile is analyst, but the profile file it commits is file when that
// is not nil.
func (o *onboarding) analystFile(t *testing.T, path string, output map[string]any, file []byte) workflow.JobRequest {
	t.Helper()
	ctx := t.Context()
	s := o.waitStep(t, path, domain.StepWaiting)
	var req workflow.JobRequest
	for _, r := range o.jobs.Requests() {
		if r.ID == s.JobID {
			req = r
		}
	}
	raw, err := json.Marshal(output)
	must(t, err)
	profile, err := json.Marshal(output["profile"])
	must(t, err)
	if file != nil {
		profile = file
	}
	o.pushes++
	o.lastSHA = fmt.Sprintf("job-%d", o.pushes) // every job commits a new head
	o.commit(t, profile)
	if pr, err := o.tracker.FindPR(ctx, "acme/app", "agent/onboarding"); err == nil {
		must(t, o.tracker.SetHeadSHA("acme/app", pr.Number, o.lastSHA))
	}
	o.finishJob(t, s.JobID, raw, o.lastSHA, 0)
	return req
}

// commit writes the profile file on the analyst's branch, at its last commit
// and at the head of the PR when there is one.
func (o *onboarding) commit(t *testing.T, content []byte) {
	t.Helper()
	o.file = content
	refs := []string{"agent/onboarding", o.lastSHA}
	if pr, err := o.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding"); err == nil {
		refs = append(refs, pr.HeadSHA)
	}
	for _, ref := range refs {
		must(t, o.tracker.SetFile("acme/app", ref, ".forecrew/profile.yaml", content))
	}
}

// checks waits for the checks step of the round and reports the conclusion
// for the head of the PR, as the webhook does.
func (o *onboarding) checks(t *testing.T, round int, conclusion domain.CheckConclusion) {
	t.Helper()
	o.waitStep(t, fmt.Sprintf("review/%d/approval/0/checks", round), domain.StepWaiting)
	pr, err := o.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding")
	must(t, err)
	o.commit(t, o.file) // the PR head is where the profile is read by the merge
	o.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: pr.Number, SHA: o.lastSHA, Conclusion: conclusion})
}

// reach runs round of the review until its gate is pending, with a valid
// profile and passing checks.
func (o *onboarding) reach(t *testing.T, round int, profile map[string]any) domain.Gate {
	t.Helper()
	o.analyst(t, analyzePath(round, 1), analystOutput(profile))
	o.checks(t, round, domain.ChecksSuccess)
	return o.pendingGate(t, domain.GateProfileApproval)
}

func (o *onboarding) projectNow(t *testing.T) domain.Project {
	t.Helper()
	p, err := o.st.Projects().Get(t.Context(), o.account, o.project.ID)
	must(t, err)
	return p
}

func analystOutput(profile map[string]any) map[string]any {
	return map[string]any{
		"summary": "A Go service.", "profile": profile,
		"assumptions": []any{map[string]any{"text": "CI runs on Linux", "confidence": "high", "impact": "low"}},
	}
}

var githubBot = domain.Actor{Kind: domain.ActorGitHub, Name: "github"}

func TestOnboardingApprovedOverMCP(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	req := o.analyst(t, analyzePath(1, 1), analystOutput(fullProfile))
	if req.Role != "onboarding-analyst" || req.Repo != "acme/app" || req.Branch != "agent/onboarding" || !req.IncludeRepoFacts ||
		len(req.Needs) != 1 || req.Needs[0] != "linux" || req.Feedback != "" || req.ProjectID != o.project.ID {
		t.Errorf("job request = %+v", req)
	}
	o.checks(t, 1, domain.ChecksSuccess)

	g := o.pendingGate(t, domain.GateProfileApproval)
	pr, err := o.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding")
	must(t, err)
	n := pr.Number
	if g.SubjectRepo != "acme/app" || g.SubjectNumber != n || !strings.Contains(g.Title, "round 1") {
		t.Errorf("gate = %+v, PR %d", g, n)
	}
	if pr.Base != "main" || !strings.Contains(pr.Body, "`web` (node)") || !strings.Contains(pr.Body, "CI runs on Linux") {
		t.Errorf("pr = %+v", pr)
	}
	c, err := o.st.Changes().GetByPR(t.Context(), o.account, "acme/app", n)
	must(t, err)
	if c.State != domain.ChangeOpen || c.HeadSHA != pr.HeadSHA {
		t.Errorf("change = %+v", c)
	}
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarding {
		t.Errorf("status before the decision = %s", got)
	}

	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	if r := o.waitRun(t, domain.RunSucceeded); r.Error != "" {
		t.Errorf("run error = %q", r.Error)
	}
	pr, _ = o.tracker.GetPR(t.Context(), "acme/app", n)
	p := o.projectNow(t)
	if !pr.Merged || p.Status != domain.ProjectOnboarded || !strings.Contains(string(p.Profile), `"name":"shop"`) {
		t.Errorf("merged = %v, project = %s %s", pr.Merged, p.Status, p.Profile)
	}
	if c, _ = o.st.Changes().GetByPR(t.Context(), o.account, "acme/app", n); c.State != domain.ChangeMerged {
		t.Errorf("change state = %s", c.State)
	}
}

// The profile stored is the file as merged, with the edits of the human.
func TestOnboardingStoresTheMergedFile(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	o.reach(t, 1, fullProfile)
	edited := `{"version": 1, "name": "shop", "summary": "Edited by a human."}`
	o.commit(t, []byte(edited))
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if p := o.projectNow(t); !strings.Contains(string(p.Profile), "Edited by a human.") || strings.Contains(string(p.Profile), "components") {
		t.Errorf("profile = %s", p.Profile)
	}
}

// onboard_project moves the project to onboarding before it starts the run.
func TestOnboardingOfAProjectAlreadyOnboarding(t *testing.T) {
	o := startOnboarding(t, newEnv(t, domain.ProjectOnboarding))
	o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
}

func TestOnboardingMergedOnGitHub(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	// What the webhook does when the PR is merged on GitHub.
	_, err := o.tracker.MergePR(t.Context(), "acme/app", g.SubjectNumber, github.MergeInput{})
	must(t, err)
	_, err = o.gates.Resolve(t.Context(), o.github(), g.ID, domain.GateApproved, "")
	must(t, err)

	o.waitRun(t, domain.RunSucceeded)
	p := o.projectNow(t)
	if p.Status != domain.ProjectOnboarded || len(p.Profile) == 0 {
		t.Errorf("project = %s %s", p.Status, p.Profile)
	}
	sigs, err := o.bus.Lookup(t.Context(), o.account, domain.PRKey("acme/app", g.SubjectNumber))
	must(t, err)
	if len(sigs) != 0 {
		t.Errorf("the playbook merged a merged PR: %d pr signals", len(sigs))
	}
}

func TestOnboardingRejected(t *testing.T) {
	tests := []struct {
		name   string
		reject func(t *testing.T, o *onboarding, g domain.Gate)
	}{
		{"over MCP", func(t *testing.T, o *onboarding, _ domain.Gate) {
			o.decide(t, domain.GateProfileApproval, mcp.ToolReject, "Wrong repo.")
		}},
		{"PR closed on GitHub", func(t *testing.T, o *onboarding, g domain.Gate) {
			must(t, o.tracker.ClosePR("acme/app", g.SubjectNumber))
			_, err := o.gates.Resolve(t.Context(), o.github(), g.ID, domain.GateRejected, "")
			must(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := startOnboarding(t, newEnv(t))
			g := o.reach(t, 1, fullProfile)
			tt.reject(t, o, g)
			o.waitRun(t, domain.RunCanceled)
			o.waitStep(t, "finally/revert", domain.StepSucceeded)
			p := o.projectNow(t)
			pr, _ := o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber)
			if p.Status != domain.ProjectRegistered || len(p.Profile) != 0 || pr.Merged {
				t.Errorf("project = %s %s, merged = %v", p.Status, p.Profile, pr.Merged)
			}
		})
	}
}

func TestOnboardingRequestChanges(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	first := o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolRequestChanges, "Add the web component.")

	better := map[string]any{"version": 1, "name": "shop", "summary": "A shop with a web app."}
	req := o.analyst(t, analyzePath(2, 1), analystOutput(better))
	if req.Branch != "agent/onboarding" || !strings.Contains(req.Feedback, "Add the web component.") {
		t.Errorf("second job request = %+v", req)
	}
	o.checks(t, 2, domain.ChecksSuccess)
	second := waitNewGate(t, o, first.ID)
	if !strings.Contains(second.Title, "round 2") || second.SubjectNumber != first.SubjectNumber {
		t.Errorf("second gate = %+v, first %+v", second, first)
	}
	pr, _ := o.tracker.GetPR(t.Context(), "acme/app", first.SubjectNumber)
	if strings.Contains(pr.Body, "`web` (node)") {
		t.Errorf("the PR body was not updated: %s", pr.Body)
	}

	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if p := o.projectNow(t); p.Status != domain.ProjectOnboarded || !strings.Contains(string(p.Profile), "A shop with a web app.") {
		t.Errorf("project = %s %s", p.Status, p.Profile)
	}
}

// waitNewGate waits for a pending profile approval other than old.
func waitNewGate(t *testing.T, o *onboarding, old uuid.UUID) domain.Gate {
	t.Helper()
	var g domain.Gate
	eventually(t, "a new profile gate", func() bool {
		gs, err := o.gates.ListPending(t.Context(), o.account, o.project.ID)
		must(t, err)
		for _, x := range gs {
			if x.Kind == domain.GateProfileApproval && x.ID != old {
				g = x
			}
		}
		return g.ID != uuid.Nil()
	})
	return g
}

func TestOnboardingInvalidThenValidProfile(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	o.analyst(t, analyzePath(1, 1), analystOutput(map[string]any{"version": 2, "name": "shop", "summary": "x"}))
	req := o.analyst(t, analyzePath(1, 2), analystOutput(fullProfile))
	if !strings.Contains(req.Feedback, "does not match the profile schema") {
		t.Errorf("rerun feedback = %q", req.Feedback)
	}
	o.checks(t, 1, domain.ChecksSuccess)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
}

func TestOnboardingProfileStaysInvalid(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	invalid := analystOutput(map[string]any{"version": 2, "name": "shop", "summary": "x"})
	o.analyst(t, analyzePath(1, 1), invalid)
	o.analyst(t, analyzePath(1, 2), invalid)
	r := o.waitRun(t, domain.RunFailed)
	if !strings.Contains(r.Error, "still invalid after 2 attempts") || !strings.Contains(r.Error, "version") {
		t.Errorf("run error = %q", r.Error)
	}
	o.waitStep(t, "finally/revert", domain.StepSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectRegistered {
		t.Errorf("status = %s", got)
	}
	if _, err := o.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding"); err == nil {
		t.Error("a PR was opened for an invalid profile")
	}
}

func TestOnboardingAnalystJobFails(t *testing.T) {
	o := startOnboarding(t, newEnv(t, domain.ProjectOnboarding))
	// Fail the job instead of completing it. The step escalates; rejecting
	// the escalation ends the run.
	s := o.waitStep(t, analyzePath(1, 1), domain.StepWaiting)
	j, err := o.st.Jobs().Get(t.Context(), o.account, s.JobID)
	must(t, err)
	leased, err := o.st.Jobs().LeaseNext(t.Context(), o.account, o.worker.ID, j.RequiredLabels(), time.Hour)
	must(t, err)
	_, err = o.st.Jobs().Complete(t.Context(), o.account, leased.ID, o.worker.ID, store.JobResult{Status: domain.JobFailed, Error: "no network"})
	must(t, err)
	o.publish(t, &domain.JobCompleted{JobID: leased.ID, Status: domain.JobFailed})

	g := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "no network") {
		t.Errorf("escalation = %+v", g)
	}
	o.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	o.waitRun(t, domain.RunCanceled)
	o.waitStep(t, "finally/revert", domain.StepSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectRegistered {
		t.Errorf("status = %s", got)
	}
}

func TestOnboardingEscalatesAfterThreeRounds(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	for round := 1; round <= 3; round++ {
		o.reach(t, round, fullProfile)
		o.decide(t, domain.GateProfileApproval, mcp.ToolRequestChanges, fmt.Sprintf("Change %d.", round))
	}
	escalation := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(escalation.Summary, "not met after 3 iterations") {
		t.Errorf("escalation = %+v", escalation)
	}
	if _, err := o.gates.Decide(t.Context(), o.human, mcp.ToolRequestChanges, mcp.GateDecisionInput{Gate: escalation.Ref(), Feedback: "x"}); err == nil {
		t.Error("request_changes was accepted on a needs_you gate")
	}
	o.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")

	req := o.analyst(t, analyzePath(4, 1), analystOutput(fullProfile))
	if !strings.Contains(req.Feedback, "Change 3.") {
		t.Errorf("fourth job request = %+v", req)
	}
	o.checks(t, 4, domain.ChecksSuccess)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
}

func TestOnboardingRejectsAfterEscalation(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	for round := 1; round <= 3; round++ {
		o.reach(t, round, fullProfile)
		o.decide(t, domain.GateProfileApproval, mcp.ToolRequestChanges, "Again.")
	}
	o.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	o.waitRun(t, domain.RunCanceled)
	o.waitStep(t, "finally/revert", domain.StepSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectRegistered {
		t.Errorf("status = %s", got)
	}
}

// The merge happens after the checks are final: success and none merge,
// failure opens a needs_you gate that merges when approved again.
func TestOnboardingMergesAfterChecks(t *testing.T) {
	for _, conclusion := range []domain.CheckConclusion{domain.ChecksSuccess, domain.ChecksNone} {
		t.Run(string(conclusion), func(t *testing.T) {
			o := startOnboarding(t, newEnv(t))
			o.analyst(t, analyzePath(1, 1), analystOutput(fullProfile))
			// No gate while the checks are running.
			o.waitStep(t, "review/1/approval/0/checks", domain.StepWaiting)
			if gs, _ := o.gates.ListPending(t.Context(), o.account, o.project.ID); len(gs) != 0 {
				t.Fatalf("gates while checks run = %+v", gs)
			}
			o.checks(t, 1, conclusion)
			g := o.pendingGate(t, domain.GateProfileApproval)
			o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
			o.waitRun(t, domain.RunSucceeded)
			if pr, _ := o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber); !pr.Merged {
				t.Error("not merged")
			}
		})
	}
}

func TestOnboardingFailedChecksEscalate(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	o.analyst(t, analyzePath(1, 1), analystOutput(fullProfile))
	o.checks(t, 1, domain.ChecksFailure)
	g := o.pendingGate(t, domain.GateProfileApproval)
	pr, _ := o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber)
	must(t, o.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksFailure, 1))

	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	escalation := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(escalation.Summary, "checks failed") {
		t.Errorf("escalation = %+v", escalation)
	}
	if pr, _ = o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber); pr.Merged {
		t.Fatal("merged with failing checks")
	}

	must(t, o.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksSuccess, 1))
	o.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if pr, _ = o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber); !pr.Merged {
		t.Error("not merged once the checks passed")
	}
}

// Checks that never report time out after an hour and ask a human.
func TestOnboardingChecksTimeOut(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	o.analyst(t, analyzePath(1, 1), analystOutput(fullProfile))
	o.waitStep(t, "review/1/approval/0/checks", domain.StepWaiting)
	o.clock.Advance(59 * time.Minute)
	time.Sleep(30 * time.Millisecond) // several sweeps
	if gs, _ := o.gates.ListPending(t.Context(), o.account, o.project.ID); len(gs) != 0 {
		t.Fatalf("gates before the timeout = %+v", gs)
	}
	o.clock.Advance(2 * time.Minute)
	g := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "timed out after 1h0m0s") {
		t.Errorf("escalation = %+v", g)
	}
	// Approving retries the wait as a new attempt; the checks report.
	o.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	eventually(t, "the retried wait", func() bool {
		steps, _ := o.st.StepRuns().ListByRun(t.Context(), o.account, o.run.ID)
		for _, s := range steps {
			if s.StepID == "review/1/approval/0/checks" && s.Attempt == 2 && s.Status == domain.StepWaiting {
				return true
			}
		}
		return false
	})
	pr, _ := o.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding")
	o.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: pr.Number, SHA: o.lastSHA, Conclusion: domain.ChecksNone})
	o.pendingGate(t, domain.GateProfileApproval)
}

func TestOnboardingAppNotInstalled(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarding)
	v.tracker = testkit.NewTracker(v.clock.Now)
	v.register()
	o := startOnboarding(t, v)
	r := o.waitRun(t, domain.RunFailed)
	if !strings.Contains(r.Error, "GitHub App is not installed on acme/app") {
		t.Errorf("run error = %q", r.Error)
	}
	o.waitStep(t, "finally/revert", domain.StepSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectRegistered {
		t.Errorf("status = %s", got)
	}
}

// A human edit that breaks the profile file is caught before the merge.
func TestOnboardingEditedFileIsValidatedBeforeTheMerge(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	o.commit(t, []byte(`{"version": 7, "name": "shop", "summary": "Edited."}`))
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")

	escalation := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(escalation.Summary, "not merging") || !strings.Contains(escalation.Summary, "version") {
		t.Errorf("escalation = %+v", escalation)
	}
	if pr, _ := o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber); pr.Merged {
		t.Fatal("merged an invalid profile")
	}
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarding {
		t.Errorf("status = %s", got)
	}

	o.commit(t, []byte(`{"version": 1, "name": "shop", "summary": "Fixed."}`))
	o.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if p := o.projectNow(t); p.Status != domain.ProjectOnboarded || !strings.Contains(string(p.Profile), "Fixed.") {
		t.Errorf("project = %s %s", p.Status, p.Profile)
	}
}

// The profile file the analyst committed is checked before the gate, not
// only the profile in its answer.
func TestOnboardingInvalidCommittedFileRerunsTheAnalyst(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	o.analystFile(t, analyzePath(1, 1), analystOutput(fullProfile), []byte(`{"version": 7, "name": "shop", "summary": "x"}`))
	req := o.analyst(t, analyzePath(1, 2), analystOutput(fullProfile))
	if !strings.Contains(req.Feedback, ".forecrew/profile.yaml at job-1") || !strings.Contains(req.Feedback, "version") {
		t.Errorf("rerun feedback = %q", req.Feedback)
	}
	o.checks(t, 1, domain.ChecksSuccess)
	o.pendingGate(t, domain.GateProfileApproval)
}

// completeEscalates checks that onboarding.complete failed after the merge
// of the PR of g: a needs_you gate with want is pending and the project is
// still onboarding. Approving the gate, after fix, finishes the run.
func (o *onboarding) completeEscalates(t *testing.T, g domain.Gate, want string, fix func()) {
	t.Helper()
	escalation := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(escalation.Summary, want) {
		t.Errorf("escalation = %+v", escalation)
	}
	pr, _ := o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber)
	if p := o.projectNow(t); !pr.Merged || p.Status != domain.ProjectOnboarding || len(p.Profile) != 0 {
		t.Errorf("merged = %v, project = %s %s", pr.Merged, p.Status, p.Profile)
	}
	if fix != nil {
		fix()
	}
	o.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if p := o.projectNow(t); p.Status != domain.ProjectOnboarded || len(p.Profile) == 0 {
		t.Errorf("project = %s %s", p.Status, p.Profile)
	}
}

// After the merge a failure never undoes the onboarding: a needs_you gate
// opens and approving it retries.
func TestOnboardingCompleteReadErrorEscalates(t *testing.T) {
	v := newEnv(t)
	v.wrapped = &failOnRef{Tracker: v.tracker, prefix: "merge-", n: 1}
	v.register()
	o := startOnboarding(t, v)
	g := o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.completeEscalates(t, g, "connection reset", nil)
}

func TestOnboardingInvalidMergedFileEscalates(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	// The PR is merged on GitHub, and the file at the merge commit is invalid.
	_, err := o.tracker.MergePR(t.Context(), "acme/app", g.SubjectNumber, github.MergeInput{})
	must(t, err)
	merge := fmt.Sprintf("merge-%d", g.SubjectNumber)
	must(t, o.tracker.SetFile("acme/app", merge, ".forecrew/profile.yaml", []byte(`{"version": 7}`)))
	_, err = o.gates.Resolve(t.Context(), o.github(), g.ID, domain.GateApproved, "")
	must(t, err)
	o.completeEscalates(t, g, "invalid project profile", func() {
		must(t, o.tracker.SetFile("acme/app", merge, ".forecrew/profile.yaml", []byte(`{"version": 1, "name": "shop", "summary": "Fixed."}`)))
	})
}

// failOnRef is a tracker whose GetFile fails n times for refs with prefix.
type failOnRef struct {
	github.Tracker
	prefix string
	n      int
}

func (f *failOnRef) GetFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	if strings.HasPrefix(ref, f.prefix) && f.n > 0 {
		f.n--
		return nil, errors.New("connection reset")
	}
	return f.Tracker.GetFile(ctx, repo, ref, path)
}

// A PR a human closed while no gate was pending is not replaced: the run
// fails and the project goes back to registered.
func TestOnboardingKeepsAPRClosedBetweenRounds(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolRequestChanges, "Shorter.")
	must(t, o.tracker.ClosePR("acme/app", g.SubjectNumber))
	o.analyst(t, analyzePath(2, 1), analystOutput(fullProfile))

	r := o.waitRun(t, domain.RunFailed)
	if !strings.Contains(r.Error, "was closed on GitHub") || !strings.Contains(r.Error, "not opening another") {
		t.Errorf("run error = %q", r.Error)
	}
	o.waitStep(t, "finally/revert", domain.StepSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectRegistered {
		t.Errorf("status = %s", got)
	}
	if _, err := o.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding"); err == nil {
		t.Error("another PR was opened")
	}
}

// A PR a human merged on GitHub while no gate was pending counts as the
// approval: no new gate, and the merged profile is stored.
func TestOnboardingKeepsAPRMergedBetweenRounds(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolRequestChanges, "Shorter.")
	edited := `{"version": 1, "name": "shop", "summary": "Merged by a human."}`
	o.commit(t, []byte(edited))
	_, err := o.tracker.MergePR(t.Context(), "acme/app", g.SubjectNumber, github.MergeInput{})
	must(t, err)
	o.analyst(t, analyzePath(2, 1), analystOutput(fullProfile))

	o.waitRun(t, domain.RunSucceeded)
	p := o.projectNow(t)
	if p.Status != domain.ProjectOnboarded || !strings.Contains(string(p.Profile), "Merged by a human.") {
		t.Errorf("project = %s %s", p.Status, p.Profile)
	}
	if gs, _ := o.gates.ListPending(t.Context(), o.account, o.project.ID); len(gs) != 0 {
		t.Errorf("pending gates = %+v", gs)
	}
}

// Approving while the checks of the PR head are running does not merge: a
// needs_you gate opens, and approving it merges once they are done.
func TestOnboardingApproveWhileChecksArePending(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	pr, _ := o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber)
	must(t, o.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksPending, 1))

	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	escalation := o.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(escalation.Summary, "still running") {
		t.Errorf("escalation = %+v", escalation)
	}
	if pr, _ = o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber); pr.Merged {
		t.Fatal("merged while the checks were running")
	}

	must(t, o.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksSuccess, 1))
	o.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if pr, _ = o.tracker.GetPR(t.Context(), "acme/app", g.SubjectNumber); !pr.Merged {
		t.Error("not merged once the checks were done")
	}
}

// Rejecting the needs_you gate that onboarding.complete opens after the merge
// is an explicit human stop: the project goes back to registered, and the
// merged profile file stays on the default branch.
func TestOnboardingRejectedCompletionRevertsTheProject(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	g := o.reach(t, 1, fullProfile)
	_, err := o.tracker.MergePR(t.Context(), "acme/app", g.SubjectNumber, github.MergeInput{})
	must(t, err)
	must(t, o.tracker.SetFile("acme/app", fmt.Sprintf("merge-%d", g.SubjectNumber), ".forecrew/profile.yaml", []byte(`{"version": 7}`)))
	_, err = o.gates.Resolve(t.Context(), o.github(), g.ID, domain.GateApproved, "")
	must(t, err)

	o.pendingGate(t, domain.GateNeedsYou)
	o.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	o.waitRun(t, domain.RunCanceled)
	o.waitStep(t, "finally/revert", domain.StepSucceeded)
	if got := o.projectNow(t).Status; got != domain.ProjectRegistered {
		t.Errorf("status = %s", got)
	}
	if _, err := o.tracker.GetFile(t.Context(), "acme/app", "main", ".forecrew/profile.yaml"); err != nil {
		t.Errorf("the merged file is gone from main: %v", err)
	}
}
