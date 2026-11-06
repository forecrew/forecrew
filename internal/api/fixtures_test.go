package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// update rewrites the web UI fixtures: go test ./internal/api -update.
var update = flag.Bool("update", false, "rewrite the JSON fixtures of the web UI")

// fixtureDir holds one JSON response per endpoint. The web UI tests serve
// these files as the API (docs/ui.md), so they cannot drift from the handlers.
const fixtureDir = "../../web/src/test/fixtures"

// fid returns a fixed UUID so that fixtures do not change between runs.
func fid(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("018f0000-0000-7000-8000-%012d", n))
}

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

const specBody = `## Scope

Users can sign in with an email link.

## Acceptance criteria

- AC-1: A user receives a sign-in link within a minute.
- AC-2: The link works once and expires after 15 minutes.

## Assumptions

- Email delivery is handled by the existing mailer (impact: low).

## Open questions

- Should admins be allowed to sign in with a password?
`

const profileJSON = `{
  "version": 1, "name": "app", "summary": "A web app with a Go API.",
  "languages": ["Go", "TypeScript"],
  "components": [
    {"path": ".", "stack": "go", "build": "go build ./...", "test": "go test ./...", "platforms": ["linux"], "needs": ["go"]},
    {"path": "web", "stack": "node", "build": "pnpm build", "test": "pnpm test", "platforms": ["linux", "macos"], "needs": ["node"]}
  ],
  "platforms": ["linux", "macos"],
  "open_questions": ["Which checks gate a merge?"]
}`

const (
	proposalJSON = `{"priority":"simplicity","position":"Use the Postgres queue we already run.","recommendation":"postgres-queue",
	  "options":[{"name":"postgres-queue","summary":"SKIP LOCKED on the jobs table.","pros":["no new service"],"cons":["polling"],"cost":"low","risk":"low"},
	             {"name":"redis-streams","summary":"A Redis stream per pool.","pros":["push"],"cons":["a new service"],"cost":"medium","risk":"medium"}],
	  "evidence":[{"claim":"Leasing exists","kind":"path","source":"internal/queue/lease.go:40-58","date":null,"confidence":"high"}],
	  "assumptions":[],"risks":[{"risk":"Polling load","likelihood":"low","impact":"medium","mitigation":"Index the lease query."}],"revision":null,"escalation":null}`
	critiqueJSON = `{"priority":"security","critiques":[{"member":"architect@claude-code","agree":[{"point":"No new service","reason":"Fewer secrets to hold.","evidence":[]}],
	  "disagree":[{"point":"Polling","reason":"A <b>busy</b> table leaks timing.","evidence":[{"claim":"c","kind":"url","source":"https://example.com/x","date":"2026-01-02","confidence":"low"}]}],
	  "challenged_assumptions":[]}],"assumptions":[],"escalation":null}`
	revisionJSON = `{"priority":"simplicity","position":"Keep Postgres, add the index.","recommendation":"postgres-queue","options":[],"evidence":[],"assumptions":[],"risks":[],
	  "revision":{"changed":false,"reason":"The critique does not change the cost.","prompted_by":[]},"escalation":null}`
)

// fixtureBrief is the brief of the fixture councils.
func fixtureBrief() *domain.CouncilBrief {
	return &domain.CouncilBrief{
		Question: "Which queue should jobs use?",
		Options: []domain.BriefOption{
			{Name: "postgres-queue", Summary: "SKIP LOCKED on the jobs table.", Pros: []string{"no new service"}, Cons: []string{"polling"}, Cost: domain.LevelLow, Risk: domain.LevelLow},
			{Name: "redis-streams", Summary: "A Redis stream per pool.", Pros: []string{"push"}, Cons: []string{"a new service"}, Cost: domain.LevelMedium, Risk: domain.LevelMedium},
		},
		Recommendation: "postgres-queue",
		Votes: []domain.BriefVote{
			{Member: "architect@claude-code", Option: "postgres-queue", Reason: "Already run."},
			{Member: "security@codex", Option: "redis-streams", Reason: "Isolates job data."},
		},
		Dissent: []domain.BriefDissent{
			{Member: "security@codex", Position: "Job data should not share the application database."},
			{Member: "devils-advocate@claude-code", Position: "Devil's advocate (mandated dissent): Polling will not scale past 50 workers."},
		},
		Assumptions:   []domain.StatedAssumption{{Statement: "Fewer than 50 workers.", Confidence: domain.LevelMedium, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByAskHuman}},
		OpenQuestions: []string{"Who owns the Redis cluster?"},
		Reversible:    true,
	}
}

// fixtureCouncil is a council of the project app.
func fixtureCouncil(number int64, status domain.CouncilStatus, brief *domain.CouncilBrief) domain.Council {
	return domain.Council{
		ID: fid(int(number) + 200), AccountID: fid(1), ProjectID: fid(10), Number: number, Kind: domain.CouncilTechnical,
		Question: "Which queue should jobs use?", Constraints: []string{"Keep it to one database"}, Status: status, Brief: brief,
		RunID: fid(int(number) + 300), CreatedAt: at(-90 + int(number)), UpdatedAt: at(-10),
	}
}

func fixtureContribution(c domain.Council, id int, role, runtime string, stage domain.CouncilStage, round int, content string, checks []domain.SourceCheck) mcp.ContributionInfo {
	return mcp.ContributionInfo{
		ID: fid(id), AccountID: fid(1), CouncilID: c.ID, Role: role, Runtime: runtime, Stage: stage, Round: round,
		Content: json.RawMessage(content), EvidenceCheck: checks, CreatedAt: at(-60 + id%10),
		Member: role + "@" + runtime,
	}
}

