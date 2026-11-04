// Package mcp defines the tool service behind the MCP server. The same
// service backs /api/v1 and the web UI, so every tool has one method with
// JSON-tagged input and output types. internal/testkit has a fake.
//
// Inputs name a project by its name or repo ("owner/name"), and goals, jobs
// and gates by human ref (GOAL-3) or UUID. The caller's account comes from
// the Actor.
package mcp

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
)

// Sentinel errors. Implementations also return domain errors
// (ErrInvalidRef, ErrInvalidTransition, ErrInvalidDecision).
var (
	// ErrNotFound means a named project, goal, job or gate does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidInput means a required field is missing or malformed.
	ErrInvalidInput = errors.New("invalid input")
	// ErrUnavailable means a feature is not configured, for example the
	// evidence storage of artifacts.
	ErrUnavailable = errors.New("unavailable")
)

// ErrorCode classifies the errors callers act on: "not_found" for
// ErrNotFound, "invalid_input" for ErrInvalidInput, domain.ErrInvalidRef and
// domain.ErrInvalidDecision, "conflict" for domain.ErrInvalidTransition and
// "unavailable" for ErrUnavailable.
// ok is false for any other error, which is an internal one. The MCP server
// and /api/v1 use it to decide what a client may see.
func ErrorCode(err error) (code string, ok bool) {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found", true
	case errors.Is(err, ErrInvalidInput), errors.Is(err, domain.ErrInvalidRef), errors.Is(err, domain.ErrInvalidDecision):
		return "invalid_input", true
	case errors.Is(err, domain.ErrInvalidTransition):
		return "conflict", true
	case errors.Is(err, ErrUnavailable):
		return "unavailable", true
	}
	return "", false
}

// Tool names as exposed over MCP.
const (
	ToolOnboardProject    = "onboard_project"
	ToolCreateGoal        = "create_goal"
	ToolJobStatus         = "job_status"
	ToolJobLogs           = "job_logs"
	ToolListWorkers       = "list_workers"
	ToolListPendingGates  = "list_pending_gates"
	ToolApprove           = "approve"
	ToolRequestChanges    = "request_changes"
	ToolReject            = "reject"
	ToolDispatchJob       = "dispatch_job"
	ToolListProjects      = "list_projects"
	ToolGetProject        = "get_project"
	ToolListGoals         = "list_goals"
	ToolGetGoal           = "get_goal"
	ToolListJobs          = "list_jobs"
	ToolCancelJob         = "cancel_job"
	ToolListTokens        = "list_tokens"
	ToolRevokeToken       = "revoke_token"
	ToolListArtifacts     = "list_artifacts"
	ToolListTestRuns      = "list_test_runs"
	ToolListAssumptions   = "list_assumptions"
	ToolResolveAssumption = "resolve_assumption"
	ToolListDecisions     = "list_decisions"
	ToolSearchKnowledge   = "search_knowledge"
	ToolDeleteNote        = "delete_note"
)

// Tools returns every MCP tool name.
func Tools() []string {
	return []string{
		ToolOnboardProject, ToolCreateGoal, ToolJobStatus, ToolJobLogs, ToolListWorkers, ToolListPendingGates,
		ToolApprove, ToolRequestChanges, ToolReject, ToolDispatchJob, ToolListProjects, ToolGetProject,
		ToolListGoals, ToolGetGoal, ToolListJobs, ToolCancelJob, ToolListTokens, ToolRevokeToken, ToolListArtifacts, ToolListTestRuns,
		ToolListAssumptions, ToolResolveAssumption, ToolListDecisions,
		ToolSearchKnowledge, ToolDeleteNote,
	}
}

