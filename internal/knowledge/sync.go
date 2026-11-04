package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

// SyncAPI is what the Syncer reads from GitHub; github.Tracker implements it.
type SyncAPI interface {
	RepoAPI
	HeadSHA(ctx context.Context, repo, branch string) (string, error)
}

// SyncResult says what one Sync did.
type SyncResult struct {
	Head      string
	Indexed   int      // entries added or changed
	Removed   int      // entries deleted
	Skipped   []string // "path: reason" for files that could not be entries
	Flagged   int      // entries marked needs_reverify
	Generated bool     // the generated section was rebuilt
}

// zeroSHA is the commit GitHub reports before a branch exists.
const zeroSHA = "0000000000000000000000000000000000000000"

// Syncer keeps the repo provider's entries and the generated section of
// projects in step with their default branch. Calls are serialized.
type Syncer struct {
	Store store.Store
	Index *Index
	API   SyncAPI
	// Snapshots analyzes the repo for the generated section; nil leaves
	// the section alone.
	Snapshots SnapshotSource
	Log       *slog.Logger
	Now       func() time.Time

	mu       sync.Mutex
	policies sync.Map // project ID to Policy, as of the last Sync
}

func (s *Syncer) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

func (s *Syncer) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
}

// Policy returns the policy of the project as of its last Sync, or the
// default one for a project that was never synced. It never reads GitHub.
func (s *Syncer) Policy(p domain.Project) Policy {
	if v, ok := s.policies.Load(p.ID); ok {
		return v.(Policy)
	}
	return DefaultPolicy(nil, nil)
}

// Sync indexes the project's default branch at head ("" reads the branch
// head): changed entries by blob SHA, deleted ones removed, the generated
// section rebuilt when head moved. When before is a commit, the entries that
// mention a path or symbol before..head changed are flagged needs_reverify
// (Drift). An entry that cannot be read is skipped and reported; a failure
// of one part does not stop the others, and the errors are joined.
func (s *Syncer) Sync(ctx context.Context, p domain.Project, before, head string) (SyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if head == "" {
		var err error
		if head, err = s.API.HeadSHA(ctx, p.Repo, p.DefaultBranch); err != nil {
			return SyncResult{}, fmt.Errorf("sync knowledge of %s: %w", p.Repo, err)
		}
	}
	res := SyncResult{Head: head}
	rc := &RepoClient{API: s.API, Repo: p.Repo, Branch: p.DefaultBranch, Ref: head}
	policy, err := s.loadPolicy(ctx, p, rc)
	if err != nil {
		return res, fmt.Errorf("sync knowledge of %s: %w", p.Repo, err)
	}
	s.policies.Store(p.ID, policy)
	fresh := map[string]bool{}
	errs := []error{s.syncEntries(ctx, p, rc, policy, &res, fresh)}
	if before != "" && before != zeroSHA && before != head {
		errs = append(errs, s.flagDrift(ctx, p, before, head, fresh, &res))
	}
	if s.Snapshots != nil {
		errs = append(errs, s.syncGenerated(ctx, p, head, &res))
	}
	return res, errors.Join(errs...)
}

// loadPolicy reads the policy file at the commit, or builds the default
// policy from the repo's files.
func (s *Syncer) loadPolicy(ctx context.Context, p domain.Project, rc *RepoClient) (Policy, error) {
	data, err := s.API.GetFile(ctx, p.Repo, rc.Ref, PolicyFile)
	switch {
	case err == nil:
		return ParsePolicy(data)
	case !errors.Is(err, github.ErrNotFound):
		return Policy{}, err
	}
	tree, err := rc.files(ctx)
	if err != nil {
		return Policy{}, err
	}
	names := make([]string, 0, len(tree))
	for name := range tree {
		names = append(names, name)
	}
	slices.Sort(names)
	sources, err := analyzer.KnowledgeSourcesOf(names, func(name string) ([]byte, error) {
		return s.API.GetFile(ctx, p.Repo, rc.Ref, name)
	})
	if err != nil {
		return Policy{}, err
	}
	return DefaultPolicy(sources, names), nil
}

func (s *Syncer) syncEntries(ctx context.Context, p domain.Project, rc *RepoClient, policy Policy, res *SyncResult, fresh map[string]bool) error {
	tree, err := rc.files(ctx)
	if err != nil {
		return err
	}
	mapping := RepoMapping(policy)
	existing, err := s.Store.Knowledge().List(ctx, p.AccountID, store.KnowledgeFilter{ProjectID: p.ID, Provider: RepoProvider})
	if err != nil {
		return err
	}
	have := map[string]domain.KnowledgeEntry{}
	for _, e := range existing {
		if !e.Generated {
			have[e.Path] = e
		}
	}
	wanted := map[string]bool{}
	var put []*domain.KnowledgeEntry
	for _, name := range slices.Sorted(mapKeys(tree)) {
		section, ok := mapping.SectionOf(name, tree[name].Size)
		if !ok {
			continue
		}
		wanted[name] = true
		if cur, ok := have[name]; ok && cur.Version == tree[name].SHA && cur.Section == section {
			continue
		}
		e, err := s.readEntry(ctx, p, rc, name, section)
		if errors.Is(err, ErrInvalid) {
			res.Skipped = append(res.Skipped, name+": "+strings.TrimPrefix(err.Error(), name+": "))
			continue // unreadable now: what was indexed stays
		}
		if err != nil {
			return err
		}
		fresh[name] = true
		put = append(put, e)
	}
	if err := s.Index.Put(ctx, put...); err != nil {
		return err
	}
	res.Indexed = len(put)
	return s.removeGone(ctx, p, have, wanted, res)
}

