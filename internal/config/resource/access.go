package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// organizationName is the name of the one organization resource.
const organizationName = "organization"

type organizationSpec struct {
	Name        string          `json:"name"`
	Slug        string          `json:"slug"`
	MaxTokenTTL domain.Duration `json:"max_token_ttl,omitzero"`
}

func organizationArea() Area {
	a := base[organizationSpec](KindOrganization, "Organization", ScopeOrganization, domain.PermOrgRead)
	a.Setting, a.Hidden = domain.Account{}, []string{"id", "created_by", "created_at"}
	a.Affects = "Everyone in the organization sees the new name."
	a.Needs = needsOf(domain.PermOrgManage)
	a.NoDelete = "the organization is not deleted here"
	a.Read = organizationRead
	a.Validate = organizationValidate
	a.Apply = organizationApply
	a.Protection = organizationProtection
	return a
}

func organizationRead(ctx context.Context, e *Env) ([]Resource, error) {
	acc, err := e.Store.Accounts().Get(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	return []Resource{{Kind: KindOrganization, Name: organizationName, Spec: encode(organizationSpec{acc.Name, acc.Slug, acc.MaxTokenTTL})}}, nil
}

func organizationValidate(_ context.Context, _ *Env, old *Resource, want Resource) error {
	s, err := decode[organizationSpec](want.Spec)
	switch {
	case err != nil:
		return err
	case old == nil || want.Name != organizationName:
		return invalid("there is one organization resource, called %s, and it cannot be created", organizationName)
	case strings.TrimSpace(s.Name) == "":
		return invalid("name is required")
	case !domain.ValidSlug(s.Slug):
		return invalid("slug %q must be a lowercase slug", s.Slug)
	case s.MaxTokenTTL < 0:
		return invalid("max_token_ttl must be positive")
	}
	return nil
}

func organizationApply(ctx context.Context, e *Env, _, want *Resource) error {
	s, err := decode[organizationSpec](want.Spec)
	if err != nil {
		return err
	}
	if err := e.Store.Accounts().Rename(ctx, e.Actor.AccountID, s.Name, s.Slug); err != nil {
		return err
	}
	return e.Store.Accounts().SetMaxTokenTTL(ctx, e.Actor.AccountID, time.Duration(s.MaxTokenTTL))
}

func organizationProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if old == nil || want == nil {
		return nil
	}
	o, _ := decode[organizationSpec](old.Spec)
	w, _ := decode[organizationSpec](want.Spec)
	if o.MaxTokenTTL > 0 && w.MaxTokenTTL == 0 {
		return []string{"Removes the cap on how long tokens last"}
	}
	return nil
}

type memberSpec struct {
	// Status is active or suspended, or invited for someone who has not
	// joined yet: then the name is an email address.
	Status string `json:"status"`
	// Roles are the organization roles an invitation gives on acceptance.
	Roles []string `json:"roles,omitempty"`
}

const memberInvited = "invited"

func memberArea() Area {
	a := base[memberSpec](KindMember, "Member", ScopeOrganization, domain.PermOrgRead)
	a.Setting, a.Hidden = domain.Membership{}, []string{"id", "account_id", "user_id", "source", "external_id", "created_at", "updated_at"}
	a.Affects = "Takes effect at the person's next request."
	a.Read = readMembers
	a.Validate = memberValidate
	a.Needs = memberNeeds
	a.Apply = applyMember
	a.Protection = memberProtection
	a.Lost = memberLost
	return a
}

func memberValidate(_ context.Context, _ *Env, old *Resource, want Resource) error {
	s, err := decode[memberSpec](want.Spec)
	if err != nil {
		return err
	}
	switch {
	case old == nil && (s.Status != memberInvited || !strings.Contains(want.Name, "@")):
		return invalid("a new member is an invitation: status invited, and an email address as its name")
	case old != nil && s.Status != string(domain.MembershipActive) && s.Status != string(domain.MembershipSuspended) && s.Status != memberInvited:
		return invalid("status must be active or suspended")
	}
	return nil
}

func memberNeeds(ctx context.Context, e *Env, _, want *Resource, _ []domain.ConfigFieldChange) ([]Need, error) {
	out := need(domain.PermMemberManage)
	if want == nil {
		return out, nil
	}
	s, err := decode[memberSpec](want.Spec)
	if err != nil {
		return nil, err
	}
	for _, name := range s.Roles {
		r, err := e.Store.Roles().GetByName(ctx, e.Actor.AccountID, domain.RoleScopeOrganization, name)
		if err != nil {
			return nil, missing(err, "role "+name)
		}
		out = append(out, need(r.Permissions...)...)
	}
	return out, nil
}

