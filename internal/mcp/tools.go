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

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
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
	// ErrConflict means the change clashes with what is stored: a name that
	// is taken, a binding that exists, the last Owner.
	ErrConflict = errors.New("conflict")
)

// ErrorCode classifies the errors callers act on: "forbidden" for
// authz.ErrDenied, "not_found" for ErrNotFound, "invalid_input" for
// ErrInvalidInput, domain.ErrInvalidRef and domain.ErrInvalidDecision,
// "conflict" for ErrConflict and domain.ErrInvalidTransition and
// "unavailable" for ErrUnavailable.
// ok is false for any other error, which is an internal one. The MCP server
// and /api/v1 use it to decide what a client may see.
func ErrorCode(err error) (code string, ok bool) {
	switch {
	case errors.Is(err, authz.ErrDenied):
		return "forbidden", true
	case errors.Is(err, ErrNotFound):
		return "not_found", true
	case errors.Is(err, ErrInvalidInput), errors.Is(err, domain.ErrInvalidRef), errors.Is(err, domain.ErrInvalidDecision):
		return "invalid_input", true
	case errors.Is(err, ErrConflict), errors.Is(err, domain.ErrInvalidTransition):
		return "conflict", true
	case errors.Is(err, ErrUnavailable):
		return "unavailable", true
	}
	return "", false
}

// Tool names as exposed over MCP.
const (
	ToolOnboardProject     = "onboard_project"
	ToolCreateGoal         = "create_goal"
	ToolJobStatus          = "job_status"
	ToolJobLogs            = "job_logs"
	ToolListWorkers        = "list_workers"
	ToolListPendingGates   = "list_pending_gates"
	ToolApprove            = "approve"
	ToolRequestChanges     = "request_changes"
	ToolReject             = "reject"
	ToolDispatchJob        = "dispatch_job"
	ToolListProjects       = "list_projects"
	ToolGetProject         = "get_project"
	ToolListGoals          = "list_goals"
	ToolGetGoal            = "get_goal"
	ToolListJobs           = "list_jobs"
	ToolCancelJob          = "cancel_job"
	ToolListTokens         = "list_tokens"
	ToolRevokeToken        = "revoke_token"
	ToolCreateToken        = "create_token"
	ToolCreateInvitation   = "create_invitation"
	ToolListInvitations    = "list_invitations"
	ToolRevokeInvitation   = "revoke_invitation"
	ToolListArtifacts      = "list_artifacts"
	ToolListTestRuns       = "list_test_runs"
	ToolListAssumptions    = "list_assumptions"
	ToolResolveAssumption  = "resolve_assumption"
	ToolListDecisions      = "list_decisions"
	ToolSearchKnowledge    = "search_knowledge"
	ToolDeleteNote         = "delete_note"
	ToolProposeKnowledge   = "propose_knowledge"
	ToolStartCouncil       = "start_council"
	ToolListCouncils       = "list_councils"
	ToolGetCouncil         = "get_council"
	ToolAddConstraint      = "add_council_constraint"
	ToolVerificationReport = "verification_report"

	ToolProjectQuality       = "project_quality"
	ToolListRoles            = "list_roles"
	ToolListTeams            = "list_teams"
	ToolCreateTeam           = "create_team"
	ToolUpdateTeam           = "update_team"
	ToolDeleteTeam           = "delete_team"
	ToolListTeamMembers      = "list_team_members"
	ToolAddTeamMember        = "add_team_member"
	ToolRemoveTeamMember     = "remove_team_member"
	ToolListRoleBindings     = "list_role_bindings"
	ToolCreateRoleBinding    = "create_role_binding"
	ToolDeleteRoleBinding    = "delete_role_binding"
	ToolListServiceAccounts  = "list_service_accounts"
	ToolCreateServiceAccount = "create_service_account"
	ToolUpdateServiceAccount = "update_service_account"
	ToolSetServiceAccount    = "set_service_account_disabled"
	ToolListMachines         = "list_machines"
	ToolListShapes           = "list_shapes"
	ToolListProviders        = "list_providers"
	ToolFleetOverview        = "fleet_overview"
)

