package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// signIn resolves claims to a user, accepts the invitation or setup link, and
// creates a session; it returns the session token. Nothing is kept when it
// fails: a refused sign-in links no identity.
func (s *Service) signIn(ctx context.Context, conn domain.Connection, c Claims, f flow, userAgent, ip string) (string, error) {
	token, err := NewToken(PrefixSession)
	if err != nil {
		return "", err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		return s.signInTx(ctx, tx, conn, c, f, token, userAgent, ip)
	})
	if err == nil {
		s.afterSetup(ctx, f)
	}
	return token, err
}

// signInTx is the part of a sign-in that is all or nothing.
func (s *Service) signInTx(ctx context.Context, tx store.Store, conn domain.Connection, c Claims, f flow, token, userAgent, ip string) error {
	proven, err := emailProven(ctx, tx, conn, c)
	if err != nil {
		return err
	}
	inv, err := s.invitation(ctx, tx, conn, c, proven, f.Invite)
	if err != nil {
		return err
	}
	link, err := s.linkOfSignIn(ctx, tx, inv, f)
	if err != nil {
		return err
	}
	user, err := resolveUser(ctx, tx, conn, c, proven, inv != nil || link != nil || s.cfg.Policy.Provisions(conn))
	if err != nil {
		return err
	}
	if !user.DisabledAt.IsZero() {
		return refusal(reasonDisabled)
	}
	if inv != nil {
		if err := accept(ctx, tx, *inv, user.ID); err != nil {
			return err
		}
	}
	if err := claimSetupLink(ctx, tx, conn, link, user.ID); err != nil {
		return err
	}
	if user, err = s.syncAdmin(ctx, tx, user, conn, c, proven); err != nil {
		return err
	}
	// After the setup link: JIT would make a member, and a link is refused
	// once the organization has one.
	if err := s.cfg.Policy.Admit(ctx, tx, conn, c, user); err != nil {
		return err
	}
	account, err := organization(ctx, tx, user, conn, f.Org, invitedAccount(inv, link))
	if err != nil {
		return err
	}
	// Every way in, a setup link's too, meets SSO enforcement; a refusal
	// rolls the whole sign-in back, the spent link included.
	if account != uuid.Nil() {
		if err := enterThrough(ctx, tx, user, account, conn.ID); err != nil {
			return err
		}
	}
	if err := tx.Users().Seen(ctx, user.ID); err != nil {
		return err
	}
	return s.newSession(ctx, tx, token, user, account, conn.ID, userAgent, ip)
}

// invitedAccount is the organization an invitation or a setup link opens,
// zero for neither.
func invitedAccount(inv *domain.Invitation, link *store.SetupLink) uuid.UUID {
	switch {
	case inv != nil:
		return inv.AccountID
	case link != nil:
		return link.AccountID
	}
	return uuid.Nil()
}

// emailDomain returns the domain of an email address, lowercase.
func emailDomain(email string) string {
	return strings.ToLower(email[strings.LastIndexByte(email, '@')+1:])
}

// emailProven says whether the provider's word on the email counts: it must
// call the email verified, through an instance connection or an organization's
// connection for a domain that organization verified. Otherwise whoever
// manages the connection could claim anyone's email at their own issuer
// (docs/contracts/identity.md, Linking by email).
func emailProven(ctx context.Context, st store.Store, conn domain.Connection, c Claims) (bool, error) {
	if c.Email == "" || !c.EmailVerified {
		return false, nil
	}
	if conn.AccountID == uuid.Nil() {
		return true, nil
	}
	d, err := st.VerifiedDomains().GetVerified(ctx, emailDomain(c.Email))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && d.AccountID == conn.AccountID, err
}

// invitation returns the pending invitation of the link, nil without a
// link. The person must be whom it was sent to: the link alone is not enough.
func (s *Service) invitation(ctx context.Context, st store.Store, conn domain.Connection, c Claims, proven bool, token string) (*domain.Invitation, error) {
	if token == "" {
		return nil, nil
	}
	inv, err := st.Invitations().GetByTokenHash(ctx, mcp.HashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !inv.Pending(s.cfg.Now())) {
		return nil, refusal(reasonInvitation)
	}
	if err != nil {
		return nil, err
	}
	if !invited(inv, conn, c, proven) {
		return nil, refusal(reasonInvitationOther)
	}
	return &inv, nil
}

