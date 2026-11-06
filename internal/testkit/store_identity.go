package testkit

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// identityData holds the rows of sign-in. The tables are global, so their
// key's account is always zero, except where a row belongs to an organization.
type identityData struct {
	users       table[domain.User]
	identities  table[domain.Identity]
	memberships table[domain.Membership]
	invitations table[domain.Invitation]
	connections table[domain.Connection]
	domains     table[domain.VerifiedDomain]
	sessions    table[domain.Session]
}

func newIdentityData() identityData {
	return identityData{
		users:       table[domain.User]{key: func(r domain.User) (uuid.UUID, uuid.UUID) { return uuid.Nil(), r.ID }},
		identities:  table[domain.Identity]{key: func(r domain.Identity) (uuid.UUID, uuid.UUID) { return r.UserID, r.ID }},
		memberships: table[domain.Membership]{key: func(r domain.Membership) (uuid.UUID, uuid.UUID) { return r.AccountID, r.UserID }},
		invitations: table[domain.Invitation]{key: func(r domain.Invitation) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		connections: table[domain.Connection]{key: func(r domain.Connection) (uuid.UUID, uuid.UUID) { return uuid.Nil(), r.ID }},
		domains:     table[domain.VerifiedDomain]{key: func(r domain.VerifiedDomain) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		sessions:    table[domain.Session]{key: func(r domain.Session) (uuid.UUID, uuid.UUID) { return r.UserID, r.ID }},
	}
}

func (i identityData) clone() identityData {
	i.users = i.users.clone()
	i.identities = i.identities.clone()
	i.memberships = i.memberships.clone()
	i.invitations = i.invitations.clone()
	i.connections = i.connections.clone()
	i.domains = i.domains.clone()
	i.sessions = i.sessions.clone()
	return i
}

func usersT(d *storeData) *table[domain.User]             { return &d.identity.users }
func identitiesT(d *storeData) *table[domain.Identity]    { return &d.identity.identities }
func membershipsT(d *storeData) *table[domain.Membership] { return &d.identity.memberships }
func invitationsT(d *storeData) *table[domain.Invitation] { return &d.identity.invitations }
func connectionsT(d *storeData) *table[domain.Connection] { return &d.identity.connections }
func domainsT(d *storeData) *table[domain.VerifiedDomain] { return &d.identity.domains }
func sessionsT(d *storeData) *table[domain.Session]       { return &d.identity.sessions }

// Users implements store.Store.
func (s *Store) Users() store.UserRepo { return userRepo{s} }

type userRepo struct{ s *Store }

func sameEmail(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

func (r userRepo) Create(_ context.Context, u *domain.User) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if u.EmailVerified && d.identity.users.exists(func(x domain.User) bool { return x.EmailVerified && sameEmail(x.Email, u.Email) }) {
			return fmt.Errorf("user %s: %w", u.Email, store.ErrConflict)
		}
		u.ID = newID(u.ID)
		u.CreatedAt, u.UpdatedAt = now, now
		d.identity.users.insert(*u)
		return nil
	})
}

func (r userRepo) Get(_ context.Context, id uuid.UUID) (domain.User, error) {
	return getRow(r.s, usersT, uuid.Nil(), id)
}

func (r userRepo) ListByIDs(_ context.Context, ids []uuid.UUID) ([]domain.User, error) {
	return listRows(r.s, usersT, 0, func(u domain.User) bool { return slices.Contains(ids, u.ID) })
}

func (r userRepo) GetByVerifiedEmail(_ context.Context, email string) (domain.User, error) {
	return findRow(r.s, usersT, func(u domain.User) bool { return u.EmailVerified && sameEmail(u.Email, email) })
}

func (r userRepo) ListByEmail(_ context.Context, email string) ([]domain.User, error) {
	return listRows(r.s, usersT, 0, func(u domain.User) bool { return sameEmail(u.Email, email) })
}

func (r userRepo) ListInstanceAdmins(context.Context) ([]domain.User, error) {
	return listRows(r.s, usersT, 0, func(u domain.User) bool { return u.InstanceAdmin })
}

func (r userRepo) Update(_ context.Context, u *domain.User) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if u.EmailVerified && d.identity.users.exists(func(x domain.User) bool { return x.ID != u.ID && x.EmailVerified && sameEmail(x.Email, u.Email) }) {
			return fmt.Errorf("user %s: %w", u.Email, store.ErrConflict)
		}
		row, err := d.identity.users.get(uuid.Nil(), u.ID)
		if err != nil {
			return err
		}
		row.Name, row.Email, row.EmailVerified, row.InstanceAdmin, row.UpdatedAt = u.Name, u.Email, u.EmailVerified, u.InstanceAdmin, now
		return nil
	})
}

