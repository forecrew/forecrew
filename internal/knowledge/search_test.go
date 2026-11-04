package knowledge_test

import (
	"maps"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestSearchFiltersMarksAndAccess(t *testing.T) {
	e := newNotesEnv(t)
	ctx := t.Context()
	files := map[string]string{}
	maps.Copy(files, sourceFiles)
	files["knowledge/vision/why.md"] = "The queue is the product.\n"
	files[knowledge.PolicyFile] = "version: 1\nsections:\n  architecture:\n    paths: [\"docs/adr/**\"]\n    access: {read: [architect]}\n  vision:\n    protected: true\n  conventions:\n    paths: [CONTRIBUTING.md]\n"
	files["CONTRIBUTING.md"] = "Keep the queue small.\n"
	e.commit(t, "h1", files)
	e.commit(t, "h1", map[string]string{"docs/adr/0001-q.md": "Jobs wait in the queue in Postgres.\n"})
	_, err := e.syncer.Sync(ctx, e.project, "", "h1")
	must(t, err)
	_, err = e.notes.Write(ctx, e.job(1, "developer", e.g1), knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "queue trick", Body: "queue notes"})
	must(t, err)

	run := func(q knowledge.Query) map[string]knowledge.Result {
		q.Project = e.project
		res, err := e.search.Run(ctx, q)
		must(t, err)
		out := map[string]knowledge.Result{}
		for _, r := range res {
			out[r.Section+":"+r.Path] = r
		}
		return out
	}
	human := run(knowledge.Query{Text: "queue", AllGoals: true})
	if len(human) != 4 {
		t.Fatalf("a human found %d results: %v", len(human), human)
	}
	vision := human["vision:knowledge/vision/why.md"]
	if vision.Untrusted || vision.Kind != domain.KnowledgeEntryKind || vision.SourceURL == "" || vision.Provenance.Created.IsZero() {
		t.Errorf("a protected entry = %+v", vision)
	}
	for key, r := range human {
		if key != "vision:knowledge/vision/why.md" && !r.Untrusted {
			t.Errorf("%s is not marked untrusted", key)
		}
	}
	if r := human["notes:"+e.entries(t)["notes/"+firstNote(t, e)].Path]; r.Kind != domain.KnowledgeNoteKind {
		t.Errorf("note result = %+v", r)
	}

	// A developer may not read architecture; an architect may.
	dev := run(knowledge.Query{Text: "queue", Role: "developer"})
	if _, ok := dev["architecture:docs/adr/0001-q.md"]; ok || len(dev) != 3 {
		t.Errorf("developer saw %v", dev)
	}
	if arch := run(knowledge.Query{Text: "queue", Role: "architect"}); len(arch) != 4 {
		t.Errorf("architect saw %v", arch)
	}

	// Filters.
	if got := run(knowledge.Query{Text: "queue", Kinds: []domain.KnowledgeKind{domain.KnowledgeGeneratedKind}}); len(got) != 0 {
		t.Errorf("generated results for queue: %v", got)
	}
	if got := run(knowledge.Query{Text: "queue", Sections: []string{"conventions"}}); len(got) != 1 {
		t.Errorf("section filter: %v", got)
	}
	if got := run(knowledge.Query{Text: "queue", Providers: []string{domain.ProviderInternal}}); len(got) != 1 {
		t.Errorf("provider filter: %v", got)
	}
	if got := run(knowledge.Query{Text: "queue", Limit: 2}); len(got) != 2 {
		t.Errorf("limit: %v", got)
	}
	if got := run(knowledge.Query{Sections: []string{"vision"}}); len(got) != 1 {
		t.Errorf("list without text: %v", got)
	}
}

func firstNote(t *testing.T, e *notesEnv) string {
	t.Helper()
	for p, x := range e.entries(t) {
		if x.Provider == domain.ProviderInternal {
			return p[len("notes/"):]
		}
	}
	t.Fatal("no note")
	return ""
}

func TestSearchWithEmbeddings(t *testing.T) {
	e := newNotesEnv(t)
	emb := &testkit.Embedder{}
	idx := &knowledge.Index{Store: e.st, Embedder: emb}
	s := &knowledge.Search{Index: idx, Policy: e.syncer.Policy}
	e.syncer.Index = idx
	e.commit(t, "h1", map[string]string{
		"knowledge/runbooks/a.md": "deploy release pipeline rollback procedure steps\n",
		"knowledge/runbooks/b.md": "completely unrelated gardening advice\n",
	})
	e.syncer.Snapshots = nil
	_, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	// No shared word with the entry's text in the title, so words alone
	// would still match the body; the semantic score reorders and adds.
	res, err := s.Run(t.Context(), knowledge.Query{Project: e.project, Text: "deploy release rollback", Role: "developer"})
	must(t, err)
	if len(res) != 1 || res[0].Path != "knowledge/runbooks/a.md" || res[0].Rank <= 0.25 {
		t.Errorf("results = %+v", res)
	}
	if emb.Calls() < 2 {
		t.Errorf("embedder calls = %d: the entries and the query must be embedded", emb.Calls())
	}
	// A failing embedder falls back to words.
	emb.Err = errTest
	res, err = s.Run(t.Context(), knowledge.Query{Project: e.project, Text: "deploy"})
	if err != nil || len(res) != 1 {
		t.Errorf("fallback = %+v, %v", res, err)
	}
	if err := idx.Put(t.Context(), &domain.KnowledgeEntry{AccountID: e.project.AccountID, ProjectID: e.project.ID, Provider: "repo", Path: "knowledge/runbooks/c.md", Section: "runbooks", Title: "c", Body: "c"}); err != nil {
		t.Errorf("Put with a failing embedder: %v", err)
	}
}
