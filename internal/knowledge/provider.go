package knowledge

import (
	"context"
	"errors"

	"github.com/forecrew/forecrew/internal/domain"
)

// ErrNotFound means a provider has no entry at the path.
var ErrNotFound = errors.New("knowledge entry not found")

// Ref names one entry at a provider: its path (repo path or the provider's
// stable id) and version (blob SHA, page version).
type Ref struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// Document is an entry as a provider returns it. Its text is untrusted
// data: it is marked as such wherever it reaches an agent.
type Document struct {
	Ref
	SourceURL string `json:"source_url"`
	Entry
}

// Change is an entry that changed since a cursor.
type Change struct {
	Ref
	Deleted bool `json:"deleted"`
}

// ProviderClient is a knowledge provider (docs/contracts/knowledge.md):
// it lists, fetches and reports changes of the entries under a section's
// locations (Section.Paths). Searcher and Proposer are optional; check for
// them with a type assertion.
type ProviderClient interface {
	// List returns every entry under the locations.
	List(ctx context.Context, locations []string) ([]Ref, error)
	// Fetch returns one entry; ErrNotFound when it is gone.
	Fetch(ctx context.Context, path string) (Document, error)
	// Changes returns the entries under the locations that changed since
	// cursor, and the cursor to pass next time (for the repo provider a
	// commit SHA). An empty cursor returns every entry.
	Changes(ctx context.Context, locations []string, cursor string) (changes []Change, next string, err error)
}

// Searcher is a provider that searches itself, for entries not indexed.
type Searcher interface {
	Search(ctx context.Context, locations []string, query string, limit int) ([]Ref, error)
}

// Proposer is a provider with its own review flow (a PR for the repo, a
// draft page or comment elsewhere). Propose returns the external ref the
// review is followed by (a PR number, a page id). Without a Proposer, a
// knowledge_proposal gate shows the change to a human.
type Proposer interface {
	Propose(ctx context.Context, p domain.KnowledgeProposal) (externalRef string, err error)
}
