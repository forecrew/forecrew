package knowledge_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	clock   *testkit.Clock
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
	return &syncEnv{clock: clock, tr: tr, st: st, emb: emb, project: p, syncer: &knowledge.Syncer{
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
	policy, err := e.syncer.Policy(t.Context(), e.project)
	must(t, err)
	if got := knowledge.MissingSections(policy); strings.Join(got, ",") != "testing,glossary" {
		t.Errorf("missing sections = %v", got)
	}
	paths, err := e.syncer.MappedPaths(t.Context(), e.project)
	must(t, err)
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
	if pol, err := e.syncer.Policy(t.Context(), e.project); err != nil || !pol.Sections["handbook"].Protected {
		t.Errorf("the policy file was not used: %v", err)
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

var errTest = errors.New("boom")

// A repo without commits has no policy file and nothing to protect, so the
// default policy holds (and is not kept); a 404 on the branch or repo and
// any other read error leave the policy unknown.
func TestPolicyOfAnEmptyRepo(t *testing.T) {
	e := newSyncEnv(t) // the fake has no branch yet
	api := &headErr{SyncAPI: e.tr, err: github.ErrEmptyRepo}
	s := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
	pol, perr := s.Policy(t.Context(), e.project)
	if perr != nil || len(knowledge.MissingSections(pol)) != len(knowledge.SeedSections) {
		t.Errorf("empty repo: policy = %+v, %v", pol, perr)
	}
	for _, err := range []error{github.ErrNotFound, errors.New("502")} {
		api := &headErr{SyncAPI: e.tr, err: err}
		s := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
		if _, perr := s.Policy(t.Context(), e.project); !errors.Is(perr, knowledge.ErrPolicyUnknown) {
			t.Errorf("%v: err = %v, want ErrPolicyUnknown", err, perr)
		}
	}
}

// A restrictive policy must not become the default when the default branch
// was renamed or the repo is no longer readable (404 on the branch): the
// policy is unknown, so search refuses and a job's write paths cannot be
// computed.
func TestPolicyFailsClosedOnBranch404(t *testing.T) {
	e := newSyncEnv(t)
	e.commit(t, "h1", map[string]string{
		knowledge.PolicyFile: "version: 1\nsections:\n  handbook:\n    paths: [\"handbook/**\"]\n    protected: true\n    access:\n      read: [architect]\n",
		"handbook/a.md":      "# Handbook\nsecret plans\n",
	})
	api := &headErr{SyncAPI: e.tr, err: fmt.Errorf("get branch: %w", github.ErrNotFound)}
	restarted := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
	if _, err := restarted.Policy(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Fatalf("Policy = %v, want ErrPolicyUnknown", err)
	}
	if _, err := restarted.MappedPaths(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("MappedPaths = %v, want ErrPolicyUnknown", err)
	}
	search := &knowledge.Search{Index: restarted.Index, Policy: restarted.Policy}
	if _, err := search.Run(t.Context(), knowledge.Query{Project: e.project, Text: "secret", Role: "developer"}); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("search = %v, want ErrPolicyUnknown", err)
	}
}

// policyDown makes the read of the policy file 404 while the file is still
// in the tree, as a transient GitHub inconsistency would.
type policyDown struct {
	knowledge.SyncAPI
	down bool
}

func (p *policyDown) GetFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	if p.down && path == knowledge.PolicyFile {
		return nil, fmt.Errorf("get %s: %w", path, github.ErrNotFound)
	}
	return p.SyncAPI.GetFile(ctx, repo, ref, path)
}

// A policy file that is listed in the tree but answers 404 is not an absent
// policy: Policy and Sync fail, the restrictive policy is not replaced, and
// nothing indexed is removed. When the read recovers, the policy is back.
func TestPolicyFileListedButUnreadable(t *testing.T) {
	e := newSyncEnv(t)
	e.commit(t, "h1", map[string]string{
		knowledge.PolicyFile: "version: 1\nsections:\n  handbook:\n    paths: [\"handbook/**\"]\n    protected: true\n    access:\n      read: [architect]\n",
		"handbook/a.md":      "# Handbook\nsecret plans\n",
	})
	must(t, e.tr.SetHeadOfBranch("acme/app", "main", "h1"))
	e.syncer.Snapshots = nil
	_, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	before := len(e.entries(t))

	api := &policyDown{SyncAPI: e.tr, down: true}
	s := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
	if _, err := s.Policy(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Fatalf("Policy = %v, want ErrPolicyUnknown", err)
	}
	if _, err := s.MappedPaths(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("MappedPaths = %v, want ErrPolicyUnknown", err)
	}
	if _, err := s.Sync(t.Context(), e.project, "", "h1"); err == nil {
		t.Error("Sync with an unreadable policy file succeeded")
	}
	if got := len(e.entries(t)); got != before {
		t.Errorf("entries = %d, want %d: a sync removed indexed entries", got, before)
	}

	api.down = false
	pol, err := s.Policy(t.Context(), e.project)
	if err != nil || !pol.Sections["handbook"].Protected {
		t.Errorf("policy after recovery = %+v, %v", pol, err)
	}
	if _, err := s.Sync(t.Context(), e.project, "", "h1"); err != nil {
		t.Errorf("Sync after recovery: %v", err)
	}
	if got := len(e.entries(t)); got != before {
		t.Errorf("entries after recovery = %d, want %d", got, before)
	}
}

type headErr struct {
	knowledge.SyncAPI
	err error
}

func (h *headErr) HeadSHA(context.Context, string, string) (string, error) { return "", h.err }

// failingAPI makes the reads of a SyncAPI fail while down is set.
type failingAPI struct {
	knowledge.SyncAPI
	down bool
}

func (f *failingAPI) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	if f.down {
		return "", errors.New("github is down")
	}
	return f.SyncAPI.HeadSHA(ctx, repo, branch)
}

