package mcp

import "github.com/forecrew/forecrew/internal/domain"

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
	// permission (their tokens, their session).
	Own bool
	// Public means the route needs no credential at all.
	Public bool
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
	}
}

// CheckAccess reports an access entry that is not well formed: a
// permission outside the catalog, Gate with a permission (the gate decides
// it), Own without the permission for other people's records, or Public
// with anything else.
func CheckAccess(a Access) error {
	switch {
	case a.Public && (a.Permission != "" || a.Gate || a.Own):
		return errAccess("a public route needs nothing else")
	case a.Gate && a.Permission != "":
		return errAccess("a gate decides its permission")
	case a.Own && a.Permission == "":
		return errAccess("own records need a permission for other people's")
	case a.Permission != "" && !a.Permission.Valid():
		return errAccess("permission " + string(a.Permission) + " is not in the catalog")
	}
	return nil
}

type errAccess string

func (e errAccess) Error() string { return string(e) }
