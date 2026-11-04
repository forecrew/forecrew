package knowledge_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/testkit"
)

// repoFixture is a repo with a mapped ADR folder, the fallback knowledge/
// folder, a file without frontmatter and files in wrong sections.
var repoFixture = map[string]string{
	"docs/adr/0001-queue.md":          "---\ntitle: Queue jobs in Postgres\nauthor_role: architect\nsource: JOB-41\ncreated: 2026-10-01\nlast_verified: 2026-10-20T09:30:00Z\n---\n# ADR 1\n\nWe lease jobs with SKIP LOCKED.\n",
	"docs/adr/template.png":           "binary",
	"CONTRIBUTING.md":                 "# Contributing\n\nSmall pull requests.\n",
	"CODEOWNERS":                      "* @acme/core\n",
	"knowledge/runbooks/deploy.md":    "---\ntitle: Deploy\nauthor_role: devops\nsource: human\ncreated: 2026-09-01\n---\nRun make deploy.\n",
	"knowledge/vision/why.md":         "We build X.\n",
	"knowledge/Bad Section/x.md":      "wrong section name",
	"knowledge/notes/x.md":            "reserved section",
	"knowledge/loose.md":              "no section folder",
	"src/main.go":                     "package main\n",
	".forecrew/knowledge-policy.yaml": "version: 1\n",
}

