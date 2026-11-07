package tools_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tools"
)

// named adds a member with a name and an email.
func (f *fixture) named(t *testing.T, name string, scope domain.RoleScope, role string, project uuid.UUID) (domain.Actor, uuid.UUID) {
	t.Helper()
	u := f.st.AddMember(f.account.ID, domain.User{Name: name, Email: name + "@acme.test"}, "")
	if role != "" {
		bind(t, f, u.ID, scope, role, project)
	}
	return domain.UserActor(f.account.ID, u.ID, domain.Credential{}), u.ID
}

func TestListMembers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, adminID := f.named(t, "ana", orgScope, domain.RoleAdmin, uuid.Nil())
	_, marta := f.named(t, "Marta", orgScope, domain.RoleMember, uuid.Nil())
	bind(t, f, marta, projectScope, domain.RoleMaintainer, f.project.ID)
	_, luca := f.named(t, "luca", "", "", uuid.Nil())
	f.st.SetMemberStatus(f.account.ID, luca, domain.MembershipSuspended)
	team, err := f.svc.CreateTeam(ctx, admin, mcp.CreateTeamInput{Name: "Mobile"})
	must(t, err)
	_, err = f.svc.AddTeamMember(ctx, admin, mcp.TeamMemberInput{Team: team.Slug, User: marta.String()})
	must(t, err)

	got, err := f.svc.ListMembers(ctx, admin)
	must(t, err)
	if len(got) != 3 || got[0].Name != "ana" || got[1].Name != "luca" || got[2].Name != "Marta" {
		t.Fatalf("members = %+v", got)
	}
	m := got[2]
	if m.Email != "Marta@acme.test" || m.Status != domain.MembershipActive || !slices.Equal(m.Teams, []string{"mobile"}) {
		t.Fatalf("Marta = %+v", m)
	}
	if want := []mcp.MemberRole{{Role: "member", Scope: orgScope}, {Role: "maintainer", Scope: projectScope, Project: "app"}}; !slices.Equal(m.Roles, want) {
		t.Fatalf("roles = %+v, want %+v", m.Roles, want)
	}
	if got[1].Status != domain.MembershipSuspended || got[1].Roles == nil || got[1].Teams == nil {
		t.Fatalf("luca = %+v", got[1])
	}

	// A Viewer sees names, not emails.
	viewer, _ := f.named(t, "vera", orgScope, domain.RoleViewer, uuid.Nil())
	got, err = f.svc.ListMembers(ctx, viewer)
	must(t, err)
	for _, m := range got {
		if m.Email != "" {
			t.Errorf("%s: email %q shown without member.manage", m.Name, m.Email)
		}
	}
	// A project role alone does not read the organization; a project the
	// caller cannot read is not named.
	contributor, _ := f.named(t, "carl", projectScope, domain.RoleContributor, f.project.ID)
	_, err = f.svc.ListMembers(ctx, contributor)
	wantErr(t, err, authz.ErrDenied)
	other := f.secondProject(t)
	bind(t, f, adminID, projectScope, domain.RoleViewer, other.ID)
	blind := f.st.AddMember(f.account.ID, domain.User{Name: "zed"}, "")
	bind(t, f, blind.ID, orgScope, domain.RoleBilling, uuid.Nil())
	got, err = f.svc.ListMembers(ctx, domain.UserActor(f.account.ID, blind.ID, domain.Credential{}))
	must(t, err)
	for _, m := range got {
		if m.Name == "ana" && slices.ContainsFunc(m.Roles, func(r mcp.MemberRole) bool { return r.Project == "second" }) {
			t.Errorf("a project the caller cannot read is named: %+v", m.Roles)
		}
	}
}

// bind gives a built-in role to a user.
func bind(t *testing.T, f *fixture, user uuid.UUID, scope domain.RoleScope, role string, project uuid.UUID) {
	t.Helper()
	b := domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectUser, SubjectID: user, ProjectID: project}
	r, err := f.st.Roles().GetByName(t.Context(), f.account.ID, scope, role)
	must(t, err)
	b.RoleID = r.ID
	must(t, f.st.RoleBindings().Create(t.Context(), &b))
}