func (r userRepo) Seen(_ context.Context, id uuid.UUID) error {
	return update(r.s, usersT, uuid.Nil(), id, func(u *domain.User, now time.Time) error {
		u.LastSeenAt = now
		return nil
	})
}

func (r userRepo) SetDisabled(_ context.Context, id uuid.UUID, disabled bool) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		u, err := d.identity.users.get(uuid.Nil(), id)
		if err != nil {
			return err
		}
		if !disabled {
			u.DisabledAt = time.Time{}
			return nil
		}
		if !u.DisabledAt.IsZero() {
			return nil
		}
		for _, m := range d.identity.memberships.filter(0, func(m domain.Membership) bool { return m.UserID == id }) {
			if d.lastOwner(m.AccountID, id) {
				return store.ErrLastOwner
			}
		}
		u.DisabledAt, u.UpdatedAt = now, now
		return nil
	})
}

// Identities implements store.Store.
func (s *Store) Identities() store.IdentityRepo { return identityRepo{s} }

type identityRepo struct{ s *Store }

//nolint:dupl // same shape as other tables, other uniqueness
func (r identityRepo) Create(_ context.Context, i *domain.Identity) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.identity.identities.exists(func(x domain.Identity) bool { return x.ConnectionID == i.ConnectionID && x.Subject == i.Subject }) {
			return fmt.Errorf("identity %s: %w", i.Subject, store.ErrConflict)
		}
		i.ID = newID(i.ID)
		i.CreatedAt = now
		d.identity.identities.insert(*i)
		return nil
	})
}

func (r identityRepo) GetBySubject(_ context.Context, connectionID uuid.UUID, subject string) (domain.Identity, error) {
	return findRow(r.s, identitiesT, func(i domain.Identity) bool { return i.ConnectionID == connectionID && i.Subject == subject })
}

func (r identityRepo) ListByLogin(_ context.Context, connectionID uuid.UUID, login string) ([]domain.Identity, error) {
	return listRows(r.s, identitiesT, 0, func(i domain.Identity) bool { return i.ConnectionID == connectionID && sameEmail(i.Login, login) })
}

func (r identityRepo) ListByUser(_ context.Context, userID uuid.UUID) ([]domain.Identity, error) {
	return listRows(r.s, identitiesT, 0, func(i domain.Identity) bool { return i.UserID == userID })
}

func (r identityRepo) Seen(_ context.Context, id uuid.UUID, login, email string) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		i, err := d.identity.identities.find(func(x domain.Identity) bool { return x.ID == id })
		if err != nil {
			return err
		}
		i.LastUsedAt, i.Login, i.Email = now, login, email
		return nil
	})
}

func (r identityRepo) Delete(_ context.Context, userID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		t := &d.identity.identities
		if _, err := t.get(userID, id); err != nil {
			return err
		}
		t.rows = slices.DeleteFunc(slices.Clone(t.rows), func(x domain.Identity) bool { return x.ID == id })
		return nil
	})
}

// Memberships implements store.Store.
func (s *Store) Memberships() store.MembershipRepo { return membershipRepo{s} }

type membershipRepo struct{ s *Store }

func (r membershipRepo) Create(_ context.Context, m *domain.Membership) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.identity.memberships.exists(func(x domain.Membership) bool { return x.AccountID == m.AccountID && x.UserID == m.UserID }) {
			return fmt.Errorf("membership: %w", store.ErrConflict)
		}
		m.ID = newID(m.ID)
		m.CreatedAt, m.UpdatedAt = now, now
		d.identity.memberships.insert(*m)
		return nil
	})
}

func (r membershipRepo) Get(_ context.Context, accountID, userID uuid.UUID) (domain.Membership, error) {
	return getRow(r.s, membershipsT, accountID, userID)
}

func (r membershipRepo) GetByExternalID(_ context.Context, accountID uuid.UUID, externalID string) (domain.Membership, error) {
	return findRow(r.s, membershipsT, func(m domain.Membership) bool { return m.AccountID == accountID && m.ExternalID == externalID })
}

