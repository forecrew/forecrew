package testkit

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// KnowledgeStoreSuite checks the contract of store.KnowledgeRepo on st, the
// fake and the Postgres store alike. embeddings says whether the store
// keeps embeddings (the fake does; Postgres only with pgvector).
func KnowledgeStoreSuite(t *testing.T, st store.Store, account, project uuid.UUID, embeddings bool) {
	t.Helper()
	s := newKnowledgeSuite(t, st, account, project)
	s.upsertAndGet()
	s.list()
	s.search()
	s.embeddings(embeddings)
	s.reverify()
	s.expiry()
	s.newestGoalNoteFirst()
}

type knowledgeSuite struct {
	t                *testing.T
	st               store.Store
	repo             store.KnowledgeRepo
	account, project uuid.UUID
	adr, conv, gen   *domain.KnowledgeEntry
	n1, n2, n3       *domain.KnowledgeEntry
	g1, g2           domain.Goal
}

func (s *knowledgeSuite) entry(path, section, title, body string) *domain.KnowledgeEntry {
	return &domain.KnowledgeEntry{AccountID: s.account, ProjectID: s.project, Provider: "repo", Path: path, Section: section,
		Title: title, Body: body, Version: "v1", Provenance: domain.Provenance{AuthorRole: "human", Source: "human", Created: time.Unix(1700000000, 0).UTC()}}
}

func (s *knowledgeSuite) note(path, title, body string, scope domain.NoteScope, goal uuid.UUID) *domain.KnowledgeEntry {
	return &domain.KnowledgeEntry{AccountID: s.account, ProjectID: s.project, Provider: domain.ProviderInternal, Path: path, Section: domain.SectionNotes,
		Title: title, Body: body, Scope: scope, GoalID: goal, Tags: []string{"flaky"}}
}

func newKnowledgeSuite(t *testing.T, st store.Store, account, project uuid.UUID) *knowledgeSuite {
	t.Helper()
	s := &knowledgeSuite{t: t, st: st, repo: st.Knowledge(), account: account, project: project}
	s.g1 = Goal(t, st, account, project, "one")
	s.g2 = Goal(t, st, account, project, "two")
	s.adr = s.entry("docs/adr/0001-queue.md", "architecture", "Queue jobs in Postgres", "We lease jobs with SELECT FOR UPDATE SKIP LOCKED.")
	s.conv = s.entry("CONTRIBUTING.md", "conventions", "Contributing", "Open small pull requests and write tests.")
	s.gen = &domain.KnowledgeEntry{AccountID: account, ProjectID: project, Provider: "repo", Path: "generated/components/api", Section: domain.SectionGenerated,
		Generated: true, Title: "Component api", Body: "Stack go. Build go build. Queue consumer.", Version: "abc"}
	s.n1 = s.note("notes/1", "Flaky queue test", "The queue test is flaky on slow disks.", domain.NoteScopeGoal, s.g1.ID)
	s.n2 = s.note("notes/2", "Other goal queue note", "A queue secret of goal two.", domain.NoteScopeGoal, s.g2.ID)
	s.n3 = s.note("notes/3", "Queue project note", "Project wide queue fact.", domain.NoteScopeProject, uuid.Nil())
	for _, e := range []*domain.KnowledgeEntry{s.adr, s.conv, s.gen, s.n1, s.n2, s.n3} {
		mustT(t, s.repo.Upsert(t.Context(), e))
	}
	if s.adr.ID == uuid.Nil() || s.adr.BodyHash == "" || s.adr.CreatedAt.IsZero() {
		t.Fatalf("Upsert wrote back %+v", s.adr)
	}
	return s
}

func (s *knowledgeSuite) upsertAndGet() {
	t, ctx := s.t, s.t.Context()
	// Upsert replaces the row of the same path and keeps its ID.
	again := s.entry(s.adr.Path, "architecture", "Queue jobs in Postgres", "We lease jobs with SKIP LOCKED, one at a time.")
	mustT(t, s.repo.Upsert(ctx, again))
	if again.ID != s.adr.ID || !again.CreatedAt.Equal(s.adr.CreatedAt) {
		t.Errorf("upsert changed identity: %v/%v vs %v/%v", again.ID, again.CreatedAt, s.adr.ID, s.adr.CreatedAt)
	}
	got, err := s.repo.GetByPath(ctx, s.account, s.project, "repo", s.adr.Path)
	if err != nil || got.Body != again.Body || got.Provenance.AuthorRole != "human" || got.Provenance.Created.IsZero() {
		t.Fatalf("GetByPath = %+v, %v", got, err)
	}
	if byID, err := s.repo.Get(ctx, s.account, s.adr.ID); err != nil || byID.Path != s.adr.Path {
		t.Fatalf("Get = %+v, %v", byID, err)
	}
	if _, err := s.repo.Get(ctx, uuid.New(), s.adr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get of another account: %v", err)
	}
	if err := s.repo.Upsert(ctx, s.entry("x.md", "Bad Section", "t", "b")); err == nil {
		t.Error("an entry with an invalid section was stored")
	}
}

