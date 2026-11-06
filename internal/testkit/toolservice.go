package testkit

import (
	"context"
	"io"
	"slices"
	"sync"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/knowledge"
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

	OnboardProjectFunc     func(context.Context, domain.Actor, mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error)
	CreateGoalFunc         func(context.Context, domain.Actor, mcp.CreateGoalInput) (domain.Goal, error)
	JobStatusFunc          func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error)
	JobLogsFunc            func(context.Context, domain.Actor, mcp.JobLogsInput) (mcp.JobLogsOutput, error)
	ListWorkersFunc        func(context.Context, domain.Actor, any) ([]mcp.WorkerInfo, error)
	ListMachinesFunc       func(context.Context, domain.Actor, mcp.ListMachinesInput) ([]mcp.MachineInfo, error)
	ListShapesFunc         func(context.Context, domain.Actor, any) ([]mcp.ShapeInfo, error)
	ListProvidersFunc      func(context.Context, domain.Actor, any) ([]mcp.ProviderInfo, error)
	FleetOverviewFunc      func(context.Context, domain.Actor, any) (fleet.Overview, error)
	ListPendingGatesFunc   func(context.Context, domain.Actor, mcp.ProjectFilterInput) ([]mcp.GateInfo, error)
	ApproveFunc            func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	RequestChangesFunc     func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	RejectFunc             func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	DispatchJobFunc        func(context.Context, domain.Actor, mcp.DispatchJobInput) (domain.Job, error)
	ListProjectsFunc       func(context.Context, domain.Actor, any) ([]domain.Project, error)
	GetProjectFunc         func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error)
	ListGoalsFunc          func(context.Context, domain.Actor, mcp.ListGoalsInput) ([]domain.Goal, error)
	GetGoalFunc            func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error)
	ListJobsFunc           func(context.Context, domain.Actor, mcp.ListJobsInput) ([]domain.Job, error)
	CancelJobFunc          func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error)
	ListTokensFunc         func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error)
	RevokeTokenFunc        func(context.Context, domain.Actor, mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error)
	CreateTokenFunc        func(context.Context, domain.Actor, mcp.CreateTokenInput) (mcp.CreateTokenOutput, error)
	CreateInvitationFunc   func(context.Context, domain.Actor, mcp.CreateInvitationInput) (mcp.CreateInvitationOutput, error)
	ListInvitationsFunc    func(context.Context, domain.Actor, any) ([]domain.Invitation, error)
	RevokeInvitationFunc   func(context.Context, domain.Actor, mcp.InvitationRefInput) (domain.Invitation, error)
	ListArtifactsFunc      func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error)
	ListTestRunsFunc       func(context.Context, domain.Actor, mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error)
	StartCouncilFunc       func(context.Context, domain.Actor, mcp.StartCouncilInput) (mcp.CouncilInfo, error)
	ListCouncilsFunc       func(context.Context, domain.Actor, mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error)
	GetCouncilFunc         func(context.Context, domain.Actor, mcp.CouncilRefInput) (mcp.CouncilDetail, error)
	AddConstraintFunc      func(context.Context, domain.Actor, mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error)
	VerificationReportFunc func(context.Context, domain.Actor, mcp.VerificationReportInput) (mcp.VerificationInfo, error)
	ProjectQualityFunc     func(context.Context, domain.Actor, mcp.ProjectInput) (mcp.QualityInfo, error)
	OpenArtifactFunc       func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error)
	SearchKnowledgeFunc    func(context.Context, domain.Actor, mcp.SearchKnowledgeInput) ([]knowledge.Result, error)
	DeleteNoteFunc         func(context.Context, domain.Actor, mcp.DeleteNoteInput) (mcp.DeleteNoteOutput, error)
	ProposeKnowledgeFunc   func(context.Context, domain.Actor, mcp.ProposeKnowledgeInput) (mcp.ProposeKnowledgeOutput, error)

	ListAssumptionsFunc   func(context.Context, domain.Actor, mcp.ListAssumptionsInput) ([]mcp.AssumptionInfo, error)
	ResolveAssumptionFunc func(context.Context, domain.Actor, mcp.ResolveAssumptionInput) (mcp.AssumptionInfo, error)
	ListDecisionsFunc     func(context.Context, domain.Actor, mcp.ListDecisionsInput) ([]mcp.DecisionInfo, error)

	ListRolesFunc                 func(context.Context, domain.Actor, any) ([]domain.Role, error)
	ListTeamsFunc                 func(context.Context, domain.Actor, any) ([]domain.Team, error)
	CreateTeamFunc                func(context.Context, domain.Actor, mcp.CreateTeamInput) (domain.Team, error)
	UpdateTeamFunc                func(context.Context, domain.Actor, mcp.UpdateTeamInput) (domain.Team, error)
	DeleteTeamFunc                func(context.Context, domain.Actor, mcp.TeamRefInput) (domain.Team, error)
	ListTeamMembersFunc           func(context.Context, domain.Actor, mcp.TeamRefInput) ([]domain.TeamMember, error)
	AddTeamMemberFunc             func(context.Context, domain.Actor, mcp.TeamMemberInput) (domain.TeamMember, error)
	RemoveTeamMemberFunc          func(context.Context, domain.Actor, mcp.TeamMemberInput) (domain.TeamMember, error)
	ListRoleBindingsFunc          func(context.Context, domain.Actor, mcp.ListRoleBindingsInput) ([]mcp.RoleBindingInfo, error)
	CreateRoleBindingFunc         func(context.Context, domain.Actor, mcp.CreateRoleBindingInput) (mcp.RoleBindingInfo, error)
	DeleteRoleBindingFunc         func(context.Context, domain.Actor, mcp.RoleBindingRefInput) (mcp.RoleBindingInfo, error)
	ListServiceAccountsFunc       func(context.Context, domain.Actor, any) ([]domain.ServiceAccount, error)
	CreateServiceAccountFunc      func(context.Context, domain.Actor, mcp.CreateServiceAccountInput) (domain.ServiceAccount, error)
	UpdateServiceAccountFunc      func(context.Context, domain.Actor, mcp.UpdateServiceAccountInput) (domain.ServiceAccount, error)
	SetServiceAccountDisabledFunc func(context.Context, domain.Actor, mcp.SetServiceAccountInput) (domain.ServiceAccount, error)

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
func (s *ToolService) JobStatus(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
	return invoke(ctx, s, mcp.ToolJobStatus, by, in, s.JobStatusFunc)
}