// fixtureCouncils answers get_council for the fixture councils: 2 is
// running, 3 decided, 4 failed and 5 waits for a human.
func fixtureCouncils(acc domain.Account) func(context.Context, domain.Actor, mcp.CouncilRefInput) (mcp.CouncilDetail, error) {
	ok := []domain.SourceCheck{{Source: "internal/queue/lease.go:40-58", Kind: domain.SourcePath, Status: domain.SourceOK}}
	bad := []domain.SourceCheck{{Source: "https://example.com/x", Kind: domain.SourceURL, Status: domain.SourceRefused, Detail: "private address"}}
	return func(_ context.Context, _ domain.Actor, in mcp.CouncilRefInput) (mcp.CouncilDetail, error) {
		var number int64
		if _, err := fmt.Sscanf(in.Council, "COUNCIL-%d", &number); err != nil || number < 2 || number > 5 {
			return mcp.CouncilDetail{}, fmt.Errorf("council %s: %w", in.Council, mcp.ErrNotFound)
		}
		status, brief := domain.CouncilDecided, fixtureBrief()
		switch number {
		case 2:
			status, brief = domain.CouncilRunning, nil
		case 4:
			status = domain.CouncilFailed
		case 5:
			status = domain.CouncilRunning
		}
		c := fixtureCouncil(number, status, brief)
		d := mcp.CouncilDetail{Council: c, Ref: c.Ref(), Contributions: []mcp.ContributionInfo{
			fixtureContribution(c, 401, "architect", "claude-code", domain.StageProposal, 1, proposalJSON, ok),
			fixtureContribution(c, 402, "security", "codex", domain.StageProposal, 1, proposalJSON, bad),
			fixtureContribution(c, 403, "devils-advocate", "claude-code", domain.StageProposal, 1, proposalJSON, nil),
			fixtureContribution(c, 404, "security", "codex", domain.StageCritique, 1, critiqueJSON, bad),
		}}
		if number != 2 {
			d.Contributions = append(d.Contributions,
				fixtureContribution(c, 405, "architect", "claude-code", domain.StageRevision, 1, revisionJSON, nil),
				fixtureContribution(c, 406, "facilitator", "claude-code", domain.StageSynthesis, 1, `{"recommendation":"postgres-queue"}`, nil))
		}
		gateStatus := domain.GateApproved
		switch number {
		case 4:
			gateStatus = domain.GateRejected
		case 5:
			gateStatus = domain.GatePending
		}
		if number != 2 {
			g := domain.Gate{ID: fid(500 + int(number)), AccountID: acc.ID, ProjectID: fid(10), Number: 7 + number, Kind: domain.GateCouncilDecision, Status: gateStatus, Title: c.Ref() + ": Which queue should jobs use?", Summary: "The brief.", CreatedAt: at(-20)}
			d.Gate = &mcp.GateInfo{Gate: g, Ref: g.Ref(), Project: "app"}
			d.GateID = g.ID
		}
		if number == 3 {
			d.Decision = &domain.Decision{
				ID: fid(600), AccountID: acc.ID, ProjectID: fid(10), Number: 5, Question: c.Question, Choice: "postgres-queue", DecidedBy: "user:ana",
				CouncilID: c.ID, GateID: d.GateID, CreatedAt: at(-5),
			}
		}
		return d, nil
	}
}

// reportJSON is the verification report of PR #9: one failing result, a flaky
// test, an untested criterion and a new test that passed on base.
const reportJSON = `{
  "version": 1, "sha": "abc1234", "status": "failed",
  "results": [
    {"environment": "linux", "cell": {"go": "1.26"}, "suite": "unit", "required": true, "status": "passed", "counts": {"passed": 40, "failed": 0, "skipped": 1, "errored": 0}, "job": "JOB-44", "attempts": 1},
    {"environment": "linux", "cell": {"go": "1.26"}, "suite": "e2e", "required": true, "status": "failed", "counts": {"passed": 7, "failed": 2, "skipped": 0, "errored": 0}, "job": "JOB-45", "attempts": 2},
    {"environment": "macos", "cell": {}, "suite": "unit", "required": false, "status": "missing", "counts": {"passed": 0, "failed": 0, "skipped": 0, "errored": 0}, "attempts": 0}
  ],
  "criteria": {
    "AC-1": {"status": "passed", "tests": {"auth::TestLinkSent": "passed"}},
    "AC-2": {"status": "failed", "tests": {"auth::TestLinkExpires": "failed"}},
    "AC-3": {"status": "untested", "tests": {}}
  },
  "new_tests": {"auth::TestLinkSent": true, "auth::TestLinkExpires": false},
  "flaky": [{"test": "web::login redirects", "environments": ["linux"]}],
  "environments": {"linux": {"versions": {"go": "go1.26.1", "node": "v24.1.0"}}},
  "blocking": ["e2e failed in linux", "AC-3 has no test", "auth::TestLinkExpires passed on base"]
}`

