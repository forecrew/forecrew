package testkit

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func testRunsT(d *storeData) *table[domain.TestRun] { return &d.testRuns }

// TestRuns implements store.Store.
func (s *Store) TestRuns() store.TestRunRepo { return testRunRepo{s} }

type testRunRepo struct{ s *Store }

func (r testRunRepo) Create(_ context.Context, run *domain.TestRun, results []domain.TestResult) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, run.AccountID, run.ProjectID), foreign(&d.jobs, run.AccountID, run.JobID), foreign(&d.changes, run.AccountID, run.ChangeID)); err != nil {
			return err
		}
		if d.testRuns.exists(func(x domain.TestRun) bool {
			return x.JobID == run.JobID && x.Attempt == run.Attempt && x.Suite == run.Suite
		}) {
			return fmt.Errorf("test run of job %s, attempt %d, suite %q: %w", run.JobID, run.Attempt, run.Suite, store.ErrConflict)
		}
		run.ID, run.CreatedAt = newID(run.ID), now
		stored := *run
		stored.Cell, stored.Versions = nonNil(run.Cell), nonNil(run.Versions)
		stored.ReportArtifacts = nonNilSlice(run.ReportArtifacts)
		d.testRuns.insert(stored)
		for i := range results {
			results[i].ID, results[i].AccountID, results[i].RunID = newID(results[i].ID), run.AccountID, run.ID
			d.testResult.insert(results[i])
		}
		return nil
	})
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return maps.Clone(m)
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

func (r testRunRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.TestRun, error) {
	run, err := getRow(r.s, testRunsT, accountID, id)
	return cloneRun(run), err
}

func (r testRunRepo) List(_ context.Context, accountID uuid.UUID, f store.TestRunFilter) ([]domain.TestRun, error) {
	runs, err := listRows(r.s, testRunsT, f.Limit, func(x domain.TestRun) bool {
		return x.AccountID == accountID && zeroOr(f.ProjectID, x.ProjectID) && zeroOr(f.JobID, x.JobID) &&
			zeroOr(f.ChangeID, x.ChangeID) && zeroOr(f.SHA, x.SHA)
	})
	for i := range runs {
		runs[i] = cloneRun(runs[i])
	}
	return runs, err
}

// cloneRun copies the maps and slices of a stored run.
func cloneRun(run domain.TestRun) domain.TestRun {
	run.Cell, run.Versions, run.ReportArtifacts = nonNil(run.Cell), nonNil(run.Versions), nonNilSlice(run.ReportArtifacts)
	return run
}

func (r testRunRepo) Results(_ context.Context, accountID, runID uuid.UUID) ([]domain.TestResult, error) {
	out, err := listRows(r.s, func(d *storeData) *table[domain.TestResult] { return &d.testResult }, 0, func(x domain.TestResult) bool {
		return x.AccountID == accountID && x.RunID == runID
	})
	slices.SortStableFunc(out, func(a, b domain.TestResult) int {
		return cmp.Or(cmp.Compare(a.Classname, b.Classname), cmp.Compare(a.Name, b.Name))
	})
	return out, err
}

func (r testRunRepo) Failures(ctx context.Context, accountID, runID uuid.UUID, limit int) ([]domain.TestResult, error) {
	all, err := r.Results(ctx, accountID, runID)
	var out []domain.TestResult
	for _, res := range all {
		if res.Status == domain.TestFailed || res.Status == domain.TestErrored {
			out = append(out, res)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}