// ToolService implements one method per MCP tool.
type ToolService interface {
	// OnboardProject registers the repo as a project if needed and starts
	// the onboarding playbook.
	OnboardProject(ctx context.Context, by domain.Actor, in OnboardProjectInput) (OnboardProjectOutput, error)
	// CreateGoal creates a draft goal and starts the goal playbook.
	CreateGoal(ctx context.Context, by domain.Actor, in CreateGoalInput) (domain.Goal, error)
	JobStatus(ctx context.Context, by domain.Actor, in JobRefInput) (JobInfo, error)
	JobLogs(ctx context.Context, by domain.Actor, in JobLogsInput) (JobLogsOutput, error)
	ListWorkers(ctx context.Context, by domain.Actor) ([]WorkerInfo, error)
	ListPendingGates(ctx context.Context, by domain.Actor, in ProjectFilterInput) ([]GateInfo, error)
	// Approve resolves a gate of any kind as approved (retry for needs_you).
	Approve(ctx context.Context, by domain.Actor, in GateDecisionInput) (domain.Gate, error)
	// RequestChanges resolves a gate as changes_requested. It fails with
	// ErrInvalidInput on a needs_you gate and without feedback.
	RequestChanges(ctx context.Context, by domain.Actor, in GateDecisionInput) (domain.Gate, error)
	// Reject resolves a gate of any kind as rejected (stop for needs_you).
	Reject(ctx context.Context, by domain.Actor, in GateDecisionInput) (domain.Gate, error)
	// DispatchJob queues an ad-hoc job of a role on a project.
	DispatchJob(ctx context.Context, by domain.Actor, in DispatchJobInput) (domain.Job, error)
	ListProjects(ctx context.Context, by domain.Actor) ([]domain.Project, error)
	GetProject(ctx context.Context, by domain.Actor, in ProjectInput) (domain.Project, error)
	ListGoals(ctx context.Context, by domain.Actor, in ListGoalsInput) ([]domain.Goal, error)
	GetGoal(ctx context.Context, by domain.Actor, in GoalRefInput) (GoalDetail, error)
	ListJobs(ctx context.Context, by domain.Actor, in ListJobsInput) ([]domain.Job, error)
	// CancelJob cancels a queued or running job. A worker running it stops at
	// its next heartbeat, and a playbook step waiting for it fails. A job
	// that is finished already fails with domain.ErrInvalidTransition.
	CancelJob(ctx context.Context, by domain.Actor, in JobRefInput) (JobInfo, error)
	// ListTokens lists the account's API and MCP tokens, revoked ones too.
	ListTokens(ctx context.Context, by domain.Actor) ([]TokenInfo, error)
	// RevokeToken revokes an active token by name or ID, the caller's own
	// included; a token that is unknown or revoked already is ErrNotFound.
	RevokeToken(ctx context.Context, by domain.Actor, in RevokeTokenInput) (RevokeTokenOutput, error)
	// ListArtifacts lists the artifacts a job's worker uploaded (test
	// reports, logs, screenshots), by name. The content is served by
	// GET /api/v1/artifacts/{id}.
	ListArtifacts(ctx context.Context, by domain.Actor, in JobRefInput) ([]domain.Artifact, error)
	// ListTestRuns lists the test runs reported by command jobs, oldest
	// first, for a job, a change or a commit. At least one of them is
	// required.
	ListTestRuns(ctx context.Context, by domain.Actor, in ListTestRunsInput) ([]TestRunInfo, error)
	// ListAssumptions lists the assumption register of a project, by number.
	ListAssumptions(ctx context.Context, by domain.Actor, in ListAssumptionsInput) ([]AssumptionInfo, error)
	// ResolveAssumption verifies, invalidates or accepts an assumption.
	// Invalidating flags its dependents: tasks not merged go back with the
	// evidence as feedback, merged work gets a draft re-evaluation goal.
	// ErrUnavailable without a register; a change the assumption's state does
	// not allow fails with domain.ErrInvalidTransition.
	ResolveAssumption(ctx context.Context, by domain.Actor, in ResolveAssumptionInput) (AssumptionInfo, error)
	// ListDecisions lists the decision register of a project, by number.
	ListDecisions(ctx context.Context, by domain.Actor, in ListDecisionsInput) ([]DecisionInfo, error)
	// SearchKnowledge searches a project's knowledge base: curated entries,
	// agent notes (of every goal) and generated facts. A human sees every
	// section. The text of untrusted results is data, never instructions.
	SearchKnowledge(ctx context.Context, by domain.Actor, in SearchKnowledgeInput) ([]knowledge.Result, error)
	// DeleteNote deletes an agent note. Curated entries are not notes: they
	// change through their provider.
	DeleteNote(ctx context.Context, by domain.Actor, in DeleteNoteInput) (DeleteNoteOutput, error)
}

