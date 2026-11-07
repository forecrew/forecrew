package testkit

import (
	"crypto/sha256"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// IdentityStoreSuite checks the contract of the sign-in repositories on st,
// the fake and the Postgres store alike. account is an organization without
// members.
func IdentityStoreSuite(t *testing.T, st store.Store, account uuid.UUID) {
	t.Helper()
	s := &identitySuite{t: t, st: st, account: account}
	s.users()
	s.memberships()
	s.identities()
	s.invitations()
	s.connections()
	s.domains()
	s.sessions()
	s.tokens()
	s.oauth()
	s.lastOwner()
}

type identitySuite struct {
	t       *testing.T
	st      store.Store
	account uuid.UUID
	n       int
}

// oauth checks clients, the single use of grants, families and the sweep.
func (s *identitySuite) oauth() {
	client := s.oauthClient()
	expired := s.oauthGrants(client)
	s.oauthMembership(client)
	s.oauthSweep(client, expired)
}

func (s *identitySuite) oauthClient() domain.OAuthClient {
	ctx := s.t.Context()
	r := s.st.OAuth()
	s.n++
	before, err := r.CountUnusedClients(ctx)
	s.must(err)
	client := domain.OAuthClient{ClientID: "client-" + strconv.Itoa(s.n), Name: "Tool", RedirectURIs: []string{"https://tool.example/cb"}}
	s.must(r.CreateClient(ctx, &client))
	if n, err := r.CountUnusedClients(ctx); err != nil || n != before+1 {
		s.t.Fatalf("CountUnusedClients = %d, %v; want %d", n, err, before+1)
	}
	s.wantErr(r.CreateClient(ctx, &domain.OAuthClient{ClientID: client.ClientID, Name: "Twin", RedirectURIs: client.RedirectURIs}), store.ErrConflict)
	_, err = r.GetClient(ctx, "nobody")
	s.wantErr(err, store.ErrNotFound)
	client.Name, client.RedirectURIs = "Renamed", []string{"https://tool.example/other"}
	s.must(r.UpdateClient(ctx, &client))
	s.must(r.TouchClient(ctx, client.ID))
	got, err := r.GetClient(ctx, client.ClientID)
	s.must(err)
	byID, err := r.GetClientByID(ctx, client.ID)
	s.must(err)
	if got.Name != "Renamed" || len(got.RedirectURIs) != 1 || got.LastUsedAt.IsZero() || byID.ClientID != client.ClientID {
		s.t.Fatalf("client = %+v, by ID %+v", got, byID)
	}
	if n, err := r.CountUnusedClients(ctx); err != nil || n != before {
		s.t.Fatalf("a used client counts as unused: %d, %v", n, err)
	}
	return client
}

// oauthGrants returns the ID of an expired grant it leaves behind.
func (s *identitySuite) oauthGrants(client domain.OAuthClient) uuid.UUID {
	ctx := s.t.Context()
	r := s.st.OAuth()
	person := s.user("oauth@example.com", true)
	s.member(person)
	u := person.ID
	family := domain.NewID()
	hash := func(v string) []byte { h := sha256.Sum256([]byte(v + strconv.Itoa(s.n))); return h[:] }
	grant := func(kind domain.OAuthGrantKind, token string, family uuid.UUID, ttl time.Duration) domain.OAuthGrant {
		g := domain.OAuthGrant{Kind: kind, TokenHash: hash(token), FamilyID: family, ClientID: client.ID, UserID: u, AccountID: s.account,
			Scopes: []domain.Scope{domain.ScopeRead}, Resource: "https://f.example/mcp", ExpiresAt: time.Now().Add(ttl)}
		if kind == domain.OAuthCode {
			g.RedirectURI, g.CodeChallenge = "https://tool.example/cb", "challenge"
		}
		s.must(r.CreateGrant(ctx, &g))
		return g
	}
	if ok, err := r.UserAuthorized(ctx, client.ID, u); err != nil || ok {
		s.t.Fatalf("UserAuthorized before = %v, %v", ok, err)
	}
	code := grant(domain.OAuthCode, "code", family, time.Minute)
	if ok, err := r.UserAuthorized(ctx, client.ID, u); err != nil || !ok {
		s.t.Fatalf("UserAuthorized after = %v, %v", ok, err)
	}
	if apps, err := r.ListFamilies(ctx, s.account, u); err != nil || len(apps) != 0 {
		s.t.Fatalf("a code is no connected app: %+v, %v", apps, err)
	}
	s.must(r.UseGrant(ctx, code.ID))
	s.wantErr(r.UseGrant(ctx, code.ID), store.ErrConflict)
	stored, err := r.GetGrantByTokenHash(ctx, hash("code"))
	s.must(err)
	if stored.Usable(time.Now()) || stored.UsedAt.IsZero() || stored.CodeChallenge != "challenge" || stored.RedirectURI == "" {
		s.t.Fatalf("code = %+v", stored)
	}

	access := grant(domain.OAuthAccess, "access", family, time.Hour)
	s.wantErr(r.UseGrant(ctx, access.ID), store.ErrConflict)
	refresh := grant(domain.OAuthRefresh, "refresh", family, time.Hour)
	other := domain.NewID()
	grant(domain.OAuthRefresh, "other", other, time.Hour)
	expired := grant(domain.OAuthRefresh, "expired", domain.NewID(), -time.Hour)
	apps, err := r.ListFamilies(ctx, s.account, u)
	s.must(err)
	if len(apps) != 2 || apps[0].FamilyID != other || apps[1].FamilyID != family {
		s.t.Fatalf("families = %+v", apps)
	}

	s.must(r.RevokeFamily(ctx, family))
	for _, id := range []uuid.UUID{access.ID, refresh.ID} {
		g, err := r.GetGrant(ctx, id)
		s.must(err)
		if g.RevokedAt.IsZero() || g.Usable(time.Now()) {
			s.t.Fatalf("grant of a revoked family = %+v", g)
		}
	}
	s.must(r.RevokeByUser(ctx, domain.NewID(), u))
	if apps, _ := r.ListFamilies(ctx, s.account, u); len(apps) != 1 {
		s.t.Fatalf("another organization's revocation reached this one: %+v", apps)
	}
	s.must(r.RevokeByUser(ctx, s.account, u))
	if apps, _ := r.ListFamilies(ctx, s.account, u); len(apps) != 0 {
		s.t.Fatalf("families after RevokeByUser = %+v", apps)
	}
	again := grant(domain.OAuthRefresh, "again", domain.NewID(), time.Hour)
	s.must(r.RevokeByUser(ctx, uuid.Nil(), u))
	if g, err := r.GetGrant(ctx, again.ID); err != nil || g.RevokedAt.IsZero() {
		s.t.Fatalf("RevokeByUser everywhere = %+v, %v", g, err)
	}
	return expired.ID
}

// oauthMembership checks that leaving an organization, even for a while,
// ends the grants there and nowhere else.
func (s *identitySuite) oauthMembership(client domain.OAuthClient) {
	ctx := s.t.Context()
	r := s.st.OAuth()
	person := s.user("leaver@example.com", true)
	s.member(person)
	grant := func() domain.OAuthGrant {
		s.n++
		h := sha256.Sum256([]byte("leaver" + strconv.Itoa(s.n)))
		g := domain.OAuthGrant{Kind: domain.OAuthRefresh, TokenHash: h[:], FamilyID: domain.NewID(), ClientID: client.ID, UserID: person.ID, AccountID: s.account,
			Scopes: []domain.Scope{domain.ScopeRead}, Resource: "https://f.example/mcp", ExpiresAt: time.Now().Add(time.Hour)}
		s.must(r.CreateGrant(ctx, &g))
		return g
	}
	revoked := func(g domain.OAuthGrant) bool {
		s.t.Helper()
		got, err := r.GetGrant(ctx, g.ID)
		s.must(err)
		return !got.RevokedAt.IsZero()
	}
	m := s.st.Memberships()
	g := grant()
	s.must(m.SetStatus(ctx, s.account, person.ID, domain.MembershipActive))
	if revoked(g) {
		s.t.Fatal("staying active revoked a grant")
	}
	s.must(m.SetStatus(ctx, s.account, person.ID, domain.MembershipSuspended))
	s.must(m.SetStatus(ctx, s.account, person.ID, domain.MembershipActive))
	if !revoked(g) {
		s.t.Fatal("a grant came back with the reinstated member")
	}
	g = grant()
	s.must(m.Remove(ctx, s.account, person.ID))
	if !revoked(g) {
		s.t.Fatal("removing the member kept the grant")
	}
}

func (s *identitySuite) oauthSweep(client domain.OAuthClient, expired uuid.UUID) {
	ctx := s.t.Context()
	r := s.st.OAuth()
	if n, err := r.DeleteUnusedClients(ctx, time.Now().Add(time.Hour)); err != nil || n != 0 {
		s.t.Fatalf("a client with grants was deleted: %d, %v", n, err)
	}
	n, err := r.DeleteExpiredGrants(ctx, time.Now())
	s.must(err)
	if n != 1 {
		s.t.Fatalf("DeleteExpiredGrants = %d", n)
	}
	_, err = r.GetGrant(ctx, expired)
	s.wantErr(err, store.ErrNotFound)
	if _, err := r.DeleteExpiredGrants(ctx, time.Now().Add(2*time.Hour)); err != nil {
		s.t.Fatal(err)
	}
	if n, err := r.DeleteUnusedClients(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		s.t.Fatalf("a client in use was deleted: %d, %v", n, err)
	}
	if n, err := r.DeleteUnusedClients(ctx, time.Now().Add(time.Hour)); err != nil || n < 1 {
		s.t.Fatalf("DeleteUnusedClients = %d, %v", n, err)
	}
	_, err = r.GetClient(ctx, client.ClientID)
	s.wantErr(err, store.ErrNotFound)
}

func (s *identitySuite) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *identitySuite) wantErr(err, want error) {
	s.t.Helper()
	if !errors.Is(err, want) {
		s.t.Fatalf("err = %v, want %v", err, want)
	}
}

