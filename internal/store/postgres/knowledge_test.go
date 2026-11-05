//go:build integration

package postgres

import (
	"testing"

	"github.com/forecrew/forecrew/internal/testkit"
)

func TestKnowledge(t *testing.T) {
	f := newFixture(t)
	has, err := f.st.vec.exists(t.Context(), f.pool)
	must(t, err)
	t.Logf("pgvector column: %v", has)
	testkit.KnowledgeStoreSuite(t, f.st, f.account, f.project.ID, has)
	testkit.KnowledgeProposalStoreSuite(t, f.st, f.account, f.project.ID)
}

// TestKnowledgeMigrationWithoutVector: 00006 does nothing without the
// embedding column and rolls back cleanly.
func TestKnowledgeMigrationRollsBack(t *testing.T) {
	pool := newPoolAt(t, 4)
	ctx := t.Context()
	p := provider(t, pool)
	_, err := p.UpTo(ctx, 5)
	must(t, err)
	_, err = p.DownTo(ctx, 4)
	must(t, err)
	_, err = p.Up(ctx)
	must(t, err)
}
