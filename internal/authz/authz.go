// Package authz answers one question for every entry point of Forecrew: may
// this caller take this action on this resource
// (docs/contracts/permissions.md). The web UI, REST, MCP, the CLI and GitHub
// commands all end in Engine.Authorize.
//
// A caller's effective permissions are the union of its role bindings, at
// organization scope and at the resource's project scope; a token or OAuth
// grant only narrows them. Actions, resources and principals are named like
// Cedar entities and the decision itself sits behind Evaluator, so a policy
// engine can take over for attribute rules without touching the callers.
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrDenied means the caller lacks a permission.
var ErrDenied = errors.New("permission denied")

// EntityUID names an entity the way Cedar does: Type::"id".
type EntityUID struct {
	Type string
	ID   string
}

// String formats the entity as Type::"id".
func (e EntityUID) String() string { return fmt.Sprintf("%s::%q", e.Type, e.ID) }

// Entity types.
const (
	TypeAction         = "Action"
	TypeOrganization   = "Organization"
	TypeProject        = "Project"
	TypeUser           = "User"
	TypeTeam           = "Team"
	TypeServiceAccount = "ServiceAccount"
)

// Action is the entity of a permission.
func Action(p domain.Permission) EntityUID { return EntityUID{TypeAction, string(p)} }

// Request is one authorization question, in the shape of Cedar's.
type Request struct {
	Principal EntityUID
	Action    EntityUID
	Resource  EntityUID
}

// Evaluator decides a request from the bindings that apply to the principal.
// RoleEvaluator is the implementation; a policy engine replaces it for
// attribute rules (4.7). Scopes and ownerless tokens are settled before an
// Evaluator is asked.
type Evaluator interface {
	IsAuthorized(req Request, bindings []store.BoundRole) bool
}

// RoleEvaluator allows a request when a binding grants the permission: a
// binding on the organization applies everywhere, a binding on a project
// applies to that project. There are no deny rules.
type RoleEvaluator struct{}

// IsAuthorized implements Evaluator.
func (RoleEvaluator) IsAuthorized(req Request, bindings []store.BoundRole) bool {
	p := domain.Permission(req.Action.ID)
	return slices.ContainsFunc(bindings, func(b store.BoundRole) bool {
		if !b.Role.Has(p) {
			return false
		}
		if b.Binding.ProjectID == uuid.Nil() {
			return true
		}
		return p.ProjectScoped() && req.Resource == projectUID(b.Binding.ProjectID)
	})
}

func projectUID(id uuid.UUID) EntityUID { return EntityUID{TypeProject, id.String()} }

// Resource is what an action is taken on: an organization, or one of its
// projects. Other objects (a job, a gate, a note) resolve to their project.
type Resource struct {
	AccountID uuid.UUID
	ProjectID uuid.UUID // zero: the organization
}

// Organization is the resource of the organization accountID.
func Organization(accountID uuid.UUID) Resource { return Resource{AccountID: accountID} }

// Project is the resource of a project of the organization accountID.
func Project(accountID, projectID uuid.UUID) Resource {
	return Resource{AccountID: accountID, ProjectID: projectID}
}

// UID returns the entity of the resource.
func (r Resource) UID() EntityUID {
	if r.ProjectID != uuid.Nil() {
		return projectUID(r.ProjectID)
	}
	return EntityUID{TypeOrganization, r.AccountID.String()}
}

// Engine is the authorization engine.
type Engine struct {
	st         store.Store
	eval       Evaluator
	protection Protection
	log        *slog.Logger
}

// New returns an engine over st that evaluates role bindings. logger
// defaults to discarding.
func New(st store.Store, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Engine{st: st, eval: RoleEvaluator{}, log: logger}
}

// WithEvaluator makes the engine ask ev instead of RoleEvaluator.
func (e *Engine) WithEvaluator(ev Evaluator) *Engine {
	e.eval = ev
	return e
}