// access gives a member a live session and a token.
func (f *fixture) access(t *testing.T, user uuid.UUID, label string) (domain.Session, domain.MCPToken) {
	t.Helper()
	s := domain.Session{TokenHash: []byte(label + user.String()), UserID: user, AccountID: f.account.ID, ExpiresAt: t0.Add(time.Hour), IdleExpiresAt: t0.Add(time.Hour)}
	must(t, f.st.Sessions().Create(t.Context(), &s))
	tok := domain.MCPToken{AccountID: f.account.ID, Name: label, TokenHash: []byte("tok" + label + user.String()), OwnerKind: domain.SubjectUser, OwnerID: user, Scopes: []domain.Scope{domain.ScopeRead}}
	must(t, f.st.MCPTokens().Create(t.Context(), &tok))
	return s, tok
}

// ended reports whether the session and the token of a member stopped working.
func (f *fixture) ended(t *testing.T, s domain.Session, tok domain.MCPToken) bool {
	t.Helper()
	got, err := f.st.Sessions().GetByTokenHash(t.Context(), s.TokenHash)
	must(t, err)
	tokens, err := f.st.MCPTokens().ListByOwner(t.Context(), f.account.ID, tok.OwnerKind, tok.OwnerID)
	must(t, err)
	i := slices.IndexFunc(tokens, func(x domain.MCPToken) bool { return x.Name == tok.Name })
	return !got.RevokedAt.IsZero() && i >= 0 && !tokens[i].RevokedAt.IsZero()
}