func (r membershipRepo) List(_ context.Context, accountID uuid.UUID, f store.MembershipFilter) ([]domain.Membership, error) {
	return listRows(r.s, membershipsT, f.Limit, func(m domain.Membership) bool { return m.AccountID == accountID && zeroOr(f.Status, m.Status) })
}

func (r membershipRepo) ListByUser(_ context.Context, userID uuid.UUID) ([]domain.Membership, error) {
	return listRows(r.s, membershipsT, 0, func(m domain.Membership) bool { return m.UserID == userID })
}

func (r membershipRepo) SetStatus(_ context.Context, accountID, userID uuid.UUID, status domain.MembershipStatus) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		m, err := d.identity.memberships.get(accountID, userID)
		if err != nil {
			return err
		}
		if status != domain.MembershipActive && d.lastOwner(accountID, userID) {
			return store.ErrLastOwner
		}
		m.Status, m.UpdatedAt = status, now
		return nil
	})
}

func (r membershipRepo) SetExternalID(_ context.Context, accountID, userID uuid.UUID, externalID string) error {
	return update(r.s, membershipsT, accountID, userID, func(m *domain.Membership, now time.Time) error {
		m.ExternalID, m.UpdatedAt = externalID, now
		return nil
	})
}

func (r membershipRepo) Remove(_ context.Context, accountID, userID uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if _, err := d.identity.memberships.get(accountID, userID); err != nil {
			return err
		}
		if d.lastOwner(accountID, userID) {
			return store.ErrLastOwner
		}
		d.removeBindings(accountID, userID)
		t := &d.identity.memberships
		t.rows = slices.DeleteFunc(slices.Clone(t.rows), func(m domain.Membership) bool { return m.AccountID == accountID && m.UserID == userID })
		return nil
	})
}

// Invitations implements store.Store.
func (s *Store) Invitations() store.InvitationRepo { return invitationRepo{s} }

type invitationRepo struct{ s *Store }

func (r invitationRepo) Create(_ context.Context, i *domain.Invitation) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		i.ID = newID(i.ID)
		i.CreatedAt = now
		d.identity.invitations.insert(*i)
		return nil
	})
}

func (r invitationRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Invitation, error) {
	return getRow(r.s, invitationsT, accountID, id)
}

func (r invitationRepo) GetByTokenHash(_ context.Context, hash []byte) (domain.Invitation, error) {
	return findRow(r.s, invitationsT, func(i domain.Invitation) bool { return bytes.Equal(i.TokenHash, hash) })
}

func (r invitationRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Invitation, error) {
	return read(r.s, func(d *storeData) ([]domain.Invitation, error) {
		return d.identity.invitations.newest(0, func(i domain.Invitation) bool { return i.AccountID == accountID }), nil
	})
}

func (r invitationRepo) Accept(_ context.Context, accountID, id, userID uuid.UUID) (domain.Invitation, error) {
	var out domain.Invitation
	err := update(r.s, invitationsT, accountID, id, func(i *domain.Invitation, now time.Time) error {
		if !i.Pending(now) {
			return fmt.Errorf("invitation %s is not pending: %w", id, store.ErrConflict)
		}
		i.AcceptedBy, i.AcceptedAt = userID, now
		out = *i
		return nil
	})
	return out, err
}

func (r invitationRepo) Revoke(_ context.Context, accountID, id uuid.UUID) error {
	return update(r.s, invitationsT, accountID, id, func(i *domain.Invitation, now time.Time) error {
		if i.RevokedAt.IsZero() {
			i.RevokedAt = now
		}
		return nil
	})
}

// Connections implements store.Store.
func (s *Store) Connections() store.ConnectionRepo { return connectionRepo{s} }

type connectionRepo struct{ s *Store }

func (r connectionRepo) Create(_ context.Context, c *domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.identity.connections.exists(func(x domain.Connection) bool { return x.AccountID == c.AccountID && x.Name == c.Name }) {
			return fmt.Errorf("connection %s: %w", c.Name, store.ErrConflict)
		}
		c.ID = newID(c.ID)
		c.CreatedAt, c.UpdatedAt = now, now
		d.identity.connections.insert(*c)
		return nil
	})
}

func (r connectionRepo) Get(_ context.Context, id uuid.UUID) (domain.Connection, error) {
	return getRow(r.s, connectionsT, uuid.Nil(), id)
}

func (r connectionRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Connection, error) {
	return listRows(r.s, connectionsT, 0, func(c domain.Connection) bool { return c.AccountID == accountID })
}

