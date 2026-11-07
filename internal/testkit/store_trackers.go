package testkit

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// trackerData holds the tracker tables.
type trackerData struct {
	connections table[domain.TrackerConnection]
	routes      table[domain.TrackerRoute]
	identities  table[domain.TrackerIdentity]
	links       table[domain.TicketLink]
	outbox      table[domain.TrackerWrite]
	cursors     map[uuid.UUID]string
}

func newTrackerData() trackerData {
	return trackerData{
		connections: table[domain.TrackerConnection]{key: func(r domain.TrackerConnection) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		routes:      table[domain.TrackerRoute]{key: func(r domain.TrackerRoute) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		identities:  table[domain.TrackerIdentity]{key: func(r domain.TrackerIdentity) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		links:       table[domain.TicketLink]{key: func(r domain.TicketLink) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		outbox:      table[domain.TrackerWrite]{key: func(r domain.TrackerWrite) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		cursors:     map[uuid.UUID]string{},
	}
}

func (t trackerData) clone() trackerData {
	c := t
	c.connections, c.routes, c.identities = t.connections.clone(), t.routes.clone(), t.identities.clone()
	c.links, c.outbox = t.links.clone(), t.outbox.clone()
	c.cursors = make(map[uuid.UUID]string, len(t.cursors))
	maps.Copy(c.cursors, t.cursors)
	return c
}

func trackerConnsT(d *storeData) *table[domain.TrackerConnection] { return &d.trackers.connections }
func routesT(d *storeData) *table[domain.TrackerRoute]            { return &d.trackers.routes }
func trackerIDsT(d *storeData) *table[domain.TrackerIdentity]     { return &d.trackers.identities }
func linksT(d *storeData) *table[domain.TicketLink]               { return &d.trackers.links }
func outboxT(d *storeData) *table[domain.TrackerWrite]            { return &d.trackers.outbox }

// remove deletes the row of account with id, or answers store.ErrNotFound.
func remove[T any](s *Store, t func(*storeData) *table[T], account, id uuid.UUID) error {
	return s.do(func(d *storeData, _ time.Time) error {
		tb := t(d)
		if _, err := tb.get(account, id); err != nil {
			return err
		}
		tb.rows = slices.DeleteFunc(tb.rows, func(r T) bool {
			a, i := tb.key(r)
			return a == account && i == id
		})
		return nil
	})
}

// TrackerConnections implements store.Store.
func (s *Store) TrackerConnections() store.TrackerConnectionRepo { return trackerConnRepo{s} }

type trackerConnRepo struct{ s *Store }

func (r trackerConnRepo) Create(_ context.Context, c *domain.TrackerConnection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.trackers.connections.exists(func(x domain.TrackerConnection) bool { return x.AccountID == c.AccountID && x.Name == c.Name }) {
			return store.ErrConflict
		}
		c.ID = newID(c.ID)
		if c.Config == nil {
			c.Config = map[string]any{}
		}
		c.CreatedAt, c.UpdatedAt = now, now
		d.trackers.connections.insert(*c)
		return nil
	})
}

func (r trackerConnRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.TrackerConnection, error) {
	return getRow(r.s, trackerConnsT, accountID, id)
}

func (r trackerConnRepo) GetByName(_ context.Context, accountID uuid.UUID, name string) (domain.TrackerConnection, error) {
	return findRow(r.s, trackerConnsT, func(c domain.TrackerConnection) bool { return c.AccountID == accountID && c.Name == name })
}

func (r trackerConnRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.TrackerConnection, error) {
	return listRows(r.s, trackerConnsT, 0, func(c domain.TrackerConnection) bool { return c.AccountID == accountID })
}

func (r trackerConnRepo) Find(_ context.Context, id uuid.UUID) (domain.TrackerConnection, error) {
	return findRow(r.s, trackerConnsT, func(c domain.TrackerConnection) bool { return c.ID == id })
}

func (r trackerConnRepo) ListByKind(_ context.Context, kind string) ([]domain.TrackerConnection, error) {
	return listRows(r.s, trackerConnsT, 0, func(c domain.TrackerConnection) bool { return c.Kind == kind })
}

// TrackerRoutes implements store.Store.
func (s *Store) TrackerRoutes() store.TrackerRouteRepo { return routeRepo{s} }

type routeRepo struct{ s *Store }

func (r routeRepo) Create(_ context.Context, t *domain.TrackerRoute) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.trackers.connections, t.AccountID, t.ConnectionID), foreign(&d.projects, t.AccountID, t.ProjectID)); err != nil {
			return err
		}
		t.ID = newID(t.ID)
		if t.Position == 0 {
			for _, x := range d.trackers.routes.rows {
				if x.ConnectionID == t.ConnectionID {
					t.Position = max(t.Position, x.Position)
				}
			}
			t.Position++
		}
		if t.States == nil {
			t.States = map[domain.TicketState]string{}
		}
		t.CreatedAt, t.UpdatedAt = now, now
		d.trackers.routes.insert(*t)
		return nil
	})
}

