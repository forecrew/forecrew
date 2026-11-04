package knowledge_test

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

type syncEnv struct {
	tr      *testkit.Tracker
	st      *testkit.Store
	emb     *testkit.Embedder
	syncer  *knowledge.Syncer
	project domain.Project
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	clock := testkit.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	tr := testkit.NewTracker(clock.Now)
	tr.Install("acme/app")
	emb := &testkit.Embedder{}
	idx := &knowledge.Index{Store: st, Embedder: emb}
	return &syncEnv{tr: tr, st: st, emb: emb, project: p, syncer: &knowledge.Syncer{
		Store: st, Index: idx, API: tr, Now: clock.Now,
		Snapshots: knowledge.ArchiveSnapshots{Archiver: tr, Analyzer: analyzer.Local{}, TempDir: t.TempDir()},
	}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *syncEnv) commit(t *testing.T, ref string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		must(t, e.tr.SetFile("acme/app", ref, p, []byte(c)))
	}
}

func (e *syncEnv) entries(t *testing.T) map[string]domain.KnowledgeEntry {
	t.Helper()
	all, err := e.st.Knowledge().List(t.Context(), e.project.AccountID, store.KnowledgeFilter{ProjectID: e.project.ID})
	must(t, err)
	out := map[string]domain.KnowledgeEntry{}
	for _, x := range all {
		out[x.Path] = x
	}
	return out
}

var sourceFiles = map[string]string{
	"go.mod":             "module github.com/acme/app\n\ngo 1.26\n",
	"internal/old.go":    "package internal\n\nfunc LeaseNext() {}\n",
	"internal/other.go":  "package internal\n",
	"docs/adr/0001-q.md": "---\ntitle: Queue\nauthor_role: architect\nsource: JOB-1\ncreated: 2026-10-01\n---\nJobs are leased in internal/old.go by LeaseNext.\n",
	"CONTRIBUTING.md":    "# Contributing\nSmall PRs.\n",
}

func TestSyncIndexesAndSkips(t *testing.T) {
	e := newSyncEnv(t)
	files := map[string]string{"knowledge/runbooks/deploy.md": "Deploy it.\n", "knowledge/Bad Section/x.md": "no", "knowledge/runbooks/broken.md": "---\ncreated: yesterday\n---\nx"}
	e.commit(t, "h1", sourceFiles)
	e.commit(t, "h1", files)
	res, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	got := e.entries(t)
	for path, section := range map[string]string{
		"docs/adr/0001-q.md": "architecture", "CONTRIBUTING.md": "conventions", "knowledge/runbooks/deploy.md": "runbooks",
		"generated/packages": "generated", "generated/dependencies": "generated", "generated/components/root": "generated",
	} {
		if got[path].Section != section {
			t.Errorf("%s: section %q, want %q", path, got[path].Section, section)
		}
	}
	if _, ok := got["knowledge/Bad Section/x.md"]; ok {
		t.Error("a file in a wrong section was indexed")
	}
	if res.Indexed != 3 || !res.Generated || len(res.Skipped) != 1 || !strings.HasPrefix(res.Skipped[0], "knowledge/runbooks/broken.md") {
		t.Errorf("result = %+v", res)
	}
	if got["docs/adr/0001-q.md"].Provenance.AuthorRole != "architect" || got["CONTRIBUTING.md"].Provenance.AuthorRole != "human" {
		t.Errorf("provenance = %+v / %+v", got["docs/adr/0001-q.md"].Provenance, got["CONTRIBUTING.md"].Provenance)
	}
	if e.emb.Calls() == 0 {
		t.Error("entries were not embedded")
	}

	// Nothing changed: nothing is read or written again.
	calls := e.emb.Calls()
	res, err = e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	if res.Indexed != 0 || res.Removed != 0 || res.Generated || e.emb.Calls() != calls {
		t.Errorf("second sync = %+v", res)
	}

	// The default policy shows in the mapped paths and the missing sections.
	policy := e.syncer.Policy(e.project)
	if got := knowledge.MissingSections(policy); strings.Join(got, ",") != "testing,glossary" {
		t.Errorf("missing sections = %v", got)
	}
	paths := e.syncer.MappedPaths(e.project)
	for _, want := range []string{"knowledge/**", knowledge.PolicyFile, "docs/adr/**", "CONTRIBUTING.md"} {
		found := false
		for _, p := range paths {
			found = found || p == want
		}
		if !found {
			t.Errorf("mapped paths %v lack %s", paths, want)
		}
	}
}

