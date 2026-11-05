package domain

import (
	"fmt"
	"slices"
	"time"
	"uuid"
)

// Permission is one action a person or service account may be allowed to
// take (docs/contracts/permissions.md). Roles group permissions; role
// bindings give roles to subjects. There are no deny rules.
type Permission string

// The permission catalog. Each permission has a scope class (Permission.Scope)
// and says whether it can be granted on one project (Permission.ProjectScoped).
const (
	PermOrgRead              Permission = "org.read"   // the organization, its members, teams, roles and service accounts
	PermOrgManage            Permission = "org.manage" // name, slug, ownership; granting the Owner role
	PermBillingManage        Permission = "billing.manage"
	PermMemberManage         Permission = "member.manage"          // invitations, membership status, organization role bindings
	PermTeamManage           Permission = "team.manage"            // teams, their members and their role bindings
	PermRoleManage           Permission = "role.manage"            // custom roles (enterprise)
	PermServiceAccountManage Permission = "service_account.manage" // service accounts and their role bindings
	PermConnectionManage     Permission = "connection.manage"      // sign-in connections, group mappings, verified domains, SSO enforcement, SCIM tokens
	PermTokenManage          Permission = "token.manage"           // service account tokens; listing and revoking other people's tokens
	PermAuditRead            Permission = "audit.read"
	PermWorkerRead           Permission = "worker.read"   // workers, machines, providers, shapes, images and fleet usage
	PermWorkerManage         Permission = "worker.manage" // workers, capacity providers, shapes and the fleet policy; shown as Manage machines
	PermPoolManage           Permission = "pool.manage"
	PermProjectCreate        Permission = "project.create" // onboard a repo

	PermProjectRead       Permission = "project.read"   // the project, its profile, councils, registers, knowledge and proposals
	PermProjectManage     Permission = "project.manage" // project settings and project role bindings
	PermGoalCreate        Permission = "goal.create"    // goals and bug reports
	PermGoalRead          Permission = "goal.read"      // goals, specs, tasks and changes
	PermJobRead           Permission = "job.read"       // jobs, logs and artifacts
	PermJobDispatch       Permission = "job.dispatch"
	PermJobCancel         Permission = "job.cancel"
	PermGateRead          Permission = "gate.read"
	PermCouncilStart      Permission = "council.start" // start a council, add a constraint
	PermAssumptionResolve Permission = "assumption.resolve"
	PermKnowledgePropose  Permission = "knowledge.propose"
	PermProposalDecide    Permission = "proposal.decide" // accept, reject or snooze an improvement proposal

	// Resolving a gate (approve, request_changes, reject) needs the
	// permission of its kind (GatePermission).
	PermGateApproveProfile            Permission = "gate.approve.profile"
	PermGateApproveSpec               Permission = "gate.approve.spec"
	PermGateApproveTestPlan           Permission = "gate.approve.test_plan"
	PermGateApproveMerge              Permission = "gate.approve.merge"
	PermGateApproveCouncil            Permission = "gate.approve.council"
	PermGateApproveAssumption         Permission = "gate.approve.assumption"
	PermGateApproveKnowledge          Permission = "gate.approve.knowledge"
	PermGateApproveKnowledgeProtected Permission = "gate.approve.knowledge_protected"
	PermGateApproveAccessRequest      Permission = "gate.approve.access_request"
	PermGateApproveImprovement        Permission = "gate.approve.improvement"
	PermGateApproveNeedsYou           Permission = "gate.approve.needs_you"
)

// Scope is a token or OAuth grant scope. Scopes are ordered: admin includes
// write, write includes read.
type Scope string

// Token scopes.
const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

// Scopes returns every scope, lowest first.
func Scopes() []Scope { return []Scope{ScopeRead, ScopeWrite, ScopeAdmin} }

func (s Scope) rank() int { return slices.Index(Scopes(), s) }

// Includes reports whether s includes o (admin includes write and read).
func (s Scope) Includes(o Scope) bool { return s.rank() >= 0 && o.rank() >= 0 && s.rank() >= o.rank() }

// ScopesAllow reports whether a token with scopes may use p: one of the
// scopes includes p's scope class. The owner's permissions still apply.
func ScopesAllow(scopes []Scope, p Permission) bool {
	need := p.Scope()
	return need != "" && slices.ContainsFunc(scopes, func(s Scope) bool { return s.Includes(need) })
}

