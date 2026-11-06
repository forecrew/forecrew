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

	OnboardProjectFunc         func(context.Context, domain.Actor, mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error)
	CreateGoalFunc             func(context.Context, domain.Actor, mcp.CreateGoalInput) (domain.Goal, error)
	ReportBugFunc              func(context.Context, domain.Actor, mcp.ReportBugInput) (domain.Goal, error)
	JobStatusFunc              func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error)
	JobLogsFunc                func(context.Context, domain.Actor, mcp.JobLogsInput) (mcp.JobLogsOutput, error)
	ListWorkersFunc            func(context.Context, domain.Actor, any) ([]mcp.WorkerInfo, error)
	ListMachinesFunc           func(context.Context, domain.Actor, mcp.ListMachinesInput) ([]mcp.MachineInfo, error)
	ListShapesFunc             func(context.Context, domain.Actor, any) ([]mcp.ShapeInfo, error)
	ListProvidersFunc          func(context.Context, domain.Actor, mcp.ListProvidersInput) ([]mcp.ProviderInfo, error)
	FleetOverviewFunc          func(context.Context, domain.Actor, any) (fleet.Overview, error)
	ListPendingGatesFunc       func(context.Context, domain.Actor, mcp.ProjectFilterInput) ([]mcp.GateInfo, error)
	ApproveFunc                func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	RequestChangesFunc         func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	RejectFunc                 func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)
	DispatchJobFunc            func(context.Context, domain.Actor, mcp.DispatchJobInput) (domain.Job, error)
	ListProjectsFunc           func(context.Context, domain.Actor, any) ([]domain.Project, error)
	GetProjectFunc             func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error)
	ListGoalsFunc              func(context.Context, domain.Actor, mcp.ListGoalsInput) ([]domain.Goal, error)
	GetGoalFunc                func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error)
	ListJobsFunc               func(context.Context, domain.Actor, mcp.ListJobsInput) ([]domain.Job, error)
	CancelJobFunc              func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error)
	ListTokensFunc             func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error)
	RevokeTokenFunc            func(context.Context, domain.Actor, mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error)
	CreateTokenFunc            func(context.Context, domain.Actor, mcp.CreateTokenInput) (mcp.CreateTokenOutput, error)
	CreateInvitationFunc       func(context.Context, domain.Actor, mcp.CreateInvitationInput) (mcp.CreateInvitationOutput, error)
	ListInvitationsFunc        func(context.Context, domain.Actor, any) ([]domain.Invitation, error)
	RevokeInvitationFunc       func(context.Context, domain.Actor, mcp.InvitationRefInput) (domain.Invitation, error)
	ListArtifactsFunc          func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error)
	ListTestRunsFunc           func(context.Context, domain.Actor, mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error)
	StartCouncilFunc           func(context.Context, domain.Actor, mcp.StartCouncilInput) (mcp.CouncilInfo, error)
	ListCouncilsFunc           func(context.Context, domain.Actor, mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error)
	GetCouncilFunc             func(context.Context, domain.Actor, mcp.CouncilRefInput) (mcp.CouncilDetail, error)
	AddConstraintFunc          func(context.Context, domain.Actor, mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error)
	VerificationReportFunc     func(context.Context, domain.Actor, mcp.VerificationReportInput) (mcp.VerificationInfo, error)
	ProjectQualityFunc         func(context.Context, domain.Actor, mcp.ProjectInput) (mcp.QualityInfo, error)
	OpenArtifactFunc           func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error)
	SearchKnowledgeFunc        func(context.Context, domain.Actor, mcp.SearchKnowledgeInput) ([]knowledge.Result, error)
	DeleteNoteFunc             func(context.Context, domain.Actor, mcp.DeleteNoteInput) (mcp.DeleteNoteOutput, error)
	ProposeKnowledgeFunc       func(context.Context, domain.Actor, mcp.ProposeKnowledgeInput) (mcp.ProposeKnowledgeOutput, error)
	GetTraceFunc               func(context.Context, domain.Actor, mcp.GetTraceInput) (mcp.Trace, error)
	ListKnowledgeFunc          func(context.Context, domain.Actor, mcp.ListKnowledgeInput) (mcp.KnowledgeListing, error)
	ListKnowledgeProposalsFunc func(context.Context, domain.Actor, mcp.ListKnowledgeProposalsInput) ([]mcp.KnowledgeProposalInfo, error)
	ListImagesFunc             func(context.Context, domain.Actor, mcp.ListImagesInput) ([]mcp.ImageInfo, error)
	CreateProviderFunc         func(context.Context, domain.Actor, mcp.CreateProviderInput) (mcp.CreateProviderOutput, error)
	RevokeProviderFunc         func(context.Context, domain.Actor, mcp.ProviderRefInput) (mcp.ProviderInfo, error)
	CreateJoinCodeFunc         func(context.Context, domain.Actor, mcp.CreateJoinCodeInput) (mcp.CreateJoinCodeOutput, error)
	ListJoinCodesFunc          func(context.Context, domain.Actor, mcp.ListJoinCodesInput) ([]mcp.JoinCodeInfo, error)
	ConfirmJoinFunc            func(context.Context, domain.Actor, mcp.ConfirmJoinInput) (mcp.ProviderInfo, error)

	ListTrackerConnectionsFunc  func(context.Context, domain.Actor, any) ([]domain.TrackerConnection, error)
	CreateTrackerConnectionFunc func(context.Context, domain.Actor, mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, error)
	DeleteTrackerConnectionFunc func(context.Context, domain.Actor, mcp.TrackerConnectionRefInput) (domain.TrackerConnection, error)
	ListTrackerRoutesFunc       func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) ([]mcp.TrackerRouteInfo, error)
	CreateTrackerRouteFunc      func(context.Context, domain.Actor, mcp.CreateTrackerRouteInput) (mcp.TrackerRouteInfo, error)
	DeleteTrackerRouteFunc      func(context.Context, domain.Actor, mcp.TrackerRouteRefInput) (domain.TrackerRoute, error)
	ListTrackerIdentitiesFunc   func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) ([]domain.TrackerIdentity, error)
	MapTrackerIdentityFunc      func(context.Context, domain.Actor, mcp.MapTrackerIdentityInput) (domain.TrackerIdentity, error)
	UnmapTrackerIdentityFunc    func(context.Context, domain.Actor, mcp.TrackerIdentityRefInput) (domain.TrackerIdentity, error)
	ListTicketsWaitingFunc      func(context.Context, domain.Actor, mcp.ListTicketsWaitingInput) ([]mcp.TicketInfo, error)
	AcceptTicketFunc            func(context.Context, domain.Actor, mcp.AcceptTicketInput) (mcp.TicketInfo, error)
	DismissTicketFunc           func(context.Context, domain.Actor, mcp.DismissTicketInput) (mcp.TicketInfo, error)

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

	ConfigSchemaFunc          func(context.Context, domain.Actor, mcp.ConfigKindInput) ([]mcp.ConfigSchema, error)
	ConfigGetFunc             func(context.Context, domain.Actor, mcp.ConfigGetInput) ([]mcp.ConfigResource, error)
	ConfigPlanFunc            func(context.Context, domain.Actor, mcp.ConfigPlanInput) (mcp.ConfigPlanView, error)
	ConfigApplyFunc           func(context.Context, domain.Actor, mcp.ConfigApplyInput) (mcp.ConfigApplied, error)
	ConfigRequestApprovalFunc func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigConfirmation, error)
	ConfigRevertFunc          func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error)
	ConfigExportFunc          func(context.Context, domain.Actor, any) (mcp.ConfigExport, error)
	ConfigImportFunc          func(context.Context, domain.Actor, mcp.ConfigImportInput) (mcp.ConfigPlanView, error)
	ConfigPlansFunc           func(context.Context, domain.Actor, mcp.ConfigPlansInput) ([]mcp.ConfigPlanView, error)
	ConfigGetPlanFunc         func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error)
	ConfigCancelFunc          func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error)
	ConfigPutSecretFunc       func(context.Context, domain.Actor, mcp.ConfigSecretInput) (mcp.ConfigPlanView, error)
	AssistantSendFunc         func(context.Context, domain.Actor, mcp.AssistantSendInput) (mcp.AssistantView, error)
	AssistantGetFunc          func(context.Context, domain.Actor, mcp.AssistantRefInput) (mcp.AssistantView, error)
	AssistantListFunc         func(context.Context, domain.Actor, mcp.AssistantListInput) ([]domain.AssistantConversation, error)
	ListMembersFunc           func(context.Context, domain.Actor, any) ([]mcp.MemberInfo, error)
	SetMemberSuspendedFunc    func(context.Context, domain.Actor, mcp.SetMemberInput) (mcp.MemberInfo, error)
	RemoveMemberFunc          func(context.Context, domain.Actor, mcp.MemberRefInput) (mcp.MemberInfo, error)
	UpdateOrganizationFunc    func(context.Context, domain.Actor, mcp.UpdateOrganizationInput) (domain.Account, error)
	ListAuditFunc             func(context.Context, domain.Actor, mcp.ListAuditInput) ([]mcp.AuditInfo, error)

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