// caller is what the engine knows of a subject for one request.
type caller struct {
	principal EntityUID
	// bounded means scopes limit the caller: it came with a token.
	bounded bool
	scopes  []domain.Scope
	// ownerless is a token without an owner, limited by its scopes only.
	ownerless bool
	// breakGlass is an instance admin recovering access: every permission,
	// on the organization of the session.
	breakGlass bool
	bindings   []store.BoundRole
}

type breakGlassKey struct{}

// WithBreakGlass marks the request as an instance admin's outside their
// memberships (docs/contracts/identity.md, Recovery). It counts for a session
// only: a token has its scopes and bindings, whoever owns it.
func WithBreakGlass(ctx context.Context) context.Context {
	return context.WithValue(ctx, breakGlassKey{}, true)
}

type cacheKey struct{}

// cache holds the callers of one request.
type cache struct {
	mu      sync.Mutex
	callers map[domain.Actor]*caller
}

// Request returns a context that remembers what the engine reads for the
// length of one request, so a caller's bindings are read once. Nothing is
// cached across requests: a change of bindings counts from the next one.
func (e *Engine) Request(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheKey{}, &cache{callers: map[domain.Actor]*caller{}})
}

// callerOf returns what by may do. Anything that is not a user, a service
// account or an ownerless token gets a caller without bindings.
func (e *Engine) callerOf(ctx context.Context, by domain.Actor) (*caller, error) {
	c, _ := ctx.Value(cacheKey{}).(*cache)
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if v, ok := c.callers[by]; ok {
			return v, nil
		}
	}
	v, err := e.load(ctx, by)
	if err != nil {
		return nil, err
	}
	if c != nil {
		c.callers[by] = v
	}
	return v, nil
}

func (e *Engine) load(ctx context.Context, by domain.Actor) (*caller, error) {
	if by.Kind == domain.ActorMCP {
		return e.loadOwnerless(ctx, by)
	}
	id, ok := by.SubjectID()
	if !ok {
		return &caller{principal: EntityUID{string(by.Kind), by.Name}}, nil
	}
	v := &caller{principal: EntityUID{TypeUser, id.String()}}
	v.breakGlass = by.Kind == domain.ActorUser && by.Credential.Kind == domain.CredentialSession && ctx.Value(breakGlassKey{}) != nil
	kind := domain.SubjectUser
	if by.Kind == domain.ActorServiceAccount {
		v.principal.Type, kind = TypeServiceAccount, domain.SubjectServiceAccount
	}
	switch by.Credential.Kind {
	case domain.CredentialToken:
		v.bounded = true
		t, ok, err := e.token(ctx, by.AccountID, func(t domain.MCPToken) bool {
			return t.ID == by.Credential.ID && t.OwnerKind == kind && t.OwnerID == id && t.Usable(time.Now())
		})
		if err != nil {
			return nil, err
		}
		if ok {
			v.scopes = t.Scopes
		}
	case domain.CredentialOAuthGrant:
		v.bounded = true // no scopes until grants exist (T55): nothing is allowed
	}
	bound, err := e.st.RoleBindings().Effective(ctx, by.AccountID, kind, id)
	if err != nil {
		return nil, fmt.Errorf("read bindings of %s: %w", by, err)
	}
	v.bindings = bound
	return v, nil
}

// loadOwnerless finds the usable ownerless token an mcp:<name> actor stands
// for; without one the caller may do nothing.
func (e *Engine) loadOwnerless(ctx context.Context, by domain.Actor) (*caller, error) {
	t, ok, err := e.token(ctx, by.AccountID, func(t domain.MCPToken) bool {
		return t.OwnerKind == "" && t.Name == by.Name && t.Usable(time.Now())
	})
	if err != nil {
		return nil, err
	}
	v := &caller{principal: EntityUID{string(by.Kind), by.Name}, bounded: true}
	if ok {
		v.ownerless, v.scopes = true, t.ScopesOrLegacy()
	}
	return v, nil
}