// Tools returns every MCP tool name.
func Tools() []string {
	return []string{
		ToolOnboardProject, ToolCreateGoal, ToolJobStatus, ToolJobLogs, ToolListWorkers, ToolListPendingGates,
		ToolApprove, ToolRequestChanges, ToolReject, ToolDispatchJob, ToolListProjects, ToolGetProject,
		ToolListGoals, ToolGetGoal, ToolListJobs, ToolCancelJob, ToolListTokens, ToolRevokeToken, ToolListArtifacts, ToolListTestRuns,
		ToolListAssumptions, ToolResolveAssumption, ToolListDecisions,
		ToolSearchKnowledge, ToolDeleteNote, ToolProposeKnowledge,
		ToolStartCouncil, ToolListCouncils, ToolGetCouncil, ToolAddConstraint,
		ToolVerificationReport, ToolProjectQuality, ToolCreateToken, ToolCreateInvitation, ToolListInvitations, ToolRevokeInvitation,
		ToolListRoles, ToolListTeams, ToolCreateTeam, ToolUpdateTeam, ToolDeleteTeam,
		ToolListTeamMembers, ToolAddTeamMember, ToolRemoveTeamMember,
		ToolListRoleBindings, ToolCreateRoleBinding, ToolDeleteRoleBinding,
		ToolListServiceAccounts, ToolCreateServiceAccount, ToolUpdateServiceAccount, ToolSetServiceAccount,
		ToolListMachines, ToolListShapes, ToolListProviders, ToolFleetOverview,
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
	// ListMachines lists the organization's machines: those that exist, or
	// those of one state.
	ListMachines(ctx context.Context, by domain.Actor, in ListMachinesInput) ([]MachineInfo, error)
	// ListShapes lists the shapes offered to the organization.
	ListShapes(ctx context.Context, by domain.Actor) ([]ShapeInfo, error)
	// ListProviders lists the organization's capacity providers and the
	// instance-wide ones.
	ListProviders(ctx context.Context, by domain.Actor) ([]ProviderInfo, error)
	// FleetOverview says whether jobs have somewhere to run, and where.
	FleetOverview(ctx context.Context, by domain.Actor) (fleet.Overview, error)
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
	// CreateToken creates a personal token of the caller, or with
	// token.manage a service account's; the secret is in the answer only.
	CreateToken(ctx context.Context, by domain.Actor, in CreateTokenInput) (CreateTokenOutput, error)
	// CreateInvitation invites someone by email or GitHub login, with roles;
	// the link is in the answer only.
	CreateInvitation(ctx context.Context, by domain.Actor, in CreateInvitationInput) (CreateInvitationOutput, error)
	// ListInvitations lists the organization's invitations, newest first.
	ListInvitations(ctx context.Context, by domain.Actor) ([]domain.Invitation, error)
	// RevokeInvitation revokes an invitation by ID.
	RevokeInvitation(ctx context.Context, by domain.Actor, in InvitationRefInput) (domain.Invitation, error)
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
	// ProposeKnowledge sends a human's change to a curated section straight
	// to a PR. For a protected section the caller is recorded as approver.
	ProposeKnowledge(ctx context.Context, by domain.Actor, in ProposeKnowledgeInput) (ProposeKnowledgeOutput, error)
	// StartCouncil starts a council of its own, tied to no goal.
	StartCouncil(ctx context.Context, by domain.Actor, in StartCouncilInput) (CouncilInfo, error)
	// ListCouncils lists councils newest first: of a project, or of every
	// project the caller may read.
	ListCouncils(ctx context.Context, by domain.Actor, in ListCouncilsInput) ([]CouncilInfo, error)
	// GetCouncil shows a council with its contributions, gate and decision.
	GetCouncil(ctx context.Context, by domain.Actor, in CouncilRefInput) (CouncilDetail, error)
	// AddCouncilConstraint adds a constraint to a running council; the next
	// round's prompts include it. A council that ended is a conflict.
	AddCouncilConstraint(ctx context.Context, by domain.Actor, in AddCouncilConstraintInput) (CouncilInfo, error)
	// VerificationReport returns the verification report of a pull request:
	// the one of its head commit, else the latest. A change with no report is
	// ErrNotFound.
	VerificationReport(ctx context.Context, by domain.Actor, in VerificationReportInput) (VerificationInfo, error)
	// ProjectQuality returns the test health of a project: the baseline on
	// the default branch and each open pull request.
	ProjectQuality(ctx context.Context, by domain.Actor, in ProjectInput) (QualityInfo, error)
	AccessService
}

// AccessService manages who may do what: teams, role bindings and service
// accounts (docs/contracts/permissions.md). Every change obeys No escalation.
type AccessService interface {
	// ListRoles lists the built-in roles and the organization's custom ones.
	ListRoles(ctx context.Context, by domain.Actor) ([]domain.Role, error)
	ListTeams(ctx context.Context, by domain.Actor) ([]domain.Team, error)
	CreateTeam(ctx context.Context, by domain.Actor, in CreateTeamInput) (domain.Team, error)
	UpdateTeam(ctx context.Context, by domain.Actor, in UpdateTeamInput) (domain.Team, error)
	// DeleteTeam deletes a team, which removes the roles bound to it.
	DeleteTeam(ctx context.Context, by domain.Actor, in TeamRefInput) (domain.Team, error)
	ListTeamMembers(ctx context.Context, by domain.Actor, in TeamRefInput) ([]domain.TeamMember, error)
	AddTeamMember(ctx context.Context, by domain.Actor, in TeamMemberInput) (domain.TeamMember, error)
	RemoveTeamMember(ctx context.Context, by domain.Actor, in TeamMemberInput) (domain.TeamMember, error)
	ListRoleBindings(ctx context.Context, by domain.Actor, in ListRoleBindingsInput) ([]RoleBindingInfo, error)
	CreateRoleBinding(ctx context.Context, by domain.Actor, in CreateRoleBindingInput) (RoleBindingInfo, error)
	// DeleteRoleBinding removes a binding; the last Owner's is a conflict.
	DeleteRoleBinding(ctx context.Context, by domain.Actor, in RoleBindingRefInput) (RoleBindingInfo, error)
	ListServiceAccounts(ctx context.Context, by domain.Actor) ([]domain.ServiceAccount, error)
	CreateServiceAccount(ctx context.Context, by domain.Actor, in CreateServiceAccountInput) (domain.ServiceAccount, error)
	UpdateServiceAccount(ctx context.Context, by domain.Actor, in UpdateServiceAccountInput) (domain.ServiceAccount, error)
	SetServiceAccountDisabled(ctx context.Context, by domain.Actor, in SetServiceAccountInput) (domain.ServiceAccount, error)
}

// CreateTeamInput is the input of create_team.
type CreateTeamInput struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"` // derived from the name when empty
}

