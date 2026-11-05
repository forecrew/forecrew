package testkit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Knowledge implements store.Store. Its search is a simple word match, not
// Postgres full-text search: every positive word of the query (words in
// quotes count as words, a leading - excludes) must occur in the title or
// body, and a title hit ranks higher. Embeddings are kept, so the semantic
// ranking can be tested without pgvector.
func (s *Store) Knowledge() store.KnowledgeRepo { return knowledgeRepo{s} }

type knowledgeRepo struct{ s *Store }

func (r knowledgeRepo) Upsert(_ context.Context, e *domain.KnowledgeEntry) error {
	if e.BodyHash == "" {
		sum := sha256.Sum256([]byte(e.Body))
		e.BodyHash = hex.EncodeToString(sum[:])
	}
	if err := e.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, e.AccountID, e.ProjectID), foreign(&d.goals, e.AccountID, e.GoalID)); err != nil {
			return err
		}
		cur, err := d.knowledge.find(func(x domain.KnowledgeEntry) bool {
			return x.AccountID == e.AccountID && x.ProjectID == e.ProjectID && x.Provider == e.Provider && x.Path == e.Path
		})
		if err != nil {
			e.ID, e.CreatedAt, e.UpdatedAt, e.SyncedAt = newID(e.ID), now, now, now
			d.knowledge.insert(*e)
			return nil
		}
		if cur.BodyHash != e.BodyHash || cur.Title != e.Title {
			delete(d.embeddings, cur.ID)
		}
		e.ID, e.CreatedAt, e.UpdatedAt, e.SyncedAt = cur.ID, cur.CreatedAt, now, now
		*cur = *e
		return nil
	})
}

func (r knowledgeRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.KnowledgeEntry, error) {
	return read(r.s, func(d *storeData) (domain.KnowledgeEntry, error) {
		e, err := d.knowledge.get(accountID, id)
		if err != nil {
			return domain.KnowledgeEntry{}, err
		}
		return *e, nil
	})
}

func (r knowledgeRepo) GetByPath(_ context.Context, accountID, projectID uuid.UUID, provider, path string) (domain.KnowledgeEntry, error) {
	return read(r.s, func(d *storeData) (domain.KnowledgeEntry, error) {
		e, err := d.knowledge.find(func(x domain.KnowledgeEntry) bool {
			return x.AccountID == accountID && x.ProjectID == projectID && x.Provider == provider && x.Path == path
		})
		if err != nil {
			return domain.KnowledgeEntry{}, err
		}
		return *e, nil
	})
}

func (r knowledgeRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if _, err := d.knowledge.get(accountID, id); err != nil {
			return err
		}
		d.knowledge.rows = slices.DeleteFunc(d.knowledge.rows, func(x domain.KnowledgeEntry) bool { return x.ID == id })
		delete(d.embeddings, id)
		return nil
	})
}

func (r knowledgeRepo) List(_ context.Context, accountID uuid.UUID, f store.KnowledgeFilter) ([]domain.KnowledgeEntry, error) {
	return read(r.s, func(d *storeData) ([]domain.KnowledgeEntry, error) {
		out := d.knowledge.filter(0, func(x domain.KnowledgeEntry) bool {
			return x.AccountID == accountID && zeroOr(f.ProjectID, x.ProjectID) && zeroOr(f.Provider, x.Provider) &&
				zeroOr(f.Section, x.Section) && (!f.NeedsReverify || x.NeedsReverify)
		})
		slices.SortFunc(out, func(a, b domain.KnowledgeEntry) int {
			return cmp.Or(cmp.Compare(a.Section, b.Section), cmp.Compare(a.Path, b.Path))
		})
		if f.Limit > 0 && len(out) > f.Limit {
			out = out[:f.Limit]
		}
		return out, nil
	})
}

func (r knowledgeRepo) SetEmbedding(_ context.Context, accountID, id uuid.UUID, embedding []float32) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if _, err := d.knowledge.get(accountID, id); err != nil {
			return err
		}
		d.embeddings[id] = slices.Clone(embedding)
		return nil
	})
}

func (r knowledgeRepo) SupportsEmbeddings(context.Context) (bool, error) { return true, nil }

func (r knowledgeRepo) WithoutEmbedding(_ context.Context, accountID, projectID uuid.UUID, limit int) ([]domain.KnowledgeEntry, error) {
	return read(r.s, func(d *storeData) ([]domain.KnowledgeEntry, error) {
		var out []domain.KnowledgeEntry
		for _, e := range d.knowledge.rows {
			if e.AccountID == accountID && e.ProjectID == projectID && d.embeddings[e.ID] == nil {
				out = append(out, e)
			}
		}
		slices.SortFunc(out, func(a, b domain.KnowledgeEntry) int {
			return cmp.Or(a.UpdatedAt.Compare(b.UpdatedAt), cmp.Compare(a.Path, b.Path))
		})
		return out[:min(len(out), limit)], nil
	})
}