func (e *Engine) token(ctx context.Context, accountID uuid.UUID, match func(domain.MCPToken) bool) (domain.MCPToken, bool, error) {
	tokens, err := e.st.MCPTokens().List(ctx, accountID)
	if err != nil {
		return domain.MCPToken{}, false, fmt.Errorf("read tokens: %w", err)
	}
	i := slices.IndexFunc(tokens, match)
	if i < 0 {
		return domain.MCPToken{}, false, nil
	}
	return tokens[i], true, nil
}

// allows decides p on res for the caller: the token's scopes first, then
// the bindings.
func (e *Engine) allows(v *caller, by domain.Actor, p domain.Permission, res Resource) bool {
	if res.AccountID != by.AccountID || !p.Valid() {
		return false
	}
	if v.bounded && !domain.ScopesAllow(v.scopes, p) {
		return false
	}
	if v.ownerless || v.breakGlass {
		return true
	}
	return e.eval.IsAuthorized(Request{Principal: v.principal, Action: Action(p), Resource: res.UID()}, v.bindings)
}

// Can reports whether by holds p on res. It records nothing: use it to show
// what a caller may do, and Authorize to guard an action.
func (e *Engine) Can(ctx context.Context, by domain.Actor, p domain.Permission, res Resource) (bool, error) {
	v, err := e.callerOf(ctx, by)
	if err != nil {
		return false, err
	}
	return e.allows(v, by, p, res), nil
}

// Authorize returns nil when by holds p on res. Otherwise the error wraps
// ErrDenied, and a denied write action is audited.
func (e *Engine) Authorize(ctx context.Context, by domain.Actor, p domain.Permission, res Resource) error {
	ok, err := e.Can(ctx, by, p, res)
	if err != nil {
		return err
	}
	if !ok {
		return e.deny(ctx, by, p, res)
	}
	return nil
}

// deny audits the denial of a write action and returns the error for by.
func (e *Engine) deny(ctx context.Context, by domain.Actor, p domain.Permission, res Resource) error {
	denied := fmt.Errorf("%s is needed on %s: %w", p, res.UID(), ErrDenied)
	if p.Scope() == domain.ScopeRead || res.AccountID != by.AccountID {
		return denied
	}
	e.record(ctx, by, res, map[string]string{"permission": string(p), "action": Action(p).String(), "resource": res.UID().String()})
	return denied
}

// record audits a denial on res. A failure is logged: the denial stands.
func (e *Engine) record(ctx context.Context, by domain.Actor, res Resource, detail map[string]string) {
	data, err := json.Marshal(detail)
	if err == nil {
		kind := "organization"
		id := res.AccountID
		if res.ProjectID != uuid.Nil() {
			kind, id = "project", res.ProjectID
		}
		// A canceled request is still a denial worth keeping.
		err = e.st.Audit().Append(context.WithoutCancel(ctx), &domain.AuditEntry{
			AccountID: by.AccountID, ProjectID: res.ProjectID, Actor: by.String(), Credential: by.Credential.String(),
			Action: "access.denied", SubjectType: kind, SubjectID: id, Data: data,
		})
	}
	if err != nil {
		e.log.ErrorContext(ctx, "audit denial", "account_id", by.AccountID, "actor", by.String(), "error", err)
	}
}

// AuthorizeScope returns nil when the credential of by reaches need. It is
// the check of a caller's own records, which need no permission from its
// roles but stay within what its token or grant may do. A session has no
// scopes to meet. Otherwise the error wraps ErrDenied, and a denied write is
// audited.
func (e *Engine) AuthorizeScope(ctx context.Context, by domain.Actor, need domain.Scope) error {
	v, err := e.callerOf(ctx, by)
	if err != nil {
		return err
	}
	if !v.bounded || slices.ContainsFunc(v.scopes, func(s domain.Scope) bool { return s.Includes(need) }) {
		return nil
	}
	if need != domain.ScopeRead {
		res := Organization(by.AccountID)
		e.record(ctx, by, res, map[string]string{"scope": string(need), "resource": res.UID().String()})
	}
	return fmt.Errorf("a credential with %s scope is needed: %w", need, ErrDenied)
}