// ReportBug implements mcp.ToolService.
func (s *ToolService) ReportBug(ctx context.Context, by domain.Actor, in mcp.ReportBugInput) (domain.Goal, error) {
	return invoke(ctx, s, mcp.ToolReportBug, by, in, s.ReportBugFunc)
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

// CreateProvider implements mcp.ToolService.
func (s *ToolService) CreateProvider(ctx context.Context, by domain.Actor, in mcp.CreateProviderInput) (mcp.CreateProviderOutput, error) {
	return invoke(ctx, s, mcp.ToolCreateProvider, by, in, s.CreateProviderFunc)
}

// RevokeProvider implements mcp.ToolService.
func (s *ToolService) RevokeProvider(ctx context.Context, by domain.Actor, in mcp.ProviderRefInput) (mcp.ProviderInfo, error) {
	return invoke(ctx, s, mcp.ToolRevokeProvider, by, in, s.RevokeProviderFunc)
}

// CreateJoinCode implements mcp.ToolService.
func (s *ToolService) CreateJoinCode(ctx context.Context, by domain.Actor, in mcp.CreateJoinCodeInput) (mcp.CreateJoinCodeOutput, error) {
	return invoke(ctx, s, mcp.ToolCreateJoinCode, by, in, s.CreateJoinCodeFunc)
}

// ListJoinCodes implements mcp.ToolService.
func (s *ToolService) ListJoinCodes(ctx context.Context, by domain.Actor, in mcp.ListJoinCodesInput) ([]mcp.JoinCodeInfo, error) {
	return invoke(ctx, s, mcp.ToolListJoinCodes, by, in, s.ListJoinCodesFunc)
}

// ConfirmJoin implements mcp.ToolService.
func (s *ToolService) ConfirmJoin(ctx context.Context, by domain.Actor, in mcp.ConfirmJoinInput) (mcp.ProviderInfo, error) {
	return invoke(ctx, s, mcp.ToolConfirmJoin, by, in, s.ConfirmJoinFunc)
}

// ListProviders implements mcp.ToolService.
func (s *ToolService) ListProviders(ctx context.Context, by domain.Actor, in mcp.ListProvidersInput) ([]mcp.ProviderInfo, error) {
	return invoke(ctx, s, mcp.ToolListProviders, by, in, s.ListProvidersFunc)
}

// FleetOverview implements mcp.ToolService.
func (s *ToolService) FleetOverview(ctx context.Context, by domain.Actor) (fleet.Overview, error) {
	return invoke[any](ctx, s, mcp.ToolFleetOverview, by, nil, s.FleetOverviewFunc)
}

// ListWorkers implements mcp.ToolService.
func (s *ToolService) ListWorkers(ctx context.Context, by domain.Actor) ([]mcp.WorkerInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListWorkers, by, nil, s.ListWorkersFunc)
}

