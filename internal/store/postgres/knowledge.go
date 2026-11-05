package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// vectorColumn remembers that knowledge_entries.embedding exists. It exists
// only where pgvector does (migration 00004), so it is looked up when first
// needed and then kept; a missing column is looked up again, because an
// administrator may add it later.
type vectorColumn struct{ found atomic.Bool }

func (v *vectorColumn) exists(ctx context.Context, db dbtx) (bool, error) {
	if v.found.Load() {
		return true, nil
	}
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'knowledge_entries' AND column_name = 'embedding')`).Scan(&ok)
	if err != nil {
		return false, mapErr(err)
	}
	if ok {
		v.found.Store(true)
	}
	return ok, nil
}

// vectorLiteral is the text form pgvector reads: [0.1,0.2].
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

type knowledgeRepo struct {
	db  dbtx
	vec *vectorColumn
}

var knowledgeCols = `id, account_id, project_id, provider, path, source_url, version, section, title, body, body_hash, provenance,
	generated, needs_reverify, reverify_reason, ` + ctext("scope") + `, ` + cuuid("goal_id") + `, ` + ctime("expires_at") + `, tags,
	synced_at, created_at, updated_at`

func scanKnowledge(row pgx.Row) (e domain.KnowledgeEntry, err error) {
	var prov []byte
	err = row.Scan(&e.ID, &e.AccountID, &e.ProjectID, &e.Provider, &e.Path, &e.SourceURL, &e.Version, &e.Section, &e.Title, &e.Body,
		&e.BodyHash, &prov, &e.Generated, &e.NeedsReverify, &e.ReverifyReason, &e.Scope, &e.GoalID, &e.ExpiresAt, &e.Tags,
		&e.SyncedAt, &e.CreatedAt, &e.UpdatedAt)
	utc(&e.SyncedAt, &e.CreatedAt, &e.UpdatedAt, &e.ExpiresAt)
	if err != nil {
		return e, err
	}
	if len(e.Tags) == 0 {
		e.Tags = nil
	}
	if err := json.Unmarshal(prov, &e.Provenance); err != nil {
		return domain.KnowledgeEntry{}, fmt.Errorf("decode provenance of %s: %w", e.Path, err)
	}
	return e, nil
}

// Upsert implements store.KnowledgeRepo. A changed title or body drops the
// stored embedding, which is stale then.
func (r knowledgeRepo) Upsert(ctx context.Context, e *domain.KnowledgeEntry) error {
	if e.BodyHash == "" {
		sum := sha256.Sum256([]byte(e.Body))
		e.BodyHash = hex.EncodeToString(sum[:])
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if err := owned(ctx, r.db, e.AccountID, ref{"projects", e.ProjectID}, ref{"goals", e.GoalID}); err != nil {
		return err
	}
	prov, err := json.Marshal(e.Provenance)
	if err != nil {
		return fmt.Errorf("encode provenance: %w", err)
	}
	hasVec, err := r.vec.exists(ctx, r.db)
	if err != nil {
		return err
	}
	stale := ""
	if hasVec {
		stale = `, embedding = CASE WHEN knowledge_entries.body_hash = EXCLUDED.body_hash AND knowledge_entries.title = EXCLUDED.title
			THEN knowledge_entries.embedding ELSE NULL END`
	}
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	e.ID = newID(e.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO knowledge_entries (id, account_id, project_id, provider, path, source_url, version, section,
			title, body, body_hash, provenance, generated, needs_reverify, reverify_reason, scope, goal_id, expires_at, tags)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (account_id, project_id, provider, path) DO UPDATE SET source_url = EXCLUDED.source_url, version = EXCLUDED.version,
			section = EXCLUDED.section, title = EXCLUDED.title, body = EXCLUDED.body, body_hash = EXCLUDED.body_hash,
			provenance = EXCLUDED.provenance, generated = EXCLUDED.generated, needs_reverify = EXCLUDED.needs_reverify,
			reverify_reason = EXCLUDED.reverify_reason, scope = EXCLUDED.scope, goal_id = EXCLUDED.goal_id,
			expires_at = EXCLUDED.expires_at, tags = EXCLUDED.tags, synced_at = now(), updated_at = now()`+stale+`
		RETURNING id, synced_at, created_at, updated_at`,
		e.ID, e.AccountID, e.ProjectID, e.Provider, e.Path, e.SourceURL, e.Version, e.Section, e.Title, e.Body, e.BodyHash, prov,
		e.Generated, e.NeedsReverify, e.ReverifyReason, opt(string(e.Scope)), opt(e.GoalID), optTime(e.ExpiresAt), tags,
	).Scan(&e.ID, &e.SyncedAt, &e.CreatedAt, &e.UpdatedAt))
}