// Visible is the set of projects where a caller holds a permission.
type Visible struct {
	All bool // every project: it holds the permission on the organization
	IDs []uuid.UUID
}

// Has reports whether the project is in the set.
func (v Visible) Has(projectID uuid.UUID) bool { return v.All || slices.Contains(v.IDs, projectID) }

// Any reports whether the set can hold a project.
func (v Visible) Any() bool { return v.All || len(v.IDs) > 0 }

// Projects returns where by holds p: lists without a project filter keep
// only the rows of these projects.
func (e *Engine) Projects(ctx context.Context, by domain.Actor, p domain.Permission) (Visible, error) {
	v, err := e.callerOf(ctx, by)
	if err != nil {
		return Visible{}, err
	}
	if e.allows(v, by, p, Organization(by.AccountID)) {
		return Visible{All: true}, nil
	}
	out := Visible{IDs: []uuid.UUID{}}
	for _, b := range v.bindings {
		id := b.Binding.ProjectID
		if id != uuid.Nil() && !slices.Contains(out.IDs, id) && e.allows(v, by, p, Project(by.AccountID, id)) {
			out.IDs = append(out.IDs, id)
		}
	}
	return out, nil
}

// AuthorizeAnywhere returns nil when by holds p on the organization or on at
// least one project. It is the check of the entry points, which do not know
// the project yet; the tool checks the project itself.
func (e *Engine) AuthorizeAnywhere(ctx context.Context, by domain.Actor, p domain.Permission) error {
	where, err := e.Projects(ctx, by, p)
	if err != nil {
		return err
	}
	if !where.Any() {
		return e.deny(ctx, by, p, Organization(by.AccountID))
	}
	return nil
}

// Protection says whether a knowledge_proposal gate is about a protected
// section; *proposals.Service implements it from the proposals of the gate.
type Protection interface {
	GateProtected(ctx context.Context, g domain.Gate) (bool, error)
}

// WithProtection makes the engine ask p about knowledge_proposal gates.
// Without it they all count as protected.
func (e *Engine) WithProtection(p Protection) *Engine {
	e.protection = p
	return e
}

// GatePermission is the permission that resolving g needs. A knowledge
// proposal that cannot be placed counts as protected: the stricter
// permission, and no error, so a provider being down costs a Contributor the
// gate and not everybody the gate list.
func (e *Engine) GatePermission(ctx context.Context, g domain.Gate) domain.Permission {
	if g.Kind != domain.GateKnowledgeProposal {
		return domain.GatePermission(g.Kind, false)
	}
	protected := true
	if e.protection != nil {
		var err error
		if protected, err = e.protection.GateProtected(ctx, g); err != nil {
			e.log.WarnContext(ctx, "read the proposals of a gate", "account_id", g.AccountID, "gate_id", g.ID, "error", err)
			protected = true
		}
	}
	return domain.GatePermission(g.Kind, protected)
}

// mayResolve reports whether the kind of actor may resolve gates at all:
// a person, or an ownerless token, which is a person's until it is assigned
// to its owner. Service accounts, agents and workers never do.
func mayResolve(by domain.Actor) bool { return IsPerson(by) }

// IsPerson reports whether by acts for a person, or for an ownerless token,
// which is a person's until it is assigned to its owner. Service accounts,
// agents and workers are not.
func IsPerson(by domain.Actor) bool {
	return by.Kind == domain.ActorUser || by.Kind == domain.ActorMCP
}

// CanResolve reports whether by may resolve g, for the gate list.
func (e *Engine) CanResolve(ctx context.Context, by domain.Actor, g domain.Gate) (bool, error) {
	if !mayResolve(by) {
		return false, nil
	}
	return e.Can(ctx, by, e.GatePermission(ctx, g), Project(g.AccountID, g.ProjectID))
}