func TestSyncPushRenamesFileAndFlagsDrift(t *testing.T) {
	e := newSyncEnv(t)
	e.commit(t, "h1", sourceFiles)
	must(t, e.tr.SetHeadOfBranch("acme/app", "main", "h1"))
	_, err := e.syncer.Sync(t.Context(), e.project, "", "")
	must(t, err)
	if rv := e.reverify(t); len(rv) != 0 {
		t.Fatalf("flagged before any push: %v", rv)
	}

	// The push: internal/old.go is renamed, the contributing guide edited,
	// and the ADR file deleted.
	after := map[string]string{}
	maps.Copy(after, sourceFiles)
	after["internal/new.go"] = after["internal/old.go"]
	delete(after, "internal/old.go")
	delete(after, "docs/adr/0001-q.md")
	after["CONTRIBUTING.md"] = "# Contributing\nSmaller PRs.\n"
	after["knowledge/lessons/l.md"] = "A lesson mentioning internal/new.go and internal/other.go.\n"
	e.commit(t, "h2", after)
	res, err := e.syncer.Sync(t.Context(), e.project, "h1", "h2")
	must(t, err)
	if res.Removed != 1 || res.Indexed != 2 {
		t.Errorf("result = %+v", res)
	}
	got := e.entries(t)
	if _, ok := got["docs/adr/0001-q.md"]; ok {
		t.Error("the deleted file is still indexed")
	}
	if got["CONTRIBUTING.md"].Body != "# Contributing\nSmaller PRs.\n" {
		t.Errorf("edited entry = %q", got["CONTRIBUTING.md"].Body)
	}
	if !got["generated/components/root"].Generated || got["generated/components/root"].Version != "h2" {
		t.Errorf("generated section = %+v", got["generated/components/root"])
	}
	// No entry left mentions old.go (the ADR is gone); the lesson mentions
	// new.go but was read in this very sync, so it is current.
	if rv := e.reverify(t); len(rv) != 0 {
		t.Errorf("flagged = %+v", rv)
	}
}

func (e *syncEnv) reverify(t *testing.T) []domain.KnowledgeEntry {
	t.Helper()
	rv, err := e.st.Knowledge().List(t.Context(), e.project.AccountID, store.KnowledgeFilter{ProjectID: e.project.ID, NeedsReverify: true})
	must(t, err)
	return rv
}

func TestSyncDriftReason(t *testing.T) {
	e := newSyncEnv(t)
	e.syncer.Snapshots = nil
	e.commit(t, "h1", sourceFiles)
	_, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	next := map[string]string{}
	maps.Copy(next, sourceFiles)
	next["internal/new.go"] = next["internal/old.go"]
	delete(next, "internal/old.go")
	e.commit(t, "h2", next)
	res, err := e.syncer.Sync(t.Context(), e.project, "h1", "h2")
	must(t, err)
	rv := e.reverify(t)
	if res.Flagged != 1 || len(rv) != 1 || rv[0].Path != "docs/adr/0001-q.md" ||
		!strings.Contains(rv[0].ReverifyReason, "commit h2 changed internal/old.go") {
		t.Fatalf("flagged %+v: %+v", res, rv)
	}
	// Reading the entry again after a human edits it clears the flag.
	next["docs/adr/0001-q.md"] = "Jobs are leased in internal/new.go.\n"
	e.commit(t, "h3", next)
	if _, err = e.syncer.Sync(t.Context(), e.project, "h2", "h3"); err != nil {
		t.Fatal(err)
	}
	if rv = e.reverify(t); len(rv) != 0 {
		t.Errorf("still flagged after an edit: %+v", rv)
	}
}

func TestSyncPolicyFile(t *testing.T) {
	e := newSyncEnv(t)
	files := map[string]string{
		knowledge.PolicyFile: "version: 1\nsections:\n  handbook:\n    paths: [\"handbook/**\"]\n    protected: true\n",
		"handbook/a.md":      "# Handbook\n", "docs/adr/0001-q.md": "adr", "knowledge/runbooks/r.md": "not mapped by this policy",
	}
	e.commit(t, "h1", files)
	e.syncer.Snapshots = nil
	_, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	got := e.entries(t)
	if len(got) != 1 || got["handbook/a.md"].Section != "handbook" {
		t.Errorf("entries = %+v", got)
	}
	if !e.syncer.Policy(e.project).Sections["handbook"].Protected {
		t.Error("the policy file was not used")
	}

	e.commit(t, "h2", map[string]string{knowledge.PolicyFile: "version: 2\n"})
	if _, err := e.syncer.Sync(t.Context(), e.project, "", "h2"); !errors.Is(err, knowledge.ErrInvalid) {
		t.Errorf("an invalid policy: %v", err)
	}
	if len(e.entries(t)) != 1 {
		t.Error("an invalid policy changed the index")
	}
}

func TestSyncErrors(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.syncer.Sync(t.Context(), e.project, "", ""); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("unknown branch: %v", err)
	}
	if _, err := e.syncer.Sync(t.Context(), e.project, "", "nope"); err == nil {
		t.Error("an unknown commit was synced")
	}
	// A vanished previous commit only skips the drift check.
	e.commit(t, "h1", sourceFiles)
	e.syncer.Snapshots = nil
	if _, err := e.syncer.Sync(t.Context(), e.project, "lost", "h1"); err != nil {
		t.Errorf("sync with a lost previous commit: %v", err)
	}
	if res, err := e.syncer.Sync(t.Context(), e.project, "0000000000000000000000000000000000000000", "h1"); err != nil || res.Flagged != 0 {
		t.Errorf("sync of a new branch: %+v, %v", res, err)
	}
}

func TestGeneratedTakesNoProposals(t *testing.T) {
	p := domain.KnowledgeProposal{Provider: "repo", Section: domain.SectionGenerated, Path: "generated/packages", Action: domain.KnowledgeEdit,
		Body: "x", Reason: "r", ProposedBy: "developer", Batch: domain.BatchStandalone, Status: domain.KnowledgeQueued}
	if err := p.Validate(); !errors.Is(err, domain.ErrInvalidRecord) || !strings.Contains(err.Error(), "takes no proposals") {
		t.Errorf("a proposal to the generated section: %v", err)
	}
}
