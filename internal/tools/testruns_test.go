package tools_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/tools"
)

// newCommandRun stores a command job with one test run of the status.
func (f *fixture) newCommandRun(t *testing.T, sha string, attempt int, status domain.TestRunStatus, counts domain.TestCounts, results ...domain.TestResult) (domain.Job, domain.TestRun) {
	t.Helper()
	j := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	return j, f.addRun(t, j, sha, attempt, status, counts, results...)
}

func (f *fixture) addRun(t *testing.T, j domain.Job, sha string, attempt int, status domain.TestRunStatus, counts domain.TestCounts, results ...domain.TestResult) domain.TestRun {
	t.Helper()
	r := domain.TestRun{AccountID: f.account.ID, ProjectID: f.project.ID, JobID: j.ID, SHA: sha, Suite: string(status), Status: status, Counts: counts, Attempt: attempt}
	if c, err := f.st.Changes().GetByPR(t.Context(), f.account.ID, "acme/app", 4); err == nil && c.HeadSHA == sha {
		r.ChangeID = c.ID
	}
	must(t, f.st.TestRuns().Create(t.Context(), &r, results))
	return r
}

func TestListTestRuns(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	change := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, Repo: "acme/app", Number: 4, HeadSHA: "abc123", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &change))
	job, run := f.newCommandRun(t, "abc123", 1, domain.TestRunFailed, domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1},
		domain.TestResult{Name: "TestOK", Status: domain.TestPassed}, domain.TestResult{Name: "TestBad", Status: domain.TestFailed, Message: "boom"},
		domain.TestResult{Name: "TestSkip", Status: domain.TestSkipped}, domain.TestResult{Name: "TestCrash", Status: domain.TestErrored})
	f.newCommandRun(t, "def456", 1, domain.TestRunPassed, domain.TestCounts{Passed: 2})

	for name, in := range map[string]mcp.ListTestRunsInput{
		"job ref":   {Job: job.Ref()},
		"job uuid":  {Job: job.ID.String()},
		"sha":       {SHA: "ABC123"},
		"change":    {Project: "app", Change: 4},
		"job+limit": {Job: job.Ref(), Limit: 5},
	} {
		got, err := f.svc.ListTestRuns(ctx, f.by, in)
		if err != nil || len(got) != 1 || got[0].ID != run.ID || got[0].JobRef != job.Ref() || got[0].Counts.Failed != 1 {
			t.Fatalf("%s: runs = %+v, %v", name, got, err)
		}
		if fs := got[0].Failures; len(fs) != 2 || fs[0].Name != "TestBad" && fs[0].Name != "TestCrash" {
			t.Errorf("%s: failures = %+v, want the failed and the errored case", name, fs)
		}
	}
	// A sha alone spans jobs and finds the job ref of each run.
	if got, err := f.svc.ListTestRuns(ctx, f.by, mcp.ListTestRunsInput{SHA: "def456", Project: "app"}); err != nil || len(got) != 1 || got[0].JobRef == "" || len(got[0].Failures) != 0 {
		t.Errorf("sha def456 = %+v, %v", got, err)
	}
	if got, err := f.svc.ListTestRuns(ctx, f.by, mcp.ListTestRunsInput{SHA: "nope"}); err != nil || len(got) != 0 {
		t.Errorf("unknown sha = %+v, %v", got, err)
	}
}

func TestListTestRunsErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	job, _ := f.newCommandRun(t, "abc", 1, domain.TestRunPassed, domain.TestCounts{})
	for name, tt := range map[string]struct {
		in   mcp.ListTestRunsInput
		want error
	}{
		"no filter":         {mcp.ListTestRunsInput{}, mcp.ErrInvalidInput},
		"project only":      {mcp.ListTestRunsInput{Project: "app"}, mcp.ErrInvalidInput},
		"negative limit":    {mcp.ListTestRunsInput{Job: job.Ref(), Limit: -1}, mcp.ErrInvalidInput},
		"change no project": {mcp.ListTestRunsInput{Change: 4}, mcp.ErrInvalidInput},
		"unknown change":    {mcp.ListTestRunsInput{Project: "app", Change: 99}, mcp.ErrNotFound},
		"unknown project":   {mcp.ListTestRunsInput{Project: "nope", SHA: "abc"}, mcp.ErrNotFound},
		"unknown job":       {mcp.ListTestRunsInput{Job: "JOB-99"}, mcp.ErrNotFound},
		"bad job":           {mcp.ListTestRunsInput{Job: "later"}, mcp.ErrInvalidInput},
	} {
		if _, err := f.svc.ListTestRuns(ctx, f.by, tt.in); err == nil || !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tt.want)
		}
	}
	other, _, otherJob := f.otherAccount(t)
	if _, err := f.svc.ListTestRuns(ctx, other, mcp.ListTestRunsInput{Job: job.ID.String()}); !errors.Is(err, mcp.ErrNotFound) {
		t.Errorf("another account's job: %v", err)
	}
	if got, err := f.svc.ListTestRuns(ctx, other, mcp.ListTestRunsInput{SHA: "abc"}); err != nil || len(got) != 0 {
		t.Errorf("another account sees %+v, %v", got, err)
	}
	_ = otherJob
}