// JobLogs implements mcp.ToolService.
func (s *ToolService) JobLogs(ctx context.Context, by domain.Actor, in mcp.JobLogsInput) (mcp.JobLogsOutput, error) {
	return invoke(ctx, s, mcp.ToolJobLogs, by, in, s.JobLogsFunc)
}

// ListMachines implements mcp.ToolService.
func (s *ToolService) ListMachines(ctx context.Context, by domain.Actor, in mcp.ListMachinesInput) ([]mcp.MachineInfo, error) {
	return invoke(ctx, s, mcp.ToolListMachines, by, in, s.ListMachinesFunc)
}

// ListShapes implements mcp.ToolService.
func (s *ToolService) ListShapes(ctx context.Context, by domain.Actor) ([]mcp.ShapeInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListShapes, by, nil, s.ListShapesFunc)
}

// ListProviders implements mcp.ToolService.
func (s *ToolService) ListProviders(ctx context.Context, by domain.Actor) ([]mcp.ProviderInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListProviders, by, nil, s.ListProvidersFunc)
}

// FleetOverview implements mcp.ToolService.
func (s *ToolService) FleetOverview(ctx context.Context, by domain.Actor) (fleet.Overview, error) {
	return invoke[any](ctx, s, mcp.ToolFleetOverview, by, nil, s.FleetOverviewFunc)
}

// ListWorkers implements mcp.ToolService.
func (s *ToolService) ListWorkers(ctx context.Context, by domain.Actor) ([]mcp.WorkerInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListWorkers, by, nil, s.ListWorkersFunc)
}