// fixtureEnv is an API whose fake service returns fixed data.
func fixtureEnv(t *testing.T) *env {
	t.Helper()
	acc := domain.Account{ID: fid(1), Name: "acme"}
	e := newEnvFor(t, func() time.Time { return t0 }, acc)
	projects := []domain.Project{
		{ID: fid(10), AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded, Profile: json.RawMessage(profileJSON), CreatedAt: at(-600), UpdatedAt: at(-500)},
		{ID: fid(11), AccountID: acc.ID, Name: "docs", Repo: "acme/docs", DefaultBranch: "main", Status: domain.ProjectOnboarding, CreatedAt: at(-30), UpdatedAt: at(-20)},
	}
	goals := []domain.Goal{
		{ID: fid(20), AccountID: acc.ID, ProjectID: fid(10), Number: 3, Title: "Sign in with an email link", Body: "Let users sign in without a password.", Platforms: []string{"web"}, Status: domain.GoalInProgress, IssueNumber: 5, CreatedBy: "mcp:laptop", CreatedAt: at(-300), UpdatedAt: at(-10)},
		{ID: fid(21), AccountID: acc.ID, ProjectID: fid(10), Number: 4, Title: "Dark mode", Body: "Follow the system setting.", Status: domain.GoalDraft, CreatedBy: "mcp:laptop", CreatedAt: at(-60), UpdatedAt: at(-60)},
		{ID: fid(22), AccountID: acc.ID, ProjectID: fid(10), Number: 2, Title: "Export to CSV", Platforms: []string{"web", "ios"}, Status: domain.GoalDone, IssueNumber: 3, CreatedBy: "mcp:laptop", CreatedAt: at(-900), UpdatedAt: at(-400)},
	}
	jobs := []domain.Job{
		fixtureJob(41, domain.JobRunning, "developer", 1, at(-5)),
		fixtureJob(42, domain.JobQueued, "reviewer", 0, time.Time{}),
		fixtureJob(40, domain.JobSucceeded, "architect", 1, at(-90)),
	}
	jobs[0].WorkerID = fid(60)
	jobs[2].FinishedAt = at(-80)
	jobs[2].CostUSD = 0.42
	gates := []mcp.GateInfo{
		{ID: fid(30), AccountID: acc.ID, ProjectID: fid(10), Number: 9, Kind: domain.GateMergeApproval, Status: domain.GatePending, Title: "Merge PR #9", Summary: "Adds email sign-in. Tests pass.\n\n- 12 files changed\n- Coverage 91%", SubjectRepo: "acme/app", SubjectNumber: 9, CreatedAt: at(-45), Ref: "GATE-9", Project: "app", GoalRef: "GOAL-3", TaskRef: "TASK-12", URL: "https://github.com/acme/app/pull/9"},
		{ID: fid(31), AccountID: acc.ID, ProjectID: fid(10), Number: 10, Kind: domain.GateSpecApproval, Status: domain.GatePending, Title: "Approve the spec", Summary: "Spec revision 2 for GOAL-4.", SubjectRepo: "acme/app", SubjectNumber: 6, CreatedAt: at(-20), Ref: "GATE-10", Project: "app", GoalRef: "GOAL-4", URL: "https://github.com/acme/app/issues/6"},
		{ID: fid(32), AccountID: acc.ID, ProjectID: fid(11), Number: 11, Kind: domain.GateNeedsYou, Status: domain.GatePending, Title: "Developer job failed", Summary: "The developer job failed 3 times: <script>alert(1)</script> tests do not compile.", SubjectRepo: "acme/docs", CreatedAt: at(-5), Ref: "GATE-11", Project: "docs", TaskRef: "TASK-14"},
	}
	councilGates := []mcp.GateInfo{
		{ID: fid(512), AccountID: acc.ID, ProjectID: fid(10), Number: 12, Kind: domain.GateCouncilDecision, Status: domain.GatePending, Title: "COUNCIL-5: Which queue should jobs use?", Summary: "The brief of COUNCIL-5. It is agent text.", SubjectRepo: "acme/app", CreatedAt: at(-15), Ref: "GATE-12", Project: "app"},
		{ID: fid(513), AccountID: acc.ID, ProjectID: fid(10), Number: 13, Kind: domain.GateAssumptionReview, Status: domain.GatePending, Title: "Review A-7: Fewer than 50 workers will poll the queue.", Summary: "A-7 is a high-impact assumption made by architect: work rests on it and nobody has checked it.", SubjectRepo: "acme/app", SubjectNumber: 6, GoalID: fid(20), TaskID: fid(40), CreatedAt: at(-12), Ref: "GATE-13", Project: "app", GoalRef: "GOAL-3", TaskRef: "TASK-12", URL: "https://github.com/acme/app/issues/6"},
		{ID: fid(514), AccountID: acc.ID, ProjectID: fid(10), Number: 14, Kind: domain.GateNeedsYou, Origin: domain.GateFromAskHuman, Status: domain.GatePending, Title: "The developer asks a question", Summary: "Should the sign-in link expire after 15 minutes or 1 hour?", SubjectRepo: "acme/app", JobID: fid(141), CreatedAt: at(-8), Ref: "GATE-14", Project: "app", GoalRef: "GOAL-3"},
		gates[2],
	}
	decided := gates[0].Gate
	decided.Status, decided.ResolvedBy, decided.ResolvedAt = domain.GateApproved, "mcp:laptop", t0

	specGate := domain.Gate{ID: fid(33), AccountID: acc.ID, ProjectID: fid(10), Number: 8, Kind: domain.GateSpecApproval, Status: domain.GateChangesRequested, Title: "Approve the spec", Summary: "Spec revision 1.", SubjectRepo: "acme/app", SubjectNumber: 5, GoalID: fid(20), ResolvedBy: "mcp:laptop", Feedback: "Say what happens to expired links.", ResolvedAt: at(-260), CreatedAt: at(-280)}

	s := e.svc
	s.ListProjectsFunc = func(context.Context, domain.Actor, any) ([]domain.Project, error) { return projects, nil }
	s.GetProjectFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error) { return projects[0], nil }
	s.OnboardProjectFunc = func(context.Context, domain.Actor, mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error) {
		return mcp.OnboardProjectOutput{Project: projects[1], RunID: fid(70)}, nil
	}
	s.ListGoalsFunc = func(context.Context, domain.Actor, mcp.ListGoalsInput) ([]domain.Goal, error) { return goals, nil }
	s.CreateGoalFunc = func(context.Context, domain.Actor, mcp.CreateGoalInput) (domain.Goal, error) { return goals[1], nil }
	s.GetGoalFunc = func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error) {
		task := domain.Task{ID: fid(40), AccountID: acc.ID, ProjectID: fid(10), GoalID: fid(20), SpecID: fid(50), Number: 12, Seq: 1, Title: "Send the sign-in link", Status: domain.TaskInReview, IssueNumber: 6, Branch: "agent/task-12", CreatedAt: at(-200), UpdatedAt: at(-50)}
		open := task
		open.ID, open.Number, open.Seq, open.Title, open.Status, open.IssueNumber, open.Branch = fid(41), 13, 2, "Verify the link", domain.TaskOpen, 0, ""
		change := domain.Change{ID: fid(80), AccountID: acc.ID, ProjectID: fid(10), TaskID: task.ID, Repo: "acme/app", Number: 9, Branch: "agent/task-12", BaseRef: "main", HeadSHA: "abc123", State: domain.ChangeOpen, URL: "https://github.com/acme/app/pull/9", CreatedAt: at(-100), UpdatedAt: at(-50)}
		return mcp.GoalDetail{
			Goal: goals[0], Ref: "GOAL-3", Project: "app", URL: "https://github.com/acme/app/issues/5",
			Spec: &domain.Spec{ID: fid(50), AccountID: acc.ID, ProjectID: fid(10), GoalID: fid(20), Number: 2, Revision: 2, Body: specBody, CreatedAt: at(-250)},
			Tasks: []mcp.TaskDetail{
				{Task: task, Ref: "TASK-12", URL: "https://github.com/acme/app/issues/6", Needs: []string{"GATE-9"}, Changes: []mcp.ChangeInfo{{Change: change, Checks: "failure"}}, Verification: &mcp.VerificationRef{Status: domain.VerificationFailed, SHA: "abc1234", Current: true, URL: "/api/v1/changes/TASK-12/verification"}, Iterations: 2, CostUSD: 0.75},
				{Task: open, Ref: "TASK-13", Needs: []string{}, Changes: []mcp.ChangeInfo{}},
			},
			Gates: []mcp.GateInfo{gates[0], {Gate: specGate, Ref: "GATE-8", Project: "app", GoalRef: "GOAL-3", URL: "https://github.com/acme/app/issues/5"}},
			Assumptions: []mcp.AssumptionInfo{{
				ID: fid(90), AccountID: acc.ID, ProjectID: fid(10), Number: 4, GoalID: fid(20), MadeBy: "architect", JobID: fid(70),
				Statement: "Links are sent by the existing mailer.", Confidence: domain.LevelMedium, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest,
				Status: domain.AssumptionUnverified, Dependents: []domain.Dependent{{Kind: domain.DependentGoal, ID: "GOAL-3"}, {Kind: domain.DependentTask, ID: "TASK-12"}},
				CreatedAt: at(-240), UpdatedAt: at(-240),
				Ref: "A-4", Source: "JOB-41",
			}},
			CostUSD: 1.75,
		}, nil
	}
	s.ListJobsFunc = func(context.Context, domain.Actor, mcp.ListJobsInput) ([]domain.Job, error) { return jobs, nil }
	s.DispatchJobFunc = func(context.Context, domain.Actor, mcp.DispatchJobInput) (domain.Job, error) { return jobs[1], nil }
	s.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{Job: jobs[0], Ref: "JOB-41", WorkerName: "build-box-1"}, nil
	}
	s.JobLogsFunc = func(_ context.Context, _ domain.Actor, in mcp.JobLogsInput) (mcp.JobLogsOutput, error) {
		events := []domain.JobEvent{
			{Seq: 1, Time: at(-5), Type: domain.EventStatus, Text: "cloning"},
			{Seq: 2, Time: at(-4), Type: domain.EventMessage, Text: "I will start with the mailer."},
			{Seq: 3, Time: at(-3), Type: domain.EventToolCall, Text: "", Data: json.RawMessage(`{"name":"Bash","input":{"command":"go test ./..."}}`)},
			{Seq: 4, Time: at(-2), Type: domain.EventToolResult, Data: json.RawMessage(`{"name":"Bash","output":"ok","is_error":false}`)},
			{Seq: 5, Time: at(-1), Type: domain.EventLog, Text: "<img src=x onerror=alert(1)> done"},
		}
		if in.AfterSeq >= 5 {
			return mcp.JobLogsOutput{Events: []domain.JobEvent{}, NextSeq: in.AfterSeq}, nil
		}
		return mcp.JobLogsOutput{Events: events, NextSeq: 5}, nil
	}
	s.ListWorkersFunc = func(context.Context, domain.Actor, any) ([]mcp.WorkerInfo, error) {
		return []mcp.WorkerInfo{
			{ID: fid(60), AccountID: acc.ID, Name: "build-box-1", Pool: domain.PoolBuild, Labels: []string{"docker", "go", "linux", "node", "runtime:claude-code"}, Version: "v0.3.1", MaxJobs: 1, LastSeenAt: at(0), CreatedAt: at(-1000), Online: true, RunningJobs: []string{"JOB-41"}},
			{ID: fid(61), AccountID: acc.ID, Name: "mac-mini", Pool: domain.PoolControl, Labels: []string{"macos", "runtime:codex"}, Version: "v0.3.0", MaxJobs: 1, LastSeenAt: at(-120), CreatedAt: at(-900), Online: false, RunningJobs: []string{}},
		}, nil
	}
	s.ListPendingGatesFunc = func(_ context.Context, _ domain.Actor, in mcp.ProjectFilterInput) ([]mcp.GateInfo, error) {
		if in.Project == "council" {
			return councilGates, nil
		}
		return gates, nil
	}
	s.ApproveFunc = func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error) { return decided, nil }
	s.RequestChangesFunc = func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error) {
		g := decided
		g.Status, g.Feedback = domain.GateChangesRequested, "Add a test for the expired link."
		return g, nil
	}
	s.RejectFunc = func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error) {
		g := decided
		g.Status = domain.GateRejected
		return g, nil
	}
	s.GetCouncilFunc = fixtureCouncils(acc)
	s.ListCouncilsFunc = func(context.Context, domain.Actor, mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error) {
		out := []mcp.CouncilInfo{}
		for _, n := range []int64{5, 4, 3, 2} {
			status, brief := domain.CouncilDecided, fixtureBrief()
			switch n {
			case 2:
				status, brief = domain.CouncilRunning, nil
			case 4:
				status = domain.CouncilFailed
			case 5:
				status = domain.CouncilRunning
			}
			c := fixtureCouncil(n, status, brief)
			c.Brief = nil // the list leaves the agent-written brief to get_council
			out = append(out, mcp.CouncilInfo{Council: c, Ref: c.Ref()})
		}
		return out, nil
	}
	s.StartCouncilFunc = func(context.Context, domain.Actor, mcp.StartCouncilInput) (mcp.CouncilInfo, error) {
		c := fixtureCouncil(6, domain.CouncilRunning, nil)
		return mcp.CouncilInfo{Council: c, Ref: c.Ref()}, nil
	}
	s.AddConstraintFunc = func(_ context.Context, _ domain.Actor, in mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error) {
		c := fixtureCouncil(2, domain.CouncilRunning, nil)
		c.Constraints = append(c.Constraints, in.Constraint)
		return mcp.CouncilInfo{Council: c, Ref: c.Ref()}, nil
	}
	assumptions := []mcp.AssumptionInfo{
		{
			ID: fid(91), AccountID: acc.ID, ProjectID: fid(10), Number: 7, GoalID: fid(20), MadeBy: "architect", JobID: fid(70),
			Statement: "Fewer than 50 workers will poll the queue.", Confidence: domain.LevelMedium, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByAskHuman,
			Status: domain.AssumptionUnverified, Dependents: []domain.Dependent{{Kind: domain.DependentGoal, ID: "GOAL-3"}, {Kind: domain.DependentTask, ID: "TASK-12"}},
			CreatedAt: at(-30), UpdatedAt: at(-30), Ref: "A-7", Source: "JOB-41",
		},
		{
			ID: fid(92), AccountID: acc.ID, ProjectID: fid(10), Number: 8, MadeBy: "product-owner", CouncilID: fid(203),
			Statement: "Users read email on their phone.", Confidence: domain.LevelLow, Impact: domain.LevelMedium, VerifyBy: domain.VerifyByResearch,
			Status: domain.AssumptionAccepted, Dependents: []domain.Dependent{}, ResolvedBy: "mcp:laptop", ResolvedAt: at(-2), CreatedAt: at(-50), UpdatedAt: at(-2), Ref: "A-8", Source: fid(203).String(),
		},
	}
	s.ListAssumptionsFunc = func(context.Context, domain.Actor, mcp.ListAssumptionsInput) ([]mcp.AssumptionInfo, error) {
		return assumptions, nil
	}
	s.ResolveAssumptionFunc = func(_ context.Context, _ domain.Actor, in mcp.ResolveAssumptionInput) (mcp.AssumptionInfo, error) {
		a := assumptions[0]
		a.Status, a.Evidence, a.ResolvedBy, a.ResolvedAt = in.Status, in.Evidence, "mcp:laptop", t0
		return a, nil
	}
	s.ListDecisionsFunc = func(context.Context, domain.Actor, mcp.ListDecisionsInput) ([]mcp.DecisionInfo, error) {
		d := domain.Decision{
			ID: fid(600), AccountID: acc.ID, ProjectID: fid(10), Number: 5, GoalID: fid(20), Question: "Which queue should jobs use?",
			Options: []domain.DecisionOption{{Name: "postgres-queue", Summary: "SKIP LOCKED on the jobs table."}, {Name: "redis-streams"}},
			Choice:  "postgres-queue", Reasons: "Already run; no new service.", Dissent: "security@codex: job data should not share the application database.",
			DecidedBy: "user:ana", CouncilID: fid(203), GateID: fid(511), CreatedAt: at(-5),
		}
		auto := d
		auto.ID, auto.Number, auto.Question, auto.Choice, auto.DecidedBy, auto.Dissent, auto.CouncilID, auto.GateID, auto.GoalID, auto.CreatedAt =
			fid(601), 4, "Which license?", "mit", domain.DecidedByPolicy, "", fid(202), uuid.Nil(), uuid.Nil(), at(-100)
		auto.Options, auto.Reasons = nil, "The policy accepts a recommendation nobody dissented from."
		return []mcp.DecisionInfo{{Decision: d, Ref: "DEC-5", Assumptions: []string{"A-7"}}, {Decision: auto, Ref: "DEC-4", Assumptions: []string{}}}, nil
	}
	tokens := []mcp.TokenInfo{
		{ID: fid(90), Name: "laptop", CreatedAt: at(-2000), LastUsedAt: at(-1), Current: true},
		{ID: fid(91), Name: "old-laptop", CreatedAt: at(-3000), LastUsedAt: at(-1500)},
		{ID: fid(92), Name: "ci-box", CreatedAt: at(-2500), RevokedAt: at(-700)},
	}
	s.ListTokensFunc = func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error) { return tokens, nil }
	s.RevokeTokenFunc = func(_ context.Context, _ domain.Actor, in mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
		tok := tokens[1]
		if in.Token == tokens[0].ID.String() {
			tok = tokens[0]
		}
		tok.RevokedAt = t0
		return mcp.NewRevokeTokenOutput(tok, tok.Current), nil
	}
	s.CancelJobFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		j := jobs[0]
		j.Status, j.FinishedAt, j.Error = domain.JobCanceled, t0, "canceled"
		return mcp.JobInfo{Job: j, Ref: "JOB-41", WorkerName: "build-box-1"}, nil
	}
	cells := []mcp.QualityCell{
		{Environment: "linux", Cell: map[string]string{"go": "1.26"}, Suite: "e2e", Status: domain.TestRunFailed, Counts: domain.TestCounts{Passed: 7, Failed: 2}, Job: "JOB-45",
			Failures: []domain.TestResult{
				{Name: "login redirects", Classname: "web", Status: domain.TestFailed, Message: "expected /home <b>got</b> /login"},
				{Name: "link expires", Classname: "web", Status: domain.TestErrored, Message: "timeout"},
			}},
		{Environment: "linux", Cell: map[string]string{"go": "1.26"}, Suite: "unit", Status: domain.TestRunPassed, Counts: domain.TestCounts{Passed: 40, Skipped: 1}, Job: "JOB-44", Failures: []domain.TestResult{}},
		{Environment: "", Cell: map[string]string{}, Suite: "lint", Status: domain.TestRunPassed, Counts: domain.TestCounts{Passed: 1}, Job: "JOB-46", Failures: []domain.TestResult{}},
	}
	s.VerificationReportFunc = func(context.Context, domain.Actor, mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
		return mcp.VerificationInfo{
			ID: fid(120), SHA: "abc1234", Status: domain.VerificationFailed, Report: json.RawMessage(reportJSON), ArtifactID: fid(121), CreatedAt: at(-40), UpdatedAt: at(-30),
			Change: 9, URL: "https://github.com/acme/app/pull/9", Current: true,
		}, nil
	}
	s.ProjectQualityFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (mcp.QualityInfo, error) {
		verified, _ := s.VerificationReportFunc(context.Background(), domain.Actor{}, mcp.VerificationReportInput{})
		return mcp.QualityInfo{
			Project: "app", DefaultBranch: "main",
			Baseline: &mcp.QualityBaseline{SHA: "0123456789abcdef", Cells: []mcp.QualityCell{
				{Environment: "linux", Cell: map[string]string{"go": "1.26"}, Suite: "unit", Status: domain.TestRunPassed, Counts: domain.TestCounts{Passed: 40}, Job: "JOB-30", Failures: []domain.TestResult{}},
				{Environment: "linux", Cell: map[string]string{"go": "1.26"}, Suite: "e2e", Status: domain.TestRunFailed, Counts: domain.TestCounts{Passed: 8, Failed: 1}, Job: "JOB-31",
					Failures: []domain.TestResult{{Name: "link expires", Classname: "web", Status: domain.TestFailed, Message: "known failure"}}},
			}},
			Changes: []mcp.QualityChange{
				{Number: 9, URL: "https://github.com/acme/app/pull/9", Branch: "agent/task-12", SHA: "abc1234", Task: "TASK-12", Cells: cells, Verification: &verified},
				{Number: 10, URL: "https://github.com/acme/app/pull/10", Branch: "dark-mode", SHA: "def5678", Cells: []mcp.QualityCell{}},
			},
		}, nil
	}
	s.ListArtifactsFunc = func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error) {
		return []domain.Artifact{
			{ID: fid(130), AccountID: acc.ID, ProjectID: fid(10), JobID: fid(141), Name: "reports/unit.xml", ContentType: "application/xml", Size: 20480, SHA256: strings.Repeat("ab", 32), CreatedAt: at(-30)},
			{ID: fid(131), AccountID: acc.ID, ProjectID: fid(10), JobID: fid(141), Name: "screenshots/login.png", ContentType: "image/png", Size: 1572864, SHA256: strings.Repeat("cd", 32), CreatedAt: at(-30)},
		}, nil
	}
	return e
}

