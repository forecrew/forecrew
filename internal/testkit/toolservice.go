package testkit

import (
	"context"
	"slices"
	"sync"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

var _ mcp.ToolService = (*ToolService)(nil)

// ToolCall is one recorded ToolService call.
type ToolCall struct {
	Tool  string // MCP tool name, e.g. mcp.ToolApprove
	Actor domain.Actor
	Input any // the input struct, or nil for tools without input
}

// ToolService is an mcp.ToolService that records calls and delegates each
// tool to its optional func field; a nil func returns the zero output. The
// gate tools (approve, request_changes, reject) without a func resolve the
// gate through Gates when it is set, applying mcp.GateDecision.
type ToolService struct {
	Gates *Gates

	OnboardProjectFunc   func(context.Context, domain.Actor, mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error)
	CreateGoalFunc       func(context.Context, domain.Actor, mcp.CreateGoalInput) (domain.Goal, error)
	JobStatusFunc        func(context.Context, domain.Actor, mcp.JobRefInput) (domain.Job, error)
	JobLogsFunc          func(context.Context, domain.Actor, mcp.JobLogsInput) (mcp.JobLogsOutput, error)
	ListWorkersFunc      func(context.Context, domain.Actor, any) ([]mcp.WorkerInfo, error)
	ListPendingGatesFunc func(context.Context, domain.Actor, mcp.ProjectFilterInput) ([]domain.Gate, error)
	ApproveFunc          func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	RequestChangesFunc   func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	RejectFunc           func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	DispatchJobFunc      func(context.Context, domain.Actor, mcp.DispatchJobInput) (domain.Job, error)
	ListProjectsFunc     func(context.Context, domain.Actor, any) ([]domain.Project, error)
	GetProjectFunc       func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error)
	ListGoalsFunc        func(context.Context, domain.Actor, mcp.ListGoalsInput) ([]domain.Goal, error)
	GetGoalFunc          func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error)
	ListJobsFunc         func(context.Context, domain.Actor, mcp.ListJobsInput) ([]domain.Job, error)

	mu    sync.Mutex
	calls []ToolCall
}

// Calls returns the recorded calls in order.
func (s *ToolService) Calls() []ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// invoke records the call and runs fn when set.
func invoke[I, O any](ctx context.Context, s *ToolService, tool string, by domain.Actor, in I, fn func(context.Context, domain.Actor, I) (O, error)) (O, error) {
	s.mu.Lock()
	s.calls = append(s.calls, ToolCall{Tool: tool, Actor: by, Input: in})
	s.mu.Unlock()
	if fn == nil {
		var zero O
		return zero, nil
	}
	return fn(ctx, by, in)
}

// OnboardProject implements mcp.ToolService.
func (s *ToolService) OnboardProject(ctx context.Context, by domain.Actor, in mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error) {
	return invoke(ctx, s, mcp.ToolOnboardProject, by, in, s.OnboardProjectFunc)
}

// CreateGoal implements mcp.ToolService.
func (s *ToolService) CreateGoal(ctx context.Context, by domain.Actor, in mcp.CreateGoalInput) (domain.Goal, error) {
	return invoke(ctx, s, mcp.ToolCreateGoal, by, in, s.CreateGoalFunc)
}

// JobStatus implements mcp.ToolService.
func (s *ToolService) JobStatus(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (domain.Job, error) {
	return invoke(ctx, s, mcp.ToolJobStatus, by, in, s.JobStatusFunc)
}

// JobLogs implements mcp.ToolService.
func (s *ToolService) JobLogs(ctx context.Context, by domain.Actor, in mcp.JobLogsInput) (mcp.JobLogsOutput, error) {
	return invoke(ctx, s, mcp.ToolJobLogs, by, in, s.JobLogsFunc)
}

// ListWorkers implements mcp.ToolService.
func (s *ToolService) ListWorkers(ctx context.Context, by domain.Actor) ([]mcp.WorkerInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListWorkers, by, nil, s.ListWorkersFunc)
}

// ListPendingGates implements mcp.ToolService.
func (s *ToolService) ListPendingGates(ctx context.Context, by domain.Actor, in mcp.ProjectFilterInput) ([]domain.Gate, error) {
	return invoke(ctx, s, mcp.ToolListPendingGates, by, in, s.ListPendingGatesFunc)
}

// decide returns fn, or when it is nil the default gate tool behavior.
func (s *ToolService) decide(tool string, fn func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)) func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error) {
	if fn != nil || s.Gates == nil {
		return fn
	}
	return func(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
		return s.Gates.Decide(ctx, by, tool, in)
	}
}

// Approve implements mcp.ToolService.
func (s *ToolService) Approve(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
	return invoke(ctx, s, mcp.ToolApprove, by, in, s.decide(mcp.ToolApprove, s.ApproveFunc))
}

// RequestChanges implements mcp.ToolService.
func (s *ToolService) RequestChanges(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
	return invoke(ctx, s, mcp.ToolRequestChanges, by, in, s.decide(mcp.ToolRequestChanges, s.RequestChangesFunc))
}

// Reject implements mcp.ToolService.
func (s *ToolService) Reject(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
	return invoke(ctx, s, mcp.ToolReject, by, in, s.decide(mcp.ToolReject, s.RejectFunc))
}

// DispatchJob implements mcp.ToolService.
func (s *ToolService) DispatchJob(ctx context.Context, by domain.Actor, in mcp.DispatchJobInput) (domain.Job, error) {
	return invoke(ctx, s, mcp.ToolDispatchJob, by, in, s.DispatchJobFunc)
}

// ListProjects implements mcp.ToolService.
func (s *ToolService) ListProjects(ctx context.Context, by domain.Actor) ([]domain.Project, error) {
	return invoke[any](ctx, s, mcp.ToolListProjects, by, nil, s.ListProjectsFunc)
}

// GetProject implements mcp.ToolService.
func (s *ToolService) GetProject(ctx context.Context, by domain.Actor, in mcp.ProjectInput) (domain.Project, error) {
	return invoke(ctx, s, mcp.ToolGetProject, by, in, s.GetProjectFunc)
}

// ListGoals implements mcp.ToolService.
func (s *ToolService) ListGoals(ctx context.Context, by domain.Actor, in mcp.ListGoalsInput) ([]domain.Goal, error) {
	return invoke(ctx, s, mcp.ToolListGoals, by, in, s.ListGoalsFunc)
}

// GetGoal implements mcp.ToolService.
func (s *ToolService) GetGoal(ctx context.Context, by domain.Actor, in mcp.GoalRefInput) (mcp.GoalDetail, error) {
	return invoke(ctx, s, mcp.ToolGetGoal, by, in, s.GetGoalFunc)
}

// ListJobs implements mcp.ToolService.
func (s *ToolService) ListJobs(ctx context.Context, by domain.Actor, in mcp.ListJobsInput) ([]domain.Job, error) {
	return invoke(ctx, s, mcp.ToolListJobs, by, in, s.ListJobsFunc)
}