// After a restart the policy is not in memory: it is loaded from the repo,
// and when that is impossible everything that needs it refuses instead of
// falling back to a default that would open protected sections.
func TestPolicySurvivesARestartAndFailsClosed(t *testing.T) {
	e := newSyncEnv(t)
	e.commit(t, "h1", map[string]string{
		knowledge.PolicyFile: "version: 1\nsections:\n  handbook:\n    paths: [\"handbook/**\"]\n    protected: true\n    access:\n      read: [architect]\n",
		"handbook/a.md":      "# Handbook\nsecret plans\n",
	})
	must(t, e.tr.SetHeadOfBranch("acme/app", "main", "h1"))
	e.syncer.Snapshots = nil
	_, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)

	api := &failingAPI{SyncAPI: e.tr}
	restarted := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
	search := &knowledge.Search{Index: restarted.Index, Policy: restarted.Policy}

	// The new Syncer never synced: the policy comes from the repo.
	hits, err := search.Run(t.Context(), knowledge.Query{Project: e.project, Text: "secret", Role: "developer"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("a developer read the protected section after a restart: %+v, %v", hits, err)
	}
	if hits, err = search.Run(t.Context(), knowledge.Query{Project: e.project, Text: "secret", Role: "architect"}); err != nil || len(hits) != 1 {
		t.Fatalf("the architect search = %+v, %v", hits, err)
	}
	paths, err := restarted.MappedPaths(t.Context(), e.project)
	if err != nil || !slices.Contains(paths, "handbook/**") {
		t.Fatalf("mapped paths = %v, %v", paths, err)
	}

	// A third Syncer that cannot reach GitHub refuses.
	api.down = true
	third := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
	if _, err := third.Policy(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("Policy = %v, want ErrPolicyUnknown", err)
	}
	if _, err := third.MappedPaths(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("MappedPaths = %v, want ErrPolicyUnknown", err)
	}
	refusing := &knowledge.Search{Index: third.Index, Policy: third.Policy}
	if _, err := refusing.Run(t.Context(), knowledge.Query{Project: e.project, Text: "secret", Role: "developer"}); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("search with an unknown policy = %v", err)
	}
	// A broken policy file is no policy either.
	api.down = false
	e.commit(t, "h2", map[string]string{knowledge.PolicyFile: "version: 2\n"})
	must(t, e.tr.SetHeadOfBranch("acme/app", "main", "h2"))
	fourth := &knowledge.Syncer{Store: e.st, Index: &knowledge.Index{Store: e.st}, API: api, Now: e.clock.Now}
	if _, err := fourth.Policy(t.Context(), e.project); !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("Policy with a broken file = %v", err)
	}
}