// UpdateTeamInput is the input of update_team.
type UpdateTeamInput struct {
	Team string `json:"team"` // slug or UUID
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// TeamRefInput names a team.
type TeamRefInput struct {
	Team string `json:"team"` // slug or UUID
}

// TeamMemberInput names a member of a team.
type TeamMemberInput struct {
	Team string `json:"team"` // slug or UUID
	User string `json:"user"` // UUID
}

// ListRoleBindingsInput is the input of list_role_bindings.
type ListRoleBindingsInput struct {
	Project     string             `json:"project,omitempty"` // name or repo
	SubjectKind domain.SubjectKind `json:"subject_kind,omitempty"`
	Subject     string             `json:"subject,omitempty"` // needs subject_kind
}

// CreateRoleBindingInput is the input of create_role_binding.
type CreateRoleBindingInput struct {
	SubjectKind domain.SubjectKind `json:"subject_kind"`
	Subject     string             `json:"subject"` // user UUID, team or service account by name or UUID
	Role        string             `json:"role"`    // role name
	Project     string             `json:"project,omitempty"`
}

// RoleBindingRefInput names a role binding.
type RoleBindingRefInput struct {
	Binding string `json:"binding"` // UUID
}

// RoleBindingInfo is a role binding with the names of its role and project.
type RoleBindingInfo struct {
	domain.RoleBinding
	Role      string           `json:"role"`
	RoleScope domain.RoleScope `json:"role_scope"`
	Project   string           `json:"project,omitempty"` // empty: the organization
}

// CreateServiceAccountInput is the input of create_service_account.
type CreateServiceAccountInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// UpdateServiceAccountInput is the input of update_service_account. Empty
// fields stay as they are; a nil Description too.
type UpdateServiceAccountInput struct {
	ServiceAccount string  `json:"service_account"` // name or UUID
	Name           string  `json:"name,omitempty"`
	Description    *string `json:"description,omitempty"`
}

// SetServiceAccountInput is the input of set_service_account_disabled.
type SetServiceAccountInput struct {
	ServiceAccount string `json:"service_account"` // name or UUID
	Disabled       bool   `json:"disabled"`
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

// ProposeKnowledgeInput is the input of propose_knowledge.
type ProposeKnowledgeInput struct {
	Project string `json:"project"`
	Section string `json:"section"`
	Path    string `json:"path"`
	Body    string `json:"body"` // the full new Markdown, without frontmatter; empty to retire
	Reason  string `json:"reason"`
	// Action is add, edit or retire; empty is edit when the entry exists,
	// else add.
	Action domain.KnowledgeAction `json:"action,omitempty"`
}

// ProposeKnowledgeOutput is the output of propose_knowledge.
type ProposeKnowledgeOutput struct {
	Proposal string                         `json:"proposal"` // KB-n
	Status   domain.KnowledgeProposalStatus `json:"status"`
	Number   int                            `json:"number"` // the pull request
	URL      string                         `json:"url"`
}

// OnboardProjectInput is the input of onboard_project.
type OnboardProjectInput struct {
	Repo string `json:"repo"`           // "owner/name", required
	Name string `json:"name,omitempty"` // defaults to the repo name
	// DefaultBranch of a repo that is registered by this call; defaults to main.
	DefaultBranch string `json:"default_branch,omitempty"`
	// Refresh runs only the test part of onboarding again, for a project that
	// is onboarded: a new test plan if one is needed, and a new baseline.
	Refresh bool `json:"refresh,omitempty"`
}

// OnboardProjectOutput is the output of onboard_project.
type OnboardProjectOutput struct {
	Project domain.Project `json:"project"`
	// RunID is the workflow run of the onboarding playbook, or of the
	// onboarding-tests playbook for a refresh.
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

// StartCouncilInput is the input of start_council.
type StartCouncilInput struct {
	Project  string `json:"project"`  // name or repo, required
	Question string `json:"question"` // required
	// Kind is a standard council; empty means adhoc.
	Kind string `json:"kind,omitempty"`
	// Roles are the members instead of the kind's; the devil's advocate
	// always sits.
	Roles       []string `json:"roles,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
}

// ListCouncilsInput is the input of list_councils.
type ListCouncilsInput struct {
	Project string               `json:"project,omitempty"` // name or repo; every project the caller may read when empty
	Status  domain.CouncilStatus `json:"status,omitempty"`  // running, decided, failed or canceled
}

// CouncilRefInput is the input of get_council.
type CouncilRefInput struct {
	Council string `json:"council"` // COUNCIL-2 or UUID
}

// AddCouncilConstraintInput is the input of add_council_constraint.
type AddCouncilConstraintInput struct {
	Council    string `json:"council"`    // COUNCIL-2 or UUID
	Constraint string `json:"constraint"` // required
}

// CouncilInfo is a council with its ref.
type CouncilInfo struct {
	domain.Council
	Ref string `json:"ref"` // COUNCIL-2
}

// ContributionInfo is a member's output. Its content is agent text:
// untrusted data.
type ContributionInfo struct {
	domain.CouncilContribution
	Member string `json:"member"` // role@runtime
}

// CouncilDetail is a council with what it produced.
type CouncilDetail struct {
	CouncilInfo
	// Contributions are in protocol order, by stage and round.
	Contributions []ContributionInfo `json:"contributions"`
	Gate          *GateInfo          `json:"gate,omitempty"` // its council_decision gate
	// Decision is the decision that ended it, set once decided.
	Decision *domain.Decision `json:"decision,omitempty"`
	// Untrusted holds the brief and the contributions between untrusted-data
	// markers, for the text of an MCP result, which an agent may read as
	// instructions. Other clients read the fields above as data.
	Untrusted string `json:"-"`
}

// VerificationReportInput is the input of verification_report.
type VerificationReportInput struct {
	// Change is a pull request number (with Project), a pull request URL
	// (https://github.com/owner/app/pull/9) or a task ref (TASK-12: its
	// newest pull request).
	Change  string `json:"change"`
	Project string `json:"project,omitempty"` // name or repo; required for a number
}

// VerificationInfo is the verification report of a pull request head.
type VerificationInfo struct {
	domain.VerificationReport
	Change int    `json:"change"` // pull request number
	URL    string `json:"url"`    // the pull request
	// Current says the report is for the pull request's head commit; a
	// report of an earlier head is stale.
	Current bool `json:"current"`
	// Untrusted holds the report as Markdown between untrusted-data markers,
	// for the text of an MCP result: test and environment names come from
	// the repo, and an agent may read them as instructions. Other clients
	// read the fields above as data.
	Untrusted string `json:"-"`
}

// MaxQualityChanges bounds the open pull requests project_quality lists.
const MaxQualityChanges = 50

// QualityInfo is the test health of a project (project_quality).
type QualityInfo struct {
	Project       string `json:"project"`
	DefaultBranch string `json:"default_branch"`
	// Baseline is the newest commit tested outside a pull request; absent
	// when there is none.
	Baseline *QualityBaseline `json:"baseline,omitempty"`
	// Changes are the open pull requests, newest first, at most
	// MaxQualityChanges.
	Changes []QualityChange `json:"changes"`
}

// QualityBaseline is the latest result of every suite and cell of the
// baseline commit.
type QualityBaseline struct {
	SHA   string        `json:"sha"`
	Cells []QualityCell `json:"cells"`
}

// QualityChange is an open pull request with its test results.
type QualityChange struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Task   string `json:"task,omitempty"` // TASK-12
	// Cells are the latest results of the head commit.
	Cells []QualityCell `json:"cells"`
	// Verification is the change's report; absent when it has none.
	Verification *VerificationInfo `json:"verification,omitempty"`
}

// QualityCell is the latest attempt of one suite in one cell of an
// environment, with the cases that failed (at most 100). Names and messages
// are written by the repo's tests: untrusted data.
type QualityCell struct {
	Environment       string               `json:"environment"`
	Cell              map[string]string    `json:"cell"`
	Suite             string               `json:"suite"`
	Status            domain.TestRunStatus `json:"status"`
	Counts            domain.TestCounts    `json:"counts"`
	Job               string               `json:"job"` // JOB-41
	Failures          []domain.TestResult  `json:"failures"`
	FailuresTruncated bool                 `json:"failures_truncated,omitempty"`
}

// VerificationRef is the verification of a task's pull request in get_goal.
type VerificationRef struct {
	Status  domain.VerificationStatus `json:"status"`
	SHA     string                    `json:"sha"`
	Current bool                      `json:"current"`
	// URL is the REST path of the report (api.md).
	URL string `json:"url"`
}

// JobRefInput is the input of job_status.
type JobRefInput struct {
	Job string `json:"job"` // JOB-41 or UUID
}

// JobInfo is a job with what a human needs to know about its state.
type JobInfo struct {
	domain.Job
	Ref string `json:"ref"` // JOB-41
	// Reason explains the state: why a queued job waits, the error of a
	// failed job.
	Reason string `json:"reason,omitempty"`
	// ReasonCode is the code of why a queued job waits (fleet.ReasonCode).
	ReasonCode string `json:"reason_code,omitempty"`
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
	// OwnerKind and OwnerID say whose token it is; empty on a token from
	// before personal tokens that has no owner yet.
	OwnerKind domain.SubjectKind `json:"owner_kind,omitempty"`
	OwnerID   uuid.UUID          `json:"owner_id,omitzero"`
	Scopes    []domain.Scope     `json:"scopes,omitempty"`
	ExpiresAt time.Time          `json:"expires_at,omitzero"`
}

// NewTokenInfo returns the view of t; current says it is the caller's token.
func NewTokenInfo(t domain.MCPToken, current bool) TokenInfo {
	return TokenInfo{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt, Current: current,
		OwnerKind: t.OwnerKind, OwnerID: t.OwnerID, Scopes: t.Scopes, ExpiresAt: t.ExpiresAt}
}

// CreateTokenInput is the input of create_token.
type CreateTokenInput struct {
	Name   string         `json:"name"`
	Scopes []domain.Scope `json:"scopes"`
	// TTL is how long the token lasts, a Go duration such as 720h; empty is
	// auth.token_default_ttl.
	TTL string `json:"ttl,omitempty"`
	// ServiceAccount names the service account the token is for; empty is
	// the caller.
	ServiceAccount string `json:"service_account,omitempty"`
}

// CreateTokenOutput is the output of create_token.
type CreateTokenOutput struct {
	Token TokenInfo `json:"token"`
	// Secret is the token. It is shown here once and never stored.
	Secret string `json:"secret"`
}

// RoleGrant is a role an invitation gives: on the organization, or on one
// project when Project is set.
type RoleGrant struct {
	Role    string `json:"role"`
	Project string `json:"project,omitempty"`
}

// CreateInvitationInput is the input of create_invitation: exactly one of
// Email and GitHubLogin.
type CreateInvitationInput struct {
	Email       string      `json:"email,omitempty"`
	GitHubLogin string      `json:"github_login,omitempty"`
	Roles       []RoleGrant `json:"roles"`
	// TTL is how long the link works, a Go duration; empty is 7 days.
	TTL string `json:"ttl,omitempty"`
}

// CreateInvitationOutput is the output of create_invitation.
type CreateInvitationOutput struct {
	Invitation domain.Invitation `json:"invitation"`
	// URL is the link to send. It is shown here once and never stored.
	URL string `json:"url"`
}

// InvitationRefInput names an invitation.
type InvitationRefInput struct {
	Invitation string `json:"invitation"` // UUID
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

// ListMachinesInput is the input of list_machines.
type ListMachinesInput struct {
	State string `json:"state,omitempty"` // one state; by default the machines that exist
	Limit int    `json:"limit,omitempty"` // default 100, at most 500
}

// MachineInfo is a machine with the names people know.
type MachineInfo struct {
	domain.Machine
	Name     string `json:"name"`     // linux-small-8f2
	Shape    string `json:"shape"`    // name of its shape
	Provider string `json:"provider"` // name of its provider
	JobRef   string `json:"job_ref,omitempty"`
}

// ShapeInfo is a shape with its provider's name and whose it is. An
// instance-wide shape comes without its provider_config, which is the
// instance's own business.
type ShapeInfo struct {
	domain.Shape
	Provider  string           `json:"provider"`
	Ownership domain.Ownership `json:"ownership"`
}

// ProviderInfo is a capacity provider with whether it is online and whose it
// is. An instance-wide provider comes without what says how the instance is
// built: host, account, checks, tags and network.
type ProviderInfo struct {
	domain.CapacityProvider
	Online    bool             `json:"online"`
	Ownership domain.Ownership `json:"ownership"`
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
	// CanResolve says whether the caller may resolve the gate.
	CanResolve bool `json:"can_resolve"`
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
	// Verification is the verification report of the task's newest pull
	// request that has one; absent when none has.
	Verification *VerificationRef `json:"verification,omitempty"`
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