func fixtureJob(number int64, status domain.JobStatus, role string, attempts int, started time.Time) domain.Job {
	return domain.Job{
		ID: fid(int(number) + 100), AccountID: fid(1), ProjectID: fid(10), Number: number, Status: status, Role: role, Runtime: "claude-code",
		Needs: []string{"go", "linux"}, GoalID: fid(20),
		Spec: domain.JobSpec{
			JobID: fid(int(number) + 100), AccountID: fid(1), Repo: "acme/app", BaseRef: "main", Branch: "agent/task-12", Role: role,
			Runtime: "claude-code", Model: "claude-sonnet-5", Prompt: "Implement TASK-12.", AllowedTools: []string{"Read", "Edit"}, MaxTurns: 40, Timeout: domain.Duration(30 * time.Minute),
		},
		Attempts: attempts, MaxAttempts: domain.DefaultMaxAttempts, StartedAt: started, Usage: domain.Usage{InputTokens: 1200, OutputTokens: 340},
		CostUSD: 0.12, CreatedAt: at(-120), UpdatedAt: at(-1),
	}
}

type fixtureCase struct {
	name, method, path, body string
	status                   int
	auth                     string
	prepare                  func(*env)
}

func emptyLists(e *env) {
	s := e.svc
	s.ListProjectsFunc, s.ListGoalsFunc, s.ListJobsFunc, s.ListWorkersFunc, s.ListPendingGatesFunc = nil, nil, nil, nil, nil
}