func memberProtection(ctx context.Context, e *Env, old, want *Resource) []string {
	if old == nil || (want != nil && !strings.Contains(string(want.Spec), string(domain.MembershipSuspended))) {
		return nil
	}
	id, err := uuid.Parse(old.Name)
	if err == nil && holdsOwner(ctx, e, id) {
		return []string{"Removes an Owner"}
	}
	return nil
}

func memberLost(old, want *Resource) string {
	switch {
	case want == nil && old != nil:
		return "Removing a member also drops their team places and roles; those do not come back."
	case old == nil:
		return "An invitation already accepted cannot be taken back."
	}
	return ""
}

func holdsOwner(ctx context.Context, e *Env, userID uuid.UUID) bool {
	bound, err := e.Store.RoleBindings().Effective(ctx, e.Actor.AccountID, domain.SubjectUser, userID)
	return err == nil && slices.ContainsFunc(bound, func(b store.BoundRole) bool {
		return b.Role.Name == domain.RoleOwner && b.Role.Scope == domain.RoleScopeOrganization
	})
}

func readMembers(ctx context.Context, e *Env) ([]Resource, error) {
	members, err := e.Store.Memberships().List(ctx, e.Actor.AccountID, store.MembershipFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(members))
	for _, m := range members {
		out = append(out, Resource{Kind: KindMember, Name: m.UserID.String(), Spec: encode(memberSpec{Status: string(m.Status)})})
	}
	invitations, err := e.Store.Invitations().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, i := range invitations {
		if i.Pending(now) && i.Email != "" {
			var names []string
			for _, g := range i.Grants {
				if r, err := e.Store.Roles().Get(ctx, e.Actor.AccountID, g.RoleID); err == nil {
					names = append(names, r.Name)
				}
			}
			out = append(out, Resource{Kind: KindMember, Name: i.Email, Spec: encode(memberSpec{Status: memberInvited, Roles: names})})
		}
	}
	return out, nil
}

func applyMember(ctx context.Context, e *Env, old, want *Resource) error {
	if old == nil {
		return invite(ctx, e, *want)
	}
	userID, err := uuid.Parse(old.Name)
	if err != nil { // an invitation
		return revokeInvitation(ctx, e, old.Name)
	}
	if err := e.Access.CheckHolder(ctx, e.Actor, domain.SubjectUser, userID); err != nil {
		return err
	}
	if want == nil {
		return e.Store.Memberships().Remove(ctx, e.Actor.AccountID, userID)
	}
	s, err := decode[memberSpec](want.Spec)
	if err != nil {
		return err
	}
	return e.Store.Memberships().SetStatus(ctx, e.Actor.AccountID, userID, domain.MembershipStatus(s.Status))
}

func invite(ctx context.Context, e *Env, want Resource) error {
	s, err := decode[memberSpec](want.Spec)
	if err != nil {
		return err
	}
	in := mcp.CreateInvitationInput{Email: want.Name, Roles: []mcp.RoleGrant{}}
	for _, name := range s.Roles {
		in.Roles = append(in.Roles, mcp.RoleGrant{Role: name})
	}
	out, err := e.Access.CreateInvitation(ctx, e.Actor, in)
	if err != nil {
		return err
	}
	e.shown = append(e.shown, ShowOnce{Label: "Invitation for " + want.Name, Value: out.URL})
	return nil
}

func revokeInvitation(ctx context.Context, e *Env, email string) error {
	all, err := e.Store.Invitations().List(ctx, e.Actor.AccountID)
	if err != nil {
		return err
	}
	for _, i := range all {
		if i.Email == email && i.Pending(time.Now()) {
			_, err := e.Access.RevokeInvitation(ctx, e.Actor, mcp.InvitationRefInput{Invitation: i.ID.String()})
			return err
		}
	}
	return invalid("no pending invitation for %s", email)
}

type domainSpec struct {
	EnforceSSO bool   `json:"enforce_sso"`
	Connection string `json:"connection,omitempty"` // the connection it enforces
}