func (s *identitySuite) hash() []byte {
	s.n++
	h := sha256.Sum256([]byte(strconv.Itoa(s.n)))
	return h[:]
}

func (s *identitySuite) user(email string, verified bool) domain.User {
	s.t.Helper()
	u := domain.User{Name: email, Email: email, EmailVerified: verified}
	s.must(s.st.Users().Create(s.t.Context(), &u))
	return u
}

func (s *identitySuite) member(u domain.User) {
	s.t.Helper()
	m := domain.Membership{AccountID: s.account, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}
	s.must(s.st.Memberships().Create(s.t.Context(), &m))
}

func (s *identitySuite) connection(name string) domain.Connection {
	s.t.Helper()
	c := domain.Connection{Kind: domain.ConnectionGitHub, Name: name, ClientID: "id", ClientSecretRef: "env:X", Enabled: true}
	s.must(s.st.Connections().UpsertInstance(s.t.Context(), &c))
	return c
}

func (s *identitySuite) users() {
	ctx := s.t.Context()
	r := s.st.Users()
	a := s.user("Ana@Example.com", true)
	dup := domain.User{Email: "ana@example.COM", EmailVerified: true}
	s.wantErr(r.Create(ctx, &dup), store.ErrConflict)
	b := s.user("ana@example.com", false)
	c := domain.User{Name: "no email"}
	s.must(r.Create(ctx, &c))

	got, err := r.GetByVerifiedEmail(ctx, "ANA@example.com")
	s.must(err)
	if got.ID != a.ID || !got.EmailVerified || got.CreatedAt.IsZero() {
		s.t.Fatalf("by email = %+v", got)
	}
	_, err = r.GetByVerifiedEmail(ctx, "nobody@example.com")
	s.wantErr(err, store.ErrNotFound)
	both, err := r.ListByEmail(ctx, "ana@EXAMPLE.com")
	s.must(err)
	if len(both) != 2 {
		s.t.Fatalf("list by email = %+v", both)
	}
	byID, err := r.ListByIDs(ctx, []uuid.UUID{a.ID, c.ID, domain.NewID()})
	s.must(err)
	if len(byID) != 2 {
		s.t.Fatalf("list by IDs = %+v", byID)
	}
	b.Name, b.InstanceAdmin = "Bea", true
	s.must(r.Update(ctx, &b))
	s.must(r.Seen(ctx, b.ID))
	got, err = r.Get(ctx, b.ID)
	s.must(err)
	if got.Name != "Bea" || !got.InstanceAdmin || got.LastSeenAt.IsZero() {
		s.t.Fatalf("updated = %+v", got)
	}
	admins, err := r.ListInstanceAdmins(ctx)
	s.must(err)
	if len(admins) != 1 || admins[0].ID != b.ID {
		s.t.Fatalf("instance admins = %+v", admins)
	}
	s.must(r.SetDisabled(ctx, b.ID, true))
	if got, _ = r.Get(ctx, b.ID); got.DisabledAt.IsZero() {
		s.t.Fatal("not disabled")
	}
	s.must(r.SetDisabled(ctx, b.ID, false))
	if got, _ = r.Get(ctx, b.ID); !got.DisabledAt.IsZero() {
		s.t.Fatal("still disabled")
	}
	_, err = r.Get(ctx, domain.NewID())
	s.wantErr(err, store.ErrNotFound)
}