func (s *knowledgeSuite) list() {
	t, ctx := s.t, s.t.Context()
	all, err := s.repo.List(ctx, s.account, store.KnowledgeFilter{ProjectID: s.project})
	if err != nil || len(all) != 6 {
		t.Fatalf("List = %d entries, %v", len(all), err)
	}
	if only, _ := s.repo.List(ctx, s.account, store.KnowledgeFilter{ProjectID: s.project, Provider: "repo", Section: "conventions"}); len(only) != 1 || only[0].Path != "CONTRIBUTING.md" {
		t.Errorf("List by section = %+v", only)
	}
}

// paths runs a search of the project and returns the paths found.
func (s *knowledgeSuite) paths(q store.KnowledgeQuery) []string {
	s.t.Helper()
	q.ProjectID = s.project
	hits, err := s.repo.Search(s.t.Context(), s.account, q)
	if err != nil {
		s.t.Fatalf("Search(%+v): %v", q, err)
	}
	var out []string
	for _, h := range hits {
		out = append(out, h.Entry.Path)
	}
	return out
}

func (s *knowledgeSuite) search() {
	t, ctx := s.t, s.t.Context()
	notes := []domain.KnowledgeKind{domain.KnowledgeNoteKind}
	tests := []struct {
		name string
		q    store.KnowledgeQuery
		want int
		path string // the only path expected, when set
	}{
		{"a word", store.KnowledgeQuery{Text: "lease"}, 1, s.adr.Path},
		{"generated only", store.KnowledgeQuery{Text: "queue", Kinds: []domain.KnowledgeKind{domain.KnowledgeGeneratedKind}}, 1, s.gen.Path},
		{"notes of goal one", store.KnowledgeQuery{Text: "queue", Providers: []string{domain.ProviderInternal}, GoalID: s.g1.ID}, 2, ""},
		{"notes without a goal", store.KnowledgeQuery{Text: "queue", Kinds: notes}, 1, s.n3.Path},
		{"all notes", store.KnowledgeQuery{Text: "queue", Kinds: notes, AllNotes: true}, 3, ""},
		{"entries of a section", store.KnowledgeQuery{Text: "queue", Kinds: []domain.KnowledgeKind{domain.KnowledgeEntryKind}, Sections: []string{"architecture"}}, 1, s.adr.Path},
		{"an exclusion", store.KnowledgeQuery{Text: "queue -slow", Kinds: notes, GoalID: s.g1.ID}, 1, ""},
		{"either of two words", store.KnowledgeQuery{Text: "lease or contributing"}, 2, ""},
		{"goal notes only", store.KnowledgeQuery{GoalOnly: true, GoalID: s.g1.ID}, 1, s.n1.Path},
		{"goal notes only, with text", store.KnowledgeQuery{GoalOnly: true, GoalID: s.g1.ID, Text: "queue"}, 1, s.n1.Path},
		{"nothing", store.KnowledgeQuery{Text: "nothingmatches"}, 0, ""},
		{"a section without text", store.KnowledgeQuery{Sections: []string{"conventions"}}, 1, ""},
	}
	for _, tt := range tests {
		got := s.paths(tt.q)
		if len(got) != tt.want || (tt.path != "" && got[0] != tt.path) {
			t.Errorf("%s: found %v", tt.name, got)
		}
	}
	hits, err := s.repo.Search(ctx, s.account, store.KnowledgeQuery{ProjectID: s.project, Text: "queue", Limit: 2, GoalID: s.g1.ID})
	if err != nil || len(hits) != 2 || hits[0].Rank < hits[1].Rank || hits[0].Snippet == "" {
		t.Errorf("limited search = %+v, %v", hits, err)
	}
	if hits, _ := s.repo.Search(ctx, uuid.New(), store.KnowledgeQuery{ProjectID: s.project, Text: "queue"}); len(hits) != 0 {
		t.Error("another account found entries")
	}
}