// invited matches an invitation with who signed in: by the GitHub user ID
// resolved when it was created, through an instance GitHub connection, or by
// an email that is proven or that the
// inviting organization's own connection calls verified.
func invited(inv domain.Invitation, conn domain.Connection, c Claims, proven bool) bool {
	if conn.AccountID != uuid.Nil() && conn.AccountID != inv.AccountID {
		return false // an organization's connection only opens that organization
	}
	if inv.GitHubLogin != "" {
		return conn.Kind == domain.ConnectionGitHub && conn.AccountID == uuid.Nil() && inv.GitHubUserID != 0 &&
			c.Subject == strconv.FormatInt(inv.GitHubUserID, 10)
	}
	own := c.EmailVerified && conn.AccountID == inv.AccountID
	return (proven || own) && strings.EqualFold(inv.Email, c.Email)
}

// resolveUser finds the user of a sign-in by identity, then by proven email
// (linking the identity), then creates one for an invitation, a setup link or
// a connection that provisions people (create). Anyone else is refused.
func resolveUser(ctx context.Context, tx store.Store, conn domain.Connection, c Claims, proven, create bool) (domain.User, error) {
	id, err := tx.Identities().GetBySubject(ctx, conn.ID, c.Subject)
	if err == nil {
		if err := tx.Identities().Seen(ctx, id.ID, c.Login, c.Email); err != nil {
			return domain.User{}, err
		}
		return tx.Users().Get(ctx, id.UserID)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return domain.User{}, err
	}
	var user domain.User
	if proven {
		// Only a verified row proves who a person is, and the unique index
		// on verified emails means at most one exists.
		users, err := tx.Users().ListByEmail(ctx, c.Email)
		if err != nil {
			return domain.User{}, err
		}
		if i := slices.IndexFunc(users, func(u domain.User) bool { return u.EmailVerified }); i >= 0 {
			user = users[i]
		}
	}
	switch {
	case user.ID != uuid.Nil():
	case !create:
		return domain.User{}, refusal(reasonNoMembership)
	default:
		user = domain.User{Name: displayName(c), Email: c.Email, EmailVerified: proven}
		if err := tx.Users().Create(ctx, &user); err != nil {
			return domain.User{}, err
		}
	}
	identity := domain.Identity{UserID: user.ID, ConnectionID: conn.ID, Subject: c.Subject, Login: c.Login, Email: c.Email}
	if err := tx.Identities().Create(ctx, &identity); err != nil {
		return domain.User{}, err
	}
	return user, tx.Identities().Seen(ctx, identity.ID, c.Login, c.Email)
}

func displayName(c Claims) string {
	if c.Name != "" {
		return c.Name
	}
	return c.Login
}

// accept records the acceptance and gives the membership and the roles.
func accept(ctx context.Context, tx store.Store, inv domain.Invitation, userID uuid.UUID) error {
	if err := inviterCovers(ctx, tx, inv); err != nil {
		return err
	}
	inv, err := tx.Invitations().Accept(ctx, inv.AccountID, inv.ID, userID)
	if errors.Is(err, store.ErrConflict) {
		return refusal(reasonInvitation)
	}
	if err != nil {
		return err
	}
	by := domain.UserActor(inv.AccountID, userID, domain.Credential{})
	if err := Join(ctx, tx, inv.AccountID, userID, domain.SourceInvitation, inv.Grants, by); err != nil {
		return err
	}
	return audit(ctx, tx, by, "invitation.accepted", "invitation", inv.ID, nil)
}

// inviterCovers refuses an invitation whose inviter no longer holds what it
// gives: No escalation counts at acceptance too, or a demoted Admin's old link
// would still make an Admin. An instance admin, who holds no role, and the
// actors that are not a user or service account are not checked.
func inviterCovers(ctx context.Context, tx store.Store, inv domain.Invitation) error {
	by, err := domain.ParseActor(inv.InvitedBy)
	if err != nil {
		return refusal(reasonInvitation)
	}
	id, ok := by.SubjectID()
	if !ok {
		return nil
	}
	kind := domain.SubjectServiceAccount
	if by.Kind == domain.ActorUser {
		kind = domain.SubjectUser
		u, err := tx.Users().Get(ctx, id)
		if err != nil {
			return err
		}
		if u.InstanceAdmin {
			return nil
		}
	}
	bound, err := tx.RoleBindings().Effective(ctx, inv.AccountID, kind, id)
	if err != nil {
		return err
	}
	for _, g := range inv.Grants {
		role, err := tx.Roles().Get(ctx, inv.AccountID, g.RoleID)
		if errors.Is(err, store.ErrNotFound) {
			return refusal(reasonInvitation)
		}
		if err != nil {
			return err
		}
		for _, p := range role.Permissions {
			if !holds(bound, p, g.ProjectID) {
				return refusal(reasonInvitation)
			}
		}
	}
	return nil
}