func (s *identitySuite) memberships() {
	ctx := s.t.Context()
	r := s.st.Memberships()
	u := s.user("member@example.com", true)
	s.member(u)
	dup := domain.Membership{AccountID: s.account, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}
	s.wantErr(r.Create(ctx, &dup), store.ErrConflict)
	s.must(r.SetExternalID(ctx, s.account, u.ID, "ext-1"))
	m, err := r.GetByExternalID(ctx, s.account, "ext-1")
	s.must(err)
	if m.UserID != u.ID {
		s.t.Fatalf("by external ID = %+v", m)
	}
	s.must(r.SetStatus(ctx, s.account, u.ID, domain.MembershipSuspended))
	active, err := r.List(ctx, s.account, store.MembershipFilter{Status: domain.MembershipSuspended})
	s.must(err)
	if len(active) != 1 || active[0].UserID != u.ID {
		s.t.Fatalf("suspended = %+v", active)
	}
	mine, err := r.ListByUser(ctx, u.ID)
	s.must(err)
	if len(mine) != 1 {
		s.t.Fatalf("by user = %+v", mine)
	}
	s.must(r.Remove(ctx, s.account, u.ID))
	_, err = r.Get(ctx, s.account, u.ID)
	s.wantErr(err, store.ErrNotFound)
	s.wantErr(r.Remove(ctx, s.account, u.ID), store.ErrNotFound)
}