func (s *knowledgeSuite) embeddings(supported bool) {
	t, ctx := s.t, s.t.Context()
	vec := make([]float32, 1536)
	vec[0] = 1
	has, err := s.repo.SupportsEmbeddings(ctx)
	if err != nil || has != supported {
		t.Fatalf("SupportsEmbeddings = %v, %v; want %v", has, err, supported)
	}
	without, err := s.repo.WithoutEmbedding(ctx, s.account, s.project, 100)
	if err != nil || len(without) != 0 && !supported || supported && len(without) != 6 {
		t.Fatalf("WithoutEmbedding = %d entries, %v", len(without), err)
	}
	err = s.repo.SetEmbedding(ctx, s.account, s.adr.ID, vec)
	if !supported {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("SetEmbedding without pgvector = %v, want ErrUnsupported", err)
		}
		return
	}
	mustT(t, err)
	if without, err = s.repo.WithoutEmbedding(ctx, s.account, s.project, 100); err != nil || len(without) != 5 {
		t.Errorf("WithoutEmbedding after one embedding = %d, %v", len(without), err)
	}
	semantic := store.KnowledgeQuery{ProjectID: s.project, Text: "unrelatedwordzzz", Embedding: vec}
	if hits, err := s.repo.Search(ctx, s.account, semantic); err != nil || len(hits) != 1 || hits[0].Entry.Path != s.adr.Path {
		t.Errorf("semantic-only search = %+v, %v", hits, err)
	}
	mixed, err := s.repo.Search(ctx, s.account, store.KnowledgeQuery{ProjectID: s.project, Text: "lease", Embedding: vec})
	if err != nil || len(mixed) != 1 || mixed[0].Rank <= 0.5 {
		t.Errorf("mixed ranking = %+v, %v", mixed, err)
	}
	// A changed body drops the embedding.
	mustT(t, s.repo.Upsert(ctx, s.entry(s.adr.Path, "architecture", "Queue jobs in Postgres", "Rewritten without the old words.")))
	if hits, _ := s.repo.Search(ctx, s.account, semantic); len(hits) != 0 {
		t.Errorf("a stale embedding survived an edit: %+v", hits)
	}
}

func (s *knowledgeSuite) reverify() {
	ctx := s.t.Context()
	mustT(s.t, s.repo.MarkReverify(ctx, s.account, []uuid.UUID{s.conv.ID}, "commit abc changed docs/x.go"))
	if rv, _ := s.repo.List(ctx, s.account, store.KnowledgeFilter{NeedsReverify: true}); len(rv) != 1 || rv[0].ReverifyReason == "" {
		s.t.Errorf("needs_reverify list = %+v", rv)
	}
}

// expiry: goal notes of a closed goal get an expiry, expired notes go.
func (s *knowledgeSuite) expiry() {
	t, ctx := s.t, s.t.Context()
	grace := 7 * 24 * time.Hour
	if n, err := s.repo.ExpireGoalNotes(ctx, s.account, grace); err != nil || n != 0 {
		t.Errorf("ExpireGoalNotes with open goals = %d, %v", n, err)
	}
	mustT(t, s.st.Goals().UpdateStatus(ctx, s.account, s.g1.ID, domain.GoalDraft, domain.GoalCanceled))
	if n, err := s.repo.ExpireGoalNotes(ctx, s.account, grace); err != nil || n != 1 {
		t.Fatalf("ExpireGoalNotes = %d, %v", n, err)
	}
	stamped, _ := s.repo.Get(ctx, s.account, s.n1.ID)
	closed, _ := s.st.Goals().Get(ctx, s.account, s.g1.ID)
	if want := closed.UpdatedAt.Add(grace); stamped.ExpiresAt.Sub(want).Abs() > time.Second {
		t.Errorf("expires_at = %v, want %v", stamped.ExpiresAt, want)
	}
	if gone, err := s.repo.DeleteExpired(ctx, s.account, stamped.ExpiresAt.Add(-time.Hour)); err != nil || gone != 0 {
		t.Errorf("DeleteExpired too early = %d, %v", gone, err)
	}
	if gone, err := s.repo.DeleteExpired(ctx, s.account, stamped.ExpiresAt.Add(time.Hour)); err != nil || gone != 1 {
		t.Errorf("DeleteExpired = %d, %v", gone, err)
	}
	if _, err := s.repo.Get(ctx, s.account, s.n1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired note still there: %v", err)
	}
	mustT(t, s.repo.Delete(ctx, s.account, s.n3.ID))
	if err := s.repo.Delete(ctx, s.account, s.n3.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second Delete = %v", err)
	}
}