func domainArea() Area {
	a := base[domainSpec](KindDomain, "Email domain", ScopeOrganization, domain.PermConnectionManage)
	a.Setting = domain.VerifiedDomain{}
	a.Hidden = []string{"id", "account_id", "domain", "verification_token", "verified_at", "connection_id", "created_at", "updated_at"}
	a.Affects = "Everyone with an address at this domain signs in through the connection."
	a.Needs = needsOf(domain.PermConnectionManage)
	a.Read = domainRead
	a.Validate = domainValidate
	a.Apply = applyDomain
	a.Protection = domainProtection
	return a
}

func domainRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.VerifiedDomains().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	conns, err := e.Store.Connections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, d := range rows {
		s := domainSpec{EnforceSSO: d.EnforceSSO}
		if i := slices.IndexFunc(conns, func(c domain.Connection) bool { return c.ID == d.ConnectionID }); i >= 0 {
			s.Connection = conns[i].Name
		}
		out = append(out, Resource{Kind: KindDomain, Name: d.Domain, Spec: encode(s)})
	}
	return out, nil
}

func domainValidate(_ context.Context, _ *Env, _ *Resource, want Resource) error {
	s, err := decode[domainSpec](want.Spec)
	if err != nil {
		return err
	}
	if s.EnforceSSO && s.Connection == "" {
		return invalid("enforcing sign-in needs a connection")
	}
	return nil
}

func domainProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if old == nil {
		return nil
	}
	o, _ := decode[domainSpec](old.Spec)
	if !o.EnforceSSO {
		return nil
	}
	if want != nil {
		if w, _ := decode[domainSpec](want.Spec); w.EnforceSSO {
			return nil
		}
	}
	return []string{"Turns off sign-in enforcement for " + old.Name}
}

func applyDomain(ctx context.Context, e *Env, old, want *Resource) error {
	acc := e.Actor.AccountID
	var row domain.VerifiedDomain
	if old != nil {
		rows, err := e.Store.VerifiedDomains().List(ctx, acc)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(rows, func(d domain.VerifiedDomain) bool { return d.Domain == old.Name })
		if i < 0 {
			return invalid("domain %s does not exist", old.Name)
		}
		row = rows[i]
	}
	if want == nil {
		return e.Store.VerifiedDomains().Delete(ctx, acc, row.ID)
	}
	s, err := decode[domainSpec](want.Spec)
	if err != nil {
		return err
	}
	if old == nil {
		row = domain.VerifiedDomain{AccountID: acc, Domain: want.Name, VerificationToken: domain.NewID().String()}
		if err := e.Store.VerifiedDomains().Create(ctx, &row); err != nil {
			return err
		}
	}
	var connection uuid.UUID
	if s.Connection != "" {
		conns, err := e.Store.Connections().List(ctx, acc)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(conns, func(c domain.Connection) bool { return c.Name == s.Connection })
		if i < 0 {
			return invalid("connection %s does not exist", s.Connection)
		}
		connection = conns[i].ID
	}
	return e.Store.VerifiedDomains().SetEnforcement(ctx, acc, row.ID, s.EnforceSSO, connection)
}

type teamSpec struct {
	DisplayName string   `json:"display_name"`
	Members     []string `json:"members"` // user IDs, sorted
}

func teamArea() Area {
	a := base[teamSpec](KindTeam, "Team", ScopeOrganization, domain.PermOrgRead)
	a.Setting = domain.Team{}
	a.Hidden = []string{"id", "account_id", "slug", "source", "external_id", "created_at", "updated_at"}
	a.Affects = "Everyone in the team gets the team's roles."
	a.Needs = needsOf(domain.PermTeamManage)
	a.Read = teamRead
	norm := a.Normalize
	a.Normalize = func(raw json.RawMessage) (json.RawMessage, error) {
		s, err := decode[teamSpec](raw)
		if err != nil {
			return norm(raw)
		}
		if s.Members == nil {
			s.Members = []string{}
		}
		slices.Sort(s.Members)
		return encode(s), nil
	}
	a.Validate = teamValidate
	a.Apply = applyTeam
	a.Lost = teamLost
	return a
}

func teamRead(ctx context.Context, e *Env) ([]Resource, error) {
	teams, err := e.Store.Teams().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(teams))
	for _, t := range teams {
		members, err := e.Store.Teams().Members(ctx, e.Actor.AccountID, t.ID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.UserID.String())
		}
		slices.Sort(ids)
		out = append(out, Resource{Kind: KindTeam, Name: t.Slug, Spec: encode(teamSpec{t.Name, ids})})
	}
	return out, nil
}