func (s *identitySuite) identities() {
	ctx := s.t.Context()
	r := s.st.Identities()
	u := s.user("identity@example.com", false)
	c := s.connection("identities")
	i := domain.Identity{UserID: u.ID, ConnectionID: c.ID, Subject: "42", Login: "Octo", Email: "a@b.c"}
	s.must(r.Create(ctx, &i))
	dup := domain.Identity{UserID: u.ID, ConnectionID: c.ID, Subject: "42"}
	s.wantErr(r.Create(ctx, &dup), store.ErrConflict)
	got, err := r.GetBySubject(ctx, c.ID, "42")
	s.must(err)
	if got.ID != i.ID || got.Login != "Octo" {
		s.t.Fatalf("by subject = %+v", got)
	}
	s.must(r.Seen(ctx, i.ID, "octo2", "x@y.z"))
	byLogin, err := r.ListByLogin(ctx, c.ID, "OCTO2")
	s.must(err)
	if len(byLogin) != 1 || byLogin[0].Email != "x@y.z" || byLogin[0].LastUsedAt.IsZero() {
		s.t.Fatalf("by login = %+v", byLogin)
	}
	mine, err := r.ListByUser(ctx, u.ID)
	s.must(err)
	if len(mine) != 1 {
		s.t.Fatalf("by user = %+v", mine)
	}
	s.must(r.Delete(ctx, u.ID, i.ID))
	s.wantErr(r.Delete(ctx, u.ID, i.ID), store.ErrNotFound)
}