func TestSuspendAndRemoveMembers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, _ := f.named(t, "ana", orgScope, domain.RoleAdmin, uuid.Nil())
	owner, ownerID := f.named(t, "olga", orgScope, domain.RoleOwner, uuid.Nil())
	_, marta := f.named(t, "marta", orgScope, domain.RoleMember, uuid.Nil())
	sess, tok := f.access(t, marta, "first")

	// Suspending ends the sessions and tokens of the member, and is audited.
	m, err := f.svc.SetMemberSuspended(ctx, admin, mcp.SetMemberInput{User: marta.String(), Suspended: true})
	must(t, err)
	if m.Name != "marta" || m.Status != domain.MembershipSuspended || !f.ended(t, sess, tok) {
		t.Fatalf("suspended = %+v, ended %v", m, f.ended(t, sess, tok))
	}
	_, err = f.svc.SetMemberSuspended(ctx, admin, mcp.SetMemberInput{User: marta.String()})
	must(t, err)
	got, err := f.svc.ListMembers(ctx, admin)
	must(t, err)
	if i := slices.IndexFunc(got, func(m mcp.MemberInfo) bool { return m.UserID == marta }); got[i].Status != domain.MembershipActive {
		t.Fatalf("resumed = %+v", got[i])
	}
	_, err = f.svc.SetMemberSuspended(ctx, admin, mcp.SetMemberInput{User: marta.String()})
	wantErr(t, err, mcp.ErrConflict) // not suspended any more

	// Removing takes the roles and the access with them.
	sess2, tok2 := f.access(t, marta, "second")
	_, err = f.svc.RemoveMember(ctx, admin, mcp.MemberRefInput{User: marta.String()})
	must(t, err)
	if !f.ended(t, sess2, tok2) {
		t.Fatal("the access of a removed member goes on")
	}
	got, _ = f.svc.ListMembers(ctx, admin)
	if slices.ContainsFunc(got, func(m mcp.MemberInfo) bool { return m.UserID == marta }) {
		t.Fatalf("marta is still a member: %+v", got)
	}
	_, err = f.svc.RemoveMember(ctx, admin, mcp.MemberRefInput{User: marta.String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.SetMemberSuspended(ctx, admin, mcp.SetMemberInput{User: "nope", Suspended: true})
	wantErr(t, err, mcp.ErrInvalidInput)

	// No escalation: an Admin does not act on an Owner; an Owner does, but
	// the last Owner stays.
	_, err = f.svc.SetMemberSuspended(ctx, admin, mcp.SetMemberInput{User: ownerID.String(), Suspended: true})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.RemoveMember(ctx, admin, mcp.MemberRefInput{User: ownerID.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.SetMemberSuspended(ctx, owner, mcp.SetMemberInput{User: ownerID.String(), Suspended: true})
	wantErr(t, err, mcp.ErrConflict)
	wantErr(t, err, store.ErrLastOwner)
	_, err = f.svc.RemoveMember(ctx, owner, mcp.MemberRefInput{User: ownerID.String()})
	wantErr(t, err, store.ErrLastOwner)

	// Only member.manage.
	viewer, _ := f.named(t, "vera", orgScope, domain.RoleViewer, uuid.Nil())
	_, err = f.svc.SetMemberSuspended(ctx, viewer, mcp.SetMemberInput{User: ownerID.String(), Suspended: true})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.RemoveMember(ctx, viewer, mcp.MemberRefInput{User: ownerID.String()})
	wantErr(t, err, authz.ErrDenied)

	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{SubjectType: "user"})
	must(t, err)
	var actions []string
	for _, e := range slices.Backward(entries) {
		actions = append(actions, e.Action)
	}
	if want := []string{"member.suspended", "member.resumed", "member.removed"}; !slices.Equal(actions, want) {
		t.Errorf("audit = %v, want %v", actions, want)
	}
}

func TestUpdateOrganization(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	owner, _ := f.named(t, "olga", orgScope, domain.RoleOwner, uuid.Nil())
	admin, _ := f.named(t, "ana", orgScope, domain.RoleAdmin, uuid.Nil())
	must(t, f.st.Accounts().Create(ctx, &domain.Account{Name: "Other", Slug: "other"}))

	a, err := f.svc.UpdateOrganization(ctx, owner, mcp.UpdateOrganizationInput{Name: " Acme Inc ", Slug: "acme"})
	must(t, err)
	if a.Name != "Acme Inc" || a.Slug != "acme" {
		t.Fatalf("organization = %+v", a)
	}
	a, err = f.svc.UpdateOrganization(ctx, owner, mcp.UpdateOrganizationInput{Name: "Acme"})
	must(t, err)
	if a.Name != "Acme" || a.Slug != "acme" {
		t.Fatalf("name only = %+v", a)
	}
	_, err = f.svc.UpdateOrganization(ctx, owner, mcp.UpdateOrganizationInput{})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.UpdateOrganization(ctx, owner, mcp.UpdateOrganizationInput{Slug: "Bad Slug"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.UpdateOrganization(ctx, owner, mcp.UpdateOrganizationInput{Slug: "other"})
	wantErr(t, err, mcp.ErrConflict)
	_, err = f.svc.UpdateOrganization(ctx, admin, mcp.UpdateOrganizationInput{Name: "x"}) // Admin lacks org.manage
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.UpdateOrganization(ctx, domain.UserActor(domain.NewID(), domain.NewID(), domain.Credential{}), mcp.UpdateOrganizationInput{Name: "x"})
	wantErr(t, err, authz.ErrDenied)

	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{Action: "organization.updated"})
	must(t, err)
	if len(entries) != 2 || entries[0].SubjectID != f.account.ID {
		t.Errorf("audit = %+v", entries)
	}
}

func TestListAudit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	owner, ownerID := f.named(t, "olga", orgScope, domain.RoleOwner, uuid.Nil())
	admin, _ := f.named(t, "ana", orgScope, domain.RoleAdmin, uuid.Nil())
	viewer, _ := f.named(t, "vera", orgScope, domain.RoleViewer, uuid.Nil())
	_, err := f.svc.CreateTeam(ctx, owner, mcp.CreateTeamInput{Name: "Core"})
	must(t, err)
	f.clock.Advance(time.Hour)
	sa, err := f.svc.CreateServiceAccount(ctx, admin, mcp.CreateServiceAccountInput{Name: "ci"})
	must(t, err)
	must(t, f.st.Audit().Append(ctx, &domain.AuditEntry{AccountID: f.account.ID, Actor: "github:octo", Action: "gate.resolved", SubjectType: "gate"}))

	got, err := f.svc.ListAudit(ctx, admin, mcp.ListAuditInput{})
	must(t, err)
	if len(got) != 3 || got[0].Action != "gate.resolved" || got[0].ActorName != "" || got[1].ActorName != "ana" || got[2].ActorName != "olga" {
		t.Fatalf("entries = %+v", got)
	}
	since := t0.Add(30 * time.Minute).Format(time.RFC3339)
	for name, tt := range map[string]struct {
		in   mcp.ListAuditInput
		want int
	}{
		"actor":   {mcp.ListAuditInput{Actor: "user:" + ownerID.String()}, 1},
		"action":  {mcp.ListAuditInput{Action: "team.created"}, 1},
		"type":    {mcp.ListAuditInput{SubjectType: "service_account"}, 1},
		"subject": {mcp.ListAuditInput{Subject: sa.ID.String()}, 1},
		"since":   {mcp.ListAuditInput{Since: since}, 2},
		"until":   {mcp.ListAuditInput{Until: since}, 1},
		"limit":   {mcp.ListAuditInput{Limit: 1}, 1},
	} {
		if got, err := f.svc.ListAudit(ctx, admin, tt.in); err != nil || len(got) != tt.want {
			t.Errorf("%s: %d entries, %v, want %d", name, len(got), err, tt.want)
		}
	}
	for name, in := range map[string]mcp.ListAuditInput{"time": {Since: "yesterday"}, "until": {Until: "x"}, "subject": {Subject: "x"}, "limit": {Limit: 9999}} {
		_, err := f.svc.ListAudit(ctx, admin, in)
		wantErr(t, err, mcp.ErrInvalidInput)
		_ = name
	}
	_, err = f.svc.ListAudit(ctx, viewer, mcp.ListAuditInput{})
	wantErr(t, err, authz.ErrDenied)
}

func TestConnectionTools(t *testing.T) {
	f := newFixture(t, func(c *tools.Config) { c.PublicURL = "https://forecrew.example/" })
	ctx := t.Context()
	admin, _ := f.named(t, "ana", orgScope, domain.RoleAdmin, uuid.Nil())
	viewer, _ := f.named(t, "vera", orgScope, domain.RoleViewer, uuid.Nil())
	must(t, f.st.Connections().UpsertInstance(ctx, &domain.Connection{Kind: domain.ConnectionGitHub, Name: "github", ClientID: "secret-id", ClientSecretRef: "env:GITHUB_SECRET", Enabled: true}))
	in := mcp.CreateConnectionInput{Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://acme.okta.com", ClientID: "c1", ClientSecretRef: "env:FORECREW_CONN_OKTA"}

	c, err := f.svc.CreateConnection(ctx, admin, in)
	must(t, err)
	if c.Enabled || c.Tested || c.Instance || c.ClientSecretRef != in.ClientSecretRef ||
		c.RedirectURL != "https://forecrew.example/auth/"+c.ID.String()+"/callback" || c.TestURL != "https://forecrew.example/auth/"+c.ID.String()+"/login?test=1" {
		t.Fatalf("connection = %+v", c)
	}
	_, err = f.svc.CreateConnection(ctx, admin, in)
	wantErr(t, err, mcp.ErrConflict)
	for name, bad := range map[string]mcp.CreateConnectionInput{
		"secret value":      {Kind: in.Kind, Name: "x", Issuer: in.Issuer, ClientID: "c", ClientSecretRef: "hunter2"},
		"outside namespace": {Kind: in.Kind, Name: "x", Issuer: in.Issuer, ClientID: "c", ClientSecretRef: "env:DATABASE_URL"},
		"http issuer":       {Kind: in.Kind, Name: "x", Issuer: "http://acme.okta.com", ClientID: "c", ClientSecretRef: in.ClientSecretRef},
		"kind":              {Kind: "saml", Name: "x", ClientID: "c", ClientSecretRef: in.ClientSecretRef},
	} {
		if _, err := f.svc.CreateConnection(ctx, admin, bad); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Turning it on needs a test sign-in that worked.
	_, err = f.svc.SetConnectionEnabled(ctx, admin, mcp.SetConnectionInput{Connection: "okta", Enabled: true})
	wantErr(t, err, mcp.ErrConflict)
	must(t, f.st.Audit().Append(ctx, &domain.AuditEntry{AccountID: f.account.ID, Actor: admin.String(), Action: "connection.tested", SubjectType: "connection", SubjectID: c.ID}))
	on, err := f.svc.SetConnectionEnabled(ctx, admin, mcp.SetConnectionInput{Connection: c.ID.String(), Enabled: true})
	must(t, err)
	if !on.Enabled || !on.Tested {
		t.Fatalf("enabled = %+v", on)
	}
	off, err := f.svc.SetConnectionEnabled(ctx, admin, mcp.SetConnectionInput{Connection: "okta"})
	must(t, err)
	if off.Enabled {
		t.Fatalf("disabled = %+v", off)
	}

	list, err := f.svc.ListConnections(ctx, admin)
	must(t, err)
	if len(list) != 2 || !list[0].Instance || list[0].ClientID != "" || list[0].ClientSecretRef != "" || list[0].TestURL != "" || list[1].Name != "okta" || !list[1].Tested {
		t.Fatalf("list = %+v", list)
	}
	_, err = f.svc.SetConnectionEnabled(ctx, admin, mcp.SetConnectionInput{Connection: "github", Enabled: false})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.SetConnectionEnabled(ctx, admin, mcp.SetConnectionInput{Connection: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.SetConnectionEnabled(ctx, admin, mcp.SetConnectionInput{})
	wantErr(t, err, mcp.ErrInvalidInput)

	for name, call := range map[string]func() error{
		"list":   func() error { _, err := f.svc.ListConnections(ctx, viewer); return err },
		"create": func() error { _, err := f.svc.CreateConnection(ctx, viewer, in); return err },
		"set": func() error {
			_, err := f.svc.SetConnectionEnabled(ctx, viewer, mcp.SetConnectionInput{Connection: "okta"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// revokeFails is a store whose personal token revocation fails, inside and
// outside transactions.
type revokeFails struct{ store.Store }

type revokeFailsTokens struct{ store.PersonalTokenRepo }

func (revokeFailsTokens) RevokeByOwner(context.Context, uuid.UUID, domain.SubjectKind, uuid.UUID) error {
	return errors.New("the database went away")
}

func (s revokeFails) MCPTokens() store.PersonalTokenRepo { return revokeFailsTokens{s.Store.MCPTokens()} }

func (s revokeFails) WithTx(ctx context.Context, fn func(context.Context, store.Store) error) error {
	return s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error { return fn(ctx, revokeFails{tx}) })
}

// Suspending and removing are one transaction with ending the member's
// access: when that fails, the member is as before.
func TestSuspendAndRemoveRollBack(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, _ := f.named(t, "ana", orgScope, domain.RoleAdmin, uuid.Nil())
	_, marta := f.named(t, "marta", orgScope, domain.RoleMember, uuid.Nil())
	sess, tok := f.access(t, marta, "first")
	svc := tools.New(tools.Config{Store: revokeFails{f.st}})

	_, err := svc.SetMemberSuspended(ctx, admin, mcp.SetMemberInput{User: marta.String(), Suspended: true})
	if err == nil {
		t.Fatal("suspend went through")
	}
	_, err = svc.RemoveMember(ctx, admin, mcp.MemberRefInput{User: marta.String()})
	if err == nil {
		t.Fatal("remove went through")
	}
	members, err := f.svc.ListMembers(ctx, admin)
	must(t, err)
	i := slices.IndexFunc(members, func(m mcp.MemberInfo) bool { return m.UserID == marta })
	if i < 0 || members[i].Status != domain.MembershipActive || len(members[i].Roles) != 1 {
		t.Fatalf("marta = %+v", members)
	}
	if f.ended(t, sess, tok) {
		t.Error("the access ended although the change did not")
	}
	if entries, _ := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{SubjectType: "user"}); len(entries) != 0 {
		t.Errorf("audited a change that did not happen: %+v", entries)
	}
}
