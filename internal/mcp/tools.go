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
)

// Sentinel errors. Implementations also return domain errors
// (ErrInvalidRef, ErrInvalidTransition, ErrInvalidDecision).
var (
	// ErrNotFound means a named project, goal, job or gate does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidInput means a required field is missing or malformed.
	ErrInvalidInput = errors.New("invalid input")
)

// ErrorCode classifies the errors callers act on: "not_found" for
// ErrNotFound, "invalid_input" for ErrInvalidInput, domain.ErrInvalidRef and
// domain.ErrInvalidDecision, and "conflict" for domain.ErrInvalidTransition.
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
	}
	return "", false
}

// Tool names as exposed over MCP.
const (
	ToolOnboardProject   = "onboard_project"
	ToolCreateGoal       = "create_goal"
	ToolJobStatus        = "job_status"
	ToolJobLogs          = "job_logs"
	ToolListWorkers      = "list_workers"
	ToolListPendingGates = "list_pending_gates"
	ToolApprove          = "approve"
	ToolRequestChanges   = "request_changes"
	ToolReject           = "reject"
	ToolDispatchJob      = "dispatch_job"
	ToolListProjects     = "list_projects"
	ToolGetProject       = "get_project"
	ToolListGoals        = "list_goals"
	ToolGetGoal          = "get_goal"
	ToolListJobs         = "list_jobs"
	ToolCancelJob        = "cancel_job"
	ToolListTokens       = "list_tokens"
	ToolRevokeToken      = "revoke_token"
)

// Tools returns every MCP tool name.
func Tools() []string {
	return []string{
		ToolOnboardProject, ToolCreateGoal, ToolJobStatus, ToolJobLogs, ToolListWorkers, ToolListPendingGates,
		ToolApprove, ToolRequestChanges, ToolReject, ToolDispatchJob, ToolListProjects, ToolGetProject,
		ToolListGoals, ToolGetGoal, ToolListJobs, ToolCancelJob, ToolListTokens, ToolRevokeToken,
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
