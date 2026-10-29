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
)

// Tools returns every MCP tool name.
func Tools() []string {
	return []string{
		ToolOnboardProject, ToolCreateGoal, ToolJobStatus, ToolJobLogs, ToolListWorkers, ToolListPendingGates,
		ToolApprove, ToolRequestChanges, ToolReject, ToolDispatchJob, ToolListProjects, ToolGetProject,
		ToolListGoals, ToolGetGoal, ToolListJobs,
	}
}

// ToolService implements one method per MCP tool.
type ToolService interface {
	// OnboardProject registers the repo as a project if needed and starts
	// onboarding (project onboarding, onboarding-analyst job).
	OnboardProject(ctx context.Context, by domain.Actor, in OnboardProjectInput) (OnboardProjectOutput, error)
	// CreateGoal creates a draft goal and starts the goal playbook.
	CreateGoal(ctx context.Context, by domain.Actor, in CreateGoalInput) (domain.Goal, error)
	JobStatus(ctx context.Context, by domain.Actor, in JobRefInput) (domain.Job, error)
	JobLogs(ctx context.Context, by domain.Actor, in JobLogsInput) (JobLogsOutput, error)
	ListWorkers(ctx context.Context, by domain.Actor) ([]WorkerInfo, error)
	ListPendingGates(ctx context.Context, by domain.Actor, in ProjectFilterInput) ([]domain.Gate, error)
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
}

// OnboardProjectInput is the input of onboard_project.
type OnboardProjectInput struct {
	Repo string `json:"repo"`           // "owner/name", required
	Name string `json:"name,omitempty"` // defaults to the repo name
}

// OnboardProjectOutput is the output of onboard_project.
type OnboardProjectOutput struct {
	Project domain.Project `json:"project"`
	Job     domain.Job     `json:"job"` // the onboarding-analyst job
}

// CreateGoalInput is the input of create_goal.
type CreateGoalInput struct {
	Project string `json:"project"` // name or repo, required
	Title   string `json:"title"`   // required
	Body    string `json:"body"`    // markdown
}

// JobRefInput is the input of job_status.
type JobRefInput struct {
	Job string `json:"job"` // JOB-41 or UUID
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

// WorkerInfo is a worker with its online state.
type WorkerInfo struct {
	domain.Worker
	Online bool `json:"online"`
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
	IncludeRepoFacts bool     `json:"include_repo_facts,omitempty"`
	Goal             string   `json:"goal,omitempty"` // GOAL-3 or UUID
	Task             string   `json:"task,omitempty"` // TASK-12 or UUID
	Feedback         string   `json:"feedback,omitempty"`
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

// GoalDetail is a goal with its latest spec, tasks, changes and pending gates.
type GoalDetail struct {
	Goal    domain.Goal     `json:"goal"`
	Spec    *domain.Spec    `json:"spec,omitempty"` // latest revision
	Tasks   []domain.Task   `json:"tasks"`
	Changes []domain.Change `json:"changes"`
	Gates   []domain.Gate   `json:"gates"` // pending
}

// ListJobsInput is the input of list_jobs.
type ListJobsInput struct {
	Project string           `json:"project,omitempty"`
	Status  domain.JobStatus `json:"status,omitempty"`
	Limit   int              `json:"limit,omitempty"` // default 50
}