func (r knowledgeRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.KnowledgeEntry, error) {
	return one(ctx, r.db, scanKnowledge, `SELECT `+knowledgeCols+` FROM knowledge_entries WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r knowledgeRepo) GetByPath(ctx context.Context, accountID, projectID uuid.UUID, provider, path string) (domain.KnowledgeEntry, error) {
	return one(ctx, r.db, scanKnowledge, `SELECT `+knowledgeCols+` FROM knowledge_entries
		WHERE account_id = $1 AND project_id = $2 AND provider = $3 AND path = $4`, accountID, projectID, provider, path)
}

func (r knowledgeRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `DELETE FROM knowledge_entries WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r knowledgeRepo) List(ctx context.Context, accountID uuid.UUID, f store.KnowledgeFilter) ([]domain.KnowledgeEntry, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "provider = ?", f.Provider)
	andNot(w, "section = ?", f.Section)
	if f.NeedsReverify {
		w.conds = append(w.conds, "needs_reverify")
	}
	return many(ctx, r.db, scanKnowledge, `SELECT `+knowledgeCols+` FROM knowledge_entries`+w.sql("section, path", f.Limit), w.args...)
}

func (r knowledgeRepo) SetEmbedding(ctx context.Context, accountID, id uuid.UUID, embedding []float32) error {
	ok, err := r.vec.exists(ctx, r.db)
	if err != nil {
		return err
	}
	if !ok {
		return errors.ErrUnsupported
	}
	return mustUpdate(ctx, r.db, `UPDATE knowledge_entries SET embedding = $3::vector WHERE account_id = $1 AND id = $2`,
		accountID, id, vectorLiteral(embedding))
}

func (r knowledgeRepo) SupportsEmbeddings(ctx context.Context) (bool, error) {
	return r.vec.exists(ctx, r.db)
}

func (r knowledgeRepo) WithoutEmbedding(ctx context.Context, accountID, projectID uuid.UUID, limit int) ([]domain.KnowledgeEntry, error) {
	ok, err := r.vec.exists(ctx, r.db)
	if err != nil || !ok {
		return nil, err
	}
	return many(ctx, r.db, scanKnowledge, `SELECT `+knowledgeCols+` FROM knowledge_entries
		WHERE account_id = $1 AND project_id = $2 AND embedding IS NULL ORDER BY updated_at, path LIMIT $3`, accountID, projectID, limit)
}

func (r knowledgeRepo) MarkReverify(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID, reason string) error {
	_, err := r.db.Exec(ctx, `UPDATE knowledge_entries SET needs_reverify = true, reverify_reason = $3, updated_at = now()
		WHERE account_id = $1 AND id = ANY($2)`, accountID, ids, reason)
	return mapErr(err)
}