func teamValidate(_ context.Context, _ *Env, _ *Resource, want Resource) error {
	s, err := decode[teamSpec](want.Spec)
	if err != nil {
		return err
	}
	if !domain.ValidSlug(want.Name) {
		return invalid("team name %q must be a lowercase slug", want.Name)
	}
	if strings.TrimSpace(s.DisplayName) == "" {
		return invalid("display_name is required")
	}
	for _, m := range s.Members {
		if _, err := uuid.Parse(m); err != nil {
			return invalid("members: %q is not a user ID", m)
		}
	}
	return nil
}

func teamLost(old, want *Resource) string {
	if want == nil && old != nil {
		return "Deleting a team drops its roles; a revert brings the team and its members back, not its roles."
	}
	return ""
}

func applyTeam(ctx context.Context, e *Env, old, want *Resource) error {
	if want == nil {
		_, err := e.Access.DeleteTeam(ctx, e.Actor, mcp.TeamRefInput{Team: old.Name})
		return err
	}
	s, err := decode[teamSpec](want.Spec)
	if err != nil {
		return err
	}
	var before []string
	if old == nil {
		if _, err := e.Access.CreateTeam(ctx, e.Actor, mcp.CreateTeamInput{Name: s.DisplayName, Slug: want.Name}); err != nil {
			return err
		}
	} else {
		o, err := decode[teamSpec](old.Spec)
		if err != nil {
			return err
		}
		before = o.Members
		if o.DisplayName != s.DisplayName {
			if _, err := e.Access.UpdateTeam(ctx, e.Actor, mcp.UpdateTeamInput{Team: want.Name, Name: s.DisplayName}); err != nil {
				return err
			}
		}
	}
	return syncMembers(ctx, e, want.Name, before, s.Members)
}

func syncMembers(ctx context.Context, e *Env, team string, before, after []string) error {
	for _, m := range after {
		if !slices.Contains(before, m) {
			if _, err := e.Access.AddTeamMember(ctx, e.Actor, mcp.TeamMemberInput{Team: team, User: m}); err != nil {
				return err
			}
		}
	}
	for _, m := range before {
		if !slices.Contains(after, m) {
			if _, err := e.Access.RemoveTeamMember(ctx, e.Actor, mcp.TeamMemberInput{Team: team, User: m}); err != nil {
				return err
			}
		}
	}
	return nil
}

type bindingSpec struct {
	SubjectKind domain.SubjectKind `json:"subject_kind"`
	Subject     string             `json:"subject"` // user ID, team slug or service account name
	Role        string             `json:"role"`
	Project     string             `json:"project,omitempty"`
}

// name is the only name a binding can have: it is what the binding is.
func (s bindingSpec) name() string {
	n := string(s.SubjectKind) + ":" + s.Subject + "/" + s.Role
	if s.Project != "" {
		n += "@" + s.Project
	}
	return n
}

func roleBindingArea() Area {
	a := base[bindingSpec](KindRoleBinding, "Role", ScopeOrganization, domain.PermOrgRead)
	a.Setting = domain.RoleBinding{}
	a.Hidden = []string{"id", "account_id", "subject_id", "role_id", "project_id", "created_by", "created_at"}
	a.Affects = "Takes effect at the person's next request."
	a.Read = readBindings
	a.Validate = roleBindingValidate
	// The tool checks the rule of No escalation against the real binding.
	a.Needs = roleBindingNeeds
	a.Apply = roleBindingApply
	a.Protection = roleBindingProtection
	return a
}

func roleBindingValidate(_ context.Context, _ *Env, old *Resource, want Resource) error {
	s, err := decode[bindingSpec](want.Spec)
	if err != nil {
		return err
	}
	if want.Name != s.name() {
		return invalid("the name of this binding must be %s", s.name())
	}
	if old != nil {
		return invalid("a role binding does not change: remove it and add another")
	}
	return nil
}

func roleBindingNeeds(_ context.Context, _ *Env, old, want *Resource, _ []domain.ConfigFieldChange) ([]Need, error) {
	r := want
	if r == nil {
		r = old
	}
	s, err := decode[bindingSpec](r.Spec)
	if err != nil {
		return nil, err
	}
	switch s.SubjectKind {
	case domain.SubjectTeam:
		return need(domain.PermTeamManage), nil
	case domain.SubjectServiceAccount:
		return need(domain.PermServiceAccountManage), nil
	}
	return need(domain.PermMemberManage), nil
}