func (r connectionRepo) UpsertInstance(_ context.Context, c *domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		row, err := d.identity.connections.find(func(x domain.Connection) bool { return x.AccountID == uuid.Nil() && x.Name == c.Name })
		if err != nil {
			c.ID = newID(c.ID)
			c.CreatedAt, c.UpdatedAt = now, now
			d.identity.connections.insert(*c)
			return nil //nolint:nilerr // not found: insert
		}
		c.ID, c.CreatedAt, c.UpdatedAt = row.ID, row.CreatedAt, now
		*row = *c
		return nil
	})
}

func (r connectionRepo) Update(_ context.Context, c *domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return update(r.s, connectionsT, uuid.Nil(), c.ID, func(row *domain.Connection, now time.Time) error {
		row.Name, row.Issuer, row.ClientID, row.ClientSecretRef, row.JIT, row.GroupClaim, row.Enabled, row.UpdatedAt =
			c.Name, c.Issuer, c.ClientID, c.ClientSecretRef, c.JIT, c.GroupClaim, c.Enabled, now
		return nil
	})
}

// AddGroupMapping implements store.ConnectionRepo. Group mapping is T56's;
// the fake keeps nothing.
func (r connectionRepo) AddGroupMapping(context.Context, *domain.GroupMapping) error { return nil }

// RemoveGroupMapping implements store.ConnectionRepo.
func (r connectionRepo) RemoveGroupMapping(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// GroupMappings implements store.ConnectionRepo.
func (r connectionRepo) GroupMappings(context.Context, uuid.UUID) ([]domain.GroupMapping, error) {
	return []domain.GroupMapping{}, nil
}

// VerifiedDomains implements store.Store.
func (s *Store) VerifiedDomains() store.VerifiedDomainRepo { return domainRepo{s} }

type domainRepo struct{ s *Store }

func (r domainRepo) Create(_ context.Context, v *domain.VerifiedDomain) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.identity.domains.exists(func(x domain.VerifiedDomain) bool { return x.AccountID == v.AccountID && x.Domain == v.Domain }) {
			return fmt.Errorf("domain %s: %w", v.Domain, store.ErrConflict)
		}
		v.ID = newID(v.ID)
		v.CreatedAt, v.UpdatedAt = now, now
		d.identity.domains.insert(*v)
		return nil
	})
}

func (r domainRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.VerifiedDomain, error) {
	return getRow(r.s, domainsT, accountID, id)
}

func (r domainRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.VerifiedDomain, error) {
	return listRows(r.s, domainsT, 0, func(v domain.VerifiedDomain) bool { return v.AccountID == accountID })
}

func (r domainRepo) GetVerified(_ context.Context, name string) (domain.VerifiedDomain, error) {
	name = strings.ToLower(name)
	return findRow(r.s, domainsT, func(v domain.VerifiedDomain) bool { return v.Domain == name && !v.VerifiedAt.IsZero() })
}

func (r domainRepo) SetVerified(_ context.Context, accountID, id uuid.UUID, verified bool) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		v, err := d.identity.domains.get(accountID, id)
		if err != nil {
			return err
		}
		if !verified {
			v.VerifiedAt, v.EnforceSSO = time.Time{}, false
			return nil
		}
		if d.identity.domains.exists(func(x domain.VerifiedDomain) bool {
			return x.ID != id && x.Domain == v.Domain && !x.VerifiedAt.IsZero()
		}) {
			return fmt.Errorf("domain %s: %w", v.Domain, store.ErrConflict)
		}
		if v.VerifiedAt.IsZero() {
			v.VerifiedAt = now
		}
		return nil
	})
}

func (r domainRepo) SetEnforcement(_ context.Context, accountID, id uuid.UUID, enforce bool, connectionID uuid.UUID) error {
	return update(r.s, domainsT, accountID, id, func(v *domain.VerifiedDomain, now time.Time) error {
		if enforce && (v.VerifiedAt.IsZero() || connectionID == uuid.Nil()) {
			return fmt.Errorf("verified_domains_check: enforce_sso needs a verified domain and a connection")
		}
		v.EnforceSSO, v.ConnectionID, v.UpdatedAt = enforce, connectionID, now
		return nil
	})
}

func (r domainRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		t := &d.identity.domains
		if _, err := t.get(accountID, id); err != nil {
			return err
		}
		t.rows = slices.DeleteFunc(slices.Clone(t.rows), func(x domain.VerifiedDomain) bool { return x.ID == id })
		return nil
	})
}