func TestJobStatusShowsTestCounts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j, _ := f.newCommandRun(t, "abc", 1, domain.TestRunFailed, domain.TestCounts{Passed: 5, Failed: 9})
	// Only the latest attempt counts.
	f.addRun(t, j, "abc", 2, domain.TestRunPassed, domain.TestCounts{Passed: 3, Skipped: 1})
	f.addRun(t, j, "abc", 2, domain.TestRunErrored, domain.TestCounts{Errored: 1})
	got, err := f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: j.Ref()})
	if err != nil || got.Tests == nil || *got.Tests != (mcp.TestSummary{Runs: 2, Status: domain.TestRunErrored, Passed: 3, Skipped: 1, Errored: 1}) {
		t.Fatalf("tests = %+v, %v", got.Tests, err)
	}

	passed, _ := f.newCommandRun(t, "abd", 1, domain.TestRunPassed, domain.TestCounts{Passed: 1})
	if got, _ := f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: passed.Ref()}); got.Tests == nil || got.Tests.Status != domain.TestRunPassed {
		t.Errorf("passed job tests = %+v", got.Tests)
	}
	failed, _ := f.newCommandRun(t, "abe", 1, domain.TestRunFailed, domain.TestCounts{Failed: 1})
	if got, _ := f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: failed.Ref()}); got.Tests == nil || got.Tests.Status != domain.TestRunFailed {
		t.Errorf("failed job tests = %+v", got.Tests)
	}

	// No runs yet, and agent jobs, show no tests.
	fresh := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, f.st.Jobs().Create(ctx, &fresh))
	agent := f.newJob(t, "reviewer", domain.Goal{}, domain.Task{}, 0)
	for _, id := range []domain.Job{fresh, agent} {
		if got, err := f.svc.JobStatus(ctx, f.by, mcp.JobRefInput{Job: id.Ref()}); err != nil || got.Tests != nil {
			t.Errorf("%s tests = %+v, %v", id.Ref(), got.Tests, err)
		}
	}
}

func TestListTestRunsCapsFailures(t *testing.T) {
	f := newFixture(t)
	var results []domain.TestResult
	for i := range tools.MaxFailures + 5 {
		results = append(results, domain.TestResult{Name: fmt.Sprintf("T%03d", i), Status: domain.TestFailed})
	}
	results = append(results, domain.TestResult{Name: "ok", Status: domain.TestPassed})
	job, _ := f.newCommandRun(t, "abc", 1, domain.TestRunFailed, domain.TestCounts{Failed: tools.MaxFailures + 5, Passed: 1}, results...)
	got, err := f.svc.ListTestRuns(t.Context(), f.by, mcp.ListTestRunsInput{Job: job.Ref()})
	if err != nil || len(got) != 1 || len(got[0].Failures) != tools.MaxFailures || !got[0].FailuresTruncated || got[0].Counts.Failed != tools.MaxFailures+5 {
		t.Fatalf("failures = %d truncated %v, %v", len(got[0].Failures), got[0].FailuresTruncated, err)
	}
	few, _ := f.newCommandRun(t, "abd", 1, domain.TestRunFailed, domain.TestCounts{Failed: 1}, domain.TestResult{Name: "x", Status: domain.TestFailed})
	got, _ = f.svc.ListTestRuns(t.Context(), f.by, mcp.ListTestRunsInput{Job: few.Ref()})
	if len(got[0].Failures) != 1 || got[0].FailuresTruncated {
		t.Errorf("failures = %+v", got[0])
	}
}