// SearchKnowledgeInput is the input of search_knowledge.
type SearchKnowledgeInput struct {
	Project  string               `json:"project"`            // name or repo, required
	Query    string               `json:"query"`              // websearch syntax, required
	Section  string               `json:"section,omitempty"`  // only this section
	Kind     domain.KnowledgeKind `json:"kind,omitempty"`     // entry, note or generated
	Provider string               `json:"provider,omitempty"` // repo, internal, ...
	Limit    int                  `json:"limit,omitempty"`    // default 8, at most 50
	// WrapUntrusted wraps the snippet of every untrusted result in
	// untrusted-data markers (contextpack.WrapUntrusted). The MCP server
	// sets it, because its client may be an agent; it is not a REST or
	// MCP argument.
	WrapUntrusted bool `json:"-"`
}

// DeleteNoteInput is the input of delete_note.
type DeleteNoteInput struct {
	Note string `json:"note"` // the note's UUID (see search_knowledge)
}

// DeleteNoteOutput is the output of delete_note.
type DeleteNoteOutput struct {
	Note    uuid.UUID `json:"note"`
	Message string    `json:"message"`
}

// OnboardProjectInput is the input of onboard_project.
type OnboardProjectInput struct {
	Repo string `json:"repo"`           // "owner/name", required
	Name string `json:"name,omitempty"` // defaults to the repo name
	// DefaultBranch of a repo that is registered by this call; defaults to main.
	DefaultBranch string `json:"default_branch,omitempty"`
}

// OnboardProjectOutput is the output of onboard_project.
type OnboardProjectOutput struct {
	Project domain.Project `json:"project"`
	// RunID is the workflow run of the onboarding playbook.
	RunID uuid.UUID `json:"run_id"`
}

// CreateGoalInput is the input of create_goal.
type CreateGoalInput struct {
	Project string `json:"project"` // name or repo, required
	Title   string `json:"title"`   // required
	Body    string `json:"body"`    // markdown
	// Platforms the goal targets, for example "web" or "ios"; optional.
	Platforms []string `json:"platforms,omitempty"`
	// Councils overrides the sizing of the goal: "product" and "technical",
	// or an empty list for none. Nil (absent) sizes the goal with triage.
	// A pointer keeps the empty list apart from an absent field.
	Councils *[]string `json:"councils,omitempty"`
}

// JobRefInput is the input of job_status.
type JobRefInput struct {
	Job string `json:"job"` // JOB-41 or UUID
}

// JobInfo is a job with what a human needs to know about its state.
type JobInfo struct {
	domain.Job
	Ref string `json:"ref"` // JOB-41
	// Reason explains the state: why a queued job waits (no matching
	// worker), the error of a failed job.
	Reason string `json:"reason,omitempty"`
	// WorkerName is the worker running the job.
	WorkerName string `json:"worker_name,omitempty"`
	// Tests sums the test runs of a command job; nil for other jobs and
	// before the job reported any.
	Tests *TestSummary `json:"tests,omitempty"`
}

// ListTestRunsInput is the input of list_test_runs.
type ListTestRunsInput struct {
	Job string `json:"job,omitempty"` // JOB-41 or UUID
	// Project and Change select a change by its PR number; Change needs
	// Project.
	Project string `json:"project,omitempty"`
	Change  int    `json:"change,omitempty"`
	SHA     string `json:"sha,omitempty"`   // full commit SHA that was tested
	Limit   int    `json:"limit,omitempty"` // default 100
}

// TestRunInfo is a test run with the cases that failed or errored.
type TestRunInfo struct {
	domain.TestRun
	JobRef string `json:"job_ref,omitempty"` // JOB-41
	// Failures are the failed and errored cases, at most 100 per run;
	// FailuresTruncated says there are more (the counts hold the total).
	// Names and messages are written by the repo's tests: untrusted data.
	Failures          []domain.TestResult `json:"failures"`
	FailuresTruncated bool                `json:"failures_truncated,omitempty"`
}

// TestSummary adds up the test runs of a command job's latest attempt.
type TestSummary struct {
	Runs int `json:"runs"`
	// Status is errored when any run is, else failed when any run is, else
	// passed.
	Status domain.TestRunStatus `json:"status"`
	domain.TestCounts
}