// Sessions implements store.Store.
func (s *Store) Sessions() store.SessionRepo { return sessionRepo{s} }

type sessionRepo struct{ s *Store }

func (r sessionRepo) Create(_ context.Context, s *domain.Session) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		s.ID = newID(s.ID)
		s.CreatedAt, s.LastSeenAt = now, now
		d.identity.sessions.insert(*s)
		return nil
	})
}

func (r sessionRepo) GetByTokenHash(_ context.Context, hash []byte) (domain.Session, error) {
	return findRow(r.s, sessionsT, func(s domain.Session) bool { return bytes.Equal(s.TokenHash, hash) })
}

func (r sessionRepo) ListByUser(_ context.Context, userID uuid.UUID) ([]domain.Session, error) {
	return listRows(r.s, sessionsT, 0, func(s domain.Session) bool { return s.UserID == userID })
}

func (r sessionRepo) byID(id uuid.UUID, fn func(s *domain.Session, now time.Time)) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		s, err := d.identity.sessions.find(func(x domain.Session) bool { return x.ID == id })
		if err != nil {
			return err
		}
		fn(s, now)
		return nil
	})
}

func (r sessionRepo) Touch(_ context.Context, id uuid.UUID, idleExpiresAt time.Time) error {
	return r.byID(id, func(s *domain.Session, now time.Time) {
		s.LastSeenAt = now
		s.IdleExpiresAt = idleExpiresAt
		if idleExpiresAt.After(s.ExpiresAt) {
			s.IdleExpiresAt = s.ExpiresAt
		}
	})
}

func (r sessionRepo) SetAccount(_ context.Context, id, accountID uuid.UUID) error {
	return r.byID(id, func(s *domain.Session, _ time.Time) { s.AccountID = accountID })
}

func (r sessionRepo) Revoke(_ context.Context, userID, id uuid.UUID) error {
	return update(r.s, sessionsT, userID, id, func(s *domain.Session, now time.Time) error {
		if s.RevokedAt.IsZero() {
			s.RevokedAt = now
		}
		return nil
	})
}

func (r sessionRepo) RevokeByUser(_ context.Context, userID, accountID uuid.UUID) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		for i := range d.identity.sessions.rows {
			s := &d.identity.sessions.rows[i]
			if s.UserID == userID && zeroOr(accountID, s.AccountID) && s.RevokedAt.IsZero() {
				s.RevokedAt = now
			}
		}
		return nil
	})
}

func (r sessionRepo) DeleteExpired(_ context.Context, t time.Time) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		rows := &d.identity.sessions.rows
		before := len(*rows)
		*rows = slices.DeleteFunc(slices.Clone(*rows), func(s domain.Session) bool {
			return s.ExpiresAt.Before(t) || s.IdleExpiresAt.Before(t) || (!s.RevokedAt.IsZero() && s.RevokedAt.Before(t))
		})
		n = before - len(*rows)
		return nil
	})
	return n, err
}

// ListByOwner implements store.PersonalTokenRepo.
func (r tokenRepo) ListByOwner(_ context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) ([]domain.MCPToken, error) {
	return listRows(r.s, tokensT, 0, func(t domain.MCPToken) bool {
		return t.AccountID == accountID && t.OwnerKind == kind && t.OwnerID == ownerID
	})
}

// AssignOwner implements store.PersonalTokenRepo.
func (r tokenRepo) AssignOwner(_ context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		used := map[string]bool{}
		for _, t := range d.tokens.rows {
			if t.AccountID == accountID && t.OwnerKind == kind && t.OwnerID == ownerID {
				used[t.Name] = true
			}
		}
		for i := range d.tokens.rows {
			t := &d.tokens.rows[i]
			if t.AccountID == accountID && t.OwnerID == uuid.Nil() {
				t.Name = store.FreeTokenName(t.Name, used)
				used[t.Name] = true
				t.OwnerKind, t.OwnerID = kind, ownerID
				n++
			}
		}
		return nil
	})
	return n, err
}

// RevokeByOwner implements store.PersonalTokenRepo.
func (r tokenRepo) RevokeByOwner(_ context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		for i := range d.tokens.rows {
			t := &d.tokens.rows[i]
			if t.AccountID == accountID && t.OwnerKind == kind && t.OwnerID == ownerID && t.RevokedAt.IsZero() {
				t.RevokedAt = now
			}
		}
		return nil
	})
}
