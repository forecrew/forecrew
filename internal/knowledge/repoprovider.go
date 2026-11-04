package knowledge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

// RepoAPI is what the repo provider reads from GitHub; github.Tracker
// implements it.
type RepoAPI interface {
	Tree(ctx context.Context, repo, ref string) ([]github.TreeEntry, error)
	GetFile(ctx context.Context, repo, ref, path string) ([]byte, error)
	Compare(ctx context.Context, repo, base, head string) (github.Comparison, error)
	LastCommit(ctx context.Context, repo, ref, path string) (github.Commit, error)
}

// RepoClient is the ProviderClient of the project repo, read at one
// commit. Its locations are the globs of a section (Section.Paths).
type RepoClient struct {
	API    RepoAPI
	Repo   string // owner/name
	Branch string // default branch, for source links
	Ref    string // the commit entries are read at

	tree map[string]github.TreeEntry
}

var _ ProviderClient = (*RepoClient)(nil)

// files returns the tree of Ref, read once.
func (p *RepoClient) files(ctx context.Context) (map[string]github.TreeEntry, error) {
	if p.tree != nil {
		return p.tree, nil
	}
	entries, err := p.API.Tree(ctx, p.Repo, p.Ref)
	if err != nil {
		return nil, err
	}
	p.tree = make(map[string]github.TreeEntry, len(entries))
	for _, e := range entries {
		p.tree[e.Path] = e
	}
	return p.tree, nil
}

// List implements ProviderClient.
func (p *RepoClient) List(ctx context.Context, locations []string) ([]Ref, error) {
	tree, err := p.files(ctx)
	if err != nil {
		return nil, err
	}
	var out []Ref
	for name, e := range tree {
		if Indexable(locations, name, e.Size) {
			out = append(out, Ref{Path: name, Version: e.SHA})
		}
	}
	slices.SortFunc(out, func(a, b Ref) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// Fetch implements ProviderClient. A file without author_role, source and
// created in its frontmatter gets them from git: the last commit that
// changed it, by a human or, for a bot, an agent.
func (p *RepoClient) Fetch(ctx context.Context, file string) (Document, error) {
	tree, err := p.files(ctx)
	if err != nil {
		return Document{}, err
	}
	te, ok := tree[file]
	if !ok {
		return Document{}, fmt.Errorf("%s: %w", file, ErrNotFound)
	}
	data, err := p.API.GetFile(ctx, p.Repo, p.Ref, file)
	if errors.Is(err, github.ErrNotFound) {
		return Document{}, fmt.Errorf("%s: %w", file, ErrNotFound)
	}
	if err != nil {
		return Document{}, err
	}
	e, err := ParseEntry(file, data)
	if err != nil {
		return Document{}, err
	}
	if !e.HasProvenance {
		if err := p.gitProvenance(ctx, file, &e.Provenance); err != nil {
			return Document{}, err
		}
	}
	return Document{Path: file, Version: te.SHA, SourceURL: "https://github.com/" + p.Repo + "/blob/" + p.Branch + "/" + file, Entry: e}, nil
}

// gitProvenance fills the provenance fields the frontmatter lacks from the
// last commit of file.
func (p *RepoClient) gitProvenance(ctx context.Context, file string, pv *domain.Provenance) error {
	c, err := p.API.LastCommit(ctx, p.Repo, p.Ref, file)
	if errors.Is(err, github.ErrNotFound) {
		c = github.Commit{}
	} else if err != nil {
		return err
	}
	if pv.AuthorRole == "" {
		pv.AuthorRole = "human"
		if c.Bot {
			pv.AuthorRole = "agent"
		}
	}
	if pv.Source == "" {
		pv.Source = "human"
		if c.SHA != "" {
			pv.Source = "https://github.com/" + p.Repo + "/commit/" + c.SHA
		}
	}
	if pv.Created.IsZero() {
		pv.Created = c.Time
	}
	return nil
}

// Changes implements ProviderClient: with an empty cursor every entry, else
// the entries that changed between the commit cursor and Ref. Renamed
// files are a deletion and a change. next is Ref.
//
// The Syncer does not call it: it compares the tree's blob SHAs with the
// index, which stays right when a push was missed. It stays because
// ProviderClient is the contract every provider implements
// (docs/contracts/knowledge.md) and the repo provider is its reference, so
// the contract and its tests keep one working example for providers that
// have no tree to compare (4.19).
func (p *RepoClient) Changes(ctx context.Context, locations []string, cursor string) ([]Change, string, error) {
	if cursor == "" {
		refs, err := p.List(ctx, locations)
		if err != nil {
			return nil, "", err
		}
		out := make([]Change, len(refs))
		for i, r := range refs {
			out[i] = Change{Ref: r}
		}
		return out, p.Ref, nil
	}
	cmp, err := p.API.Compare(ctx, p.Repo, cursor, p.Ref)
	if err != nil {
		return nil, "", err
	}
	return changesOf(cmp, locations), p.Ref, nil
}

func changesOf(cmp github.Comparison, locations []string) []Change {
	var out []Change
	for _, f := range cmp.Files {
		if f.PreviousPath != "" && Indexable(locations, f.PreviousPath, 0) {
			out = append(out, Change{Path: f.PreviousPath, Deleted: true})
		}
		if !Indexable(locations, f.Path, 0) {
			continue
		}
		if f.Status == github.FileRemoved {
			out = append(out, Change{Path: f.Path, Deleted: true})
		} else {
			out = append(out, Change{Path: f.Path, Version: f.SHA})
		}
	}
	return out
}