// profileOf makes get_project answer with the given approved profile.
func profileOf(profile string) func(*env) {
	return func(e *env) {
		get := e.svc.GetProjectFunc
		e.svc.GetProjectFunc = func(ctx context.Context, by domain.Actor, in mcp.ProjectInput) (domain.Project, error) {
			p, err := get(ctx, by, in)
			p.Profile = json.RawMessage(profile)
			return p, err
		}
	}
}

func failWith(err error) func(*env) {
	return func(e *env) {
		e.svc.ApproveFunc = func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error) {
			return domain.Gate{}, err
		}
		e.svc.RequestChangesFunc = e.svc.ApproveFunc
	}
}

func fixtureCases() []fixtureCase {
	return []fixtureCase{
		{"session", "POST", "/api/v1/session", `{"token":"` + token + `"}`, 200, "", nil},
		{"me", "GET", "/api/v1/me", "", 200, "bearer", nil},
		{"projects", "GET", "/api/v1/projects", "", 200, "bearer", nil},
		{"projects-empty", "GET", "/api/v1/projects", "", 200, "bearer", emptyLists},
		{"project", "GET", "/api/v1/projects/acme/app", "", 200, "bearer", nil},
		{"project-minimal-profile", "GET", "/api/v1/projects/acme/app", "", 200, "bearer", profileOf(`{"version":1,"name":"app","summary":"Small."}`)},
		{"project-unknown-commands", "GET", "/api/v1/projects/acme/app", "", 200, "bearer", profileOf(`{"version":1,"name":"app","summary":"x","components":[{"path":".","stack":"make"}]}`)},
		{"project-onboard", "POST", "/api/v1/projects", `{"repo":"acme/docs"}`, 202, "bearer", nil},
		{"goals", "GET", "/api/v1/goals", "", 200, "bearer", nil},
		{"goals-empty", "GET", "/api/v1/goals", "", 200, "bearer", emptyLists},
		{"goal", "GET", "/api/v1/goals/GOAL-3", "", 200, "bearer", nil},
		{"goal-create", "POST", "/api/v1/goals", `{"project":"app","title":"Dark mode","body":"Follow the system setting."}`, 201, "bearer", nil},
		{"jobs", "GET", "/api/v1/jobs", "", 200, "bearer", nil},
		{"jobs-empty", "GET", "/api/v1/jobs", "", 200, "bearer", emptyLists},
		{"job", "GET", "/api/v1/jobs/JOB-41", "", 200, "bearer", nil},
		{"job-queued", "GET", "/api/v1/jobs/JOB-42", "", 200, "bearer", func(e *env) {
			e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
				j := fixtureJob(42, domain.JobQueued, "reviewer", 0, time.Time{})
				return mcp.JobInfo{Job: j, Ref: "JOB-42", Reason: "no online worker in pool build with labels go, linux, runtime:claude-code"}, nil
			}
		}},
		{"job-failed", "GET", "/api/v1/jobs/JOB-43", "", 200, "bearer", func(e *env) {
			e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
				j := fixtureJob(43, domain.JobFailed, "developer", 3, at(-30))
				j.FinishedAt, j.Error = at(-20), "attempts exhausted: lease expired"
				return mcp.JobInfo{Job: j, Ref: "JOB-43", Reason: j.Error, WorkerName: "build-box-1"}, nil
			}
		}},
		{"job-dispatch", "POST", "/api/v1/jobs", `{"project":"app","role":"reviewer","prompt":"Look at the login flow."}`, 201, "bearer", nil},
		{"job-logs", "GET", "/api/v1/jobs/JOB-41/logs", "", 200, "bearer", nil},
		{"job-logs-caught-up", "GET", "/api/v1/jobs/JOB-41/logs?after_seq=5", "", 200, "bearer", nil},
		{"job-logs-empty", "GET", "/api/v1/jobs/JOB-42/logs", "", 200, "bearer", func(e *env) {
			e.svc.JobLogsFunc = func(context.Context, domain.Actor, mcp.JobLogsInput) (mcp.JobLogsOutput, error) {
				return mcp.JobLogsOutput{Events: []domain.JobEvent{}}, nil
			}
		}},
		{"job-logs-all-kinds", "GET", "/api/v1/jobs/JOB-41/logs", "", 200, "bearer", func(e *env) {
			e.svc.JobLogsFunc = func(context.Context, domain.Actor, mcp.JobLogsInput) (mcp.JobLogsOutput, error) {
				return mcp.JobLogsOutput{NextSeq: 5, Events: []domain.JobEvent{
					{Seq: 1, Time: at(-5), Type: domain.EventUsage, Data: json.RawMessage(`{"input_tokens":5,"output_tokens":2,"cost_usd":0.01}`)},
					{Seq: 2, Time: at(-4), Type: domain.EventToolResult, Data: json.RawMessage(`{"name":"Bash","output":"boom","is_error":true}`)},
					{Seq: 3, Time: at(-3), Type: domain.EventError, Text: "it broke"},
					{Seq: 4, Time: at(-2), Type: domain.EventToolCall},
					{Seq: 5, Time: at(-1), Type: domain.EventToolResult},
				}}, nil
			}
		}},
		{"job-cancel", "POST", "/api/v1/jobs/JOB-41/cancel", "", 200, "bearer", nil},
		{"job-cancel-conflict", "POST", "/api/v1/jobs/JOB-41/cancel", "", 409, "bearer", func(e *env) {
			e.svc.CancelJobFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
				return mcp.JobInfo{}, fmt.Errorf("job JOB-41 is succeeded: %w", domain.ErrInvalidTransition)
			}
		}},
		{"job-artifacts", "GET", "/api/v1/jobs/JOB-44/artifacts", "", 200, "bearer", nil},
		{"job-artifacts-empty", "GET", "/api/v1/jobs/JOB-41/artifacts", "", 200, "bearer", func(e *env) {
			e.svc.ListArtifactsFunc = func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error) {
				return []domain.Artifact{}, nil
			}
		}},
		{"job-command", "GET", "/api/v1/jobs/JOB-45", "", 200, "bearer", func(e *env) {
			e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
				j := fixtureJob(45, domain.JobFailed, "", 1, at(-40))
				j.Kind, j.Runtime, j.FinishedAt, j.CostUSD = domain.JobKindCommand, "", at(-35), 0
				j.Spec.Runtime, j.Spec.Model = "", ""
				return mcp.JobInfo{Job: j, Ref: "JOB-45", WorkerName: "build-box-1",
					Tests: &mcp.TestSummary{Runs: 1, Status: domain.TestRunFailed, Passed: 7, Failed: 2, Skipped: 1}}, nil
			}
		}},
		{"quality", "GET", "/api/v1/quality?project=app", "", 200, "bearer", nil},
		{"quality-empty", "GET", "/api/v1/quality?project=app", "", 200, "bearer", func(e *env) {
			e.svc.ProjectQualityFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (mcp.QualityInfo, error) {
				return mcp.QualityInfo{Project: "app", DefaultBranch: "main", Changes: []mcp.QualityChange{}}, nil
			}
		}},
		{"quality-baseline-only", "GET", "/api/v1/quality?project=app", "", 200, "bearer", func(e *env) {
			get := e.svc.ProjectQualityFunc
			e.svc.ProjectQualityFunc = func(ctx context.Context, by domain.Actor, in mcp.ProjectInput) (mcp.QualityInfo, error) {
				q, err := get(ctx, by, in)
				q.Changes = []mcp.QualityChange{}
				return q, err
			}
		}},
		{"verification", "GET", "/api/v1/changes/9/verification?project=app", "", 200, "bearer", nil},
		{"workers", "GET", "/api/v1/workers", "", 200, "bearer", nil},
		{"workers-empty", "GET", "/api/v1/workers", "", 200, "bearer", emptyLists},
		{"gates", "GET", "/api/v1/gates", "", 200, "bearer", nil},
		{"gates-empty", "GET", "/api/v1/gates", "", 200, "bearer", emptyLists},
		{"gates-council", "GET", "/api/v1/gates?project=council", "", 200, "bearer", nil},
		{"councils", "GET", "/api/v1/councils?project=app", "", 200, "bearer", nil},
		{"councils-empty", "GET", "/api/v1/councils?project=app", "", 200, "bearer", func(e *env) {
			e.svc.ListCouncilsFunc = func(context.Context, domain.Actor, mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error) {
				return []mcp.CouncilInfo{}, nil
			}
		}},
		{"council-running", "GET", "/api/v1/councils/COUNCIL-2", "", 200, "bearer", nil},
		{"council-decided", "GET", "/api/v1/councils/COUNCIL-3", "", 200, "bearer", nil},
		{"council-failed", "GET", "/api/v1/councils/COUNCIL-4", "", 200, "bearer", nil},
		{"council-pending", "GET", "/api/v1/councils/COUNCIL-5", "", 200, "bearer", nil},
		{"council-start", "POST", "/api/v1/councils", `{"project":"app","question":"Which queue should jobs use?"}`, 202, "bearer", nil},
		{"council-constraint", "POST", "/api/v1/councils/COUNCIL-2/constraints", `{"constraint":"No new services"}`, 200, "bearer", nil},
		{"council-constraint-conflict", "POST", "/api/v1/councils/COUNCIL-3/constraints", `{"constraint":"No new services"}`, 409, "bearer", func(e *env) {
			e.svc.AddConstraintFunc = func(context.Context, domain.Actor, mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error) {
				return mcp.CouncilInfo{}, fmt.Errorf("COUNCIL-3 is decided: %w", domain.ErrInvalidTransition)
			}
		}},
		{"assumptions", "GET", "/api/v1/assumptions?project=app", "", 200, "bearer", nil},
		{"assumption-resolve", "POST", "/api/v1/assumptions/A-7/resolve", `{"status":"verified","evidence":"Load test with 80 workers."}`, 200, "bearer", nil},
		{"decisions", "GET", "/api/v1/decisions?project=app", "", 200, "bearer", nil},
		{"decisions-empty", "GET", "/api/v1/decisions?project=app", "", 200, "bearer", func(e *env) {
			e.svc.ListDecisionsFunc = func(context.Context, domain.Actor, mcp.ListDecisionsInput) ([]mcp.DecisionInfo, error) {
				return []mcp.DecisionInfo{}, nil
			}
		}},
		{"gate-approve", "POST", "/api/v1/gates/GATE-9/approve", `{"feedback":"Looks good."}`, 200, "bearer", nil},
		{"gate-request-changes", "POST", "/api/v1/gates/GATE-9/request-changes", `{"feedback":"Add a test for the expired link."}`, 200, "bearer", nil},
		{"gate-reject", "POST", "/api/v1/gates/GATE-11/reject", "", 200, "bearer", nil},
		{"tokens", "GET", "/api/v1/tokens", "", 200, "bearer", nil},
		{"tokens-empty", "GET", "/api/v1/tokens", "", 200, "bearer", func(e *env) {
			e.svc.ListTokensFunc = func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error) { return []mcp.TokenInfo{}, nil }
		}},
		{"token-revoke", "POST", "/api/v1/tokens/" + fid(91).String() + "/revoke", "", 200, "bearer", nil},
		{"token-revoke-self", "POST", "/api/v1/tokens/" + fid(90).String() + "/revoke", "", 200, "bearer", nil},
		{"error-unauthorized", "GET", "/api/v1/me", "", 401, "", nil},
		{"error-not-found", "GET", "/api/v1/goals/GOAL-99", "", 404, "bearer", func(e *env) {
			e.svc.GetGoalFunc = func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error) {
				return mcp.GoalDetail{}, fmt.Errorf("goal GOAL-99: %w", mcp.ErrNotFound)
			}
		}},
		{"error-invalid-input", "POST", "/api/v1/gates/GATE-9/request-changes", `{}`, 400, "bearer", failWith(fmt.Errorf("feedback is required: %w", mcp.ErrInvalidInput))},
		{"error-conflict", "POST", "/api/v1/gates/GATE-9/approve", `{}`, 409, "bearer", failWith(fmt.Errorf("gate GATE-9 is already approved: %w", domain.ErrInvalidTransition))},
	}
}