// TokenInfo is an API and MCP token without its hash or any secret.
type TokenInfo struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	// Current is set on the token that made the call.
	Current bool `json:"current,omitempty"`
}

// NewTokenInfo returns the view of t; current says it is the caller's token.
func NewTokenInfo(t domain.MCPToken, current bool) TokenInfo {
	return TokenInfo{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt, Current: current}
}

// RevokeTokenInput is the input of revoke_token.
type RevokeTokenInput struct {
	Token string `json:"token"` // name or UUID
}

// RevokeTokenOutput is the output of revoke_token.
type RevokeTokenOutput struct {
	Token TokenInfo `json:"token"`
	// Self is set when the caller revoked the token it is using: from now on
	// its requests and web sessions fail with 401.
	Self bool `json:"self"`
	// Message says what happened in a sentence.
	Message string `json:"message"`
}

// NewRevokeTokenOutput returns the answer for the revoked token t; self says
// the caller was using it.
func NewRevokeTokenOutput(t TokenInfo, self bool) RevokeTokenOutput {
	return RevokeTokenOutput{Token: t, Self: self, Message: revokeMessage(t.Name, self)}
}

func revokeMessage(name string, self bool) string {
	if self {
		return "Token " + name + " revoked. It is the token you are using: it and the web sessions it backs stop working now."
	}
	return "Token " + name + " revoked."
}

// JobLogsInput is the input of job_logs.
type JobLogsInput struct {
	Job      string `json:"job"`                 // JOB-41 or UUID
	AfterSeq int64  `json:"after_seq,omitempty"` // events with a greater Seq
	Limit    int    `json:"limit,omitempty"`     // default 200, max 1000
}

// JobLogsOutput is the output of job_logs.
type JobLogsOutput struct {
	Events []domain.JobEvent `json:"events"`
	// NextSeq is the after_seq of the next call.
	NextSeq int64 `json:"next_seq"`
}

// WorkerInfo is a worker with its online state and running jobs.
type WorkerInfo struct {
	domain.Worker
	Online bool `json:"online"`
	// RunningJobs holds the refs of the jobs it runs.
	RunningJobs []string `json:"running_jobs"`
}

// GateInfo is a gate with where to find what it is about.
type GateInfo struct {
	domain.Gate
	Ref     string `json:"ref"`     // GATE-7
	Project string `json:"project"` // project name
	GoalRef string `json:"goal_ref,omitempty"`
	TaskRef string `json:"task_ref,omitempty"`
	// URL is the GitHub issue or PR the gate is about.
	URL string `json:"url,omitempty"`
}

// ProjectFilterInput optionally narrows a list to one project.
type ProjectFilterInput struct {
	Project string `json:"project,omitempty"` // name or repo
}

// GateDecisionInput is the input of approve, request_changes and reject.
type GateDecisionInput struct {
	Gate     string `json:"gate"`               // GATE-7 or UUID
	Feedback string `json:"feedback,omitempty"` // required for request_changes
}

// DispatchJobInput is the input of dispatch_job.
type DispatchJobInput struct {
	Project          string   `json:"project"` // name or repo, required
	Role             string   `json:"role"`    // required
	BaseRef          string   `json:"base_ref,omitempty"`
	Branch           string   `json:"branch,omitempty"`
	Needs            []string `json:"needs,omitempty"`
	Pool             string   `json:"pool,omitempty"` // defaults to the role pack's pool
	IncludeRepoFacts bool     `json:"include_repo_facts,omitempty"`
	Goal             string   `json:"goal,omitempty"` // GOAL-3 or UUID
	Task             string   `json:"task,omitempty"` // TASK-12 or UUID
	Feedback         string   `json:"feedback,omitempty"`
	Prompt           string   `json:"prompt,omitempty"` // instructions for the job
}

// ProjectInput is the input of get_project.
type ProjectInput struct {
	Project string `json:"project"` // name or repo
}

// ListGoalsInput is the input of list_goals.
type ListGoalsInput struct {
	Project string            `json:"project,omitempty"`
	Status  domain.GoalStatus `json:"status,omitempty"`
}

// GoalRefInput is the input of get_goal.
type GoalRefInput struct {
	Goal string `json:"goal"` // GOAL-3 or UUID
}