// holds says whether a binding of bound gives p at the scope of a grant: an
// organization binding, or one on that project.
func holds(bound []store.BoundRole, p domain.Permission, project uuid.UUID) bool {
	return slices.ContainsFunc(bound, func(b store.BoundRole) bool {
		return b.Role.Has(p) && (b.Binding.ProjectID == uuid.Nil() || (project != uuid.Nil() && b.Binding.ProjectID == project))
	})
}

// Join makes the user an active member with the roles: it creates the
// membership, or activates an invited one, and the bindings it does not have.
// A suspended member stays out: only whoever suspended them can undo it.
// Every change is audited as by.
func Join(ctx context.Context, tx store.Store, accountID, userID uuid.UUID, source domain.Source, grants []domain.Grant, by domain.Actor) error {
	// One join at a time per organization, so a setup link, which only makes
	// the first Owner, can check that nobody joined (claimSetupLink).
	if err := tx.Accounts().Lock(ctx, accountID); err != nil {
		return err
	}
	return join(ctx, tx, accountID, userID, source, grants, by)
}

func join(ctx context.Context, tx store.Store, accountID, userID uuid.UUID, source domain.Source, grants []domain.Grant, by domain.Actor) error {
	m, err := tx.Memberships().Get(ctx, accountID, userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		m = domain.Membership{AccountID: accountID, UserID: userID, Status: domain.MembershipActive, Source: source}
		if err := tx.Memberships().Create(ctx, &m); err != nil {
			return err
		}
		if err := audit(ctx, tx, by, "membership.created", "user", userID, nil); err != nil {
			return err
		}
	case err != nil:
		return err
	case m.Status == domain.MembershipSuspended:
		return refusal(reasonSuspended)
	case m.Status == domain.MembershipInvited:
		if err := tx.Memberships().SetStatus(ctx, accountID, userID, domain.MembershipActive); err != nil {
			return err
		}
		if err := audit(ctx, tx, by, "membership.activated", "user", userID, nil); err != nil {
			return err
		}
	}
	have, err := tx.RoleBindings().List(ctx, accountID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser, SubjectID: userID})
	if err != nil {
		return err
	}
	for _, g := range grants {
		if slices.ContainsFunc(have, func(b domain.RoleBinding) bool { return b.RoleID == g.RoleID && b.ProjectID == g.ProjectID }) {
			continue
		}
		b := domain.RoleBinding{AccountID: accountID, SubjectKind: domain.SubjectUser, SubjectID: userID, RoleID: g.RoleID, ProjectID: g.ProjectID, CreatedBy: by.String()}
		if err := tx.RoleBindings().Create(ctx, &b); err != nil {
			return fmt.Errorf("bind role %s: %w", g.RoleID, err)
		}
		data, err := json.Marshal(map[string]any{"subject_kind": b.SubjectKind, "subject_id": userID.String(), "role_id": g.RoleID.String(), "project_id": g.ProjectID.String()})
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, by, "binding.created", "role_binding", b.ID, data); err != nil {
			return err
		}
	}
	return nil
}

// syncAdmin makes the user's instance_admin flag follow config. The flag is
// granted only by this sign-in, through an instance connection, on a proven
// email or the GitHub user ID: an organization's connection never makes
// anyone an admin of the instance. It is kept while config still lists the user.
func (s *Service) syncAdmin(ctx context.Context, tx store.Store, u domain.User, conn domain.Connection, c Claims, proven bool) (domain.User, error) {
	listed, err := s.listedAdmin(ctx, tx, u)
	if err != nil {
		return u, err
	}
	admin := u.InstanceAdmin && listed
	if conn.AccountID == uuid.Nil() {
		email, github := "", ""
		if proven {
			email = c.Email
		}
		if conn.Kind == domain.ConnectionGitHub {
			github = c.Subject
		}
		admin = admin || s.cfg.Auth.IsInstanceAdmin(email, github)
	}
	if admin == u.InstanceAdmin {
		return u, nil
	}
	u.InstanceAdmin = admin
	return u, tx.Users().Update(ctx, &u)
}

// listedAdmin says whether config lists the user, by their verified email or
// the subject of an identity at an instance GitHub connection.
func (s *Service) listedAdmin(ctx context.Context, st store.Store, u domain.User) (bool, error) {
	if u.EmailVerified && s.cfg.Auth.IsInstanceAdmin(u.Email, "") {
		return true, nil
	}
	identities, err := st.Identities().ListByUser(ctx, u.ID)
	if err != nil {
		return false, err
	}
	for _, id := range identities {
		conn, err := st.Connections().Get(ctx, id.ConnectionID)
		if err != nil {
			return false, err
		}
		if conn.Kind == domain.ConnectionGitHub && conn.AccountID == uuid.Nil() && s.cfg.Auth.IsInstanceAdmin("", id.Subject) {
			return true, nil
		}
	}
	return false, nil
}