func (r knowledgeRepo) MarkReverify(_ context.Context, accountID uuid.UUID, ids []uuid.UUID, reason string) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		for i := range d.knowledge.rows {
			if e := &d.knowledge.rows[i]; e.AccountID == accountID && slices.Contains(ids, e.ID) {
				e.NeedsReverify, e.ReverifyReason, e.UpdatedAt = true, reason, now
			}
		}
		return nil
	})
}

func (r knowledgeRepo) DeleteExpired(_ context.Context, accountID uuid.UUID, t time.Time) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		d.knowledge.rows = slices.DeleteFunc(d.knowledge.rows, func(x domain.KnowledgeEntry) bool {
			gone := x.AccountID == accountID && x.Provider == domain.ProviderInternal && !x.ExpiresAt.IsZero() && x.ExpiresAt.Before(t)
			if gone {
				n++
				delete(d.embeddings, x.ID)
			}
			return gone
		})
		return nil
	})
	return n, err
}

func (r knowledgeRepo) ExpireGoalNotes(_ context.Context, accountID uuid.UUID, grace time.Duration) (int, error) {
	n := 0
	err := r.s.do(func(d *storeData, _ time.Time) error {
		for i := range d.knowledge.rows {
			e := &d.knowledge.rows[i]
			if e.AccountID != accountID || e.GoalID == uuid.Nil() || !e.ExpiresAt.IsZero() {
				continue
			}
			g, err := d.goals.get(accountID, e.GoalID)
			if err == nil && (g.Status == domain.GoalDone || g.Status == domain.GoalCanceled) {
				e.ExpiresAt = g.UpdatedAt.Add(grace)
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r knowledgeRepo) Search(_ context.Context, accountID uuid.UUID, q store.KnowledgeQuery) ([]store.KnowledgeHit, error) {
	return read(r.s, func(d *storeData) ([]store.KnowledgeHit, error) {
		wq := queryWords(q.Text)
		hits := []store.KnowledgeHit{}
		for _, e := range d.knowledge.rows {
			if !visible(e, accountID, q) {
				continue
			}
			rank, ok := 0.0, true
			if strings.TrimSpace(q.Text) != "" {
				rank, ok = score(e, d.embeddings[e.ID], q, wq)
			}
			if ok {
				hits = append(hits, store.KnowledgeHit{Entry: e, Rank: rank, Snippet: snippet(e.Body)})
			}
		}
		slices.SortStableFunc(hits, func(a, b store.KnowledgeHit) int {
			if q.GoalOnly && strings.TrimSpace(q.Text) == "" {
				return cmp.Or(b.Entry.CreatedAt.Compare(a.Entry.CreatedAt), cmp.Compare(a.Entry.Path, b.Entry.Path))
			}
			return cmp.Or(cmp.Compare(b.Rank, a.Rank), cmp.Compare(a.Entry.Section, b.Entry.Section), cmp.Compare(a.Entry.Path, b.Entry.Path))
		})
		return hits[:min(len(hits), cmp.Or(q.Limit, 20))], nil
	})
}

// score ranks e for a text query: its word match, mixed with the cosine
// similarity of its embedding (vec, nil without one) as the Postgres store
// does. ok is false when e is no hit.
func score(e domain.KnowledgeEntry, vec []float32, q store.KnowledgeQuery, wq wordQuery) (rank float64, ok bool) {
	rank, ok = textRank(e, wq)
	if vec == nil || len(q.Embedding) == 0 {
		return rank, ok
	}
	sim := max(0, cosine(vec, q.Embedding))
	switch {
	case ok:
		return (1-store.SemanticWeight)*rank + store.SemanticWeight*sim, true
	case sim >= store.SemanticFloor:
		return store.SemanticWeight * sim, true
	}
	return 0, false
}

func visible(e domain.KnowledgeEntry, accountID uuid.UUID, q store.KnowledgeQuery) bool {
	switch {
	case e.AccountID != accountID, q.ProjectID != uuid.Nil() && e.ProjectID != q.ProjectID,
		len(q.Sections) > 0 && !slices.Contains(q.Sections, e.Section),
		len(q.Providers) > 0 && !slices.Contains(q.Providers, e.Provider),
		len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind()):
		return false
	}
	if q.GoalOnly {
		return e.Provider == domain.ProviderInternal && e.Scope == domain.NoteScopeGoal && e.GoalID == q.GoalID
	}
	return e.Provider != domain.ProviderInternal || e.Scope == domain.NoteScopeProject || q.AllNotes ||
		(q.GoalID != uuid.Nil() && e.GoalID == q.GoalID)
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// wordQuery is a parsed websearch query: the words that must occur (or, with
// any, of which one must) and the words that must not.
type wordQuery struct {
	pos, neg []string
	any      bool
}

// queryWords splits a websearch query into its words. An "or" between words
// makes any one of them enough.
func queryWords(text string) wordQuery {
	var q wordQuery
	for f := range strings.FieldsSeq(text) {
		if w, ok := strings.CutPrefix(f, "-"); ok {
			q.neg = append(q.neg, words(w)...)
			continue
		}
		if strings.EqualFold(f, "or") {
			q.any = true
			continue
		}
		q.pos = append(q.pos, words(f)...)
	}
	return q
}

// textRank is 1 for a title hit and 0.5 for a body hit of the best word, in
// [0, 1): a match needs every positive word (one of them for an "or" query)
// and no negative one.
func textRank(e domain.KnowledgeEntry, q wordQuery) (float64, bool) {
	title, body := words(e.Title), words(e.Body)
	if len(q.pos) == 0 {
		return 0, false
	}
	score, hit := 0.0, false
	for _, w := range q.pos {
		switch {
		case slices.Contains(title, w):
			score, hit = score+1, true
		case slices.Contains(body, w):
			score, hit = score+0.5, true
		case !q.any:
			return 0, false
		}
	}
	if !hit {
		return 0, false
	}
	for _, w := range q.neg {
		if slices.Contains(title, w) || slices.Contains(body, w) {
			return 0, false
		}
	}
	return score / (score + 1), true
}

func snippet(body string) string {
	if r := []rune(body); len(r) > 240 {
		return string(r[:240]) + "..."
	}
	return body
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range min(len(a), len(b)) {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func kbProposalsT(d *storeData) *table[domain.KnowledgeProposal] { return &d.kbProposals }

// KnowledgeProposals implements store.Store.
func (s *Store) KnowledgeProposals() store.KnowledgeProposalRepo { return knowledgeProposalRepo{s} }

type knowledgeProposalRepo struct{ s *Store }

func (r knowledgeProposalRepo) Create(_ context.Context, k *domain.KnowledgeProposal) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		err := firstErr(foreign(&d.projects, k.AccountID, k.ProjectID), foreign(&d.jobs, k.AccountID, k.JobID), foreign(&d.goals, k.AccountID, k.GoalID),
			foreign(&d.changes, k.AccountID, k.ChangeID), foreign(&d.gates, k.AccountID, k.GateID))
		if err != nil {
			return err
		}
		k.ID = newID(k.ID)
		k.Number = d.nextNumber(k.AccountID, domain.RefKnowledge)
		k.CreatedAt, k.UpdatedAt = now, now
		d.kbProposals.insert(*k)
		return nil
	})
}

func (r knowledgeProposalRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.KnowledgeProposal, error) {
	return getRow(r.s, kbProposalsT, accountID, id)
}

func (r knowledgeProposalRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.KnowledgeProposal, error) {
	return findRow(r.s, kbProposalsT, func(k domain.KnowledgeProposal) bool { return k.AccountID == accountID && k.Number == number })
}

func (r knowledgeProposalRepo) List(_ context.Context, accountID uuid.UUID, f store.KnowledgeProposalFilter) ([]domain.KnowledgeProposal, error) {
	return listRows(r.s, kbProposalsT, f.Limit, func(k domain.KnowledgeProposal) bool {
		return k.AccountID == accountID && zeroOr(f.ProjectID, k.ProjectID) && zeroOr(f.GoalID, k.GoalID) && zeroOr(f.JobID, k.JobID) &&
			zeroOr(f.ChangeID, k.ChangeID) && zeroOr(f.Batch, k.Batch) && zeroOr(f.Status, k.Status)
	})
}

func (r knowledgeProposalRepo) Update(_ context.Context, k *domain.KnowledgeProposal, from domain.KnowledgeProposalStatus) error {
	if k.Status != from {
		if _, err := from.Transition(k.Status); err != nil {
			return err
		}
	}
	return update(r.s, kbProposalsT, k.AccountID, k.ID, func(cur *domain.KnowledgeProposal, now time.Time) error {
		if cur.Status != from {
			return fmt.Errorf("knowledge proposal %s is %q, not %q: %w", cur.Ref(), cur.Status, from, store.ErrConflict)
		}
		cur.Status, cur.ExternalRef, cur.ChangeID, cur.GateID, cur.UpdatedAt = k.Status, k.ExternalRef, k.ChangeID, k.GateID, now
		k.UpdatedAt = now
		return nil
	})
}
