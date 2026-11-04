package knowledge

import (
	"context"
	"errors"
	"log/slog"
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

// Put stores the entries and embeds them. An embedding that fails is
// logged and skipped: the entry is searchable by words at once.
func (x *Index) Put(ctx context.Context, entries ...*domain.KnowledgeEntry) error {
	for _, e := range entries {
		if err := x.Store.Knowledge().Upsert(ctx, e); err != nil {
			return err
		}
	}
	if x.Embedder == nil || len(entries) == 0 {
		return nil
	}
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Title + "\n\n" + e.Body
	}
	vecs, err := x.Embedder.Embed(ctx, texts)
	if err != nil {
		x.log().WarnContext(ctx, "embed knowledge entries", "count", len(entries), "error", err)
		return nil
	}
	for i, e := range entries {
		err := x.Store.Knowledge().SetEmbedding(ctx, e.AccountID, e.ID, vecs[i])
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			return nil // no pgvector: words only
		case err != nil:
			return err
		}
	}
	return nil
}

// Search runs q. With an embedder the text is embedded too; if that fails
// the search goes on with words only.
func (x *Index) Search(ctx context.Context, accountID uuid.UUID, q store.KnowledgeQuery) ([]store.KnowledgeHit, error) {
	if x.Embedder != nil && strings.TrimSpace(q.Text) != "" {
		vecs, err := x.Embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			x.log().WarnContext(ctx, "embed knowledge query", "error", err)
		} else {
			q.Embedding = vecs[0]
		}
	}
	return x.Store.Knowledge().Search(ctx, accountID, q)
}