func (r routeRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.TrackerRoute, error) {
	return getRow(r.s, routesT, accountID, id)
}

func (r routeRepo) List(_ context.Context, accountID, connectionID uuid.UUID) ([]domain.TrackerRoute, error) {
	out, err := listRows(r.s, routesT, 0, func(t domain.TrackerRoute) bool {
		return t.AccountID == accountID && zeroOr(connectionID, t.ConnectionID)
	})
	slices.SortStableFunc(out, func(a, b domain.TrackerRoute) int { return a.Position - b.Position })
	return out, err
}

func (r routeRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	if err := remove(r.s, routesT, accountID, id); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, _ time.Time) error { // ON DELETE SET NULL
		for i := range d.trackers.links.rows {
			if d.trackers.links.rows[i].RouteID == id {
				d.trackers.links.rows[i].RouteID = uuid.Nil()
			}
		}
		return nil
	})
}

// TrackerIdentities implements store.Store.
func (s *Store) TrackerIdentities() store.TrackerIdentityRepo { return trackerIdentityRepo{s} }

type trackerIdentityRepo struct{ s *Store }

func (r trackerIdentityRepo) Create(_ context.Context, i *domain.TrackerIdentity) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.trackers.connections, i.AccountID, i.ConnectionID); err != nil {
			return err
		}
		if d.trackers.identities.exists(func(x domain.TrackerIdentity) bool {
			return x.ConnectionID == i.ConnectionID && x.ExternalID == i.ExternalID
		}) {
			return store.ErrConflict
		}
		i.ID, i.CreatedAt = newID(i.ID), now
		d.trackers.identities.insert(*i)
		return nil
	})
}

func (r trackerIdentityRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.TrackerIdentity, error) {
	return getRow(r.s, trackerIDsT, accountID, id)
}

func (r trackerIdentityRepo) Find(_ context.Context, accountID, connectionID uuid.UUID, externalID string) (domain.TrackerIdentity, error) {
	return findRow(r.s, trackerIDsT, func(i domain.TrackerIdentity) bool {
		return i.AccountID == accountID && i.ConnectionID == connectionID && i.ExternalID == externalID
	})
}

func (r trackerIdentityRepo) List(_ context.Context, accountID, connectionID uuid.UUID) ([]domain.TrackerIdentity, error) {
	return listRows(r.s, trackerIDsT, 0, func(i domain.TrackerIdentity) bool {
		return i.AccountID == accountID && zeroOr(connectionID, i.ConnectionID)
	})
}

func (r trackerIdentityRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return remove(r.s, trackerIDsT, accountID, id)
}

// TrackerCursors implements store.Store.
func (s *Store) TrackerCursors() store.TrackerCursorRepo { return cursorRepo{s} }

type cursorRepo struct{ s *Store }

func (r cursorRepo) Get(_ context.Context, _, connectionID uuid.UUID) (string, error) {
	return read(r.s, func(d *storeData) (string, error) { return d.trackers.cursors[connectionID], nil })
}

func (r cursorRepo) Set(_ context.Context, accountID, connectionID uuid.UUID, cursor string) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if err := foreign(&d.trackers.connections, accountID, connectionID); err != nil {
			return err
		}
		d.trackers.cursors[connectionID] = cursor
		return nil
	})
}

// TicketLinks implements store.Store.
func (s *Store) TicketLinks() store.TicketLinkRepo { return linkRepo{s} }

type linkRepo struct{ s *Store }

// linkConflict reports a second live link of a ticket or a second link of a
// goal, as the Postgres indexes do.
func linkConflict(d *storeData, l domain.TicketLink) error {
	if d.trackers.links.exists(func(x domain.TicketLink) bool {
		if x.ID == l.ID {
			return false
		}
		sameTicket := x.ConnectionID == l.ConnectionID && x.Key == l.Key && x.Status.Live() && l.Status.Live()
		return sameTicket || (l.GoalID != uuid.Nil() && x.GoalID == l.GoalID)
	}) {
		return store.ErrConflict
	}
	return nil
}

func (r linkRepo) Create(_ context.Context, l *domain.TicketLink) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.trackers.connections, l.AccountID, l.ConnectionID); err != nil {
			return err
		}
		l.ID = newID(l.ID)
		if err := linkConflict(d, *l); err != nil {
			return err
		}
		l.CreatedAt, l.UpdatedAt = now, now
		d.trackers.links.insert(*l)
		return nil
	})
}

func (r linkRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.TicketLink, error) {
	return getRow(r.s, linksT, accountID, id)
}

