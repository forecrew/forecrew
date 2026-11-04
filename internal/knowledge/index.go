package knowledge

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Index is the project knowledge index: it writes entries, embeds them when
// an Embedder is set and searches them. Full-text search is always on; the
// embedding only adds to the ranking (store.SemanticWeight) where the
// database has pgvector.
type Index struct {
	Store store.Store
	// Embedder is optional.
	Embedder Embedder
	Log      *slog.Logger
}

func (x *Index) log() *slog.Logger {
	if x.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return x.Log
}

// EmbedGroup is how many entries are embedded and saved together, so a
// refused request loses at most one group's embeddings, not a whole sync's.
const EmbedGroup = 16

// Put stores the entries and embeds them. An embedding that fails is logged
// and skipped: the entry is searchable by words at once and Backfill tries
// the embedding again later.
func (x *Index) Put(ctx context.Context, entries ...*domain.KnowledgeEntry) error {
	for _, e := range entries {
		if err := x.Store.Knowledge().Upsert(ctx, e); err != nil {
			return err
		}
	}
	return x.embed(ctx, entries)
}

// embed sends the text of the entries to the embeddings provider, only when
// there is one and the database can keep the vectors: without pgvector
// nothing would be stored, and text must not leave for nothing.
func (x *Index) embed(ctx context.Context, entries []*domain.KnowledgeEntry) error {
	if x.Embedder == nil || len(entries) == 0 {
		return nil
	}
	if ok, err := x.Store.Knowledge().SupportsEmbeddings(ctx); err != nil || !ok {
		return err
	}
	for group := range slices.Chunk(entries, EmbedGroup) {
		texts := make([]string, len(group))
		for i, e := range group {
			texts[i] = e.Title + "\n\n" + e.Body
		}
		vecs, err := x.Embedder.Embed(ctx, texts)
		if err != nil {
			// The rest stays without embeddings; Backfill retries them.
			x.log().WarnContext(ctx, "embed knowledge entries", "count", len(group), "error", err)
			return nil
		}
		for i, e := range group {
			err := x.Store.Knowledge().SetEmbedding(ctx, e.AccountID, e.ID, vecs[i])
			switch {
			case errors.Is(err, errors.ErrUnsupported):
				return nil // no pgvector: words only
			case err != nil:
				return err
			}
		}
	}
	return nil
}

// Backfill embeds up to limit entries of the project that have no embedding:
// the ones whose request failed, and the ones stored before a key was set.
// It returns how many entries it tried.
func (x *Index) Backfill(ctx context.Context, accountID, projectID uuid.UUID, limit int) (int, error) {
	if x.Embedder == nil {
		return 0, nil
	}
	missing, err := x.Store.Knowledge().WithoutEmbedding(ctx, accountID, projectID, limit)
	if err != nil || len(missing) == 0 {
		return 0, err
	}
	ptrs := make([]*domain.KnowledgeEntry, len(missing))
	for i := range missing {
		ptrs[i] = &missing[i]
	}
	return len(ptrs), x.embed(ctx, ptrs)
}

// Search runs q. With an embedder the text is embedded too; if that fails
// the search goes on with words only.
func (x *Index) Search(ctx context.Context, accountID uuid.UUID, q store.KnowledgeQuery) ([]store.KnowledgeHit, error) {
	if x.Embedder != nil && strings.TrimSpace(q.Text) != "" && x.canEmbed(ctx) {
		vecs, err := x.Embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			x.log().WarnContext(ctx, "embed knowledge query", "error", err)
		} else {
			q.Embedding = vecs[0]
		}
	}
	return x.Store.Knowledge().Search(ctx, accountID, q)
}

// canEmbed reports whether queries may go to the embeddings provider: only
// when the database can rank by them.
func (x *Index) canEmbed(ctx context.Context) bool {
	ok, err := x.Store.Knowledge().SupportsEmbeddings(ctx)
	return err == nil && ok
}