func (r knowledgeRepo) DeleteExpired(ctx context.Context, accountID uuid.UUID, t time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM knowledge_entries WHERE account_id = $1 AND provider = 'internal' AND expires_at < $2`, accountID, t.UTC())
	return int(tag.RowsAffected()), mapErr(err)
}

func (r knowledgeRepo) ExpireGoalNotes(ctx context.Context, accountID uuid.UUID, grace time.Duration) (int, error) {
	tag, err := r.db.Exec(ctx, `UPDATE knowledge_entries k SET expires_at = g.updated_at + make_interval(secs => $2)
		FROM goals g WHERE k.goal_id = g.id AND k.account_id = $1 AND g.account_id = $1 AND k.expires_at IS NULL
			AND g.status IN ('done', 'canceled')`, accountID, grace.Seconds())
	return int(tag.RowsAffected()), mapErr(err)
}

// searchFilters returns the WHERE clause that narrows a search to the
// project, sections, providers, kinds and the notes the searcher may see.
func searchFilters(accountID uuid.UUID, q store.KnowledgeQuery) *where {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", q.ProjectID)
	if len(q.Sections) > 0 {
		w.and("section = ANY(?)", q.Sections)
	}
	if len(q.Providers) > 0 {
		w.and("provider = ANY(?)", q.Providers)
	}
	if len(q.Kinds) > 0 {
		var kinds []string
		for _, k := range q.Kinds {
			switch k {
			case domain.KnowledgeNoteKind:
				kinds = append(kinds, "provider = 'internal'")
			case domain.KnowledgeGeneratedKind:
				kinds = append(kinds, "generated")
			case domain.KnowledgeEntryKind:
				kinds = append(kinds, "(provider <> 'internal' AND NOT generated)")
			}
		}
		w.conds = append(w.conds, "("+strings.Join(kinds, " OR ")+")")
	}
	switch {
	case q.GoalOnly:
		w.and("provider = 'internal' AND scope = 'goal' AND goal_id = ?", q.GoalID)
	case q.AllNotes:
	case q.GoalID == uuid.Nil():
		w.conds = append(w.conds, "(provider <> 'internal' OR scope = 'project')")
	default:
		w.and("(provider <> 'internal' OR scope = 'project' OR goal_id = ?)", q.GoalID)
	}
	return w
}

// Search implements store.KnowledgeRepo; see knowledge.md, Search, and
// knowledge.Index for the ranking.
func (r knowledgeRepo) Search(ctx context.Context, accountID uuid.UUID, q store.KnowledgeQuery) ([]store.KnowledgeHit, error) {
	w := searchFilters(accountID, q)
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if strings.TrimSpace(q.Text) == "" {
		order := "section, path"
		if q.GoalOnly {
			order = "created_at DESC, path"
		}
		return many(ctx, r.db, func(row pgx.Row) (store.KnowledgeHit, error) {
			e, err := scanKnowledge(row)
			return store.KnowledgeHit{Entry: e, Snippet: firstRunes(e.Body, 240)}, err
		}, `SELECT `+knowledgeCols+` FROM knowledge_entries`+w.sql(order, limit), w.args...)
	}
	hasVec := false
	if len(q.Embedding) > 0 {
		var err error
		if hasVec, err = r.vec.exists(ctx, r.db); err != nil {
			return nil, err
		}
	}
	w.args = append(w.args, q.Text)
	tsq := "websearch_to_tsquery('english', $" + strconv.Itoa(len(w.args)) + ")"
	rank, match := "ts_rank(search, "+tsq+", 32)", "search @@ "+tsq
	if hasVec {
		w.args = append(w.args, vectorLiteral(q.Embedding))
		rank, match = mixedRank(rank, match, "$"+strconv.Itoa(len(w.args))+"::vector")
	}
	w.conds = append(w.conds, match)
	return many(ctx, r.db, scanHit, `SELECT `+knowledgeCols+`, `+rank+` AS rank,
			ts_headline('english', body, `+tsq+`, 'MaxWords=40, MinWords=15, MaxFragments=1')
		FROM knowledge_entries`+w.sql("rank DESC, updated_at DESC, path", limit), w.args...)
}

// mixedRank adds the cosine similarity to emb (a vector placeholder) to
// the rank and the match of a text search (store.SemanticWeight).
func mixedRank(rank, match, emb string) (mixedRank, mixedMatch string) {
	weight := strconv.FormatFloat(store.SemanticWeight, 'f', -1, 64)
	rest := strconv.FormatFloat(1-store.SemanticWeight, 'f', -1, 64)
	floor := strconv.FormatFloat(store.SemanticFloor, 'f', -1, 64)
	sim := "greatest(0, 1 - (embedding <=> " + emb + "))"
	mixedRank = "CASE WHEN embedding IS NULL THEN " + rank + " ELSE " + rest + " * " + rank + " + " + weight + " * " + sim + " END"
	mixedMatch = "(" + match + " OR (embedding IS NOT NULL AND 1 - (embedding <=> " + emb + ") >= " + floor + "))"
	return mixedRank, mixedMatch
}

func scanHit(row pgx.Row) (store.KnowledgeHit, error) {
	var h store.KnowledgeHit
	var prov []byte
	e := &h.Entry
	err := row.Scan(&e.ID, &e.AccountID, &e.ProjectID, &e.Provider, &e.Path, &e.SourceURL, &e.Version, &e.Section, &e.Title, &e.Body,
		&e.BodyHash, &prov, &e.Generated, &e.NeedsReverify, &e.ReverifyReason, &e.Scope, &e.GoalID, &e.ExpiresAt, &e.Tags,
		&e.SyncedAt, &e.CreatedAt, &e.UpdatedAt, &h.Rank, &h.Snippet)
	utc(&e.SyncedAt, &e.CreatedAt, &e.UpdatedAt, &e.ExpiresAt)
	if err != nil {
		return h, err
	}
	if len(e.Tags) == 0 {
		e.Tags = nil
	}
	if err := json.Unmarshal(prov, &e.Provenance); err != nil {
		return store.KnowledgeHit{}, fmt.Errorf("decode provenance of %s: %w", e.Path, err)
	}
	return h, nil
}

// firstRunes returns the first n runes of s.
func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// KnowledgeProposals implements store.Store.
func (s *Store) KnowledgeProposals() store.KnowledgeProposalRepo { return knowledgeProposalRepo{s.db} }

type knowledgeProposalRepo struct{ db dbtx }

var knowledgeProposalCols = `id, account_id, project_id, number, provider, section, path, action, body, reason, proposed_by,
	` + cuuid("job_id") + `, ` + cuuid("goal_id") + `, batch, external_ref, ` + cuuid("change_id") + `, ` + cuuid("gate_id") + `, status, created_at, updated_at`

func scanKnowledgeProposal(row pgx.Row) (k domain.KnowledgeProposal, err error) {
	err = row.Scan(&k.ID, &k.AccountID, &k.ProjectID, &k.Number, &k.Provider, &k.Section, &k.Path, &k.Action, &k.Body, &k.Reason, &k.ProposedBy,
		&k.JobID, &k.GoalID, &k.Batch, &k.ExternalRef, &k.ChangeID, &k.GateID, &k.Status, &k.CreatedAt, &k.UpdatedAt)
	utc(&k.CreatedAt, &k.UpdatedAt)
	return k, err
}

func (r knowledgeProposalRepo) Create(ctx context.Context, k *domain.KnowledgeProposal) error {
	k.ID = newID(k.ID)
	return inTx(ctx, r.db, func(tx dbtx) error {
		refs := []ref{{"projects", k.ProjectID}, {"jobs", k.JobID}, {"goals", k.GoalID}, {"changes", k.ChangeID}, {"gates", k.GateID}}
		if err := owned(ctx, tx, k.AccountID, refs...); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, k.AccountID, string(domain.RefKnowledge))
		if err != nil {
			return mapErr(err)
		}
		k.Number = n
		err = tx.QueryRow(ctx, `INSERT INTO knowledge_proposals (id, account_id, project_id, number, provider, section, path, action, body, reason,
				proposed_by, job_id, goal_id, batch, external_ref, change_id, gate_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) RETURNING created_at, updated_at`,
			k.ID, k.AccountID, k.ProjectID, k.Number, k.Provider, k.Section, k.Path, string(k.Action), k.Body, k.Reason,
			k.ProposedBy, opt(k.JobID), opt(k.GoalID), string(k.Batch), k.ExternalRef, opt(k.ChangeID), opt(k.GateID), string(k.Status)).Scan(&k.CreatedAt, &k.UpdatedAt)
		utc(&k.CreatedAt, &k.UpdatedAt)
		return mapErr(err)
	})
}

func (r knowledgeProposalRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.KnowledgeProposal, error) {
	return one(ctx, r.db, scanKnowledgeProposal, `SELECT `+knowledgeProposalCols+` FROM knowledge_proposals WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r knowledgeProposalRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.KnowledgeProposal, error) {
	return one(ctx, r.db, scanKnowledgeProposal, `SELECT `+knowledgeProposalCols+` FROM knowledge_proposals WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r knowledgeProposalRepo) List(ctx context.Context, accountID uuid.UUID, f store.KnowledgeProposalFilter) ([]domain.KnowledgeProposal, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "goal_id = ?", f.GoalID)
	andNot(w, "job_id = ?", f.JobID)
	andNot(w, "change_id = ?", f.ChangeID)
	andNot(w, "batch = ?", string(f.Batch))
	andNot(w, "status = ?", string(f.Status))
	return many(ctx, r.db, scanKnowledgeProposal, `SELECT `+knowledgeProposalCols+` FROM knowledge_proposals`+w.sql("number", f.Limit), w.args...)
}

func (r knowledgeProposalRepo) Update(ctx context.Context, k *domain.KnowledgeProposal, from domain.KnowledgeProposalStatus) error {
	if k.Status != from {
		if _, err := from.Transition(k.Status); err != nil {
			return err
		}
	}
	if err := owned(ctx, r.db, k.AccountID, ref{"changes", k.ChangeID}, ref{"gates", k.GateID}); err != nil {
		return err
	}
	err := r.db.QueryRow(ctx, `UPDATE knowledge_proposals SET status = $4, external_ref = $5, change_id = $6, gate_id = $7, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND status = $3 RETURNING updated_at`,
		k.AccountID, k.ID, string(from), string(k.Status), k.ExternalRef, opt(k.ChangeID), opt(k.GateID)).Scan(&k.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("knowledge proposal %s is not %q: %w", k.Ref(), from, store.ErrConflict)
	}
	utc(&k.UpdatedAt)
	return mapErr(err)
}