func (s *identitySuite) invitations() {
	ctx := s.t.Context()
	r := s.st.Invitations()
	u := s.user("invited@example.com", false)
	role, err := s.st.Roles().GetByName(ctx, s.account, domain.RoleScopeOrganization, domain.RoleMember)
	s.must(err)
	inv := domain.Invitation{AccountID: s.account, Email: "invited@example.com", Grants: []domain.Grant{{RoleID: role.ID}}, TokenHash: s.hash(),
		ExpiresAt: time.Now().Add(time.Hour), InvitedBy: "user:someone"}
	s.must(r.Create(ctx, &inv))
	got, err := r.GetByTokenHash(ctx, inv.TokenHash)
	s.must(err)
	if got.ID != inv.ID || len(got.Grants) != 1 || got.Grants[0].RoleID != role.ID {
		s.t.Fatalf("by hash = %+v", got)
	}
	acc, err := r.Accept(ctx, s.account, inv.ID, u.ID)
	s.must(err)
	if acc.AcceptedBy != u.ID || acc.AcceptedAt.IsZero() {
		s.t.Fatalf("accepted = %+v", acc)
	}
	_, err = r.Accept(ctx, s.account, inv.ID, u.ID)
	s.wantErr(err, store.ErrConflict)
	_, err = r.Accept(ctx, s.account, domain.NewID(), u.ID)
	s.wantErr(err, store.ErrNotFound)

	login := domain.Invitation{AccountID: s.account, GitHubLogin: "octo", GitHubUserID: 583231, TokenHash: s.hash(), ExpiresAt: time.Now().Add(time.Hour), InvitedBy: "user:someone"}
	s.must(r.Create(ctx, &login))
	s.must(r.Revoke(ctx, s.account, login.ID))
	_, err = r.Accept(ctx, s.account, login.ID, u.ID)
	s.wantErr(err, store.ErrConflict)
	got, err = r.Get(ctx, s.account, login.ID)
	s.must(err)
	if got.RevokedAt.IsZero() || got.GitHubUserID != 583231 {
		s.t.Fatalf("revoked = %+v", got)
	}
	all, err := r.List(ctx, s.account)
	s.must(err)
	if len(all) != 2 || all[0].ID != login.ID {
		s.t.Fatalf("list = %+v", all)
	}
}

