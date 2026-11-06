package mcp

import (
	"context"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
)

// Access is what a caller needs to use a tool or an /api/v1 route
// (docs/contracts/permissions.md). The authorization engine checks it on
// the project the call is about, or on the organization for a call that is
// not about one project. The zero Access is any signed-in subject.
type Access struct {
	// Permission is the permission the caller needs. With Own it is only
	// needed for records of other people.
	Permission domain.Permission
	// Gate means the permission depends on the gate: domain.GatePermission
	// of its kind, on its project.
	Gate bool
	// Own means the call works on the caller's own records without a
	// permission (their tokens, their session), within the scopes of its
	// credential.
	Own bool
	// Target means the permission depends on what the call is about (the
	// subject and scope of a role binding); the tool decides it.
	Target bool
	// Public means the route needs no credential at all.
	Public bool
}

// Check is the check of the entry points, before the call runs: by holds the
// permission somewhere, or for Own records a credential that may read. They
// do not know the project yet; the tool checks the project, and the Gate, Own
// and Target cases, through the same engine.
func (a Access) Check(ctx context.Context, e *authz.Engine, by domain.Actor) error {
	switch {
	case a.Own:
		return e.AuthorizeScope(ctx, by, domain.ScopeRead)
	case a.Permission == "":
		return nil
	}
	return e.AuthorizeAnywhere(ctx, by, a.Permission)
}

// ToolAccess returns what each MCP tool needs, by tool name. Every tool of
// Tools has an entry; a task adding a tool adds its entry (and the
// permission, if new, to the catalog).
func ToolAccess() map[string]Access {
	return map[string]Access{
		ToolOnboardProject:     {Permission: domain.PermProjectCreate},
		ToolCreateGoal:         {Permission: domain.PermGoalCreate},
		ToolJobStatus:          {Permission: domain.PermJobRead},
		ToolJobLogs:            {Permission: domain.PermJobRead},
		ToolListWorkers:        {Permission: domain.PermWorkerRead},
		ToolListPendingGates:   {Permission: domain.PermGateRead},
		ToolApprove:            {Gate: true},
		ToolRequestChanges:     {Gate: true},
		ToolReject:             {Gate: true},
		ToolDispatchJob:        {Permission: domain.PermJobDispatch},
		ToolListProjects:       {Permission: domain.PermProjectRead},
		ToolGetProject:         {Permission: domain.PermProjectRead},
		ToolListGoals:          {Permission: domain.PermGoalRead},
		ToolGetGoal:            {Permission: domain.PermGoalRead},
		ToolListJobs:           {Permission: domain.PermJobRead},
		ToolCancelJob:          {Permission: domain.PermJobCancel},
		ToolListTokens:         {Permission: domain.PermTokenManage, Own: true},
		ToolRevokeToken:        {Permission: domain.PermTokenManage, Own: true},
		ToolListArtifacts:      {Permission: domain.PermJobRead},
		ToolListTestRuns:       {Permission: domain.PermJobRead},
		ToolListAssumptions:    {Permission: domain.PermProjectRead},
		ToolResolveAssumption:  {Permission: domain.PermAssumptionResolve},
		ToolListDecisions:      {Permission: domain.PermProjectRead},
		ToolSearchKnowledge:    {Permission: domain.PermProjectRead},
		ToolDeleteNote:         {Permission: domain.PermKnowledgePropose},
		ToolProposeKnowledge:   {Permission: domain.PermKnowledgePropose},
		ToolStartCouncil:       {Permission: domain.PermCouncilStart},
		ToolListCouncils:       {Permission: domain.PermProjectRead},
		ToolGetCouncil:         {Permission: domain.PermProjectRead},
		ToolAddConstraint:      {Permission: domain.PermCouncilStart},
		ToolVerificationReport: {Permission: domain.PermJobRead},
		ToolCreateToken:        {Permission: domain.PermTokenManage, Own: true},
		ToolCreateInvitation:   {Permission: domain.PermMemberManage},
		ToolListInvitations:    {Permission: domain.PermMemberManage},
		ToolRevokeInvitation:   {Permission: domain.PermMemberManage},

		ToolProjectQuality:       {Permission: domain.PermJobRead},
		ToolListRoles:            {Permission: domain.PermOrgRead},
		ToolListTeams:            {Permission: domain.PermOrgRead},
		ToolCreateTeam:           {Permission: domain.PermTeamManage},
		ToolUpdateTeam:           {Permission: domain.PermTeamManage},
		ToolDeleteTeam:           {Permission: domain.PermTeamManage},
		ToolListTeamMembers:      {Permission: domain.PermOrgRead},
		ToolAddTeamMember:        {Permission: domain.PermTeamManage},
		ToolRemoveTeamMember:     {Permission: domain.PermTeamManage},
		ToolListRoleBindings:     {Target: true},
		ToolCreateRoleBinding:    {Target: true},
		ToolDeleteRoleBinding:    {Target: true},
		ToolListServiceAccounts:  {Permission: domain.PermOrgRead},
		ToolCreateServiceAccount: {Permission: domain.PermServiceAccountManage},
		ToolUpdateServiceAccount: {Permission: domain.PermServiceAccountManage},
		ToolSetServiceAccount:    {Permission: domain.PermServiceAccountManage},
		ToolListMachines:         {Permission: domain.PermWorkerRead},
		ToolListShapes:           {Permission: domain.PermWorkerRead},
		ToolListProviders:        {Permission: domain.PermWorkerRead},
		ToolFleetOverview:        {Permission: domain.PermWorkerRead},
	}
}

// CheckAccess reports an access entry that is not well formed: a
// permission outside the catalog, Gate or Target with a permission (the gate
// or the tool decides it), Own without the permission for other people's
// records, or Public with anything else.
func CheckAccess(a Access) error {
	switch {
	case a.Public && (a.Permission != "" || a.Gate || a.Own || a.Target):
		return errAccess("a public route needs nothing else")
	case a.Gate && a.Permission != "":
		return errAccess("a gate decides its permission")
	case a.Target && a.Permission != "":
		return errAccess("the tool decides the permission of its target")
	case a.Own && a.Permission == "":
		return errAccess("own records need a permission for other people's")
	case a.Permission != "" && !a.Permission.Valid():
		return errAccess("permission " + string(a.Permission) + " is not in the catalog")
	}
	return nil
}

type errAccess string

func (e errAccess) Error() string { return string(e) }