func roleBindingApply(ctx context.Context, e *Env, old, want *Resource) error {
	if want != nil {
		s, err := decode[bindingSpec](want.Spec)
		if err != nil {
			return err
		}
		_, err = e.Access.CreateRoleBinding(ctx, e.Actor, mcp.CreateRoleBindingInput{SubjectKind: s.SubjectKind, Subject: s.Subject, Role: s.Role, Project: s.Project})
		return err
	}
	id, err := findBinding(ctx, e, old.Name)
	if err != nil {
		return err
	}
	_, err = e.Access.DeleteRoleBinding(ctx, e.Actor, mcp.RoleBindingRefInput{Binding: id.String()})
	return err
}

func roleBindingProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if want != nil || old == nil {
		return nil
	}
	if s, _ := decode[bindingSpec](old.Spec); s.Role == domain.RoleOwner && s.Project == "" {
		return []string{"Removes an Owner"}
	}
	return nil
}

// bindings lists the organization's bindings with their spec.
func bindings(ctx context.Context, e *Env) (map[string]domain.RoleBinding, []Resource, error) {
	acc := e.Actor.AccountID
	rows, err := e.Store.RoleBindings().List(ctx, acc, store.RoleBindingFilter{})
	if err != nil {
		return nil, nil, err
	}
	teams, err := e.Store.Teams().List(ctx, acc)
	if err != nil {
		return nil, nil, err
	}
	accounts, err := e.Store.ServiceAccounts().List(ctx, acc)
	if err != nil {
		return nil, nil, err
	}
	projects, err := e.Store.Projects().List(ctx, acc)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]domain.RoleBinding{}
	out := make([]Resource, 0, len(rows))
	for _, b := range rows {
		r, err := e.Store.Roles().Get(ctx, acc, b.RoleID)
		if err != nil {
			return nil, nil, fmt.Errorf("role of binding %s: %w", b.ID, err)
		}
		s := bindingSpec{SubjectKind: b.SubjectKind, Subject: b.SubjectID.String(), Role: r.Name}
		switch b.SubjectKind {
		case domain.SubjectTeam:
			if i := slices.IndexFunc(teams, func(t domain.Team) bool { return t.ID == b.SubjectID }); i >= 0 {
				s.Subject = teams[i].Slug
			}
		case domain.SubjectServiceAccount:
			if i := slices.IndexFunc(accounts, func(a domain.ServiceAccount) bool { return a.ID == b.SubjectID }); i >= 0 {
				s.Subject = accounts[i].Name
			}
		}
		if i := slices.IndexFunc(projects, func(p domain.Project) bool { return p.ID == b.ProjectID }); i >= 0 {
			s.Project = projects[i].Name
		}
		byID[s.name()] = b
		out = append(out, Resource{Kind: KindRoleBinding, Name: s.name(), Spec: encode(s)})
	}
	return byID, out, nil
}

func readBindings(ctx context.Context, e *Env) ([]Resource, error) {
	_, out, err := bindings(ctx, e)
	return out, err
}

func findBinding(ctx context.Context, e *Env, name string) (uuid.UUID, error) {
	byName, _, err := bindings(ctx, e)
	if err != nil {
		return uuid.Nil(), err
	}
	b, ok := byName[name]
	if !ok {
		return uuid.Nil(), invalid("binding %s does not exist", name)
	}
	return b.ID, nil
}

type jitSpec struct {
	Enabled      bool     `json:"enabled"`
	DefaultRoles []string `json:"default_roles,omitempty"` // role names
}

type connectionSpec struct {
	ConnectionKind  domain.ConnectionKind `json:"kind"`
	Issuer          string                `json:"issuer,omitempty"`
	ClientID        string                `json:"client_id"`
	ClientSecretRef string                `json:"client_secret_ref"`
	JIT             jitSpec               `json:"jit"`
	GroupClaim      string                `json:"group_claim,omitempty"`
	Enabled         bool                  `json:"enabled"`
}

// connectionTrust are the fields that say who the connection believes.
var connectionTrust = []string{"issuer", "client_id", "group_claim"}