// Goal creates a goal for suites.
func Goal(t *testing.T, st store.Store, account, project uuid.UUID, title string) domain.Goal {
	t.Helper()
	g := domain.Goal{AccountID: account, ProjectID: project, Title: title, CreatedBy: "user:me", Status: domain.GoalDraft}
	mustT(t, st.Goals().Create(t.Context(), &g))
	return g
}

func mustT(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// KnowledgeProposalStoreSuite checks the contract of
// store.KnowledgeProposalRepo on st, the fake and Postgres alike.
func KnowledgeProposalStoreSuite(t *testing.T, st store.Store, account, project uuid.UUID) {
	t.Helper()
	ctx, repo := t.Context(), st.KnowledgeProposals()
	k := domain.KnowledgeProposal{
		AccountID: account, ProjectID: project, Provider: "repo", Section: "architecture", Path: "docs/adr/1.md", Action: domain.KnowledgeAdd,
		Body: "# ADR", Reason: "why", ProposedBy: "architect", Batch: domain.BatchStandalone, Status: domain.KnowledgeQueued,
	}
	if err := repo.Create(ctx, &k); err != nil {
		t.Fatal(err)
	}
	second := k
	second.ID, second.Path, second.Batch = uuid.Nil(), "docs/adr/2.md", domain.BatchStandalone
	second.Status = domain.KnowledgeOpen
	if err := repo.Create(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if k.Number == 0 || second.Number != k.Number+1 || k.CreatedAt.IsZero() {
		t.Errorf("numbers %d, %d, created %v", k.Number, second.Number, k.CreatedAt)
	}
	got, err := repo.Get(ctx, account, k.ID)
	if err != nil || got.Path != k.Path || got.Ref() != k.Ref() || got.Action != domain.KnowledgeAdd {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if got, err = repo.GetByNumber(ctx, account, second.Number); err != nil || got.ID != second.ID {
		t.Errorf("GetByNumber = %+v, %v", got, err)
	}
	if _, err := repo.Get(ctx, uuid.New(), k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get of another account: %v", err)
	}
	list, err := repo.List(ctx, account, store.KnowledgeProposalFilter{ProjectID: project, Status: domain.KnowledgeQueued})
	if err != nil || len(list) != 1 || list[0].ID != k.ID {
		t.Errorf("List = %+v, %v", list, err)
	}
	k.Status, k.ExternalRef = domain.KnowledgeOpen, "12"
	if err := repo.Update(ctx, &k, domain.KnowledgeQueued); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.Get(ctx, account, k.ID); got.Status != domain.KnowledgeOpen || got.ExternalRef != "12" {
		t.Errorf("after Update = %+v", got)
	}
	k.Status = domain.KnowledgeRejected
	if err := repo.Update(ctx, &k, domain.KnowledgeQueued); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale Update: %v", err)
	}
	k.Status = domain.KnowledgeQueued
	if err := repo.Update(ctx, &k, domain.KnowledgeOpen); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("backward Update: %v", err)
	}
	k.Status, k.ExternalRef = domain.KnowledgeOpen, "13" // same status: only the fields move
	if err := repo.Update(ctx, &k, domain.KnowledgeOpen); err != nil {
		t.Errorf("same-status Update: %v", err)
	}
}

// newestGoalNoteFirst: a goal-only search without text lists the newest note
// first, whatever the paths say.
func (s *knowledgeSuite) newestGoalNoteFirst() {
	t, ctx := s.t, s.t.Context()
	g := Goal(t, s.st, s.account, s.project, "ordered")
	older := s.note("notes/a-older", "Older", "written first", domain.NoteScopeGoal, g.ID)
	mustT(t, s.repo.Upsert(ctx, older))
	time.Sleep(20 * time.Millisecond)
	newer := s.note("notes/z-newer", "Newer", "written second", domain.NoteScopeGoal, g.ID)
	mustT(t, s.repo.Upsert(ctx, newer))
	if got := s.paths(store.KnowledgeQuery{GoalOnly: true, GoalID: g.ID, Limit: 1}); len(got) != 1 || got[0] != newer.Path {
		t.Errorf("newest goal note = %v, want %s", got, newer.Path)
	}
	if got := s.paths(store.KnowledgeQuery{GoalOnly: true, GoalID: g.ID}); len(got) != 2 || got[0] != newer.Path || got[1] != older.Path {
		t.Errorf("goal notes = %v, want newest first", got)
	}
}