// ListPendingGates implements mcp.ToolService.
func (s *ToolService) ListPendingGates(ctx context.Context, by domain.Actor, in mcp.ProjectFilterInput) ([]mcp.GateInfo, error) {
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

// CancelJob implements mcp.ToolService.
func (s *ToolService) CancelJob(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
	return invoke(ctx, s, mcp.ToolCancelJob, by, in, s.CancelJobFunc)
}

// ListTokens implements mcp.ToolService.
func (s *ToolService) ListTokens(ctx context.Context, by domain.Actor) ([]mcp.TokenInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListTokens, by, nil, s.ListTokensFunc)
}

// RevokeToken implements mcp.ToolService.
func (s *ToolService) RevokeToken(ctx context.Context, by domain.Actor, in mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
	return invoke(ctx, s, mcp.ToolRevokeToken, by, in, s.RevokeTokenFunc)
}

// CreateToken implements mcp.ToolService.
func (s *ToolService) CreateToken(ctx context.Context, by domain.Actor, in mcp.CreateTokenInput) (mcp.CreateTokenOutput, error) {
	return invoke(ctx, s, mcp.ToolCreateToken, by, in, s.CreateTokenFunc)
}

// CreateInvitation implements mcp.ToolService.
func (s *ToolService) CreateInvitation(ctx context.Context, by domain.Actor, in mcp.CreateInvitationInput) (mcp.CreateInvitationOutput, error) {
	return invoke(ctx, s, mcp.ToolCreateInvitation, by, in, s.CreateInvitationFunc)
}

// ListInvitations implements mcp.ToolService.
func (s *ToolService) ListInvitations(ctx context.Context, by domain.Actor) ([]domain.Invitation, error) {
	return invoke[any](ctx, s, mcp.ToolListInvitations, by, nil, s.ListInvitationsFunc)
}

// RevokeInvitation implements mcp.ToolService.
func (s *ToolService) RevokeInvitation(ctx context.Context, by domain.Actor, in mcp.InvitationRefInput) (domain.Invitation, error) {
	return invoke(ctx, s, mcp.ToolRevokeInvitation, by, in, s.RevokeInvitationFunc)
}

// ListArtifacts implements mcp.ToolService.
func (s *ToolService) ListArtifacts(ctx context.Context, by domain.Actor, in mcp.JobRefInput) ([]domain.Artifact, error) {
	return invoke(ctx, s, mcp.ToolListArtifacts, by, in, s.ListArtifactsFunc)
}

// StartCouncil implements mcp.ToolService.
func (s *ToolService) StartCouncil(ctx context.Context, by domain.Actor, in mcp.StartCouncilInput) (mcp.CouncilInfo, error) {
	return invoke(ctx, s, mcp.ToolStartCouncil, by, in, s.StartCouncilFunc)
}

// ListCouncils implements mcp.ToolService.
func (s *ToolService) ListCouncils(ctx context.Context, by domain.Actor, in mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error) {
	return invoke(ctx, s, mcp.ToolListCouncils, by, in, s.ListCouncilsFunc)
}

// GetCouncil implements mcp.ToolService.
func (s *ToolService) GetCouncil(ctx context.Context, by domain.Actor, in mcp.CouncilRefInput) (mcp.CouncilDetail, error) {
	return invoke(ctx, s, mcp.ToolGetCouncil, by, in, s.GetCouncilFunc)
}

// AddCouncilConstraint implements mcp.ToolService.
func (s *ToolService) AddCouncilConstraint(ctx context.Context, by domain.Actor, in mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error) {
	return invoke(ctx, s, mcp.ToolAddConstraint, by, in, s.AddConstraintFunc)
}

// VerificationReport implements mcp.ToolService.
func (s *ToolService) VerificationReport(ctx context.Context, by domain.Actor, in mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
	return invoke(ctx, s, mcp.ToolVerificationReport, by, in, s.VerificationReportFunc)
}