// AuthorizeGate returns nil when by may resolve g. It is the check of every
// way to resolve a gate.
func (e *Engine) AuthorizeGate(ctx context.Context, by domain.Actor, g domain.Gate) error {
	p, res := e.GatePermission(ctx, g), Project(g.AccountID, g.ProjectID)
	if !mayResolve(by) {
		return e.deny(ctx, by, p, res)
	}
	return e.Authorize(ctx, by, p, res)
}

// AuthorizeProtectedKnowledge returns nil when by may change a protected
// section of a project: the proposal of a person goes straight to a pull
// request with them as its approver, so this is resolving a protected gate.
func (e *Engine) AuthorizeProtectedKnowledge(ctx context.Context, by domain.Actor, projectID uuid.UUID) error {
	p, res := domain.PermGateApproveKnowledgeProtected, Project(by.AccountID, projectID)
	if !mayResolve(by) {
		return e.deny(ctx, by, p, res)
	}
	return e.Authorize(ctx, by, p, res)
}

// Covers returns nil when by holds every permission of role on res, the rule
// of No escalation: nobody gives a permission they lack or acts on someone
// who holds more than they do. Otherwise the error wraps ErrDenied and names
// the first permission missing.
func (e *Engine) Covers(ctx context.Context, by domain.Actor, role domain.Role, res Resource) error {
	for _, p := range role.Permissions {
		if err := e.Authorize(ctx, by, p, res); err != nil {
			return fmt.Errorf("role %s: %w", role.Name, err)
		}
	}
	return nil
}

// HeldRole is a role a subject holds and where it holds it.
type HeldRole struct {
	Role      string           `json:"role"`
	Scope     domain.RoleScope `json:"scope"`
	ProjectID uuid.UUID        `json:"project_id,omitzero"` // zero: the organization
	// TeamID is the team it comes through; zero when bound directly.
	TeamID uuid.UUID `json:"team_id,omitzero"`
}

// Effective is everything a caller may do right now, for the UI to hide what
// it cannot. A token shows what it can use, not all its owner could.
type Effective struct {
	Roles []HeldRole `json:"roles"`
	// Permissions hold on the organization and, for project-scoped ones, on
	// every project.
	Permissions []domain.Permission `json:"permissions"`
	// Projects has, by project ID, the project-scoped permissions on each
	// project where the caller has any.
	Projects map[string][]domain.Permission `json:"projects"`
}

// EffectiveOf returns what by may do.
func (e *Engine) EffectiveOf(ctx context.Context, by domain.Actor) (Effective, error) {
	v, err := e.callerOf(ctx, by)
	if err != nil {
		return Effective{}, err
	}
	out := Effective{Roles: []HeldRole{}, Permissions: []domain.Permission{}, Projects: map[string][]domain.Permission{}}
	for _, b := range v.bindings {
		held := HeldRole{Role: b.Role.Name, Scope: b.Role.Scope, ProjectID: b.Binding.ProjectID}
		if b.Binding.SubjectKind == domain.SubjectTeam {
			held.TeamID = b.Binding.SubjectID
		}
		out.Roles = append(out.Roles, held)
	}
	all := domain.Permissions()
	for _, p := range all {
		if e.allows(v, by, p, Organization(by.AccountID)) {
			out.Permissions = append(out.Permissions, p)
		}
	}
	projects, err := e.st.Projects().List(ctx, by.AccountID)
	if err != nil {
		return Effective{}, fmt.Errorf("list projects: %w", err)
	}
	for _, project := range projects {
		var perms []domain.Permission
		for _, p := range all {
			if p.ProjectScoped() && e.allows(v, by, p, Project(by.AccountID, project.ID)) {
				perms = append(perms, p)
			}
		}
		if perms != nil {
			out.Projects[project.ID.String()] = perms
		}
	}
	return out, nil
}