func TestFixtures(t *testing.T) {
	var names []string
	for _, c := range fixtureCases() {
		names = append(names, c.name+".json")
		t.Run(c.name, func(t *testing.T) {
			e := fixtureEnv(t)
			if c.prepare != nil {
				c.prepare(e)
			}
			r := e.do(t, c.method, c.path, c.body, c.auth)
			if r.status != c.status {
				t.Fatalf("status %d, want %d: %s", r.status, c.status, r.body)
			}
			got, err := prettyJSON(r.body)
			must(t, err)
			must(t, checkGolden(filepath.Join(fixtureDir, c.name+".json"), got, *update))
		})
	}
	if !*update {
		entries, err := os.ReadDir(fixtureDir)
		must(t, err)
		for _, ent := range entries {
			if !slices.Contains(names, ent.Name()) && ent.Name() != ".gitkeep" {
				t.Errorf("fixture %s belongs to no endpoint: remove it", ent.Name())
			}
		}
	}
}

func prettyJSON(body string) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(body), "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// checkGolden compares got with the file at path, or writes it when update is set.
func checkGolden(path string, got []byte, update bool) error {
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, got, 0o644) //nolint:gosec // a fixture, not a secret
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist: run go test ./internal/api -update", path)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf("%s no longer matches the API response: run go test ./internal/api -update and commit the result\n--- fixture\n%s\n--- response\n%s", path, want, got)
	}
	return nil
}

func TestCheckGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.json")
	body := []byte("{\n  \"a\": 1\n}\n")
	if err := checkGolden(path, body, false); err == nil || !strings.Contains(err.Error(), "-update") {
		t.Errorf("missing fixture: %v", err)
	}
	must(t, checkGolden(path, body, true))
	must(t, checkGolden(path, body, false))
	// A renamed field must fail until the fixture is regenerated.
	renamed := []byte("{\n  \"b\": 1\n}\n")
	if err := checkGolden(path, renamed, false); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Errorf("changed field: %v", err)
	}
	must(t, checkGolden(path, renamed, true))
	must(t, checkGolden(path, renamed, false))
	if err := checkGolden(t.TempDir(), body, false); err == nil {
		t.Error("a directory is not a fixture")
	}
}