// permissionInfo is the scope class of a permission and whether it can be
// granted on one project.
type permissionInfo struct {
	scope   Scope
	project bool
}

var permissionTable = map[Permission]permissionInfo{
	PermOrgRead:              {ScopeRead, false},
	PermOrgManage:            {ScopeAdmin, false},
	PermBillingManage:        {ScopeAdmin, false},
	PermMemberManage:         {ScopeAdmin, false},
	PermTeamManage:           {ScopeAdmin, false},
	PermRoleManage:           {ScopeAdmin, false},
	PermServiceAccountManage: {ScopeAdmin, false},
	PermConnectionManage:     {ScopeAdmin, false},
	PermTokenManage:          {ScopeAdmin, false},
	PermAuditRead:            {ScopeRead, false},
	PermWorkerRead:           {ScopeRead, false},
	PermWorkerManage:         {ScopeAdmin, false},
	PermPoolManage:           {ScopeAdmin, false},
	PermProjectCreate:        {ScopeWrite, false},

	PermProjectRead:       {ScopeRead, true},
	PermProjectManage:     {ScopeAdmin, true},
	PermGoalCreate:        {ScopeWrite, true},
	PermGoalRead:          {ScopeRead, true},
	PermJobRead:           {ScopeRead, true},
	PermJobDispatch:       {ScopeWrite, true},
	PermJobCancel:         {ScopeWrite, true},
	PermGateRead:          {ScopeRead, true},
	PermCouncilStart:      {ScopeWrite, true},
	PermAssumptionResolve: {ScopeWrite, true},
	PermKnowledgePropose:  {ScopeWrite, true},
	PermProposalDecide:    {ScopeWrite, true},

	PermGateApproveProfile:            {ScopeWrite, true},
	PermGateApproveSpec:               {ScopeWrite, true},
	PermGateApproveTestPlan:           {ScopeWrite, true},
	PermGateApproveMerge:              {ScopeWrite, true},
	PermGateApproveCouncil:            {ScopeWrite, true},
	PermGateApproveAssumption:         {ScopeWrite, true},
	PermGateApproveKnowledge:          {ScopeWrite, true},
	PermGateApproveKnowledgeProtected: {ScopeWrite, true},
	PermGateApproveAccessRequest:      {ScopeWrite, true},
	PermGateApproveImprovement:        {ScopeWrite, true},
	PermGateApproveNeedsYou:           {ScopeWrite, true},
}

// Permissions returns the whole catalog, sorted.
func Permissions() []Permission {
	all := make([]Permission, 0, len(permissionTable))
	for p := range permissionTable {
		all = append(all, p)
	}
	slices.Sort(all)
	return all
}

// Valid reports whether p is in the catalog.
func (p Permission) Valid() bool { _, ok := permissionTable[p]; return ok }

// Scope is the lowest token scope that may use p; empty for an unknown
// permission.
func (p Permission) Scope() Scope { return permissionTable[p].scope }

// ProjectScoped reports whether p can be granted on one project. The others
// are organization-wide only.
func (p Permission) ProjectScoped() bool { return permissionTable[p].project }

// GatePermission is the permission that resolving a gate of kind needs.
// protected is true for a knowledge_proposal gate about a protected
// section, which needs gate.approve.knowledge_protected. Empty for an
// unknown kind.
func GatePermission(kind GateKind, protected bool) Permission {
	switch kind {
	case GateProfileApproval:
		return PermGateApproveProfile
	case GateSpecApproval:
		return PermGateApproveSpec
	case GateTestPlanApproval:
		return PermGateApproveTestPlan
	case GateMergeApproval:
		return PermGateApproveMerge
	case GateCouncilDecision:
		return PermGateApproveCouncil
	case GateAssumptionReview:
		return PermGateApproveAssumption
	case GateKnowledgeProposal:
		if protected {
			return PermGateApproveKnowledgeProtected
		}
		return PermGateApproveKnowledge
	case GateAccessRequest:
		return PermGateApproveAccessRequest
	case GateImprovementProposal:
		return PermGateApproveImprovement
	case GateNeedsYou:
		return PermGateApproveNeedsYou
	}
	return ""
}

// RoleScope says where a role can be bound.
type RoleScope string

