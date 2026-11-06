package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// SweepEvery is how often ended sessions are deleted.
const SweepEvery = time.Hour

// Start brings the database in line with the config: it upserts the instance
// connections, turns off those no longer in the file, clears the flag of
// instance admins it no longer lists, and assigns ownerless tokens.
func (s *Service) Start(ctx context.Context) error {
	st := s.cfg.Store
	stored, err := st.Connections().List(ctx, uuid.Nil())
	if err != nil {
		return fmt.Errorf("list connections: %w", err)
	}
	changes := []connectionChange{}
	for name, c := range s.cfg.Auth.Connections {
		conn := c.Domain(name)
		if err := st.Connections().UpsertInstance(ctx, &conn); err != nil {
			return fmt.Errorf("connection %s: %w", name, err)
		}
		i := slices.IndexFunc(stored, func(x domain.Connection) bool { return x.ID == conn.ID })
		switch {
		case i < 0:
			changes = append(changes, connectionChange{conn, "connection.created"})
		case trustChanged(stored[i], conn):
			changes = append(changes, connectionChange{conn, "connection.updated"})
		}
	}
	for _, c := range stored {
		if _, ok := s.cfg.Auth.Connections[c.Name]; !ok && c.Enabled {
			c.Enabled = false
			if err := st.Connections().Update(ctx, &c); err != nil {
				return fmt.Errorf("turn off connection %s: %w", c.Name, err)
			}
			changes = append(changes, connectionChange{c, "connection.disabled"})
		}
	}
	if err := auditConnections(ctx, st, changes); err != nil {
		return err
	}
	if err := s.dropAdmins(ctx); err != nil {
		return err
	}
	return AssignTokens(ctx, st)
}

// connectionChange is what a start did to an instance connection.
type connectionChange struct {
	conn   domain.Connection
	action string
}

// trustChanged says whether the file changed what a connection trusts.
func trustChanged(a, b domain.Connection) bool {
	return a.Kind != b.Kind || a.Issuer != b.Issuer || a.ClientID != b.ClientID || a.ClientSecretRef != b.ClientSecretRef ||
		a.GroupClaim != b.GroupClaim || a.Enabled != b.Enabled
}

// auditConnections records the changes in every organization: an instance
// connection is usable by all of them.
func auditConnections(ctx context.Context, st store.Store, changes []connectionChange) error {
	if len(changes) == 0 {
		return nil
	}
	accounts, err := st.Accounts().List(ctx)
	if err != nil {
		return fmt.Errorf("list organizations: %w", err)
	}
	for _, a := range accounts {
		for _, c := range changes {
			data, err := json.Marshal(map[string]any{"name": c.conn.Name, "kind": c.conn.Kind})
			if err != nil {
				return err
			}
			if err := audit(ctx, st, domain.SystemActor(a.ID, domain.ForecrewdName), c.action, "connection", c.conn.ID, data); err != nil {
				return fmt.Errorf("audit connection %s: %w", c.conn.Name, err)
			}
		}
	}
	return nil
}

// dropAdmins clears instance_admin on the users config no longer lists, so
// they lose break-glass in the sessions they have too.
func (s *Service) dropAdmins(ctx context.Context) error {
	st := s.cfg.Store
	admins, err := st.Users().ListInstanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("list instance admins: %w", err)
	}
	for _, u := range admins {
		listed, err := s.listedAdmin(ctx, st, u)
		if err != nil {
			return fmt.Errorf("check instance admin %s: %w", u.ID, err)
		}
		if listed {
			continue
		}
		u.InstanceAdmin = false
		if err := st.Users().Update(ctx, &u); err != nil {
			return fmt.Errorf("clear instance admin %s: %w", u.ID, err)
		}
		s.cfg.Logger.InfoContext(ctx, "no longer an instance admin", "user_id", u.ID)
	}
	return nil
}

// AssignTokens gives the ownerless tokens of every organization to its first
// Owner, once it has one (docs/contracts/identity.md, Tokens).
func AssignTokens(ctx context.Context, st store.Store) error {
	accounts, err := st.Accounts().List(ctx)
	if err != nil {
		return fmt.Errorf("list organizations: %w", err)
	}
	for _, a := range accounts {
		owner, err := firstOwner(ctx, st, a.ID)
		if err != nil {
			return fmt.Errorf("find the first owner of %s: %w", a.Slug, err)
		}
		if owner == uuid.Nil() {
			continue
		}
		err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			n, err := tx.MCPTokens().AssignOwner(ctx, a.ID, domain.SubjectUser, owner)
			if err != nil || n == 0 {
				return err
			}
			data, err := json.Marshal(map[string]any{"owner": owner.String(), "tokens": n})
			if err != nil {
				return err
			}
			return audit(ctx, tx, domain.SystemActor(a.ID, domain.ForecrewdName), "token.assigned", "organization", a.ID, data)
		})
		if err != nil {
			return fmt.Errorf("assign the tokens of %s: %w", a.Slug, err)
		}
	}
	return nil
}

// firstOwner returns the oldest active Owner of an organization, zero when
// it has none.
func firstOwner(ctx context.Context, st store.Store, accountID uuid.UUID) (uuid.UUID, error) {
	role, err := st.Roles().GetByName(ctx, accountID, domain.RoleScopeOrganization, domain.RoleOwner)
	if err != nil {
		return uuid.Nil(), err
	}
	bindings, err := st.RoleBindings().List(ctx, accountID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser, RoleID: role.ID, OrganizationOnly: true})
	if err != nil {
		return uuid.Nil(), err
	}
	for _, b := range bindings {
		effective, err := st.RoleBindings().Effective(ctx, accountID, domain.SubjectUser, b.SubjectID)
		if err != nil {
			return uuid.Nil(), err
		}
		if slices.ContainsFunc(effective, func(e store.BoundRole) bool { return e.Binding.ID == b.ID }) {
			return b.SubjectID, nil
		}
	}
	return uuid.Nil(), nil
}

// RunSweep deletes ended sessions every interval until ctx ends.
func (s *Service) RunSweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.cfg.Store.Sessions().DeleteExpired(ctx, s.cfg.Now()); err != nil && !errors.Is(err, context.Canceled) {
				s.cfg.Logger.WarnContext(ctx, "delete ended sessions", "error", err)
			}
		}
	}
}
