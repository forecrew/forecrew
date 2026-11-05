package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Limits of list_test_runs.
const (
	DefaultTestRunLimit = 100
	MaxTestRunLimit     = 500
	// MaxFailures is the most failed or errored results listed per run; the
	// counts of the run hold the total.
	MaxFailures = 100
)

// ListTestRuns implements mcp.ToolService.
func (s *Service) ListTestRuns(ctx context.Context, by domain.Actor, in mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error) {
	f, jobRef, err := s.testRunFilter(ctx, by, in)
	if err != nil {
		return nil, err
	}
	visible, err := s.authz.Projects(ctx, by, domain.PermJobRead)
	if err != nil {
		return nil, err
	}
	runs, err := s.cfg.Store.TestRuns().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	runs = slices.DeleteFunc(runs, func(r domain.TestRun) bool { return !visible.Has(r.ProjectID) })
	out := make([]mcp.TestRunInfo, len(runs))
	for i, r := range runs {
		out[i] = mcp.TestRunInfo{TestRun: r, JobRef: jobRef, Failures: []domain.TestResult{}}
		if out[i].JobRef == "" {
			if j, err := s.cfg.Store.Jobs().Get(ctx, by.AccountID, r.JobID); err == nil {
				out[i].JobRef = j.Ref()
			}
		}
		fails, err := s.cfg.Store.TestRuns().Failures(ctx, by.AccountID, r.ID, MaxFailures+1)
		if err != nil {
			return nil, err
		}
		if len(fails) > MaxFailures {
			fails, out[i].FailuresTruncated = fails[:MaxFailures], true
		}
		out[i].Failures = append(out[i].Failures, fails...)
	}
	return out, nil
}

// testRunFilter resolves the input of list_test_runs; jobRef is the ref of
// the job filtered on, if any.
func (s *Service) testRunFilter(ctx context.Context, by domain.Actor, in mcp.ListTestRunsInput) (f store.TestRunFilter, jobRef string, err error) {
	if in.Job == "" && in.Change == 0 && in.SHA == "" {
		return f, "", fmt.Errorf("give a job, a change (with its project) or a sha: %w", mcp.ErrInvalidInput)
	}
	if in.Limit < 0 || in.Change < 0 {
		return f, "", fmt.Errorf("limit and change cannot be negative: %w", mcp.ErrInvalidInput)
	}
	f = store.TestRunFilter{SHA: strings.ToLower(in.SHA), Limit: min(cmp.Or(in.Limit, DefaultTestRunLimit), MaxTestRunLimit)}
	if in.Job != "" {
		j, err := s.job(ctx, by.AccountID, in.Job)
		if err != nil {
			return f, "", err
		}
		if err := s.allow(ctx, by, domain.PermJobRead, j.ProjectID); err != nil {
			return f, "", err
		}
		f.JobID, jobRef = j.ID, j.Ref()
	}
	if in.Change == 0 {
		if f.ProjectID, err = s.optionalProject(ctx, by.AccountID, in.Project); err != nil || f.ProjectID == uuid.Nil() {
			return f, jobRef, err
		}
		return f, jobRef, s.allow(ctx, by, domain.PermJobRead, f.ProjectID)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return f, "", err
	}
	if err := s.allow(ctx, by, domain.PermJobRead, p.ID); err != nil {
		return f, "", err
	}
	c, err := s.cfg.Store.Changes().GetByPR(ctx, by.AccountID, p.Repo, in.Change)
	if errors.Is(err, store.ErrNotFound) {
		return f, "", fmt.Errorf("pull request #%d of %s: %w", in.Change, p.Name, mcp.ErrNotFound)
	}
	f.ChangeID = c.ID
	return f, jobRef, err
}

// testSummary adds up the test runs of the latest attempt of a command job;
// nil when it has none.
func (s *Service) testSummary(ctx context.Context, accountID uuid.UUID, j domain.Job) (*mcp.TestSummary, error) {
	if j.Kind.OrDefault() != domain.JobKindCommand {
		return nil, nil
	}
	runs, err := s.cfg.Store.TestRuns().List(ctx, accountID, store.TestRunFilter{JobID: j.ID})
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	latest := slices.MaxFunc(runs, func(a, b domain.TestRun) int { return cmp.Compare(a.Attempt, b.Attempt) }).Attempt
	sum := &mcp.TestSummary{Status: domain.TestRunPassed}
	for _, r := range runs {
		if r.Attempt != latest {
			continue
		}
		sum.Runs++
		sum.Passed += r.Counts.Passed
		sum.Failed += r.Counts.Failed
		sum.Skipped += r.Counts.Skipped
		sum.Errored += r.Counts.Errored
		switch {
		case r.Status == domain.TestRunErrored:
			sum.Status = domain.TestRunErrored
		case r.Status == domain.TestRunFailed && sum.Status != domain.TestRunErrored:
			sum.Status = domain.TestRunFailed
		}
	}
	return sum, nil
}