// organization picks the session's organization: the one the sign-in was
// for, or the only one the user is in. Zero means the user picks later. Only
// instance admins sign in without a membership; they enter an organization
// through a switch, which is audited. An organization's connection opens that
// organization and no other: its provider vouches for nothing else.
func organization(ctx context.Context, tx store.Store, u domain.User, conn domain.Connection, slug string, invited uuid.UUID) (uuid.UUID, error) {
	memberships, err := tx.Memberships().ListByUser(ctx, u.ID)
	if err != nil {
		return uuid.Nil(), err
	}
	active, closed := []uuid.UUID{}, false
	for _, m := range memberships {
		if m.Status != domain.MembershipActive {
			continue
		}
		ok, _, err := ssoAllows(ctx, tx, u, m.AccountID, conn.ID)
		if err != nil {
			return uuid.Nil(), err
		}
		if ok {
			active = append(active, m.AccountID)
		}
		closed = closed || !ok
	}
	if len(active) == 0 && !u.InstanceAdmin {
		if closed {
			return uuid.Nil(), refusal(reasonSSORequired)
		}
		return uuid.Nil(), refusal(reasonNoMembership)
	}
	want := conn.AccountID
	if invited != uuid.Nil() {
		want = invited
	} else if slug != "" {
		a, err := tx.Accounts().GetBySlug(ctx, slug)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return uuid.Nil(), err
		}
		if conn.AccountID != uuid.Nil() && a.ID != conn.AccountID {
			return uuid.Nil(), refusal(reasonNoMembership)
		}
		if want = a.ID; err != nil {
			want = domain.NewID() // no such organization: nobody is a member
		}
	}
	switch {
	case want != uuid.Nil() && slices.Contains(active, want):
		return want, nil
	case want != uuid.Nil() && !u.InstanceAdmin && closed:
		return uuid.Nil(), refusal(reasonSSORequired)
	case want != uuid.Nil() && !u.InstanceAdmin:
		return uuid.Nil(), refusal(reasonNoMembership)
	case want == uuid.Nil() && len(active) == 1:
		return active[0], nil
	}
	return uuid.Nil(), nil
}

// enterThrough refuses a session in an organization that requires another
// connection of the user. An instance admin and a break-glass sign-in, without
// a connection, are exempt, and each such entry is audited there.
func enterThrough(ctx context.Context, tx store.Store, u domain.User, account, connection uuid.UUID) error {
	ok, exempt, err := ssoAllows(ctx, tx, u, account, connection)
	if err != nil {
		return err
	}
	if !ok {
		return refusal(reasonSSORequired)
	}
	if !exempt {
		return nil
	}
	data, err := json.Marshal(map[string]string{"connection_id": connection.String()})
	if err != nil {
		return err
	}
	return audit(ctx, tx, domain.UserActor(account, u.ID, domain.Credential{}), "sso.exempted", "user", u.ID, data)
}

// ssoAllows says whether connection opens the organization for u, and whether
// that is only because u is exempt from what the organization enforces.
func ssoAllows(ctx context.Context, st store.Store, u domain.User, account, connection uuid.UUID) (ok, exempt bool, err error) {
	enforced, err := store.EnforcedConnections(ctx, st, account, u.ID)
	if err != nil {
		return false, false, err
	}
	if len(enforced) == 0 || slices.Contains(enforced, connection) {
		return true, false, nil
	}
	if u.InstanceAdmin || connection == uuid.Nil() {
		return true, true, nil
	}
	return false, false, nil
}

// newSession stores the session of token: a new ID at every sign-in.
func (s *Service) newSession(ctx context.Context, tx store.Store, token string, u domain.User, account, connection uuid.UUID, userAgent, ip string) error {
	now := s.cfg.Now()
	expires := now.Add(s.cfg.Auth.SessionAbsolute)
	sess := domain.Session{TokenHash: mcp.HashToken(token), UserID: u.ID, AccountID: account, ConnectionID: connection,
		IdleExpiresAt: earliest(now.Add(s.cfg.Auth.SessionIdle), expires), ExpiresAt: expires, UserAgent: userAgent, IP: ip}
	if err := tx.Sessions().Create(ctx, &sess); err != nil {
		return err
	}
	if account == uuid.Nil() {
		return nil
	}
	data, err := json.Marshal(map[string]string{"connection_id": connection.String()})
	if err != nil {
		return err
	}
	by := domain.UserActor(account, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: sess.ID})
	return audit(ctx, tx, by, "session.created", "session", sess.ID, data)
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
