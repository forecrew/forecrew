package tools_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/tools"
)

// qualityRun stores a run of a suite in an environment for a commit.
func (f *fixture) qualityRun(t *testing.T, change domain.Change, sha, suite, env string, attempt int, status domain.TestRunStatus, counts domain.TestCounts, results ...domain.TestResult) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	f.clock.Advance(time.Minute)
	r := domain.TestRun{AccountID: f.account.ID, ProjectID: f.project.ID, JobID: j.ID, ChangeID: change.ID, SHA: sha, Suite: suite, Environment: env,
		Status: status, Counts: counts, Attempt: attempt}
	must(t, f.st.TestRuns().Create(t.Context(), &r, results))
	return j
}

// overlayRun stores a run of a base check job: it lays files over another ref.
func (f *fixture) overlayRun(t *testing.T, sha, suite string, status domain.TestRunStatus) {
	t.Helper()
	j := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand,
		Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand, Overlay: &domain.Overlay{}}}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	f.clock.Advance(time.Minute)
	r := domain.TestRun{AccountID: f.account.ID, ProjectID: f.project.ID, JobID: j.ID, SHA: sha, Suite: suite, Status: status, Attempt: 1}
	must(t, f.st.TestRuns().Create(t.Context(), &r, nil))
}

func TestProjectQuality(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// Nothing tested yet.
	got, err := f.svc.ProjectQuality(ctx, f.by, mcp.ProjectInput{Project: "app"})
	if err != nil || got.Project != "app" || got.DefaultBranch == "" || got.Baseline != nil || got.Changes == nil || len(got.Changes) != 0 {
		t.Fatalf("empty = %+v, %v", got, err)
	}

	// Baseline: an old commit, then the newest one with a rerun of one suite.
	f.qualityRun(t, domain.Change{}, "old0001", "unit", "", 1, domain.TestRunPassed, domain.TestCounts{Passed: 9})
	f.qualityRun(t, domain.Change{}, "base001", "unit", "linux", 1, domain.TestRunFailed, domain.TestCounts{Passed: 1, Failed: 1})
	rerun := f.qualityRun(t, domain.Change{}, "base001", "unit", "linux", 2, domain.TestRunPassed, domain.TestCounts{Passed: 2})
	e2e := f.qualityRun(t, domain.Change{}, "base001", "e2e", "", 1, domain.TestRunFailed, domain.TestCounts{Failed: 1},
		domain.TestResult{Name: "TestLogin", Classname: "web", Status: domain.TestFailed, Message: "boom"})

	task, _, open := f.newTaskWithReports(t)
	merged := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, Repo: "acme/app", Number: 3, HeadSHA: "mmm0001", State: domain.ChangeMerged}
	must(t, f.st.Changes().Upsert(ctx, &merged))
	f.qualityRun(t, merged, "mmm0001", "unit", "", 1, domain.TestRunPassed, domain.TestCounts{Passed: 1})
	// A run of an earlier head of the open PR does not show.
	f.qualityRun(t, open, "earlier", "slow", "", 1, domain.TestRunFailed, domain.TestCounts{Failed: 7})
	// Another pull request's run at the same sha, and a base check, do not count.
	f.qualityRun(t, merged, open.HeadSHA, "other-pr", "", 1, domain.TestRunFailed, domain.TestCounts{Failed: 5})
	f.overlayRun(t, open.HeadSHA, "unit", domain.TestRunPassed)
	f.overlayRun(t, "base001", "e2e", domain.TestRunPassed)
	// A run at the head of the open PR that was not tied to it counts for the PR, not for the baseline.
	f.qualityRun(t, domain.Change{}, open.HeadSHA, "untied", "", 1, domain.TestRunPassed, domain.TestCounts{Passed: 2})
	f.qualityRun(t, open, open.HeadSHA, "unit", "", 1, domain.TestRunFailed, domain.TestCounts{Failed: 1},
		domain.TestResult{Name: "TestBad", Status: domain.TestFailed, Message: "no"})
	f.storeReport(t, open, open.HeadSHA, domain.VerificationFailed)

	got, err = f.svc.ProjectQuality(ctx, f.by, mcp.ProjectInput{Project: "app"})
	if err != nil {
		t.Fatal(err)
	}
	b := got.Baseline
	if b == nil || b.SHA != "base001" || len(b.Cells) != 2 {
		t.Fatalf("baseline = %+v", b)
	}
	// Sorted by suite: e2e, then unit with its newest attempt.
	if c := b.Cells[0]; c.Suite != "e2e" || c.Job != e2e.Ref() || len(c.Failures) != 1 || c.Failures[0].Name != "TestLogin" || c.Cell == nil {
		t.Errorf("e2e cell = %+v", c)
	}
	if c := b.Cells[1]; c.Suite != "unit" || c.Environment != "linux" || c.Status != domain.TestRunPassed || c.Job != rerun.Ref() || len(c.Failures) != 0 || c.Failures == nil {
		t.Errorf("unit cell = %+v", c)
	}

	if b.SHA == open.HeadSHA {
		t.Fatalf("the head of an open pull request is the baseline: %+v", b)
	}
	if len(got.Changes) != 1 {
		t.Fatalf("changes = %+v, want only the open pull request", got.Changes)
	}
	c := got.Changes[0]
	if c.Number != 9 || c.Task != task.Ref() || c.SHA != open.HeadSHA || len(c.Cells) != 2 || c.Cells[0].Suite != "unit" || c.Cells[0].Counts.Failed != 1 || len(c.Cells[0].Failures) != 1 || c.Cells[1].Suite != "untied" {
		t.Errorf("change = %+v", c)
	}
	if c.Verification == nil || c.Verification.Status != domain.VerificationFailed || !c.Verification.Current {
		t.Errorf("verification = %+v", c.Verification)
	}

	// An open PR with no task and no report.
	bare := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, Repo: "acme/app", Number: 12, HeadSHA: "bare001", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &bare))
	got, err = f.svc.ProjectQuality(ctx, f.by, mcp.ProjectInput{Project: "app"})
	if err != nil || len(got.Changes) != 2 || got.Changes[0].Number != 12 || got.Changes[0].Task != "" || got.Changes[0].Verification != nil || len(got.Changes[0].Cells) != 0 {
		t.Errorf("bare change: %+v, %v", got.Changes, err)
	}
}