// ProjectQuality implements mcp.ToolService.
func (s *ToolService) ProjectQuality(ctx context.Context, by domain.Actor, in mcp.ProjectInput) (mcp.QualityInfo, error) {
	return invoke(ctx, s, mcp.ToolProjectQuality, by, in, s.ProjectQualityFunc)
}

// ListTestRuns implements mcp.ToolService.
func (s *ToolService) ListTestRuns(ctx context.Context, by domain.Actor, in mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error) {
	return invoke(ctx, s, mcp.ToolListTestRuns, by, in, s.ListTestRunsFunc)
}

// ListAssumptions implements mcp.ToolService.
func (s *ToolService) ListAssumptions(ctx context.Context, by domain.Actor, in mcp.ListAssumptionsInput) ([]mcp.AssumptionInfo, error) {
	return invoke(ctx, s, mcp.ToolListAssumptions, by, in, s.ListAssumptionsFunc)
}

// ResolveAssumption implements mcp.ToolService.
func (s *ToolService) ResolveAssumption(ctx context.Context, by domain.Actor, in mcp.ResolveAssumptionInput) (mcp.AssumptionInfo, error) {
	return invoke(ctx, s, mcp.ToolResolveAssumption, by, in, s.ResolveAssumptionFunc)
}

// ListDecisions implements mcp.ToolService.
func (s *ToolService) ListDecisions(ctx context.Context, by domain.Actor, in mcp.ListDecisionsInput) ([]mcp.DecisionInfo, error) {
	return invoke(ctx, s, mcp.ToolListDecisions, by, in, s.ListDecisionsFunc)
}

// SearchKnowledge implements mcp.ToolService.
func (s *ToolService) SearchKnowledge(ctx context.Context, by domain.Actor, in mcp.SearchKnowledgeInput) ([]knowledge.Result, error) {
	return invoke(ctx, s, mcp.ToolSearchKnowledge, by, in, s.SearchKnowledgeFunc)
}

// ProposeKnowledge implements mcp.ToolService.
func (s *ToolService) ProposeKnowledge(ctx context.Context, by domain.Actor, in mcp.ProposeKnowledgeInput) (mcp.ProposeKnowledgeOutput, error) {
	return invoke(ctx, s, mcp.ToolProposeKnowledge, by, in, s.ProposeKnowledgeFunc)
}

// DeleteNote implements mcp.ToolService.
func (s *ToolService) DeleteNote(ctx context.Context, by domain.Actor, in mcp.DeleteNoteInput) (mcp.DeleteNoteOutput, error) {
	return invoke(ctx, s, mcp.ToolDeleteNote, by, in, s.DeleteNoteFunc)
}

// ListRoles implements mcp.ToolService.
func (s *ToolService) ListRoles(ctx context.Context, by domain.Actor) ([]domain.Role, error) {
	return invoke[any](ctx, s, mcp.ToolListRoles, by, nil, s.ListRolesFunc)
}

// ListTeams implements mcp.ToolService.
func (s *ToolService) ListTeams(ctx context.Context, by domain.Actor) ([]domain.Team, error) {
	return invoke[any](ctx, s, mcp.ToolListTeams, by, nil, s.ListTeamsFunc)
}

// CreateTeam implements mcp.ToolService.
func (s *ToolService) CreateTeam(ctx context.Context, by domain.Actor, in mcp.CreateTeamInput) (domain.Team, error) {
	return invoke(ctx, s, mcp.ToolCreateTeam, by, in, s.CreateTeamFunc)
}

// UpdateTeam implements mcp.ToolService.
func (s *ToolService) UpdateTeam(ctx context.Context, by domain.Actor, in mcp.UpdateTeamInput) (domain.Team, error) {
	return invoke(ctx, s, mcp.ToolUpdateTeam, by, in, s.UpdateTeamFunc)
}

// DeleteTeam implements mcp.ToolService.
func (s *ToolService) DeleteTeam(ctx context.Context, by domain.Actor, in mcp.TeamRefInput) (domain.Team, error) {
	return invoke(ctx, s, mcp.ToolDeleteTeam, by, in, s.DeleteTeamFunc)
}

