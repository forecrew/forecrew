package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/store"
)

// Zero values stand in for NULL when reading, so rows scan straight into
// domain types (the domain uses zero UUIDs, times and strings for unset).
const (
	zeroUUID = `'00000000-0000-0000-0000-000000000000'::uuid`
	zeroTime = `'0001-01-01 00:00:00+00'::timestamptz`
)

func cuuid(col string) string { return "COALESCE(" + col + ", " + zeroUUID + ")" }
func ctime(col string) string { return "COALESCE(" + col + ", " + zeroTime + ")" }
func ctext(col string) string { return "COALESCE(" + col + ", '')" }
func cint(col string) string  { return "COALESCE(" + col + ", 0)" }

// opt returns nil (NULL) for the zero value of v.
func opt[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func optTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func optJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// utc converts scanned times to UTC (pgx returns the local zone).
func utc(ts ...*time.Time) {
	for _, t := range ts {
		*t = t.UTC()
	}
}

type scanFn[T any] func(row pgx.Row) (T, error)

func one[T any](ctx context.Context, db dbtx, scan scanFn[T], q string, args ...any) (T, error) {
	v, err := scan(db.QueryRow(ctx, q, args...))
	return v, mapErr(err)
}

func many[T any](ctx context.Context, db dbtx, scan scanFn[T], q string, args ...any) ([]T, error) {
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
	if err != nil {
		return nil, mapErr(err)
	}
	if out == nil {
		out = []T{}
	}
	return out, nil
}

// where builds a WHERE clause; every list starts with the account filter.
type where struct {
	conds []string
	args  []any
}

func newWhere(accountID uuid.UUID) *where {
	return (&where{}).and("account_id = ?", accountID)
}

// and adds cond, whose single ? is replaced by the next placeholder.
func (w *where) and(cond string, arg any) *where {
	w.args = append(w.args, arg)
	w.conds = append(w.conds, strings.Replace(cond, "?", "$"+strconv.Itoa(len(w.args)), 1))
	return w
}

// andNot adds cond only when v is not the zero value.
func andNot[T comparable](w *where, cond string, v T) {
	var zero T
	if v != zero {
		w.and(cond, v)
	}
}

func (w *where) sql(order string, limit int) string {
	s := " WHERE " + strings.Join(w.conds, " AND ") + " ORDER BY " + order
	if limit > 0 {
		s += " LIMIT " + strconv.Itoa(limit)
	}
	return s
}

// inTx runs fn in a transaction (a savepoint when already in one), so a
// ref number is only consumed by an insert that commits.
func inTx(ctx context.Context, db dbtx, fn func(tx dbtx) error) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return fn(tx) })
}

// updateStatus is the compare-and-set shared by every UpdateStatus. The
// domain transition is checked first; table must be a constant.
func updateStatus[S ~string](ctx context.Context, db dbtx, table string, accountID, id uuid.UUID, from, to S, transition func(S, S) error) error {
	if err := transition(from, to); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, "UPDATE "+table+" SET status = $4, updated_at = now() WHERE account_id = $1 AND id = $2 AND status = $3",
		accountID, id, string(from), string(to))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var current string
	err = db.QueryRow(ctx, "SELECT status FROM "+table+" WHERE account_id = $1 AND id = $2", accountID, id).Scan(&current)
	if err != nil {
		return fmt.Errorf("%s %s: %w", table, id, mapErr(err))
	}
	return fmt.Errorf("%s %s is %q, not %q: %w", table, id, current, from, store.ErrConflict)
}

// transition adapts a domain Transition method to updateStatus.
func transition[S interface {
	~string
	Transition(S) (S, error)
}](from, to S) error {
	_, err := from.Transition(to)
	return err
}

// ref names a row an insert points at; a zero ID means "not set".
type ref struct {
	table string // a constant
	id    uuid.UUID
}

// owned checks that every referenced row belongs to accountID. Foreign keys
// only prove a row exists, so without this a caller could attach its rows to
// another account's project. A missing or foreign row is ErrNotFound.
func owned(ctx context.Context, db dbtx, accountID uuid.UUID, refs ...ref) error {
	for _, r := range refs {
		if r.id == uuid.Nil() {
			continue
		}
		var ok bool
		err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+r.table+" WHERE account_id = $1 AND id = $2)", accountID, r.id).Scan(&ok)
		if err != nil {
			return mapErr(err)
		}
		if !ok {
			return fmt.Errorf("%s %s: %w", r.table, r.id, store.ErrNotFound)
		}
	}
	return nil
}
