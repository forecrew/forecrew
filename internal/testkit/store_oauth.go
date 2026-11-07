package testkit

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func oauthClientsT(d *storeData) *table[domain.OAuthClient] { return &d.identity.oauthClients }
func oauthGrantsT(d *storeData) *table[domain.OAuthGrant]   { return &d.identity.oauthGrants }

// OAuth implements store.Store.
func (s *Store) OAuth() store.OAuthRepo { return oauthRepo{s} }

type oauthRepo struct{ s *Store }

func (r oauthRepo) CreateClient(_ context.Context, c *domain.OAuthClient) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if _, err := d.identity.oauthClients.find(func(x domain.OAuthClient) bool { return x.ClientID == c.ClientID }); err == nil {
			return store.ErrConflict
		}
		c.ID = newID(c.ID)
		c.CreatedAt = now
		d.identity.oauthClients.insert(*c)
		return nil
	})
}

func (r oauthRepo) GetClient(_ context.Context, clientID string) (domain.OAuthClient, error) {
	return findRow(r.s, oauthClientsT, func(c domain.OAuthClient) bool { return c.ClientID == clientID })
}

func (r oauthRepo) GetClientByID(_ context.Context, id uuid.UUID) (domain.OAuthClient, error) {
	return findRow(r.s, oauthClientsT, func(c domain.OAuthClient) bool { return c.ID == id })
}

func (r oauthRepo) client(id uuid.UUID, fn func(c *domain.OAuthClient, now time.Time)) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		c, err := d.identity.oauthClients.find(func(x domain.OAuthClient) bool { return x.ID == id })
		if err != nil {
			return err
		}
		fn(c, now)
		return nil
	})
}

func (r oauthRepo) UpdateClient(_ context.Context, c *domain.OAuthClient) error {
	return r.client(c.ID, func(x *domain.OAuthClient, _ time.Time) { x.Name, x.RedirectURIs = c.Name, c.RedirectURIs })
}

func (r oauthRepo) TouchClient(_ context.Context, id uuid.UUID) error {
	return r.client(id, func(c *domain.OAuthClient, now time.Time) { c.LastUsedAt = now })
}

func (r oauthRepo) DeleteUnusedClients(_ context.Context, t time.Time) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		rows := &d.identity.oauthClients.rows
		before := len(*rows)
		*rows = slices.DeleteFunc(slices.Clone(*rows), func(c domain.OAuthClient) bool {
			last := c.LastUsedAt
			if last.IsZero() {
				last = c.CreatedAt
			}
			return last.Before(t) && !slices.ContainsFunc(d.identity.oauthGrants.rows, func(g domain.OAuthGrant) bool { return g.ClientID == c.ID })
		})
		n = before - len(*rows)
		return nil
	})
	return n, err
}

// neverUsed reports whether nobody ever authorized c.
func (d *storeData) neverUsed(c domain.OAuthClient) bool {
	return c.LastUsedAt.IsZero() && !slices.ContainsFunc(d.identity.oauthGrants.rows, func(g domain.OAuthGrant) bool { return g.ClientID == c.ID })
}

func (r oauthRepo) CountUnusedClients(_ context.Context) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		for _, c := range d.identity.oauthClients.rows {
			if d.neverUsed(c) {
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r oauthRepo) DeleteNeverUsedClients(_ context.Context, t time.Time) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		rows := &d.identity.oauthClients.rows
		before := len(*rows)
		*rows = slices.DeleteFunc(slices.Clone(*rows), func(c domain.OAuthClient) bool { return c.CreatedAt.Before(t) && d.neverUsed(c) })
		n = before - len(*rows)
		return nil
	})
	return n, err
}

func (r oauthRepo) DeleteOldestUnusedClient(_ context.Context) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		rows := &d.identity.oauthClients.rows
		oldest := -1
		for i, c := range *rows {
			if d.neverUsed(c) && (oldest < 0 || cmp.Or(c.CreatedAt.Compare((*rows)[oldest].CreatedAt), bytes.Compare(c.ID[:], (*rows)[oldest].ID[:])) < 0) {
				oldest = i
			}
		}
		if oldest >= 0 {
			*rows = slices.Delete(slices.Clone(*rows), oldest, oldest+1)
		}
		return nil
	})
}

func (r oauthRepo) CreateGrant(_ context.Context, g *domain.OAuthGrant) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		g.ID = newID(g.ID)
		g.CreatedAt = now
		d.identity.oauthGrants.insert(*g)
		return nil
	})
}

func (r oauthRepo) GetGrant(_ context.Context, id uuid.UUID) (domain.OAuthGrant, error) {
	return findRow(r.s, oauthGrantsT, func(g domain.OAuthGrant) bool { return g.ID == id })
}

func (r oauthRepo) GetGrantByTokenHash(_ context.Context, hash []byte) (domain.OAuthGrant, error) {
	return findRow(r.s, oauthGrantsT, func(g domain.OAuthGrant) bool { return bytes.Equal(g.TokenHash, hash) })
}

func (r oauthRepo) UseGrant(_ context.Context, id uuid.UUID) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		g, err := d.identity.oauthGrants.find(func(x domain.OAuthGrant) bool { return x.ID == id })
		if err != nil || g.Kind == domain.OAuthAccess || !g.UsedAt.IsZero() {
			return store.ErrConflict
		}
		g.UsedAt = now
		return nil
	})
}

// revoke revokes the unrevoked grants that match.
func (r oauthRepo) revoke(match func(domain.OAuthGrant) bool) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		for i := range d.identity.oauthGrants.rows {
			g := &d.identity.oauthGrants.rows[i]
			if match(*g) && g.RevokedAt.IsZero() {
				g.RevokedAt = now
			}
		}
		return nil
	})
}

func (r oauthRepo) RevokeFamily(_ context.Context, familyID uuid.UUID) error {
	return r.revoke(func(g domain.OAuthGrant) bool { return g.FamilyID == familyID })
}

func (r oauthRepo) ListFamilies(_ context.Context, accountID, userID uuid.UUID) ([]domain.OAuthGrant, error) {
	now := time.Now()
	rows, err := listRows(r.s, oauthGrantsT, 0, func(g domain.OAuthGrant) bool {
		return g.AccountID == accountID && g.UserID == userID && g.Kind != domain.OAuthCode && g.Usable(now)
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(rows, func(a, b domain.OAuthGrant) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), bytes.Compare(b.ID[:], a.ID[:]))
	})
	seen := map[uuid.UUID]bool{}
	return slices.DeleteFunc(rows, func(g domain.OAuthGrant) bool {
		dup := seen[g.FamilyID]
		seen[g.FamilyID] = true
		return dup
	}), nil
}

func (r oauthRepo) UserAuthorized(_ context.Context, clientID, userID uuid.UUID) (bool, error) {
	rows, err := listRows(r.s, oauthGrantsT, 1, func(g domain.OAuthGrant) bool { return g.ClientID == clientID && g.UserID == userID })
	return len(rows) > 0, err
}

func (r oauthRepo) RevokeByUser(_ context.Context, accountID, userID uuid.UUID) error {
	return r.revoke(func(g domain.OAuthGrant) bool { return g.UserID == userID && zeroOr(accountID, g.AccountID) })
}

func (r oauthRepo) DeleteExpiredGrants(_ context.Context, t time.Time) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		rows := &d.identity.oauthGrants.rows
		before := len(*rows)
		*rows = slices.DeleteFunc(slices.Clone(*rows), func(g domain.OAuthGrant) bool { return g.ExpiresAt.Before(t) })
		n = before - len(*rows)
		return nil
	})
	return n, err
}