func TestProjectQualityCapsChanges(t *testing.T) {
	f := newFixture(t)
	for i := range mcp.MaxQualityChanges + 3 {
		c := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, Repo: "acme/app", Number: 100 + i, HeadSHA: "sha", State: domain.ChangeOpen}
		must(t, f.st.Changes().Upsert(t.Context(), &c))
	}
	got, err := f.svc.ProjectQuality(t.Context(), f.by, mcp.ProjectInput{Project: "app"})
	if err != nil || len(got.Changes) != mcp.MaxQualityChanges {
		t.Fatalf("changes = %d, %v", len(got.Changes), err)
	}
}

func TestProjectQualityErrors(t *testing.T) {
	f := newFixture(t)
	for name, in := range map[string]string{"no project": "", "unknown project": "nope"} {
		_, err := f.svc.ProjectQuality(t.Context(), f.by, mcp.ProjectInput{Project: in})
		want := mcp.ErrInvalidInput
		if in != "" {
			want = mcp.ErrNotFound
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", name, err, want)
		}
	}
	other, _, _ := f.otherAccount(t)
	f.qualityRun(t, domain.Change{}, "base001", "unit", "", 1, domain.TestRunPassed, domain.TestCounts{Passed: 1})
	// The other account has a project of the same name, and sees none of it.
	if got, err := f.svc.ProjectQuality(t.Context(), other, mcp.ProjectInput{Project: "app"}); err != nil || got.Baseline != nil || len(got.Changes) != 0 {
		t.Errorf("another account sees %+v, %v", got, err)
	}
}

// The baseline is the newest run outside a pull request, however many older
// runs there are.
func TestProjectQualityBaselineIsTheNewestPastTheLimit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	must(t, f.st.Jobs().Create(ctx, &j))
	for i := range tools.MaxTestRunLimit + 20 {
		r := domain.TestRun{AccountID: f.account.ID, ProjectID: f.project.ID, JobID: j.ID, SHA: "old0001", Suite: fmt.Sprintf("s%d", i), Status: domain.TestRunPassed, Attempt: 1}
		must(t, f.st.TestRuns().Create(ctx, &r, nil))
	}
	f.clock.Advance(time.Hour)
	f.qualityRun(t, domain.Change{}, "new0001", "unit", "", 1, domain.TestRunFailed, domain.TestCounts{Failed: 1})
	got, err := f.svc.ProjectQuality(ctx, f.by, mcp.ProjectInput{Project: "app"})
	if err != nil || got.Baseline == nil || got.Baseline.SHA != "new0001" || len(got.Baseline.Cells) != 1 {
		t.Fatalf("baseline = %+v, %v", got.Baseline, err)
	}
}

// Only runs of base checks and of open pull requests heads: no baseline.
func TestProjectQualityHasNoBaselineFromBaseChecksOrPullRequestHeads(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	open := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, Repo: "acme/app", Number: 5, HeadSHA: "head001", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &open))
	f.overlayRun(t, "base001", "unit", domain.TestRunPassed)
	f.qualityRun(t, domain.Change{}, "head001", "unit", "", 1, domain.TestRunPassed, domain.TestCounts{Passed: 1})
	got, err := f.svc.ProjectQuality(ctx, f.by, mcp.ProjectInput{Project: "app"})
	if err != nil || got.Baseline != nil {
		t.Fatalf("baseline = %+v, %v", got.Baseline, err)
	}
}