func connectionArea() Area {
	a := base[connectionSpec](KindConnection, "Sign-in connection", ScopeOrganization, domain.PermConnectionManage)
	a.Setting = domain.Connection{}
	a.Hidden = []string{"id", "account_id", "name", "created_at", "updated_at"}
	a.Affects = "People sign in through it from now on. Sessions already open are not touched."
	a.Read = connectionRead
	a.Validate = connectionValidate
	a.Needs = connectionNeeds
	a.Secrets = connectionSecrets
	a.Apply = connectionApply
	a.Retire = connectionRetire
	a.Lost = connectionLost
	return a
}

func connectionRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.Connections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, c := range rows {
		s := connectionSpec{c.Kind, c.Issuer, c.ClientID, c.ClientSecretRef, jitSpec{Enabled: c.JIT.Enabled}, c.GroupClaim, c.Enabled}
		for _, roleID := range c.JIT.DefaultRoles {
			r, err := e.Store.Roles().Get(ctx, e.Actor.AccountID, roleID)
			if err != nil {
				return nil, err
			}
			s.JIT.DefaultRoles = append(s.JIT.DefaultRoles, r.Name)
		}
		out = append(out, Resource{Kind: KindConnection, Name: c.Name, Spec: encode(s)})
	}
	return out, nil
}

func connectionValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	c, err := connectionRow(ctx, e, want)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	return nil
}

func connectionSecrets(want Resource) []SecretField {
	s, _ := decode[connectionSpec](want.Spec)
	return []SecretField{{Ref: s.ClientSecretRef, Title: "Client secret of " + want.Name}}
}

func connectionApply(ctx context.Context, e *Env, old, want *Resource) error {
	c, err := connectionRow(ctx, e, *want)
	if err != nil {
		return err
	}
	if old == nil {
		return e.Store.Connections().Create(ctx, &c)
	}
	rows, err := e.Store.Connections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(x domain.Connection) bool { return x.Name == old.Name })
	if i < 0 {
		return invalid("connection %s does not exist", old.Name)
	}
	c.ID = rows[i].ID
	return e.Store.Connections().Update(ctx, &c)
}

func connectionRetire(spec json.RawMessage) json.RawMessage {
	s, _ := decode[connectionSpec](spec)
	s.Enabled = false
	return encode(s)
}

func connectionLost(old, want *Resource) string {
	if old != nil && want != nil && !touches(diffSpecs(old, want), "client_secret_ref") {
		return ""
	}
	return "A replaced client secret does not come back."
}

// connectionRow turns a spec into the domain row of the actor's organization.
func connectionRow(ctx context.Context, e *Env, want Resource) (domain.Connection, error) {
	s, err := decode[connectionSpec](want.Spec)
	if err != nil {
		return domain.Connection{}, err
	}
	c := domain.Connection{AccountID: e.Actor.AccountID, Kind: s.ConnectionKind, Name: want.Name, Issuer: s.Issuer, ClientID: s.ClientID,
		ClientSecretRef: s.ClientSecretRef, JIT: domain.JIT{Enabled: s.JIT.Enabled, DefaultRoles: []uuid.UUID{}}, GroupClaim: s.GroupClaim, Enabled: s.Enabled}
	for _, name := range s.JIT.DefaultRoles {
		r, err := e.Store.Roles().GetByName(ctx, e.Actor.AccountID, domain.RoleScopeOrganization, name)
		if err != nil {
			return c, missing(err, "role "+name)
		}
		c.JIT.DefaultRoles = append(c.JIT.DefaultRoles, r.ID)
	}
	return c, nil
}

// connectionNeeds is permissions.md, No escalation: changing what a
// connection trusts, or the roles it gives, takes every permission of those
// roles.
func connectionNeeds(ctx context.Context, e *Env, old, want *Resource, fields []domain.ConfigFieldChange) ([]Need, error) {
	out := need(domain.PermConnectionManage)
	if want == nil || (old != nil && !touches(fields, append(connectionTrust, "jit")...)) {
		return out, nil
	}
	s, err := decode[connectionSpec](want.Spec)
	if err != nil {
		return nil, err
	}
	for _, name := range s.JIT.DefaultRoles {
		r, err := e.Store.Roles().GetByName(ctx, e.Actor.AccountID, domain.RoleScopeOrganization, name)
		if err != nil {
			return nil, missing(err, "role "+name)
		}
		out = append(out, need(r.Permissions...)...)
	}
	return out, nil
}
