package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// TrackerConnections implements store.Store.
func (s *Store) TrackerConnections() store.TrackerConnectionRepo { return trackerConnectionRepo{s.db} }

// TrackerRoutes implements store.Store.
func (s *Store) TrackerRoutes() store.TrackerRouteRepo { return trackerRouteRepo{s.db} }

// TrackerIdentities implements store.Store.
func (s *Store) TrackerIdentities() store.TrackerIdentityRepo { return trackerIdentityRepo{s.db} }

// TrackerCursors implements store.Store.
func (s *Store) TrackerCursors() store.TrackerCursorRepo { return trackerCursorRepo{s.db} }

// TicketLinks implements store.Store.
func (s *Store) TicketLinks() store.TicketLinkRepo { return ticketLinkRepo{s.db} }

// TrackerOutbox implements store.Store.
func (s *Store) TrackerOutbox() store.TrackerOutboxRepo { return trackerOutboxRepo{s.db} }

// exactlyOne makes a statement that touched no row store.ErrNotFound.
func exactlyOne(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- connections

type trackerConnectionRepo struct{ db dbtx }

const trackerConnectionCols = `id, account_id, name, kind, config, triggers, created_at, updated_at`

func scanTrackerConnection(row pgx.Row) (c domain.TrackerConnection, err error) {
	var config, triggers []byte
	if err = row.Scan(&c.ID, &c.AccountID, &c.Name, &c.Kind, &config, &triggers, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return c, err
	}
	utc(&c.CreatedAt, &c.UpdatedAt)
	if err := json.Unmarshal(config, &c.Config); err != nil {
		return c, fmt.Errorf("decode tracker config: %w", err)
	}
	if err := json.Unmarshal(triggers, &c.Triggers); err != nil {
		return c, fmt.Errorf("decode tracker triggers: %w", err)
	}
	return c, nil
}

func (r trackerConnectionRepo) Create(ctx context.Context, c *domain.TrackerConnection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.ID = newID(c.ID)
	if c.Config == nil {
		c.Config = map[string]any{}
	}
	config, err := json.Marshal(c.Config)
	if err != nil {
		return fmt.Errorf("encode tracker config: %w", err)
	}
	triggers, err := json.Marshal(c.Triggers)
	if err != nil {
		return fmt.Errorf("encode tracker triggers: %w", err)
	}
	err = r.db.QueryRow(ctx, `INSERT INTO tracker_connections (id, account_id, name, kind, config, triggers)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at, updated_at`,
		c.ID, c.AccountID, c.Name, c.Kind, config, triggers).Scan(&c.CreatedAt, &c.UpdatedAt)
	utc(&c.CreatedAt, &c.UpdatedAt)
	return mapErr(err)
}

func (r trackerConnectionRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.TrackerConnection, error) {
	return one(ctx, r.db, scanTrackerConnection, `SELECT `+trackerConnectionCols+` FROM tracker_connections WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r trackerConnectionRepo) GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.TrackerConnection, error) {
	return one(ctx, r.db, scanTrackerConnection, `SELECT `+trackerConnectionCols+` FROM tracker_connections WHERE account_id = $1 AND name = $2`, accountID, name)
}

func (r trackerConnectionRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.TrackerConnection, error) {
	return many(ctx, r.db, scanTrackerConnection, `SELECT `+trackerConnectionCols+` FROM tracker_connections WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r trackerConnectionRepo) Find(ctx context.Context, id uuid.UUID) (domain.TrackerConnection, error) {
	return one(ctx, r.db, scanTrackerConnection, `SELECT `+trackerConnectionCols+` FROM tracker_connections WHERE id = $1`, id)
}

func (r trackerConnectionRepo) ListByKind(ctx context.Context, kind string) ([]domain.TrackerConnection, error) {
	return many(ctx, r.db, scanTrackerConnection, `SELECT `+trackerConnectionCols+` FROM tracker_connections WHERE kind = $1 ORDER BY created_at, id`, kind)
}

func (r trackerConnectionRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return inTx(ctx, r.db, func(tx dbtx) error {
		// The row lock waits for a link being made and keeps new ones out.
		// Unsent writes of ended links count too: deleting the connection
		// would drop them.
		var live, unsent bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ticket_links WHERE connection_id = c.id AND status IN ('waiting', 'queued', 'active')),
				EXISTS (SELECT 1 FROM tracker_outbox WHERE connection_id = c.id AND sent_at IS NULL)
			FROM tracker_connections c WHERE c.account_id = $1 AND c.id = $2 FOR UPDATE`, accountID, id).Scan(&live, &unsent)
		if err != nil {
			return mapErr(err)
		}
		if live {
			return fmt.Errorf("tracker connection %s has live tickets: %w", id, store.ErrConflict)
		}
		if unsent {
			return fmt.Errorf("tracker connection %s has writes not sent yet: %w", id, store.ErrConflict)
		}
		return exactlyOne(tx.Exec(ctx, `DELETE FROM tracker_connections WHERE account_id = $1 AND id = $2`, accountID, id))
	})
}

// ---- routes

type trackerRouteRepo struct{ db dbtx }

var trackerRouteCols = `id, account_id, connection_id, position, match, project_id, ` + ctext("goal_kind") + `, ` + ctext("playbook") + `, states, created_at, updated_at`

func scanTrackerRoute(row pgx.Row) (t domain.TrackerRoute, err error) {
	var match, states []byte
	if err = row.Scan(&t.ID, &t.AccountID, &t.ConnectionID, &t.Position, &match, &t.ProjectID, &t.GoalKind, &t.Playbook, &states, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	utc(&t.CreatedAt, &t.UpdatedAt)
	if err := json.Unmarshal(match, &t.Match); err != nil {
		return t, fmt.Errorf("decode route match: %w", err)
	}
	if err := json.Unmarshal(states, &t.States); err != nil {
		return t, fmt.Errorf("decode route states: %w", err)
	}
	return t, nil
}

func (r trackerRouteRepo) Create(ctx context.Context, t *domain.TrackerRoute) error {
	if err := t.Validate(); err != nil {
		return err
	}
	t.ID = newID(t.ID)
	if t.States == nil {
		t.States = map[domain.TicketState]string{}
	}
	match, err := json.Marshal(t.Match)
	if err != nil {
		return fmt.Errorf("encode route match: %w", err)
	}
	states, err := json.Marshal(t.States)
	if err != nil {
		return fmt.Errorf("encode route states: %w", err)
	}
	err = r.db.QueryRow(ctx, `INSERT INTO tracker_routes (id, account_id, connection_id, position, match, project_id, goal_kind, playbook, states)
		VALUES ($1, $2, $3, CASE WHEN $4 > 0 THEN $4 ELSE (SELECT COALESCE(max(position), 0) + 1 FROM tracker_routes WHERE connection_id = $3) END, $5, $6, $7, $8, $9)
		RETURNING position, created_at, updated_at`,
		t.ID, t.AccountID, t.ConnectionID, t.Position, match, t.ProjectID, opt(string(t.GoalKind)), opt(t.Playbook), states).Scan(&t.Position, &t.CreatedAt, &t.UpdatedAt)
	utc(&t.CreatedAt, &t.UpdatedAt)
	return mapErr(err)
}

func (r trackerRouteRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.TrackerRoute, error) {
	return one(ctx, r.db, scanTrackerRoute, `SELECT `+trackerRouteCols+` FROM tracker_routes WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r trackerRouteRepo) List(ctx context.Context, accountID, connectionID uuid.UUID) ([]domain.TrackerRoute, error) {
	return many(ctx, r.db, scanTrackerRoute, `SELECT `+trackerRouteCols+` FROM tracker_routes
		WHERE account_id = $1 AND ($2::uuid IS NULL OR connection_id = $2) ORDER BY connection_id, position, created_at, id`, accountID, opt(connectionID))
}

func (r trackerRouteRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return exactlyOne(r.db.Exec(ctx, `DELETE FROM tracker_routes WHERE account_id = $1 AND id = $2`, accountID, id))
}

// ---- identities

type trackerIdentityRepo struct{ db dbtx }

const trackerIdentityCols = `id, account_id, connection_id, external_id, external_name, user_id, created_by, created_at`

func scanTrackerIdentity(row pgx.Row) (i domain.TrackerIdentity, err error) {
	err = row.Scan(&i.ID, &i.AccountID, &i.ConnectionID, &i.ExternalID, &i.ExternalName, &i.UserID, &i.CreatedBy, &i.CreatedAt)
	utc(&i.CreatedAt)
	return i, err
}

func (r trackerIdentityRepo) Create(ctx context.Context, i *domain.TrackerIdentity) error {
	i.ID = newID(i.ID)
	err := r.db.QueryRow(ctx, `INSERT INTO tracker_identities (id, account_id, connection_id, external_id, external_name, user_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		i.ID, i.AccountID, i.ConnectionID, i.ExternalID, i.ExternalName, i.UserID, i.CreatedBy).Scan(&i.CreatedAt)
	utc(&i.CreatedAt)
	return mapErr(err)
}

func (r trackerIdentityRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.TrackerIdentity, error) {
	return one(ctx, r.db, scanTrackerIdentity, `SELECT `+trackerIdentityCols+` FROM tracker_identities WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r trackerIdentityRepo) Find(ctx context.Context, accountID, connectionID uuid.UUID, externalID string) (domain.TrackerIdentity, error) {
	return one(ctx, r.db, scanTrackerIdentity, `SELECT `+trackerIdentityCols+` FROM tracker_identities
		WHERE account_id = $1 AND connection_id = $2 AND external_id = $3`, accountID, connectionID, externalID)
}

func (r trackerIdentityRepo) List(ctx context.Context, accountID, connectionID uuid.UUID) ([]domain.TrackerIdentity, error) {
	return many(ctx, r.db, scanTrackerIdentity, `SELECT `+trackerIdentityCols+` FROM tracker_identities
		WHERE account_id = $1 AND ($2::uuid IS NULL OR connection_id = $2) ORDER BY created_at, id`, accountID, opt(connectionID))
}

func (r trackerIdentityRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return exactlyOne(r.db.Exec(ctx, `DELETE FROM tracker_identities WHERE account_id = $1 AND id = $2`, accountID, id))
}

// ---- cursors

type trackerCursorRepo struct{ db dbtx }

func (r trackerCursorRepo) Get(ctx context.Context, accountID, connectionID uuid.UUID) (string, error) {
	var cursor string
	err := r.db.QueryRow(ctx, `SELECT cursor FROM tracker_cursors WHERE account_id = $1 AND connection_id = $2`, accountID, connectionID).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return cursor, mapErr(err)
}

func (r trackerCursorRepo) Set(ctx context.Context, accountID, connectionID uuid.UUID, cursor string) error {
	_, err := r.db.Exec(ctx, `INSERT INTO tracker_cursors (connection_id, account_id, cursor) VALUES ($2, $1, $3)
		ON CONFLICT (connection_id) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = now()`, accountID, connectionID, cursor)
	return mapErr(err)
}

// ---- ticket links

type ticketLinkRepo struct{ db dbtx }

var ticketLinkCols = `id, account_id, ` + cuuid("connection_id") + `, key, title, url, status, ` + ctext("reason") + `, ` + cuuid("project_id") + `, ` + cuuid("route_id") + `, ` +
	cuuid("goal_id") + `, handed_by, ` + cuuid("user_id") + `, ` + cuuid("gate_id") + `, ` + cuuid("change_gate_id") + `, ` + ctext("state") +
	`, content_hash, comment_cursor, ` + cint("issue_number") + `, created_at, updated_at`

func scanTicketLink(row pgx.Row) (l domain.TicketLink, err error) {
	err = row.Scan(&l.ID, &l.AccountID, &l.ConnectionID, &l.Key, &l.Title, &l.URL, &l.Status, &l.Reason, &l.ProjectID, &l.RouteID,
		&l.GoalID, &l.HandedBy, &l.UserID, &l.GateID, &l.ChangeGateID, &l.State, &l.ContentHash, &l.CommentCursor, &l.IssueNumber, &l.CreatedAt, &l.UpdatedAt)
	utc(&l.CreatedAt, &l.UpdatedAt)
	return l, err
}

func (r ticketLinkRepo) Create(ctx context.Context, l *domain.TicketLink) error {
	l.ID = newID(l.ID)
	err := r.db.QueryRow(ctx, `INSERT INTO ticket_links (id, account_id, connection_id, key, title, url, status, reason, project_id, route_id,
		goal_id, handed_by, user_id, gate_id, change_gate_id, state, content_hash, comment_cursor, issue_number)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19) RETURNING created_at, updated_at`,
		l.ID, l.AccountID, l.ConnectionID, l.Key, l.Title, l.URL, string(l.Status), opt(string(l.Reason)), opt(l.ProjectID), opt(l.RouteID),
		opt(l.GoalID), l.HandedBy, opt(l.UserID), opt(l.GateID), opt(l.ChangeGateID), opt(string(l.State)), l.ContentHash, l.CommentCursor, opt(l.IssueNumber)).Scan(&l.CreatedAt, &l.UpdatedAt)
	utc(&l.CreatedAt, &l.UpdatedAt)
	return mapErr(err)
}

func (r ticketLinkRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.TicketLink, error) {
	return one(ctx, r.db, scanTicketLink, `SELECT `+ticketLinkCols+` FROM ticket_links WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r ticketLinkRepo) GetLive(ctx context.Context, accountID, connectionID uuid.UUID, key string) (domain.TicketLink, error) {
	return one(ctx, r.db, scanTicketLink, `SELECT `+ticketLinkCols+` FROM ticket_links
		WHERE account_id = $1 AND connection_id = $2 AND key = $3 AND status IN ('waiting', 'queued', 'active')`, accountID, connectionID, key)
}

func (r ticketLinkRepo) LastEnded(ctx context.Context, accountID, connectionID uuid.UUID, key string) (domain.TicketLink, error) {
	return one(ctx, r.db, scanTicketLink, `SELECT `+ticketLinkCols+` FROM ticket_links
		WHERE account_id = $1 AND connection_id = $2 AND key = $3 AND status NOT IN ('waiting', 'queued', 'active')
		ORDER BY updated_at DESC, id DESC LIMIT 1`, accountID, connectionID, key)
}

func (r ticketLinkRepo) GetByGoal(ctx context.Context, accountID, goalID uuid.UUID) (domain.TicketLink, error) {
	return one(ctx, r.db, scanTicketLink, `SELECT `+ticketLinkCols+` FROM ticket_links WHERE account_id = $1 AND goal_id = $2`, accountID, goalID)
}

func (r ticketLinkRepo) GetByIssue(ctx context.Context, accountID, projectID uuid.UUID, number int) (domain.TicketLink, error) {
	return one(ctx, r.db, scanTicketLink, `SELECT `+ticketLinkCols+` FROM ticket_links
		WHERE account_id = $1 AND project_id = $2 AND issue_number = $3 AND status IN ('waiting', 'queued', 'active')`, accountID, projectID, number)
}

func (r ticketLinkRepo) List(ctx context.Context, accountID uuid.UUID, f store.TicketLinkFilter) ([]domain.TicketLink, error) {
	w := newWhere(accountID)
	andNot(w, "connection_id = ?", f.ConnectionID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "status = ?", string(f.Status))
	if f.Live {
		w.conds = append(w.conds, `status IN ('waiting', 'queued', 'active')`)
	}
	return many(ctx, r.db, scanTicketLink, `SELECT `+ticketLinkCols+` FROM ticket_links`+w.sql("created_at, id", f.Limit), w.args...)
}

func (r ticketLinkRepo) CountActive(ctx context.Context, accountID, projectID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM ticket_links WHERE account_id = $1 AND project_id = $2 AND status = 'active'`, accountID, projectID).Scan(&n)
	return n, mapErr(err)
}

func (r ticketLinkRepo) Update(ctx context.Context, l *domain.TicketLink) error {
	err := r.db.QueryRow(ctx, `UPDATE ticket_links SET title = $3, url = $4, status = $5, reason = $6, project_id = $7, route_id = $8, goal_id = $9,
		handed_by = $10, user_id = $11, gate_id = $12, change_gate_id = $13, state = $14, content_hash = $15, comment_cursor = $16, issue_number = $17, updated_at = now()
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		l.AccountID, l.ID, l.Title, l.URL, string(l.Status), opt(string(l.Reason)), opt(l.ProjectID), opt(l.RouteID), opt(l.GoalID),
		l.HandedBy, opt(l.UserID), opt(l.GateID), opt(l.ChangeGateID), opt(string(l.State)), l.ContentHash, l.CommentCursor, opt(l.IssueNumber)).Scan(&l.UpdatedAt)
	utc(&l.UpdatedAt)
	return mapErr(err)
}

// ---- outbox

type trackerOutboxRepo struct{ db dbtx }

var trackerWriteCols = `id, account_id, connection_id, link_id, key, dedupe_key, kind, value, attempts, next_attempt_at, ` + ctime("sent_at") + `, external_id, last_error, created_at`

func scanTrackerWrite(row pgx.Row) (w domain.TrackerWrite, err error) {
	err = row.Scan(&w.ID, &w.AccountID, &w.ConnectionID, &w.LinkID, &w.Key, &w.DedupeKey, &w.Kind, &w.Value, &w.Attempts, &w.NextAttempt, &w.SentAt, &w.ExternalID, &w.LastError, &w.CreatedAt)
	utc(&w.NextAttempt, &w.SentAt, &w.CreatedAt)
	return w, err
}

func (r trackerOutboxRepo) Enqueue(ctx context.Context, w *domain.TrackerWrite) (bool, error) {
	w.ID = newID(w.ID)
	if w.NextAttempt.IsZero() {
		w.NextAttempt = time.Now().UTC()
	}
	err := r.db.QueryRow(ctx, `INSERT INTO tracker_outbox (id, account_id, connection_id, link_id, key, dedupe_key, kind, value, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (link_id, dedupe_key) DO NOTHING RETURNING created_at`,
		w.ID, w.AccountID, w.ConnectionID, w.LinkID, w.Key, w.DedupeKey, string(w.Kind), w.Value, w.NextAttempt).Scan(&w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	utc(&w.CreatedAt)
	return err == nil, mapErr(err)
}

func (r trackerOutboxRepo) DropTransitions(ctx context.Context, accountID, linkID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM tracker_outbox WHERE account_id = $1 AND link_id = $2 AND kind = 'transition' AND sent_at IS NULL`, accountID, linkID)
	return mapErr(err)
}

func (r trackerOutboxRepo) DiscardEnded(ctx context.Context, accountID, connectionID uuid.UUID) ([]domain.TrackerWrite, error) {
	return many(ctx, r.db, scanTrackerWrite, `DELETE FROM tracker_outbox WHERE id IN (
			SELECT o.id FROM tracker_outbox o JOIN ticket_links l ON l.id = o.link_id
			WHERE o.account_id = $1 AND o.connection_id = $2 AND o.sent_at IS NULL AND l.status NOT IN ('waiting', 'queued', 'active'))
		RETURNING `+trackerWriteCols, accountID, connectionID)
}

func (r trackerOutboxRepo) Due(ctx context.Context, accountID uuid.UUID, now time.Time, limit int) ([]domain.TrackerWrite, error) {
	return many(ctx, r.db, scanTrackerWrite, `SELECT `+trackerWriteCols+` FROM tracker_outbox
		WHERE account_id = $1 AND sent_at IS NULL AND next_attempt_at <= $2 ORDER BY created_at, id LIMIT $3`, accountID, now, limit)
}

func (r trackerOutboxRepo) MarkSent(ctx context.Context, accountID, id uuid.UUID, externalID string, at time.Time) error {
	return exactlyOne(r.db.Exec(ctx, `UPDATE tracker_outbox SET sent_at = $3, external_id = $4, attempts = attempts + 1, last_error = ''
		WHERE account_id = $1 AND id = $2`, accountID, id, at, externalID))
}

func (r trackerOutboxRepo) MarkFailed(ctx context.Context, accountID, id uuid.UUID, reason string, next time.Time) error {
	return exactlyOne(r.db.Exec(ctx, `UPDATE tracker_outbox SET attempts = attempts + 1, last_error = $3, next_attempt_at = $4
		WHERE account_id = $1 AND id = $2`, accountID, id, reason, next))
}

func (r trackerOutboxRepo) Sent(ctx context.Context, accountID, linkID uuid.UUID, externalID string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tracker_outbox WHERE account_id = $1 AND link_id = $2 AND external_id = $3)`, accountID, linkID, externalID).Scan(&ok)
	return ok, mapErr(err)
}