func newRepo(t *testing.T) (*testkit.Tracker, *knowledge.RepoClient) {
	t.Helper()
	tr := testkit.NewTracker(nil)
	tr.Install("acme/app")
	for p, c := range repoFixture {
		if err := tr.SetFile("acme/app", "head1", p, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	return tr, &knowledge.RepoClient{API: tr, Repo: "acme/app", Branch: "main", Ref: "head1"}
}

func defaultPolicy() knowledge.Policy {
	var files []string
	for p := range repoFixture {
		files = append(files, p)
	}
	src, _ := analyzer.KnowledgeSourcesOf(files, func(string) ([]byte, error) { return nil, errors.New("none") })
	return knowledge.DefaultPolicy(src, files)
}

func TestDefaultPolicyMapping(t *testing.T) {
	p := defaultPolicy()
	if !p.Sections["vision"].Protected || !p.Sections["security"].Protected || p.Sections["architecture"].Protected {
		t.Errorf("protected sections = %+v", p.Sections)
	}
	for _, name := range []string{"architecture", "conventions", "owners", "runbooks", "vision", "security"} {
		if _, ok := p.Sections[name]; !ok {
			t.Errorf("section %s missing", name)
		}
	}
	if _, ok := p.Sections["Bad Section"]; ok {
		t.Error("an invalid folder name became a section")
	}
	if _, ok := p.Sections["notes"]; ok {
		t.Error("a reserved folder became a section")
	}
	m := knowledge.RepoMapping(p)
	want := map[string]string{
		"docs/adr/0001-queue.md": "architecture", "CONTRIBUTING.md": "conventions", "CODEOWNERS": "owners",
		"knowledge/runbooks/deploy.md": "runbooks", "knowledge/vision/why.md": "vision",
	}
	for file, section := range want {
		if got, ok := m.SectionOf(file, 10); !ok || got != section {
			t.Errorf("SectionOf(%s) = %q, %v, want %s", file, got, ok, section)
		}
	}
	for _, file := range []string{"docs/adr/template.png", "knowledge/Bad Section/x.md", "knowledge/notes/x.md", "knowledge/loose.md", "src/main.go", knowledge.PolicyFile} {
		if got, ok := m.SectionOf(file, 10); ok {
			t.Errorf("SectionOf(%s) = %q, want none", file, got)
		}
	}
	if _, ok := m.SectionOf("CONTRIBUTING.md", knowledge.MaxEntryBytes+1); ok {
		t.Error("a huge file is an entry")
	}
	if len(m.Globs()) < 6 {
		t.Errorf("globs = %v", m.Globs())
	}
}

func TestIndexableExcludes(t *testing.T) {
	globs := []string{"docs/**", "!docs/private/**"}
	if !knowledge.Indexable(globs, "docs/a.md", 1) || knowledge.Indexable(globs, "docs/private/a.md", 1) || knowledge.Indexable(globs, "docs/a.go", 1) {
		t.Error("exclusions or extensions not applied")
	}
}

func TestRepoClientListFetch(t *testing.T) {
	_, rc := newRepo(t)
	ctx := t.Context()
	m := knowledge.RepoMapping(defaultPolicy())
	refs, err := rc.List(ctx, m.Globs())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range refs {
		paths = append(paths, r.Path)
		if len(r.Version) != 40 {
			t.Errorf("%s version = %q", r.Path, r.Version)
		}
	}
	want := []string{"CODEOWNERS", "CONTRIBUTING.md", "docs/adr/0001-queue.md", "knowledge/runbooks/deploy.md", "knowledge/vision/why.md"}
	if !slices.Equal(paths, want) {
		t.Errorf("listed %v, want %v", paths, want)
	}

	doc, err := rc.Fetch(ctx, "docs/adr/0001-queue.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Queue jobs in Postgres" || doc.Provenance.AuthorRole != "architect" || doc.Provenance.Source != "JOB-41" ||
		doc.Provenance.LastVerified.IsZero() || doc.SourceURL != "https://github.com/acme/app/blob/main/docs/adr/0001-queue.md" {
		t.Errorf("document = %+v", doc)
	}
	if doc.Body == "" || doc.Body[0] == '-' {
		t.Errorf("body = %q", doc.Body)
	}

	// No frontmatter: provenance from git, human and bot authors.
	plain, err := rc.Fetch(ctx, "CONTRIBUTING.md")
	if err != nil || plain.Provenance.AuthorRole != "human" || plain.Provenance.Source != "https://github.com/acme/app/commit/c0ffee0" || plain.Provenance.Created.IsZero() {
		t.Errorf("plain document = %+v, %v", plain, err)
	}
	tr, rc2 := newRepo(t)
	_ = tr.SetLastCommit("acme/app", "CONTRIBUTING.md", github.Commit{SHA: "b0t", Author: "forecrew[bot]", Bot: true})
	if d, _ := rc2.Fetch(ctx, "CONTRIBUTING.md"); d.Provenance.AuthorRole != "agent" {
		t.Errorf("bot document = %+v", d.Provenance)
	}

	if _, err := rc.Fetch(ctx, "nope.md"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Errorf("Fetch of a missing file = %v", err)
	}
	// A frontmatter that breaks its schema is refused.
	tr3, rc3 := newRepo(t)
	_ = tr3.SetFile("acme/app", "head1", "knowledge/runbooks/broken.md", []byte("---\ncreated: yesterday\n---\nbody"))
	if _, err := rc3.Fetch(ctx, "knowledge/runbooks/broken.md"); !errors.Is(err, knowledge.ErrInvalid) {
		t.Errorf("Fetch of a broken file = %v", err)
	}
	empty := &knowledge.RepoClient{API: tr, Repo: "acme/app", Branch: "main", Ref: "unknown"}
	if _, err := empty.List(ctx, []string{"**"}); err == nil {
		t.Error("an unknown ref was listed")
	}
	if _, err := empty.Fetch(ctx, "a.md"); err == nil {
		t.Error("an unknown ref was fetched")
	}
}

func TestRepoClientChanges(t *testing.T) {
	tr, rc := newRepo(t)
	ctx := t.Context()
	globs := knowledge.RepoMapping(defaultPolicy()).Globs()
	all, next, err := rc.Changes(ctx, globs, "")
	if err != nil || next != "head1" || len(all) != 5 || all[0].Deleted {
		t.Fatalf("full changes = %+v, %q, %v", all, next, err)
	}
	// head0 is the repo before: one entry edited, one renamed, one removed, one added.
	for p, c := range repoFixture {
		_ = tr.SetFile("acme/app", "head0", p, []byte(c))
	}
	_ = tr.SetFile("acme/app", "head0", "CONTRIBUTING.md", []byte("# Old\n"))
	_ = tr.SetFile("acme/app", "head0", "knowledge/runbooks/old-deploy.md", []byte("---\ntitle: Deploy\nauthor_role: devops\nsource: human\ncreated: 2026-09-01\n---\nOlder.\n"))
	_ = tr.SetFile("acme/app", "head0", "docs/adr/0002-gone.md", []byte("gone"))
	_ = tr.SetFile("acme/app", "head0", "knowledge/runbooks/deploy.md", []byte("---\ntitle: Deploy\nauthor_role: devops\nsource: human\ncreated: 2026-09-01\n---\nOlder.\n"))
	got, next, err := rc.Changes(ctx, globs, "head0")
	if err != nil || next != "head1" {
		t.Fatalf("changes = %+v, %q, %v", got, next, err)
	}
	summary := map[string]bool{}
	for _, c := range got {
		summary[c.Path] = c.Deleted
	}
	if d, ok := summary["docs/adr/0002-gone.md"]; !ok || !d {
		t.Errorf("a removed entry is not a deletion: %v", summary)
	}
	if d, ok := summary["CONTRIBUTING.md"]; !ok || d {
		t.Errorf("an edited entry is not a change: %v", summary)
	}
	if _, ok := summary["src/main.go"]; ok {
		t.Errorf("a source file is an entry change: %v", summary)
	}
	if _, _, err := rc.Changes(ctx, globs, "nowhere"); err == nil {
		t.Error("an unknown cursor was accepted")
	}
}
