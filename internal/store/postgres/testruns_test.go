//go:build integration

package postgres

import (
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func TestTestRuns(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.TestRuns()
	job, other := f.newJob(t), f.newJob(t)
	change := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/forecrew", Number: 3, HeadSHA: "abc123", State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(ctx, &change))

	run := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, ChangeID: change.ID, SHA: "abc123", Suite: "unit",
		Environment: "linux-go", Cell: map[string]string{"os": "linux"}, Status: domain.TestRunFailed, Counts: domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1},
		DurationMS: 1500, ExitCode: 1, Versions: map[string]string{"go": "go1.27"}, ReportArtifacts: []string{"reports/unit.xml"}, Attempt: 1}
	results := []domain.TestResult{
		{Name: "TestB", Classname: "pkg", Status: domain.TestFailed, DurationMS: 30, Message: "boom"},
		{Name: "TestA", Classname: "pkg", Status: domain.TestPassed, DurationMS: 12},
		{Name: "TestC", Classname: "alpha", Status: domain.TestSkipped},
	}
	must(t, repo.Create(ctx, &run, results))
	if run.ID == uuid.Nil() || run.CreatedAt.IsZero() || results[0].ID == uuid.Nil() || results[0].RunID != run.ID || results[0].AccountID != f.account {
		t.Fatalf("Create did not fill %+v, %+v", run, results[0])
	}
	got, err := repo.Get(ctx, f.account, run.ID)
	run.CreatedAt = got.CreatedAt
	if err != nil || got.Suite != "unit" || got.Cell["os"] != "linux" || got.Versions["go"] != "go1.27" || got.ChangeID != change.ID ||
		got.Counts != run.Counts || got.DurationMS != 1500 || got.ExitCode != 1 || !slices.Equal(got.ReportArtifacts, run.ReportArtifacts) || got.Environment != "linux-go" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	rs, err := repo.Results(ctx, f.account, run.ID)
	if err != nil || len(rs) != 3 || rs[0].Name != "TestC" || rs[1].Name != "TestA" || rs[2].Name != "TestB" || rs[2].Message != "boom" || rs[2].DurationMS != 30 {
		t.Fatalf("Results = %+v, %v", rs, err)
	}

	// No change, no maps: stored as empty ones.
	bare := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: other.ID, SHA: "def456", Suite: "unit", Status: domain.TestRunErrored, ExitCode: -1, Attempt: 1}
	must(t, repo.Create(ctx, &bare, nil))
	got, err = repo.Get(ctx, f.account, bare.ID)
	if err != nil || got.ChangeID != uuid.Nil() || got.Cell == nil || got.Versions == nil || got.ReportArtifacts == nil || got.ExitCode != -1 {
		t.Fatalf("bare Get = %+v, %v", got, err)
	}
	if rs, err := repo.Results(ctx, f.account, bare.ID); err != nil || len(rs) != 0 {
		t.Fatalf("bare Results = %+v, %v", rs, err)
	}

	dup := run
	dup.ID = uuid.Nil()
	wantErr(t, repo.Create(ctx, &dup, []domain.TestResult{{Name: "x", Status: domain.TestPassed}}), store.ErrConflict)
	if list, _ := repo.List(ctx, f.account, store.TestRunFilter{JobID: job.ID}); len(list) != 1 {
		t.Fatalf("a failed Create left rows: %+v", list)
	}

	for _, tt := range []struct {
		name string
		f    store.TestRunFilter
		want int
	}{
		{"all", store.TestRunFilter{}, 2},
		{"job", store.TestRunFilter{JobID: job.ID}, 1},
		{"change", store.TestRunFilter{ChangeID: change.ID}, 1},
		{"sha", store.TestRunFilter{SHA: "def456"}, 1},
		{"project and limit", store.TestRunFilter{ProjectID: f.project.ID, Limit: 1}, 1},
	} {
		if list, err := repo.List(ctx, f.account, tt.f); err != nil || len(list) != tt.want {
			t.Errorf("List %s = %d runs, %v; want %d", tt.name, len(list), err, tt.want)
		}
	}
	if cs, err := f.st.Changes().List(ctx, f.account, store.ChangeFilter{HeadSHA: "abc123"}); err != nil || len(cs) != 1 {
		t.Errorf("changes by head = %+v, %v", cs, err)
	}

	// Another account sees nothing and cannot attach a run to this project.
	stranger := domain.NewID()
	_, err = repo.Get(ctx, stranger, run.ID)
	wantErr(t, err, store.ErrNotFound)
	if list, _ := repo.List(ctx, stranger, store.TestRunFilter{}); len(list) != 0 {
		t.Errorf("foreign List = %+v", list)
	}
	foreign := domain.TestRun{AccountID: stranger, ProjectID: f.project.ID, JobID: job.ID, SHA: "x", Suite: "s", Status: domain.TestRunPassed, Attempt: 1}
	wantErr(t, repo.Create(ctx, &foreign, nil), store.ErrNotFound)
}

