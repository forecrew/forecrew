package playbooks_test

import (
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/workflow"
)

func startCurate(t *testing.T, c *curateEnv) *runner {
	t.Helper()
	return startRun(t, c.env, "curate", workflow.StartRequest{
		AccountID: c.account, ProjectID: c.project.ID, Inputs: map[string]any{"repo": "acme/app"},
	})
}

// The curator job is given the entry that was flagged and the old note; what it
// found true loses its flag, the note it names is deleted; and discovery runs
// again: the tools that are not in the profile become PRs on it.
func TestCuratePlaybookVerifiesAndFindsNewTools(t *testing.T) {
	c := newCurateEnv(t)
	ctx := t.Context()
	// The repo links Jira (in the profile), Sentry and Linear (not).
	must(t, c.tracker.SetFile("acme/app", "head1", ".forecrew/profile.yaml", []byte(toolsProfile)))
	must(t, c.tracker.SetFile("acme/app", "main", ".forecrew/profile.yaml", []byte(toolsProfile)))
	must(t, c.tracker.SetFile("acme/app", "head1", "README.md", []byte("# app\n")))
	must(t, c.tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	must(t, c.tracker.AddAutolink("acme/app", github.Autolink{KeyPrefix: "ENG-", URLTemplate: "https://acme.atlassian.net/browse/ENG-<num>"}))
	_, err := c.tracker.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "Crash", Body: "Errors: https://acme.sentry.io/issues/7"})
	must(t, err)
	must(t, c.tracker.AddBranch("acme/app", "agent/plan"))
	_, err = c.tracker.CreatePR(ctx, "acme/app", github.PRInput{Title: "Plan", Body: "Spec: https://linear.app/acme/issue/ENG-1", Head: "agent/plan", Base: "main"})
	must(t, err)

	r := startCurate(t, c)
	s := r.waitStep(t, "curate/0/curator", domain.StepWaiting)
	reqs := r.jobs.Requests()
	if len(reqs) != 1 || reqs[0].Role != "knowledge-curator" || reqs[0].Task != "curate" ||
		!strings.Contains(reqs[0].Feedback, "docs/adr/flagged.md") || !strings.Contains(reqs[0].Feedback, "note:"+c.notes["unread"].ID.String()) {
		t.Fatalf("curator request = %+v", reqs)
	}
	r.finishJob(t, s.JobID, []byte(`{"summary":"checked","reverified":[{"path":"docs/adr/flagged.md","reason":"lease.go still leases with SKIP LOCKED"}],`+
		`"drop_notes":[{"note":"`+c.notes["unread"].ID.String()+`","reason":"stale"}],"proposals":[],"assumptions":[]}`), "", 0.7)
	run := r.waitRun(t, domain.RunSucceeded)

	flagged, err := c.st.Knowledge().GetByPath(ctx, c.account, c.project.ID, "repo", "docs/adr/flagged.md")
	must(t, err)
	if flagged.NeedsReverify || flagged.Provenance.LastVerified.IsZero() {
		t.Errorf("the flag was not cleared: %+v", flagged)
	}
	if _, err := c.st.Knowledge().Get(ctx, c.account, c.notes["unread"].ID); err == nil {
		t.Error("the unread note was not deleted")
	}
	steps, err := c.st.StepRuns().ListByRun(ctx, c.account, run.ID)
	must(t, err)
	var tools string
	for _, st := range steps {
		if st.StepID == "tools" {
			tools = string(st.Output)
		}
	}
	for _, want := range []string{`"tool":"sentry"`, `"tool":"linear"`, `"reason":"new"`} {
		if !strings.Contains(tools, want) {
			t.Errorf("the tools step output lacks %s: %s", want, tools)
		}
	}
	if n := strings.Count(tools, `"number"`); n != 2 {
		t.Errorf("%d PRs in the tools step output, want 2: %s", n, tools)
	}
}

// With nothing to curate no job is created, and the run still looks at the
// tools.
func TestCuratePlaybookWithNothingToCurate(t *testing.T) {
	v := toolsEnv(t, toolsProfile)
	c := &curateEnv{env: v}
	r := startCurate(t, c)
	r.waitRun(t, domain.RunSucceeded)
	if reqs := r.jobs.Requests(); len(reqs) != 0 {
		t.Errorf("jobs = %+v", reqs)
	}
}
