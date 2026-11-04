package playbooks_test

import (
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// seedEnv is an onboarding of a repo at commit head1 with files, indexed by
// a real syncer.
func seedEnv(t *testing.T, files map[string]string) *onboarding {
	t.Helper()
	v := newEnv(t)
	for p, c := range files {
		must(t, v.tracker.SetFile("acme/app", "head1", p, []byte(c)))
	}
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	v.knowledge = &knowledge.Syncer{Store: v.st, Index: &knowledge.Index{Store: v.st}, API: v.tracker, Now: v.clock.Now}
	v.register()
	return startOnboarding(t, v)
}

// approve plays the human on the profile gate and waits for the curator.
func (o *onboarding) approveProfile(t *testing.T) {
	t.Helper()
	o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
}

func TestSeedAsksOnlyForSectionsWithoutALocation(t *testing.T) {
	o := seedEnv(t, map[string]string{
		"docs/adr/0001-queue.md": "# Queue jobs\n\nWe lease jobs.\n", "CONTRIBUTING.md": "# Contributing\n",
		"knowledge/runbooks/deploy.md": "Run make deploy.\n", "README.md": "# App\n",
	})
	o.approveProfile(t)

	w := o.waitStep(t, "curate/0/write", domain.StepWaiting)
	var req = o.request(w.JobID)
	if req.Role != "knowledge-curator" || req.Branch != "agent/kb-seed" || req.Repo != "acme/app" {
		t.Errorf("curator job = %+v", req)
	}
	fill, existing, _ := strings.Cut(req.Feedback, "## Sections that already have a location")
	if !strings.Contains(fill, "testing, glossary") || strings.Contains(fill, "architecture") || strings.Contains(fill, "conventions") || strings.Contains(fill, "runbooks") {
		t.Errorf("sections to fill: %q", fill)
	}
	for _, want := range []string{"architecture: docs/adr/**", "conventions: CONTRIBUTING.md", "runbooks: knowledge/runbooks/**"} {
		if !strings.Contains(existing, want) {
			t.Errorf("existing locations lack %q:\n%s", want, existing)
		}
	}
	// The repo's own entries were indexed before the curator started.
	got, err := o.st.Knowledge().List(t.Context(), o.account, store.KnowledgeFilter{ProjectID: o.project.ID})
	must(t, err)
	if len(got) != 3 {
		t.Errorf("indexed %d entries, want the ADR, the guide and the runbook", len(got))
	}

	// The curator writes its entries on agent/kb-seed; its PR waits for a human.
	o.finishJob(t, w.JobID, []byte(`{"summary":"Added testing notes from AGENTS.md.","entries":[],"skipped":[{"section":"glossary","reason":"no source"}],"assumptions":[]}`), "kb1", 0)
	g := o.pendingGate(t, domain.GateKnowledgeProposal)
	pr, err := o.tracker.FindPR(t.Context(), "acme/app", "agent/kb-seed")
	must(t, err)
	if g.SubjectRepo != "acme/app" || g.SubjectNumber != pr.Number || !strings.Contains(pr.Body, "testing notes") || pr.Base != "main" {
		t.Errorf("gate %+v, pr %+v", g, pr)
	}
	o.decide(t, domain.GateKnowledgeProposal, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	if merged, _ := o.tracker.GetPR(t.Context(), "acme/app", pr.Number); !merged.Merged {
		t.Error("the seed PR was not merged")
	}
}

func TestSeedWithEverySectionMappedSkipsTheCurator(t *testing.T) {
	files := map[string]string{"docs/adr/0001.md": "adr", "CONTRIBUTING.md": "c"}
	for _, s := range knowledge.SeedSections {
		files["knowledge/"+s+"/x.md"] = "x"
	}
	o := seedEnv(t, files)
	o.approveProfile(t)
	o.waitRun(t, domain.RunSucceeded)
	for _, j := range o.jobs.Requests() {
		if j.Role == "knowledge-curator" {
			t.Errorf("curator started: %+v", j)
		}
	}
}

func TestSeedIsQuietWithoutAKnowledgeBaseOrARepo(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "knowledge.seed", nil)
	if m := toMap(t, out); err != nil || len(m["curate"].([]any)) != 0 {
		t.Errorf("without a knowledge base: %v, %v", m, err)
	}
	// A repo that cannot be read: the branch is unknown to the fake GitHub.
	v.knowledge = &knowledge.Syncer{Store: v.st, Index: &knowledge.Index{Store: v.st}, API: v.tracker}
	v.register()
	out, err = v.call(t, "knowledge.seed", nil)
	if m := toMap(t, out); err != nil || m["error"] == "" || len(m["curate"].([]any)) != 0 {
		t.Errorf("unreadable repo: %v, %v", m, err)
	}
}