func (s *identitySuite) connections() {
	ctx := s.t.Context()
	r := s.st.Connections()
	inst := s.connection("github")
	again := domain.Connection{Kind: domain.ConnectionGitHub, Name: "github", ClientID: "new", ClientSecretRef: "env:Y", Enabled: true}
	s.must(r.UpsertInstance(ctx, &again))
	if again.ID != inst.ID {
		s.t.Fatalf("upsert made a new row: %s, %s", again.ID, inst.ID)
	}
	own := domain.Connection{AccountID: s.account, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://okta.example.com", ClientID: "id", ClientSecretRef: "env:FORECREW_CONN_OKTA", Enabled: true} // #nosec G101 -- names a secret
	s.must(r.Create(ctx, &own))
	dup := own
	dup.ID = uuid.Nil()
	s.wantErr(r.Create(ctx, &dup), store.ErrConflict)
	bad := own
	bad.ID, bad.Name, bad.ClientSecretRef = uuid.Nil(), "bad", "env:DATABASE_URL"
	s.wantErr(r.Create(ctx, &bad), domain.ErrInvalidRecord)
	got, err := r.Get(ctx, inst.ID)
	s.must(err)
	if got.ClientID != "new" || got.AccountID != uuid.Nil() {
		s.t.Fatalf("instance = %+v", got)
	}
	instance, err := r.List(ctx, uuid.Nil())
	s.must(err)
	if !slices.ContainsFunc(instance, func(c domain.Connection) bool { return c.ID == inst.ID }) || slices.ContainsFunc(instance, func(c domain.Connection) bool { return c.ID == own.ID }) {
		s.t.Fatalf("instance connections = %+v", instance)
	}
	mine, err := r.List(ctx, s.account)
	s.must(err)
	if len(mine) != 1 || mine[0].ID != own.ID {
		s.t.Fatalf("own connections = %+v", mine)
	}
	own.Enabled = false
	s.must(r.Update(ctx, &own))
	if got, _ = r.Get(ctx, own.ID); got.Enabled {
		s.t.Fatal("still enabled")
	}
	maps, err := r.GroupMappings(ctx, own.ID)
	s.must(err)
	if len(maps) != 0 {
		s.t.Fatalf("mappings = %+v", maps)
	}
}

func (s *identitySuite) domains() {
	ctx := s.t.Context()
	r := s.st.VerifiedDomains()
	c := s.connection("sso")
	d := domain.VerifiedDomain{AccountID: s.account, Domain: "acme.example", VerificationToken: "t"}
	s.must(r.Create(ctx, &d))
	_, err := r.GetVerified(ctx, "acme.example")
	s.wantErr(err, store.ErrNotFound)
	s.must(r.SetVerified(ctx, s.account, d.ID, true))
	s.must(r.SetEnforcement(ctx, s.account, d.ID, true, c.ID))
	got, err := r.GetVerified(ctx, "ACME.example")
	s.must(err)
	if got.ID != d.ID || !got.EnforceSSO || got.ConnectionID != c.ID || got.VerifiedAt.IsZero() {
		s.t.Fatalf("verified = %+v", got)
	}
	s.must(r.SetVerified(ctx, s.account, d.ID, false))
	if got, _ = r.Get(ctx, s.account, d.ID); got.EnforceSSO || !got.VerifiedAt.IsZero() {
		s.t.Fatalf("unverified = %+v", got)
	}
	all, err := r.List(ctx, s.account)
	s.must(err)
	if len(all) != 1 {
		s.t.Fatalf("list = %+v", all)
	}
	s.must(r.Delete(ctx, s.account, d.ID))
	s.wantErr(r.Delete(ctx, s.account, d.ID), store.ErrNotFound)
}

func (s *identitySuite) sessions() {
	ctx := s.t.Context()
	r := s.st.Sessions()
	u := s.user("session@example.com", true)
	s.member(u)
	now := time.Now().UTC()
	sess := domain.Session{TokenHash: s.hash(), UserID: u.ID, AccountID: s.account, IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour), UserAgent: "ua", IP: "192.0.2.1"}
	s.must(r.Create(ctx, &sess))
	got, err := r.GetByTokenHash(ctx, sess.TokenHash)
	s.must(err)
	if got.ID != sess.ID || got.IP != "192.0.2.1" || got.UserAgent != "ua" || got.AccountID != s.account || got.ConnectionID != uuid.Nil() {
		s.t.Fatalf("by hash = %+v", got)
	}
	s.must(r.Touch(ctx, sess.ID, now.Add(5*time.Hour)))
	if got, _ = r.GetByTokenHash(ctx, sess.TokenHash); !got.IdleExpiresAt.Equal(got.ExpiresAt) {
		s.t.Fatalf("idle expiry past the absolute one: %+v", got)
	}
	s.must(r.SetAccount(ctx, sess.ID, uuid.Nil()))
	if got, _ = r.GetByTokenHash(ctx, sess.TokenHash); got.AccountID != uuid.Nil() {
		s.t.Fatalf("account = %s", got.AccountID)
	}
	s.must(r.SetAccount(ctx, sess.ID, s.account))
	other := domain.Session{TokenHash: s.hash(), UserID: u.ID, IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(time.Hour)}
	s.must(r.Create(ctx, &other))
	s.must(r.RevokeByUser(ctx, u.ID, s.account))
	all, err := r.ListByUser(ctx, u.ID)
	s.must(err)
	if len(all) != 2 || all[0].RevokedAt.IsZero() || !all[1].RevokedAt.IsZero() {
		s.t.Fatalf("after revoking in the organization: %+v", all)
	}
	s.must(r.RevokeByUser(ctx, u.ID, uuid.Nil()))
	if all, _ = r.ListByUser(ctx, u.ID); all[1].RevokedAt.IsZero() {
		s.t.Fatal("not every session ended")
	}
	s.wantErr(r.Revoke(ctx, domain.NewID(), sess.ID), store.ErrNotFound)
	s.must(r.Revoke(ctx, u.ID, sess.ID))
	n, err := r.DeleteExpired(ctx, now.Add(3*time.Hour))
	s.must(err)
	if n != 2 {
		s.t.Fatalf("deleted %d sessions", n)
	}
}