// One file that cannot be an entry is skipped with its reason; it must not
// fail the sync on every retry.
func TestSyncSkipsFilesThatCannotBeEntries(t *testing.T) {
	e := newSyncEnv(t)
	e.syncer.Snapshots = nil
	e.commit(t, "h1", map[string]string{
		"knowledge/runbooks/good.md":     "# Good\nDeploy with care.\n",
		"knowledge/runbooks/nul.md":      "# Nul\nbad \x00 byte\n",
		"knowledge/runbooks/latin1.md":   "caf\xe9 au lait\n",
		"knowledge/runbooks/blank.md":    "---\ntitle: \" \"\n---\nbody\n",
		"knowledge/runbooks/nultitle.md": "---\ntitle: \"a\\0b\"\n---\nbody\n",
	})
	for range 2 { // a restarted forecrewd retries and fails the same way: it must not fail at all
		e.syncer = &knowledge.Syncer{Store: e.st, Index: e.syncer.Index, API: e.tr, Now: e.clock.Now}
		res, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(res.Skipped) != 4 {
			t.Fatalf("skipped = %v", res.Skipped)
		}
		for _, want := range []string{"nul.md: ", "latin1.md: ", "blank.md: ", "nultitle.md: "} {
			if !slices.ContainsFunc(res.Skipped, func(s string) bool { return strings.Contains(s, want) && strings.Contains(s, "invalid") }) {
				t.Errorf("no skip reason for %s in %v", want, res.Skipped)
			}
		}
	}
	if got := e.entries(t); len(got) != 1 || got["knowledge/runbooks/good.md"].Title != "Good" {
		t.Errorf("entries = %+v", got)
	}
}

// failAfter makes GetFile fail once n files were read.
type failAfter struct {
	knowledge.SyncAPI
	n, reads int
}

func (f *failAfter) GetFile(ctx context.Context, repo, ref, file string) ([]byte, error) {
	if strings.HasPrefix(file, "knowledge/") {
		if f.reads++; f.reads > f.n {
			return nil, errors.New("rate limited")
		}
	}
	return f.SyncAPI.GetFile(ctx, repo, ref, file)
}

// A run reads a bounded number of files and saves them in batches, so a
// rate limit in the middle keeps what was read and the next run goes on.
func TestSyncIsBoundedAndKeepsProgress(t *testing.T) {
	e := newSyncEnv(t)
	e.syncer.Snapshots = nil
	files := map[string]string{}
	total := knowledge.MaxReadsPerSync + 60
	for i := range total {
		files[fmt.Sprintf("knowledge/runbooks/r%03d.md", i)] = fmt.Sprintf("# Runbook %d\nbody %d\n", i, i)
	}
	e.commit(t, "h1", files)

	api := &failAfter{SyncAPI: e.tr, n: 2*knowledge.SyncBatch + 5}
	e.syncer.API = api
	if _, err := e.syncer.Sync(t.Context(), e.project, "", "h1"); err == nil {
		t.Fatal("a rate limit was not reported")
	}
	saved := 2*knowledge.SyncBatch + 5 // every read before the failure, also the partial batch
	if got := len(e.entries(t)); got != saved {
		t.Fatalf("after the rate limit %d entries are saved, want %d", got, saved)
	}

	api.n = 1 << 30
	res, err := e.syncer.Sync(t.Context(), e.project, "", "h1")
	must(t, err)
	if !res.More || res.Indexed != knowledge.MaxReadsPerSync {
		t.Fatalf("second run = %+v", res)
	}
	for res.More {
		if res, err = e.syncer.Sync(t.Context(), e.project, "", "h1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.entries(t)); got != total {
		t.Errorf("%d entries, want %d", got, total)
	}
	// Done: the head counts as synced and nothing is read again.
	api.reads = 0
	if res, err = e.syncer.Sync(t.Context(), e.project, "", "h1"); err != nil || res.Indexed != 0 || api.reads != 0 {
		t.Errorf("after the last run: %+v, %v, %d reads", res, err, api.reads)
	}
}
