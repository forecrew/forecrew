package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
)

// knowledgeEnv is an onboarded project whose repo has an ADR that mentions
// internal/old.go. Commit before1 has the file, after1 has it renamed.
func knowledgeEnv(t *testing.T) (*env, *knowledge.Syncer) {
	t.Helper()
	e := newEnv(t)
	for _, to := range []domain.ProjectStatus{domain.ProjectOnboarding, domain.ProjectOnboarded} {
		must(t, e.st.Projects().UpdateStatus(t.Context(), e.account, e.project.ID, e.project.Status, to))
		e.project.Status = to
	}
	syncer := &knowledge.Syncer{
		Store: e.st, Index: &knowledge.Index{Store: e.st}, API: e.tracker, Now: e.clock.Now,
		Snapshots: knowledge.ArchiveSnapshots{Archiver: e.tracker, Analyzer: analyzer.Local{}, TempDir: t.TempDir()},
	}
	files := map[string]string{
		"go.mod": "module x\n", "internal/old.go": "package internal\n\nfunc LeaseNext() {}\n",
		"docs/adr/0001.md": "Jobs are leased in internal/old.go.\n",
	}
	for p, c := range files {
		must(t, e.tracker.SetFile(repo, "before1", p, []byte(c)))
		if p != "internal/old.go" {
			must(t, e.tracker.SetFile(repo, "after1", p, []byte(c)))
		}
	}
	must(t, e.tracker.SetFile(repo, "after1", "internal/new.go", []byte(files["internal/old.go"])))
	must(t, e.tracker.SetHeadOfBranch(repo, "main", "before1"))
	e.eng.Knowledge = syncer
	return e, syncer
}

func (e *env) entries(t *testing.T) map[string]domain.KnowledgeEntry {
	t.Helper()
	all, err := e.st.Knowledge().List(t.Context(), e.account, store.KnowledgeFilter{ProjectID: e.project.ID})
	must(t, err)
	out := map[string]domain.KnowledgeEntry{}
	for _, x := range all {
		out[x.Path] = x
	}
	return out
}

func TestPushReindexesAndFlagsDrift(t *testing.T) {
	e, syncer := knowledgeEnv(t)
	if _, err := syncer.Sync(t.Context(), e.project, "", "before1"); err != nil {
		t.Fatal(err)
	}
	if got := e.entries(t); got["docs/adr/0001.md"].NeedsReverify || got["generated/packages"].Version != "before1" {
		t.Fatalf("entries before the push = %+v", got)
	}

	e.send("p1", "push", fixture(t, "push_main.json"))

	got := e.entries(t)
	adr := got["docs/adr/0001.md"]
	if !adr.NeedsReverify || !strings.Contains(adr.ReverifyReason, "commit after1 changed internal/old.go") {
		t.Errorf("the ADR was not flagged: %+v", adr)
	}
	if got["generated/packages"].Version != "after1" || !strings.Contains(got["generated/packages"].Body, "internal") {
		t.Errorf("generated = %+v", got["generated/packages"])
	}
}

func TestPushIgnored(t *testing.T) {
	e, _ := knowledgeEnv(t)
	e.send("p1", "push", fixture(t, "push_feature.json"))
	e.send("p2", "push", fixture(t, "push_deleted.json"))
	if len(e.entries(t)) != 0 {
		t.Error("a push to another branch or a deleted branch was indexed")
	}
	// A project that is not onboarded is left alone.
	other := newEnv(t)
	other.eng.Knowledge = e.eng.Knowledge
	other.send("p3", "push", fixture(t, "push_main.json"))
	if len(other.entries(t)) != 0 {
		t.Error("a project that is not onboarded was indexed")
	}
	// Without a syncer a push is only acknowledged.
	e.eng.Knowledge = nil
	e.send("p4", "push", fixture(t, "push_main.json"))
}

type failingSyncer struct{}

func (failingSyncer) Sync(context.Context, domain.Project, string, string) (knowledge.SyncResult, error) {
	return knowledge.SyncResult{}, errors.New("boom")
}

func TestPushSyncFailureIsReported(t *testing.T) {
	e, _ := knowledgeEnv(t)
	e.eng.Knowledge = failingSyncer{}
	ev := github.Event{Kind: "push", Action: "pushed", Repo: repo, Ref: "refs/heads/main", SHA: "after1"}
	if err := e.eng.handle(t.Context(), ev); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("handle = %v", err)
	}
}

func TestPollerSyncsOnboardedProjects(t *testing.T) {
	e, _ := knowledgeEnv(t)
	must(t, e.poller().Reconcile(t.Context()))
	got := e.entries(t)
	if got["docs/adr/0001.md"].Version == "" || got["generated/packages"].Version != "before1" {
		t.Fatalf("entries = %+v", got)
	}
	// A new head is picked up; the same head is not read again.
	must(t, e.tracker.SetHeadOfBranch(repo, "main", "after1"))
	must(t, e.poller().Reconcile(t.Context()))
	if e.entries(t)["generated/packages"].Version != "after1" {
		t.Error("the poller did not follow the branch")
	}
	// A failing sync is reported but does not stop the pass.
	e.eng.Knowledge = failingSyncer{}
	if err := e.poller().Reconcile(t.Context()); err == nil || !strings.Contains(err.Error(), "sync knowledge") {
		t.Errorf("Reconcile = %v", err)
	}
}
