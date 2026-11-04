package knowledge_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// noVectors is a store whose database has no pgvector.
type noVectors struct{ store.Store }

func (n noVectors) Knowledge() store.KnowledgeRepo { return noVectorsRepo{n.Store.Knowledge()} }

type noVectorsRepo struct{ store.KnowledgeRepo }

func (noVectorsRepo) SupportsEmbeddings(context.Context) (bool, error) { return false, nil }
func (noVectorsRepo) WithoutEmbedding(context.Context, uuid.UUID, uuid.UUID, int) ([]domain.KnowledgeEntry, error) {
	return nil, nil
}

func indexEntries(account, project uuid.UUID, n int) []*domain.KnowledgeEntry {
	out := make([]*domain.KnowledgeEntry, n)
	for i := range out {
		out[i] = &domain.KnowledgeEntry{AccountID: account, ProjectID: project, Provider: "repo", Path: fmt.Sprintf("knowledge/runbooks/r%02d.md", i),
			Section: "runbooks", Title: fmt.Sprintf("Runbook %d", i), Body: fmt.Sprintf("secret deploy body %d", i), Version: "v"}
	}
	return out
}

// Repo text, notes and queries must not go to a third party when the
// database could not keep the vectors anyway.
func TestIndexSendsNothingWithoutPgvector(t *testing.T) {
	e := newSyncEnv(t)
	emb := &testkit.Embedder{}
	idx := &knowledge.Index{Store: noVectors{e.st}, Embedder: emb}
	must(t, idx.Put(t.Context(), indexEntries(e.project.AccountID, e.project.ID, 3)...))
	if _, err := idx.Search(t.Context(), e.project.AccountID, store.KnowledgeQuery{ProjectID: e.project.ID, Text: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if n, err := idx.Backfill(t.Context(), e.project.AccountID, e.project.ID, 10); err != nil || n != 0 {
		t.Errorf("Backfill = %d, %v", n, err)
	}
	if emb.Calls() != 0 {
		t.Errorf("the embeddings API was called %d times", emb.Calls())
	}
}

// failNth fails the nth call of an Embedder.
type failNth struct {
	knowledge.Embedder
	n, calls int
}

func (f *failNth) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if f.calls++; f.calls == f.n {
		return nil, errors.New("embed: the API answered status 429")
	}
	return f.Embedder.Embed(ctx, texts)
}

// A refused request costs only its group; the entries without an embedding
// are embedded again later, not dropped for good.
func TestIndexBackfillsRefusedEmbeddings(t *testing.T) {
	e := newSyncEnv(t)
	inner := &testkit.Embedder{}
	emb := &failNth{Embedder: inner, n: 2}
	idx := &knowledge.Index{Store: e.st, Embedder: emb}
	total := 2*knowledge.EmbedGroup + 5
	must(t, idx.Put(t.Context(), indexEntries(e.project.AccountID, e.project.ID, total)...))
	missing, err := e.st.Knowledge().WithoutEmbedding(t.Context(), e.project.AccountID, e.project.ID, 100)
	must(t, err)
	// Group 1 was embedded; group 2 was refused and the request was not repeated at once.
	if len(missing) != total-knowledge.EmbedGroup || emb.calls != 2 {
		t.Fatalf("%d entries without an embedding after %d calls", len(missing), emb.calls)
	}

	n, err := idx.Backfill(t.Context(), e.project.AccountID, e.project.ID, 100)
	must(t, err)
	missing, _ = e.st.Knowledge().WithoutEmbedding(t.Context(), e.project.AccountID, e.project.ID, 100)
	if n != total-knowledge.EmbedGroup || len(missing) != 0 {
		t.Errorf("Backfill tried %d; %d still without", n, len(missing))
	}
}

// A sync, even a no-op one, retries the entries whose embedding failed.
func TestSyncBackfillsEmbeddings(t *testing.T) {
	e := newSyncEnv(t)
	e.syncer.Snapshots = nil
	e.commit(t, "h1", map[string]string{"knowledge/runbooks/a.md": "# A\nbody\n"})
	e.emb.Err = errors.New("embed: the API answered status 500")
	if _, err := e.syncer.Sync(t.Context(), e.project, "", "h1"); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.st.Knowledge().WithoutEmbedding(t.Context(), e.project.AccountID, e.project.ID, 10); len(m) != 1 {
		t.Fatalf("without an embedding: %d", len(m))
	}
	e.emb.Err = nil
	if _, err := e.syncer.Sync(t.Context(), e.project, "", "h1"); err != nil { // nothing moved
		t.Fatal(err)
	}
	if m, _ := e.st.Knowledge().WithoutEmbedding(t.Context(), e.project.AccountID, e.project.ID, 10); len(m) != 0 {
		t.Errorf("still without an embedding: %d", len(m))
	}
}