// ListTeamMembers implements mcp.ToolService.
func (s *ToolService) ListTeamMembers(ctx context.Context, by domain.Actor, in mcp.TeamRefInput) ([]domain.TeamMember, error) {
	return invoke(ctx, s, mcp.ToolListTeamMembers, by, in, s.ListTeamMembersFunc)
}

// AddTeamMember implements mcp.ToolService.
func (s *ToolService) AddTeamMember(ctx context.Context, by domain.Actor, in mcp.TeamMemberInput) (domain.TeamMember, error) {
	return invoke(ctx, s, mcp.ToolAddTeamMember, by, in, s.AddTeamMemberFunc)
}

// RemoveTeamMember implements mcp.ToolService.
func (s *ToolService) RemoveTeamMember(ctx context.Context, by domain.Actor, in mcp.TeamMemberInput) (domain.TeamMember, error) {
	return invoke(ctx, s, mcp.ToolRemoveTeamMember, by, in, s.RemoveTeamMemberFunc)
}

// ListRoleBindings implements mcp.ToolService.
func (s *ToolService) ListRoleBindings(ctx context.Context, by domain.Actor, in mcp.ListRoleBindingsInput) ([]mcp.RoleBindingInfo, error) {
	return invoke(ctx, s, mcp.ToolListRoleBindings, by, in, s.ListRoleBindingsFunc)
}

// CreateRoleBinding implements mcp.ToolService.
func (s *ToolService) CreateRoleBinding(ctx context.Context, by domain.Actor, in mcp.CreateRoleBindingInput) (mcp.RoleBindingInfo, error) {
	return invoke(ctx, s, mcp.ToolCreateRoleBinding, by, in, s.CreateRoleBindingFunc)
}

// DeleteRoleBinding implements mcp.ToolService.
func (s *ToolService) DeleteRoleBinding(ctx context.Context, by domain.Actor, in mcp.RoleBindingRefInput) (mcp.RoleBindingInfo, error) {
	return invoke(ctx, s, mcp.ToolDeleteRoleBinding, by, in, s.DeleteRoleBindingFunc)
}

// ListServiceAccounts implements mcp.ToolService.
func (s *ToolService) ListServiceAccounts(ctx context.Context, by domain.Actor) ([]domain.ServiceAccount, error) {
	return invoke[any](ctx, s, mcp.ToolListServiceAccounts, by, nil, s.ListServiceAccountsFunc)
}

// CreateServiceAccount implements mcp.ToolService.
func (s *ToolService) CreateServiceAccount(ctx context.Context, by domain.Actor, in mcp.CreateServiceAccountInput) (domain.ServiceAccount, error) {
	return invoke(ctx, s, mcp.ToolCreateServiceAccount, by, in, s.CreateServiceAccountFunc)
}

// UpdateServiceAccount implements mcp.ToolService.
func (s *ToolService) UpdateServiceAccount(ctx context.Context, by domain.Actor, in mcp.UpdateServiceAccountInput) (domain.ServiceAccount, error) {
	return invoke(ctx, s, mcp.ToolUpdateServiceAccount, by, in, s.UpdateServiceAccountFunc)
}

// SetServiceAccountDisabled implements mcp.ToolService.
func (s *ToolService) SetServiceAccountDisabled(ctx context.Context, by domain.Actor, in mcp.SetServiceAccountInput) (domain.ServiceAccount, error) {
	return invoke(ctx, s, mcp.ToolSetServiceAccount, by, in, s.SetServiceAccountDisabledFunc)
}

// OpenArtifact implements api.ArtifactOpener. It records no call; without
// OpenArtifactFunc it answers mcp.ErrNotFound.
func (s *ToolService) OpenArtifact(ctx context.Context, by domain.Actor, id string) (domain.Artifact, io.ReadCloser, error) {
	if s.OpenArtifactFunc == nil {
		return domain.Artifact{}, nil, mcp.ErrNotFound
	}
	return s.OpenArtifactFunc(ctx, by, id)
}