func (s *identitySuite) tokens() {
	ctx := s.t.Context()
	r := s.st.MCPTokens()
	u := s.user("tokens@example.com", true)
	s.member(u)
	mine := domain.MCPToken{AccountID: s.account, Name: "laptop", TokenHash: s.hash(), OwnerKind: domain.SubjectUser, OwnerID: u.ID, Scopes: []domain.Scope{domain.ScopeRead}}
	s.must(r.Create(ctx, &mine))
	for range 2 {
		legacy := domain.MCPToken{AccountID: s.account, Name: "laptop", TokenHash: s.hash()}
		if s.n%2 == 0 {
			legacy.Name = "laptop-2"
		}
		s.must(r.Create(ctx, &legacy))
	}
	n, err := r.AssignOwner(ctx, s.account, domain.SubjectUser, u.ID)
	s.must(err)
	if n != 2 {
		s.t.Fatalf("assigned %d", n)
	}
	owned, err := r.ListByOwner(ctx, s.account, domain.SubjectUser, u.ID)
	s.must(err)
	names := []string{}
	for _, t := range owned {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"laptop", "laptop-2", "laptop-3"}) && !slices.Equal(names, []string{"laptop", "laptop-2", "laptop-2-2"}) {
		s.t.Fatalf("names = %v", names)
	}
	s.must(r.RevokeByOwner(ctx, s.account, domain.SubjectUser, u.ID))
	if owned, _ = r.ListByOwner(ctx, s.account, domain.SubjectUser, u.ID); owned[0].RevokedAt.IsZero() || owned[2].RevokedAt.IsZero() {
		s.t.Fatalf("not revoked: %+v", owned)
	}
}

// lastOwner goes last: the organization then has a guarded Owner.
func (s *identitySuite) lastOwner() {
	ctx := s.t.Context()
	u := s.user("owner@example.com", true)
	s.member(u)
	role, err := s.st.Roles().GetByName(ctx, s.account, domain.RoleScopeOrganization, domain.RoleOwner)
	s.must(err)
	if got, err := s.st.Roles().Get(ctx, s.account, role.ID); err != nil || got.Name != domain.RoleOwner {
		s.t.Fatalf("role = %+v, %v", got, err)
	}
	b := domain.RoleBinding{AccountID: s.account, SubjectKind: domain.SubjectUser, SubjectID: u.ID, RoleID: role.ID, CreatedBy: "user:x"}
	s.must(s.st.RoleBindings().Create(ctx, &b))
	dup := b
	dup.ID = uuid.Nil()
	s.wantErr(s.st.RoleBindings().Create(ctx, &dup), store.ErrConflict)
	eff, err := s.st.RoleBindings().Effective(ctx, s.account, domain.SubjectUser, u.ID)
	s.must(err)
	if len(eff) != 1 || !eff[0].Role.Has(domain.PermOrgManage) {
		s.t.Fatalf("effective = %+v", eff)
	}
	s.wantErr(s.st.Memberships().SetStatus(ctx, s.account, u.ID, domain.MembershipSuspended), store.ErrLastOwner)
	s.wantErr(s.st.Users().SetDisabled(ctx, u.ID, true), store.ErrLastOwner)
	s.wantErr(s.st.Memberships().Remove(ctx, s.account, u.ID), store.ErrLastOwner)
	s.wantErr(s.st.RoleBindings().Delete(ctx, s.account, b.ID), store.ErrLastOwner)
}