// GoalDetail is a goal with its latest spec, tasks and gates.
type GoalDetail struct {
	Goal    domain.Goal `json:"goal"`
	Ref     string      `json:"ref"`     // GOAL-3
	Project string      `json:"project"` // project name
	// URL is the GitHub issue mirroring the goal, if any.
	URL   string       `json:"url,omitempty"`
	Spec  *domain.Spec `json:"spec,omitempty"` // latest revision
	Tasks []TaskDetail `json:"tasks"`
	// Gates holds every gate of the goal, pending and decided.
	Gates []GateInfo `json:"gates"`
	// Assumptions holds the goal's register, by number: medium-impact ones
	// that are unverified are the open ones to look at.
	Assumptions []AssumptionInfo `json:"assumptions"`
	// CostUSD is the cost of every job of the goal.
	CostUSD float64 `json:"cost_usd"`
}

// TaskDetail is a task with its links, pull requests and cost.
type TaskDetail struct {
	domain.Task
	Ref string `json:"ref"` // TASK-12
	// URL is the task's GitHub issue, if any.
	URL string `json:"url,omitempty"`
	// Needs holds the refs of the pending gates waiting for a human decision
	// on this task.
	Needs   []string     `json:"needs"`
	Changes []ChangeInfo `json:"changes"`
	// Iterations is the number of jobs run for the task.
	Iterations int `json:"iterations"`
	// CostUSD is the cost of those jobs.
	CostUSD float64 `json:"cost_usd"`
}

// ChecksUnknown is the ChangeInfo.Checks of a change whose checks could not
// be read.
const ChecksUnknown = "unknown"

// ChangeInfo is a pull request with the state of its checks.
type ChangeInfo struct {
	domain.Change
	// Checks is the aggregate check status of an open PR's head commit:
	// failure, pending, success, none, or ChecksUnknown. Empty for a PR that
	// is not open.
	Checks string `json:"checks,omitempty"`
}

// ListJobsInput is the input of list_jobs.
type ListJobsInput struct {
	Project string           `json:"project,omitempty"`
	Goal    string           `json:"goal,omitempty"` // GOAL-3 or UUID
	Status  domain.JobStatus `json:"status,omitempty"`
	Limit   int              `json:"limit,omitempty"` // default 50
}

// ListAssumptionsInput is the input of list_assumptions.
type ListAssumptionsInput struct {
	Project  string                  `json:"project"`             // name or repo, required
	Goal     string                  `json:"goal,omitempty"`      // GOAL-3 or UUID
	Task     string                  `json:"task,omitempty"`      // TASK-12 or UUID: what the task rests on
	Status   domain.AssumptionStatus `json:"status,omitempty"`    // unverified, verified, invalidated or accepted
	Impact   domain.Level            `json:"impact,omitempty"`    // low, medium or high
	VerifyBy domain.VerifyBy         `json:"verify_by,omitempty"` // test, ask-human or research
}

// AssumptionInfo is a registered assumption with its ref and where it came
// from.
type AssumptionInfo struct {
	domain.Assumption
	Ref string `json:"ref"` // A-42
	// Source is the job (JOB-41), else the council (its ID), else the spec
	// (SPEC-4) the assumption came from.
	Source string `json:"source,omitempty"`
}

// ResolveAssumptionInput is the input of resolve_assumption.
type ResolveAssumptionInput struct {
	Assumption string                  `json:"assumption"`         // A-42 or UUID
	Status     domain.AssumptionStatus `json:"status"`             // verified, invalidated or accepted
	Evidence   string                  `json:"evidence,omitempty"` // required for verified and invalidated
}

// ListDecisionsInput is the input of list_decisions.
type ListDecisionsInput struct {
	Project string `json:"project"`           // name or repo, required
	Goal    string `json:"goal,omitempty"`    // GOAL-3 or UUID
	Council string `json:"council,omitempty"` // the council's UUID
}

// DecisionInfo is a recorded decision with its ref.
type DecisionInfo struct {
	domain.Decision
	Ref string `json:"ref"` // DEC-9
	// Assumptions are the refs of the assumptions the decision rests on.
	Assumptions []string `json:"assumptions"`
}