// Role scopes. An organization role is bound to the whole organization
// only; a project role is bound to one project, or to the organization,
// where it applies to every project.
const (
	RoleScopeOrganization RoleScope = "organization"
	RoleScopeProject      RoleScope = "project"
)

// RoleScopes returns every role scope.
func RoleScopes() []RoleScope { return []RoleScope{RoleScopeOrganization, RoleScopeProject} }

// Role is a named set of permissions. Built-in roles have no AccountID and
// are the same in every organization; custom roles (enterprise) belong to
// one.
type Role struct {
	ID          uuid.UUID    `json:"id"`
	AccountID   uuid.UUID    `json:"account_id,omitzero"`
	Name        string       `json:"name"` // a ValidName, unique per organization and scope
	Description string       `json:"description,omitempty"`
	Scope       RoleScope    `json:"scope"`
	Permissions []Permission `json:"permissions"` // sorted
	BuiltIn     bool         `json:"built_in"`
	Custom      bool         `json:"custom"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Has reports whether the role grants p.
func (r Role) Has(p Permission) bool { return slices.Contains(r.Permissions, p) }

// Validate checks a role against the catalog: a valid name and scope, at
// least one permission, every permission in the catalog and none twice, and
// only project-scoped permissions in a project role. Built-in roles have no
// organization; custom roles have one.
func (r Role) Validate() error {
	var p problems
	if !ValidName(r.Name) {
		p.addf("name %q must be a lowercase name", r.Name)
	}
	checkEnum(&p, "scope", r.Scope, RoleScopes())
	if len(r.Permissions) == 0 {
		p.addf("permissions must not be empty")
	}
	seen := map[Permission]bool{}
	for _, perm := range r.Permissions {
		switch {
		case !perm.Valid():
			p.addf("unknown permission %q", perm)
		case seen[perm]:
			p.addf("permission %q listed twice", perm)
		case r.Scope == RoleScopeProject && !perm.ProjectScoped():
			p.addf("permission %q cannot be granted on a project", perm)
		}
		seen[perm] = true
	}
	if r.BuiltIn == r.Custom {
		p.addf("a role is either built in or custom")
	}
	if r.BuiltIn != (r.AccountID == uuid.Nil()) {
		p.addf("built-in roles have no organization, custom roles have one")
	}
	return p.errOf(ErrInvalidRecord, "role "+r.Name)
}

// Built-in role names.
const (
	RoleOwner   = "owner"
	RoleAdmin   = "admin"
	RoleBilling = "billing"
	RoleMember  = "member"
	RoleViewer  = "viewer" // both an organization and a project role

	RoleProjectAdmin = "project_admin"
	RoleMaintainer   = "maintainer"
	RoleContributor  = "contributor"
)

// Permission sets the built-in project roles grow from: a viewer reads,
// a contributor works, a maintainer also resolves every gate.
var (
	projectReads = []Permission{PermProjectRead, PermGoalRead, PermJobRead, PermGateRead}
	contributor  = append(slices.Clone(projectReads), PermGoalCreate, PermJobDispatch, PermJobCancel,
		PermCouncilStart, PermKnowledgePropose,
		PermGateApproveSpec, PermGateApproveCouncil, PermGateApproveAssumption, PermGateApproveKnowledge, PermGateApproveNeedsYou)
	maintainer = append(slices.Clone(contributor), PermAssumptionResolve, PermProposalDecide,
		PermGateApproveProfile, PermGateApproveTestPlan, PermGateApproveMerge, PermGateApproveKnowledgeProtected,
		PermGateApproveAccessRequest, PermGateApproveImprovement)
)

// BuiltInRoles returns the built-in roles with their permissions
// (docs/contracts/permissions.md), organization roles first. IDs and
// timestamps are those of the rows migration 00005 seeds, so they are zero
// here; look roles up by scope and name.
func BuiltInRoles() []Role {
	all := Permissions()
	projectAll := slices.DeleteFunc(slices.Clone(all), func(p Permission) bool { return !p.ProjectScoped() })
	without := func(perms []Permission, drop ...Permission) []Permission {
		return slices.DeleteFunc(slices.Clone(perms), func(p Permission) bool { return slices.Contains(drop, p) })
	}
	role := func(name string, scope RoleScope, desc string, perms ...Permission) Role {
		perms = slices.Clone(perms)
		slices.Sort(perms)
		return Role{Name: name, Scope: scope, Description: desc, Permissions: slices.Compact(perms), BuiltIn: true}
	}
	return []Role{
		role(RoleOwner, RoleScopeOrganization, "Everything, including ownership and billing", all...),
		role(RoleAdmin, RoleScopeOrganization, "Everything but ownership and billing", without(all, PermOrgManage, PermBillingManage)...),
		role(RoleBilling, RoleScopeOrganization, "Billing only", PermOrgRead, PermBillingManage),
		role(RoleMember, RoleScopeOrganization, "Reads every project, creates goals and proposes knowledge",
			append([]Permission{PermOrgRead, PermWorkerRead, PermGoalCreate, PermKnowledgePropose}, projectReads...)...),
		role(RoleViewer, RoleScopeOrganization, "Reads every project",
			append([]Permission{PermOrgRead, PermWorkerRead}, projectReads...)...),
		role(RoleProjectAdmin, RoleScopeProject, "Everything on the project, its access included", projectAll...),
		role(RoleMaintainer, RoleScopeProject, "Runs the project and resolves every gate", maintainer...),
		role(RoleContributor, RoleScopeProject, "Creates goals and jobs, approves specs, councils and questions, not merges", contributor...),
		role(RoleViewer, RoleScopeProject, "Reads the project", projectReads...),
	}
}

// BuiltInRole returns the built-in role of scope called name.
func BuiltInRole(scope RoleScope, name string) (Role, bool) {
	i := slices.IndexFunc(BuiltInRoles(), func(r Role) bool { return r.Scope == scope && r.Name == name })
	if i < 0 {
		return Role{}, false
	}
	return BuiltInRoles()[i], true
}

// SubjectKind is who a role binding is for.
type SubjectKind string

// Subject kinds.
const (
	SubjectUser           SubjectKind = "user"
	SubjectTeam           SubjectKind = "team"
	SubjectServiceAccount SubjectKind = "service_account"
)

// SubjectKinds returns every subject kind.
func SubjectKinds() []SubjectKind {
	return []SubjectKind{SubjectUser, SubjectTeam, SubjectServiceAccount}
}

// RoleBinding gives a role to a subject on the organization (ProjectID
// zero) or on one project. (subject, role, project) is unique.
type RoleBinding struct {
	ID          uuid.UUID   `json:"id"`
	AccountID   uuid.UUID   `json:"account_id"`
	SubjectKind SubjectKind `json:"subject_kind"`
	SubjectID   uuid.UUID   `json:"subject_id"`
	RoleID      uuid.UUID   `json:"role_id"`
	ProjectID   uuid.UUID   `json:"project_id,omitzero"` // zero: organization scope
	CreatedBy   string      `json:"created_by"`          // Actor.String()
	CreatedAt   time.Time   `json:"created_at"`
}

// ValidateFor checks the binding of role: a known subject, an organization
// role only at organization scope, the role in the binding's organization
// (or built in), and the Owner role only for a user on the organization.
func (b RoleBinding) ValidateFor(role Role) error {
	var p problems
	checkEnum(&p, "subject_kind", b.SubjectKind, SubjectKinds())
	if b.SubjectID == uuid.Nil() {
		p.addf("subject_id must be set")
	}
	if role.ID != b.RoleID {
		p.addf("role %s is not the role of the binding", role.ID)
	}
	if !role.BuiltIn && role.AccountID != b.AccountID {
		p.addf("role %s belongs to another organization", role.Name)
	}
	if role.Scope == RoleScopeOrganization && b.ProjectID != uuid.Nil() {
		p.addf("organization role %s cannot be bound to a project", role.Name)
	}
	if role.BuiltIn && role.Name == RoleOwner && role.Scope == RoleScopeOrganization && b.SubjectKind != SubjectUser {
		p.addf("the owner role is only for users")
	}
	return p.errOf(ErrInvalidRecord, fmt.Sprintf("binding of %s", role.Name))
}

// Grant is a role an invitation gives on acceptance: on the organization
// (ProjectID zero) or on one project.
type Grant struct {
	RoleID    uuid.UUID `json:"role_id"`
	ProjectID uuid.UUID `json:"project_id,omitzero"`
}