func TestTestRunFailures(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.TestRuns()
	job := f.newJob(t)
	run := domain.TestRun{AccountID: f.account, ProjectID: f.project.ID, JobID: job.ID, SHA: "x", Suite: "s", Status: domain.TestRunFailed, Attempt: 1}
	must(t, repo.Create(ctx, &run, []domain.TestResult{
		{Name: "D", Status: domain.TestPassed}, {Name: "C", Status: domain.TestFailed, Message: "boom"},
		{Name: "B", Status: domain.TestFailed}, {Name: "A", Status: domain.TestErrored}, {Name: "S", Status: domain.TestSkipped},
	}))
	fails, err := repo.Failures(ctx, f.account, run.ID, 0)
	if err != nil || len(fails) != 3 || fails[0].Name != "A" || fails[2].Name != "C" || fails[2].Message != "boom" {
		t.Fatalf("Failures = %+v, %v", fails, err)
	}
	if fails, err := repo.Failures(ctx, f.account, run.ID, 2); err != nil || len(fails) != 2 || fails[1].Name != "B" {
		t.Fatalf("limited Failures = %+v, %v", fails, err)
	}
	if fails, _ := repo.Failures(ctx, domain.NewID(), run.ID, 0); len(fails) != 0 {
		t.Fatalf("foreign Failures = %+v", fails)
	}
}

func TestVerificationReports(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.VerificationReports()
	change := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/forecrew", Number: 4, Branch: "b", BaseRef: "main", HeadSHA: "abc123", State: domain.ChangeOpen, URL: "u"}
	must(t, f.st.Changes().Upsert(ctx, &change))
	art := domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, Name: "verification/abc123.md", ContentType: "text/markdown", Size: 1,
		SHA256: strings.Repeat("a", 64), StorageKey: "verification-" + domain.NewID().String()}
	must(t, f.st.Artifacts().Put(ctx, &art))

	r := domain.VerificationReport{AccountID: f.account, ProjectID: f.project.ID, ChangeID: change.ID, SHA: "abc123",
		Status: domain.VerificationIncomplete, Report: []byte(`{"version": 1}`), ArtifactID: art.ID}
	must(t, repo.Upsert(ctx, &r))
	if r.ID == uuid.Nil() || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		t.Fatalf("Upsert did not fill %+v", r)
	}
	again := domain.VerificationReport{AccountID: f.account, ProjectID: f.project.ID, ChangeID: change.ID, SHA: "abc123",
		Status: domain.VerificationPassed, Report: []byte(`{"version": 2}`)}
	must(t, repo.Upsert(ctx, &again))
	if again.ID != r.ID || !again.CreatedAt.Equal(r.CreatedAt) || !again.UpdatedAt.After(r.UpdatedAt) {
		t.Fatalf("rebuild = %+v, first %+v", again, r)
	}
	got, err := repo.Get(ctx, f.account, r.ID)
	if err != nil || got.Status != domain.VerificationPassed || got.ArtifactID != uuid.Nil() || !strings.Contains(string(got.Report), "2") {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	next := domain.VerificationReport{AccountID: f.account, ProjectID: f.project.ID, ChangeID: change.ID, SHA: "def456", Status: domain.VerificationFailed, Report: []byte(`{}`)}
	must(t, repo.Upsert(ctx, &next))
	if got, err := repo.GetBySHA(ctx, f.account, change.ID, "abc123"); err != nil || got.ID != r.ID {
		t.Fatalf("GetBySHA = %+v, %v", got, err)
	}
	if got, err := repo.Latest(ctx, f.account, change.ID); err != nil || got.ID != next.ID {
		t.Fatalf("Latest = %+v, %v", got, err)
	}
	_, err = repo.GetBySHA(ctx, f.account, change.ID, "zzz")
	wantErr(t, err, store.ErrNotFound)
	_, err = repo.Latest(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
	_, err = repo.Get(ctx, domain.NewID(), r.ID)
	wantErr(t, err, store.ErrNotFound)
	bad := next
	bad.ID, bad.ChangeID = uuid.Nil(), domain.NewID()
	wantErr(t, repo.Upsert(ctx, &bad), store.ErrNotFound)
}