func (r linkRepo) GetLive(_ context.Context, accountID, connectionID uuid.UUID, key string) (domain.TicketLink, error) {
	return findRow(r.s, linksT, func(l domain.TicketLink) bool {
		return l.AccountID == accountID && l.ConnectionID == connectionID && l.Key == key && l.Status.Live()
	})
}

func (r linkRepo) LastEnded(_ context.Context, accountID, connectionID uuid.UUID, key string) (domain.TicketLink, error) {
	ended, err := listRows(r.s, linksT, 0, func(l domain.TicketLink) bool {
		return l.AccountID == accountID && l.ConnectionID == connectionID && l.Key == key && !l.Status.Live()
	})
	if err != nil {
		return domain.TicketLink{}, err
	}
	if len(ended) == 0 {
		return domain.TicketLink{}, store.ErrNotFound
	}
	return ended[len(ended)-1], nil
}

func (r linkRepo) GetByGoal(_ context.Context, accountID, goalID uuid.UUID) (domain.TicketLink, error) {
	return findRow(r.s, linksT, func(l domain.TicketLink) bool { return l.AccountID == accountID && l.GoalID == goalID })
}

func (r linkRepo) GetByIssue(_ context.Context, accountID, projectID uuid.UUID, number int) (domain.TicketLink, error) {
	return findRow(r.s, linksT, func(l domain.TicketLink) bool {
		return l.AccountID == accountID && l.ProjectID == projectID && l.IssueNumber == number && l.Status.Live()
	})
}

func (r linkRepo) List(_ context.Context, accountID uuid.UUID, f store.TicketLinkFilter) ([]domain.TicketLink, error) {
	return listRows(r.s, linksT, f.Limit, func(l domain.TicketLink) bool {
		return l.AccountID == accountID && zeroOr(f.ConnectionID, l.ConnectionID) && zeroOr(f.ProjectID, l.ProjectID) &&
			zeroOr(f.Status, l.Status) && (!f.Live || l.Status.Live())
	})
}

func (r linkRepo) CountActive(ctx context.Context, accountID, projectID uuid.UUID) (int, error) {
	active, err := r.List(ctx, accountID, store.TicketLinkFilter{ProjectID: projectID, Status: domain.LinkActive})
	return len(active), err
}

func (r linkRepo) Update(_ context.Context, l *domain.TicketLink) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		cur, err := d.trackers.links.get(l.AccountID, l.ID)
		if err != nil {
			return err
		}
		next := *l
		next.ConnectionID, next.Key, next.CreatedAt, next.UpdatedAt = cur.ConnectionID, cur.Key, cur.CreatedAt, now
		if err := linkConflict(d, next); err != nil {
			return err
		}
		*cur, *l = next, next
		return nil
	})
}

// TrackerOutbox implements store.Store.
func (s *Store) TrackerOutbox() store.TrackerOutboxRepo { return outboxRepo{s} }

type outboxRepo struct{ s *Store }

func (r outboxRepo) Enqueue(_ context.Context, w *domain.TrackerWrite) (bool, error) {
	isNew := false
	err := r.s.do(func(d *storeData, now time.Time) error {
		if d.trackers.outbox.exists(func(x domain.TrackerWrite) bool { return x.LinkID == w.LinkID && x.DedupeKey == w.DedupeKey }) {
			return nil
		}
		w.ID, w.CreatedAt = newID(w.ID), now
		if w.NextAttempt.IsZero() {
			w.NextAttempt = now
		}
		d.trackers.outbox.insert(*w)
		isNew = true
		return nil
	})
	return isNew, err
}

func (r outboxRepo) Due(_ context.Context, accountID uuid.UUID, now time.Time, limit int) ([]domain.TrackerWrite, error) {
	return listRows(r.s, outboxT, limit, func(w domain.TrackerWrite) bool {
		return w.AccountID == accountID && w.SentAt.IsZero() && !w.NextAttempt.After(now)
	})
}

func (r outboxRepo) MarkSent(_ context.Context, accountID, id uuid.UUID, externalID string, at time.Time) error {
	return update(r.s, outboxT, accountID, id, func(w *domain.TrackerWrite, _ time.Time) error {
		w.SentAt, w.ExternalID, w.LastError = at, externalID, ""
		w.Attempts++
		return nil
	})
}

func (r outboxRepo) MarkFailed(_ context.Context, accountID, id uuid.UUID, reason string, next time.Time) error {
	return update(r.s, outboxT, accountID, id, func(w *domain.TrackerWrite, _ time.Time) error {
		w.LastError, w.NextAttempt = reason, next
		w.Attempts++
		return nil
	})
}

func (r outboxRepo) Sent(_ context.Context, accountID, linkID uuid.UUID, externalID string) (bool, error) {
	_, err := findRow(r.s, outboxT, func(w domain.TrackerWrite) bool {
		return w.AccountID == accountID && w.LinkID == linkID && w.ExternalID == externalID
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
