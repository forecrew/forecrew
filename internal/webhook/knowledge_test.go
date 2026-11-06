package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
)

// knowledgeEnv is an onboarded project whose repo has an ADR that mentions
// internal/old.go. Commit before1 has the file, after1 has it renamed.
func knowledgeEnv(t *testing.T) (*env, *knowledge.Syncer, *knowledge.Scheduler) {
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
	sched := &knowledge.Scheduler{Syncer: syncer, Now: e.clock.Now}
	e.eng.Knowledge = sched
	return e, syncer, sched
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
	e, syncer, sched := knowledgeEnv(t)
	if _, err := syncer.Sync(t.Context(), e.project, "", "before1"); err != nil {
		t.Fatal(err)
	}
	if got := e.entries(t); got["docs/adr/0001.md"].NeedsReverify || got["generated/packages"].Version != "before1" {
		t.Fatalf("entries before the push = %+v", got)
	}

	e.send("p1", "push", fixture(t, "push_main.json"))
	// The handler only queued the sync: nothing was read on its goroutine.
	if sched.Pending() != 1 || e.entries(t)["generated/packages"].Version != "before1" {
		t.Fatalf("pending = %d, entries = %+v", sched.Pending(), e.entries(t))
	}
	sched.Drain(t.Context())

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
	e, _, sched := knowledgeEnv(t)
	e.send("p1", "push", fixture(t, "push_feature.json"))
	e.send("p2", "push", fixture(t, "push_deleted.json"))
	if sched.Pending() != 0 {
		t.Error("a push to another branch or a deleted branch was queued")
	}
	// A project that is not onboarded is left alone.
	other := newEnv(t)
	other.eng.Knowledge = e.eng.Knowledge
	other.send("p3", "push", fixture(t, "push_main.json"))
	if sched.Pending() != 0 {
		t.Error("a project that is not onboarded was queued")
	}
	// Without a syncer a push is only acknowledged.
	e.eng.Knowledge = nil
	e.send("p4", "push", fixture(t, "push_main.json"))
}

func TestPollerQueuesOnboardedProjects(t *testing.T) {
	e, _, sched := knowledgeEnv(t)
	must(t, e.poller().Reconcile(t.Context()))
	sched.Drain(t.Context())
	got := e.entries(t)
	if got["docs/adr/0001.md"].Version == "" || got["generated/packages"].Version != "before1" {
		t.Fatalf("entries = %+v", got)
	}
	// A new head is picked up; the same head is not read again.
	must(t, e.tracker.SetHeadOfBranch(repo, "main", "after1"))
	must(t, e.poller().Reconcile(t.Context()))
	sched.Drain(t.Context())
	if e.entries(t)["generated/packages"].Version != "after1" {
		t.Error("the poller did not follow the branch")
	}
}

func TestMergedPRApprovesKnowledgeProposalGate(t *testing.T) {
	e := newEnv(t)
	e.addChange(9, "abc123")
	g := e.openGate(domain.GateKnowledgeProposal, 9)
	payload := strings.ReplaceAll(string(fixture(t, "pull_request_merged.json")), `"number": 7`, `"number": 9`)
	payload = strings.ReplaceAll(payload, `"number":7`, `"number":9`)
	e.send("kb1", "pull_request", []byte(payload))
	if got := e.gate(g); got.Status != domain.GateApproved {
		t.Errorf("knowledge gate = %s, want approved", got.Status)
	}
}

type proposalFollower struct {
	got []string
	err error
}

func (f *proposalFollower) PRClosed(_ context.Context, _ uuid.UUID, repo string, number int, merged bool) error {
	f.got = append(f.got, fmt.Sprint(repo, "#", number, " ", merged))
	return f.err
}

func TestClosedPRSettlesItsProposals(t *testing.T) {
	e := newEnv(t)
	f := &proposalFollower{err: errors.New("no")}
	e.eng.Proposals = f
	e.addChange(7, "abc123")
	e.send("kb2", "pull_request", fixture(t, "pull_request_merged.json"))
	if len(f.got) != 1 || !strings.HasSuffix(f.got[0], "#7 true") {
		t.Errorf("follower = %v", f.got)
	}
	// It failed: the change stays open, for the poller to try again.
	if got := e.change(7); got.State != domain.ChangeOpen {
		t.Errorf("change = %+v", got)
	}
}

func TestPRClosedRejectsTestPlanApprovalGate(t *testing.T) {
	e := newEnv(t)
	e.addChange(9, "abc123")
	g := e.openGate(domain.GateTestPlanApproval, 9)
	payload := strings.ReplaceAll(string(fixture(t, "pull_request_closed.json")), `"number": 7`, `"number": 9`)
	payload = strings.ReplaceAll(payload, `"number":7`, `"number":9`)
	e.send("tp1", "pull_request", []byte(payload))
	if got := e.gate(g); got.Status != domain.GateRejected {
		t.Errorf("test plan gate = %s, want rejected", got.Status)
	}
}

func TestMergedPRApprovesTestPlanApprovalGate(t *testing.T) {
	e := newEnv(t)
	e.addChange(9, "abc123")
	g := e.openGate(domain.GateTestPlanApproval, 9)
	payload := strings.ReplaceAll(string(fixture(t, "pull_request_merged.json")), `"number": 7`, `"number": 9`)
	payload = strings.ReplaceAll(payload, `"number":7`, `"number":9`)
	e.send("tp2", "pull_request", []byte(payload))
	if got := e.gate(g); got.Status != domain.GateApproved {
		t.Errorf("test plan gate = %s, want approved", got.Status)
	}
}