// ListImages implements mcp.ToolService.
func (s *ToolService) ListImages(ctx context.Context, by domain.Actor, in mcp.ListImagesInput) ([]mcp.ImageInfo, error) {
	return invoke(ctx, s, mcp.ToolListImages, by, in, s.ListImagesFunc)
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

// GetTrace implements mcp.ToolService.
func (s *ToolService) GetTrace(ctx context.Context, by domain.Actor, in mcp.GetTraceInput) (mcp.Trace, error) {
	return invoke(ctx, s, mcp.ToolGetTrace, by, in, s.GetTraceFunc)
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

// ListKnowledgeProposals implements mcp.ToolService.
func (s *ToolService) ListKnowledgeProposals(ctx context.Context, by domain.Actor, in mcp.ListKnowledgeProposalsInput) ([]mcp.KnowledgeProposalInfo, error) {
	return invoke(ctx, s, mcp.ToolListKnowledgeProposals, by, in, s.ListKnowledgeProposalsFunc)
}

// ListKnowledge implements mcp.ToolService.
func (s *ToolService) ListKnowledge(ctx context.Context, by domain.Actor, in mcp.ListKnowledgeInput) (mcp.KnowledgeListing, error) {
	return invoke(ctx, s, mcp.ToolListKnowledge, by, in, s.ListKnowledgeFunc)
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

// ListTrackerConnections implements mcp.ToolService.
func (s *ToolService) ListTrackerConnections(ctx context.Context, by domain.Actor) ([]domain.TrackerConnection, error) {
	return invoke[any](ctx, s, mcp.ToolListTrackerConnections, by, nil, s.ListTrackerConnectionsFunc)
}

// DeleteTrackerConnection implements mcp.ToolService.
func (s *ToolService) DeleteTrackerConnection(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionRefInput) (domain.TrackerConnection, error) {
	return invoke(ctx, s, mcp.ToolDeleteTrackerConnection, by, in, s.DeleteTrackerConnectionFunc)
}

// ListTrackerRoutes implements mcp.ToolService.
func (s *ToolService) ListTrackerRoutes(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionFilterInput) ([]mcp.TrackerRouteInfo, error) {
	return invoke(ctx, s, mcp.ToolListTrackerRoutes, by, in, s.ListTrackerRoutesFunc)
}

// CreateTrackerConnection implements mcp.ToolService.
func (s *ToolService) CreateTrackerConnection(ctx context.Context, by domain.Actor, in mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, error) {
	return invoke(ctx, s, mcp.ToolCreateTrackerConnection, by, in, s.CreateTrackerConnectionFunc)
}

// CreateTrackerRoute implements mcp.ToolService.
func (s *ToolService) CreateTrackerRoute(ctx context.Context, by domain.Actor, in mcp.CreateTrackerRouteInput) (mcp.TrackerRouteInfo, error) {
	return invoke(ctx, s, mcp.ToolCreateTrackerRoute, by, in, s.CreateTrackerRouteFunc)
}

// DeleteTrackerRoute implements mcp.ToolService.
func (s *ToolService) DeleteTrackerRoute(ctx context.Context, by domain.Actor, in mcp.TrackerRouteRefInput) (domain.TrackerRoute, error) {
	return invoke(ctx, s, mcp.ToolDeleteTrackerRoute, by, in, s.DeleteTrackerRouteFunc)
}

// ListTrackerIdentities implements mcp.ToolService.
func (s *ToolService) ListTrackerIdentities(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionFilterInput) ([]domain.TrackerIdentity, error) {
	return invoke(ctx, s, mcp.ToolListTrackerIdentities, by, in, s.ListTrackerIdentitiesFunc)
}

// MapTrackerIdentity implements mcp.ToolService.
func (s *ToolService) MapTrackerIdentity(ctx context.Context, by domain.Actor, in mcp.MapTrackerIdentityInput) (domain.TrackerIdentity, error) {
	return invoke(ctx, s, mcp.ToolMapTrackerIdentity, by, in, s.MapTrackerIdentityFunc)
}

// UnmapTrackerIdentity implements mcp.ToolService.
func (s *ToolService) UnmapTrackerIdentity(ctx context.Context, by domain.Actor, in mcp.TrackerIdentityRefInput) (domain.TrackerIdentity, error) {
	return invoke(ctx, s, mcp.ToolUnmapTrackerIdentity, by, in, s.UnmapTrackerIdentityFunc)
}

// ListTicketsWaiting implements mcp.ToolService.
func (s *ToolService) ListTicketsWaiting(ctx context.Context, by domain.Actor, in mcp.ListTicketsWaitingInput) ([]mcp.TicketInfo, error) {
	return invoke(ctx, s, mcp.ToolListTicketsWaiting, by, in, s.ListTicketsWaitingFunc)
}

// AcceptTicket implements mcp.ToolService.
func (s *ToolService) AcceptTicket(ctx context.Context, by domain.Actor, in mcp.AcceptTicketInput) (mcp.TicketInfo, error) {
	return invoke(ctx, s, mcp.ToolAcceptTicket, by, in, s.AcceptTicketFunc)
}

// DismissTicket implements mcp.ToolService.
func (s *ToolService) DismissTicket(ctx context.Context, by domain.Actor, in mcp.DismissTicketInput) (mcp.TicketInfo, error) {
	return invoke(ctx, s, mcp.ToolDismissTicket, by, in, s.DismissTicketFunc)
}

// ConfigSchema implements mcp.ToolService.
func (s *ToolService) ConfigSchema(ctx context.Context, by domain.Actor, in mcp.ConfigKindInput) ([]mcp.ConfigSchema, error) {
	return invoke(ctx, s, mcp.ToolConfigSchema, by, in, s.ConfigSchemaFunc)
}

// ConfigGet implements mcp.ToolService.
func (s *ToolService) ConfigGet(ctx context.Context, by domain.Actor, in mcp.ConfigGetInput) ([]mcp.ConfigResource, error) {
	return invoke(ctx, s, mcp.ToolConfigGet, by, in, s.ConfigGetFunc)
}

// ConfigPlan implements mcp.ToolService.
func (s *ToolService) ConfigPlan(ctx context.Context, by domain.Actor, in mcp.ConfigPlanInput) (mcp.ConfigPlanView, error) {
	return invoke(ctx, s, mcp.ToolConfigPlan, by, in, s.ConfigPlanFunc)
}

// ConfigApply implements mcp.ToolService; it is recorded as "config_apply_confirmed" because config_apply over MCP is ConfigRequestApproval.
func (s *ToolService) ConfigApply(ctx context.Context, by domain.Actor, in mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
	return invoke(ctx, s, "config_apply_confirmed", by, in, s.ConfigApplyFunc)
}

// ConfigRequestApproval implements mcp.ToolService.
func (s *ToolService) ConfigRequestApproval(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigConfirmation, error) {
	return invoke(ctx, s, mcp.ToolConfigApply, by, in, s.ConfigRequestApprovalFunc)
}

// ConfigRevert implements mcp.ToolService.
func (s *ToolService) ConfigRevert(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	return invoke(ctx, s, mcp.ToolConfigRevert, by, in, s.ConfigRevertFunc)
}

// ConfigExport implements mcp.ToolService.
func (s *ToolService) ConfigExport(ctx context.Context, by domain.Actor) (mcp.ConfigExport, error) {
	return invoke[any](ctx, s, mcp.ToolConfigExport, by, nil, s.ConfigExportFunc)
}

// ConfigImport implements mcp.ToolService.
func (s *ToolService) ConfigImport(ctx context.Context, by domain.Actor, in mcp.ConfigImportInput) (mcp.ConfigPlanView, error) {
	return invoke(ctx, s, mcp.ToolConfigImport, by, in, s.ConfigImportFunc)
}

// ConfigPlans implements mcp.ToolService.
func (s *ToolService) ConfigPlans(ctx context.Context, by domain.Actor, in mcp.ConfigPlansInput) ([]mcp.ConfigPlanView, error) {
	return invoke(ctx, s, mcp.ToolConfigPlans, by, in, s.ConfigPlansFunc)
}

// ConfigGetPlan implements mcp.ToolService.
func (s *ToolService) ConfigGetPlan(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	return invoke(ctx, s, mcp.ToolConfigGetPlan, by, in, s.ConfigGetPlanFunc)
}

// ConfigCancel implements mcp.ToolService.
func (s *ToolService) ConfigCancel(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	return invoke(ctx, s, mcp.ToolConfigCancel, by, in, s.ConfigCancelFunc)
}

// ConfigPutSecret implements mcp.ToolService; the call is recorded without
// its value.
func (s *ToolService) ConfigPutSecret(ctx context.Context, by domain.Actor, in mcp.ConfigSecretInput) (mcp.ConfigPlanView, error) {
	fn := s.ConfigPutSecretFunc
	return invoke(ctx, s, "config_put_secret", by, mcp.ConfigSecretInput{Plan: in.Plan, Secret: in.Secret}, func(ctx context.Context, by domain.Actor, _ mcp.ConfigSecretInput) (mcp.ConfigPlanView, error) {
		if fn == nil {
			return mcp.ConfigPlanView{}, nil
		}
		return fn(ctx, by, in)
	})
}

// AssistantSend implements mcp.ToolService.
func (s *ToolService) AssistantSend(ctx context.Context, by domain.Actor, in mcp.AssistantSendInput) (mcp.AssistantView, error) {
	return invoke(ctx, s, mcp.ToolAssistantSend, by, in, s.AssistantSendFunc)
}

// AssistantGet implements mcp.ToolService.
func (s *ToolService) AssistantGet(ctx context.Context, by domain.Actor, in mcp.AssistantRefInput) (mcp.AssistantView, error) {
	return invoke(ctx, s, mcp.ToolAssistantGet, by, in, s.AssistantGetFunc)
}

// AssistantList implements mcp.ToolService.
func (s *ToolService) AssistantList(ctx context.Context, by domain.Actor, in mcp.AssistantListInput) ([]domain.AssistantConversation, error) {
	return invoke(ctx, s, mcp.ToolAssistantList, by, in, s.AssistantListFunc)
}

// ListMembers implements mcp.ToolService.
func (s *ToolService) ListMembers(ctx context.Context, by domain.Actor) ([]mcp.MemberInfo, error) {
	return invoke[any](ctx, s, mcp.ToolListMembers, by, nil, s.ListMembersFunc)
}

// SetMemberSuspended implements mcp.ToolService.
func (s *ToolService) SetMemberSuspended(ctx context.Context, by domain.Actor, in mcp.SetMemberInput) (mcp.MemberInfo, error) {
	return invoke(ctx, s, mcp.ToolSetMemberSuspended, by, in, s.SetMemberSuspendedFunc)
}

// RemoveMember implements mcp.ToolService.
func (s *ToolService) RemoveMember(ctx context.Context, by domain.Actor, in mcp.MemberRefInput) (mcp.MemberInfo, error) {
	return invoke(ctx, s, mcp.ToolRemoveMember, by, in, s.RemoveMemberFunc)
}

// UpdateOrganization implements mcp.ToolService.
func (s *ToolService) UpdateOrganization(ctx context.Context, by domain.Actor, in mcp.UpdateOrganizationInput) (domain.Account, error) {
	return invoke(ctx, s, mcp.ToolUpdateOrganization, by, in, s.UpdateOrganizationFunc)
}

// ListAudit implements mcp.ToolService.
func (s *ToolService) ListAudit(ctx context.Context, by domain.Actor, in mcp.ListAuditInput) ([]mcp.AuditInfo, error) {
	return invoke(ctx, s, mcp.ToolListAudit, by, in, s.ListAuditFunc)
}