// removeGone deletes the indexed entries that are no longer mapped files of
// the head, except the ones that could not be read this time.
func (s *Syncer) removeGone(ctx context.Context, p domain.Project, have map[string]domain.KnowledgeEntry, wanted map[string]bool, res *SyncResult) error {
	skipped := func(name string) bool {
		return slices.ContainsFunc(res.Skipped, func(x string) bool { return strings.HasPrefix(x, name+": ") })
	}
	for name, e := range have {
		if wanted[name] || skipped(name) {
			continue
		}
		if err := s.Store.Knowledge().Delete(ctx, p.AccountID, e.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		res.Removed++
	}
	return nil
}

// readEntry reads the file name at the synced commit as an entry of section.
func (s *Syncer) readEntry(ctx context.Context, p domain.Project, rc *RepoClient, name, section string) (*domain.KnowledgeEntry, error) {
	doc, err := rc.Fetch(ctx, name)
	if err != nil {
		return nil, err
	}
	return &domain.KnowledgeEntry{
		AccountID: p.AccountID, ProjectID: p.ID, Provider: RepoProvider, Path: name, SourceURL: doc.SourceURL, Version: doc.Version,
		Section: section, Title: doc.Title, Body: doc.Body, Provenance: doc.Provenance,
	}, nil
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// flagDrift marks the entries that mention what before..head changed.
func (s *Syncer) flagDrift(ctx context.Context, p domain.Project, before, head string, fresh map[string]bool, res *SyncResult) error {
	cmp, err := s.API.Compare(ctx, p.Repo, before, head)
	if errors.Is(err, github.ErrNotFound) {
		s.log().WarnContext(ctx, "knowledge drift skipped: the previous commit is gone", "project", p.Name, "before", before)
		return nil
	}
	if err != nil {
		return err
	}
	d := DriftOf(cmp)
	if len(d.Paths)+len(d.Symbols) == 0 {
		return nil
	}
	entries, err := s.Store.Knowledge().List(ctx, p.AccountID, store.KnowledgeFilter{ProjectID: p.ID, Provider: RepoProvider})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Generated || fresh[e.Path] {
			continue // generated text is rebuilt; an entry read in this sync is current
		}
		paths, symbols := d.Mentions(e.Body)
		if len(paths)+len(symbols) == 0 {
			continue
		}
		if err := s.Store.Knowledge().MarkReverify(ctx, p.AccountID, []uuid.UUID{e.ID}, DriftReason(head, paths, symbols)); err != nil {
			return err
		}
		res.Flagged++
	}
	return nil
}

// syncGenerated rebuilds the generated section when it describes another
// commit than head.
func (s *Syncer) syncGenerated(ctx context.Context, p domain.Project, head string, res *SyncResult) error {
	existing, err := s.Store.Knowledge().List(ctx, p.AccountID, store.KnowledgeFilter{ProjectID: p.ID, Section: domain.SectionGenerated})
	if err != nil {
		return err
	}
	if len(existing) > 0 && !slices.ContainsFunc(existing, func(e domain.KnowledgeEntry) bool { return e.Version != head }) {
		return nil
	}
	snap, err := s.Snapshots.Snapshot(ctx, p.Repo, head)
	if err != nil {
		return fmt.Errorf("rebuild the generated knowledge of %s: %w", p.Repo, err)
	}
	built := BuildGenerated(p.AccountID, p.ID, p.Repo, head, snap, s.now())
	put := make([]*domain.KnowledgeEntry, len(built))
	keep := map[string]bool{}
	for i := range built {
		put[i] = &built[i]
		keep[built[i].Path] = true
	}
	if err := s.Index.Put(ctx, put...); err != nil {
		return err
	}
	for _, e := range existing {
		if !keep[e.Path] {
			if err := s.Store.Knowledge().Delete(ctx, p.AccountID, e.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	res.Generated = true
	return nil
}

// SeedSections are the sections the knowledge curator may fill from a
// repo's own documents (CLAUDE.md, AGENTS.md, README, docs/). The others
// (vision, market, lessons, security) are written by people or learned from
// goals.
var SeedSections = []string{"architecture", "conventions", "testing", "runbooks", "glossary"}

// MissingSections returns the SeedSections that policy gives no location,
// so the curator proposes entries for those only.
func MissingSections(p Policy) []string {
	var out []string
	for _, name := range SeedSections {
		if s, ok := p.Sections[name]; !ok || len(s.Paths) == 0 {
			out = append(out, name)
		}
	}
	return out
}

// MappedPaths returns every glob of the project's sections served by the
// repo, which writing roles may not change (curated knowledge changes only
// through proposals), plus knowledge/** and the policy file.
func (s *Syncer) MappedPaths(p domain.Project) []string {
	globs := RepoMapping(s.Policy(p)).Globs()
	out := []string{"knowledge/**", PolicyFile}
	for _, g := range globs {
		if !strings.HasPrefix(g, "!") {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
