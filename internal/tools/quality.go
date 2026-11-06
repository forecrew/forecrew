package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
)

// ProjectQuality implements mcp.ToolService.
func (s *Service) ProjectQuality(ctx context.Context, by domain.Actor, in mcp.ProjectInput) (mcp.QualityInfo, error) {
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return mcp.QualityInfo{}, err
	}
	if err := s.allow(ctx, by, domain.PermJobRead, p.ID); err != nil {
		return mcp.QualityInfo{}, err
	}
	out := mcp.QualityInfo{Project: p.Name, DefaultBranch: p.DefaultBranch, Changes: []mcp.QualityChange{}}
	refs := map[uuid.UUID]string{} // job ID -> JOB-41
	if out.Baseline, err = s.baseline(ctx, by.AccountID, p, refs); err != nil {
		return out, err
	}
	changes, err := s.cfg.Store.Changes().List(ctx, by.AccountID, store.ChangeFilter{ProjectID: p.ID, State: domain.ChangeOpen})
	if err != nil {
		return out, fmt.Errorf("open changes of %s: %w", p.Name, err)
	}
	slices.SortFunc(changes, func(a, b domain.Change) int { return b.CreatedAt.Compare(a.CreatedAt) })
	for _, c := range changes[:min(len(changes), mcp.MaxQualityChanges)] {
		q := mcp.QualityChange{Number: c.Number, URL: c.URL, Branch: c.Branch, SHA: c.HeadSHA}
		if c.TaskID != uuid.Nil() {
			t, err := s.cfg.Store.Tasks().Get(ctx, by.AccountID, c.TaskID)
			if err != nil {
				return out, fmt.Errorf("task of %s#%d: %w", c.Repo, c.Number, err)
			}
			q.Task = t.Ref()
		}
		runs, err := s.cfg.Store.TestRuns().List(ctx, by.AccountID, store.TestRunFilter{
			ProjectID: p.ID, ChangeID: c.ID, SHA: strings.ToLower(c.HeadSHA), Limit: MaxTestRunLimit,
		})
		if err != nil {
			return out, fmt.Errorf("test runs of %s#%d: %w", c.Repo, c.Number, err)
		}
		if q.Cells, err = s.qualityCells(ctx, by.AccountID, runs, refs); err != nil {
			return out, err
		}
		v, err := s.verificationOf(ctx, by.AccountID, c)
		switch {
		case err == nil:
			q.Verification = &v
		case !errors.Is(err, mcp.ErrNotFound):
			return out, err
		}
		out.Changes = append(out.Changes, q)
	}
	return out, nil
}

// baseline returns the results of the newest commit tested outside a pull
// request (the onboarding baseline); nil when there is none.
func (s *Service) baseline(ctx context.Context, accountID uuid.UUID, p domain.Project, refs map[uuid.UUID]string) (*mcp.QualityBaseline, error) {
	all, err := s.cfg.Store.TestRuns().List(ctx, accountID, store.TestRunFilter{ProjectID: p.ID, Limit: MaxTestRunLimit})
	if err != nil {
		return nil, fmt.Errorf("test runs of %s: %w", p.Name, err)
	}
	var runs []domain.TestRun
	for _, r := range all {
		if r.ChangeID == uuid.Nil() {
			runs = append(runs, r)
		}
	}
	if len(runs) == 0 {
		return nil, nil
	}
	sha := slices.MaxFunc(runs, func(a, b domain.TestRun) int { return a.CreatedAt.Compare(b.CreatedAt) }).SHA
	runs = slices.DeleteFunc(runs, func(r domain.TestRun) bool { return r.SHA != sha })
	cells, err := s.qualityCells(ctx, accountID, runs, refs)
	return &mcp.QualityBaseline{SHA: sha, Cells: cells}, err
}

// qualityCells keeps the newest attempt of each suite in each cell of each
// environment, in a stable order, with its failed cases.
func (s *Service) qualityCells(ctx context.Context, accountID uuid.UUID, runs []domain.TestRun, refs map[uuid.UUID]string) ([]mcp.QualityCell, error) {
	latest := map[string]domain.TestRun{}
	for _, r := range runs {
		k := r.Environment + "\x00" + verification.CellKey(r.Cell) + "\x00" + r.Suite
		if old, ok := latest[k]; !ok || r.CreatedAt.After(old.CreatedAt) {
			latest[k] = r
		}
	}
	kept := make([]domain.TestRun, 0, len(latest))
	for _, r := range latest {
		kept = append(kept, r)
	}
	slices.SortFunc(kept, func(a, b domain.TestRun) int {
		return cmp.Or(cmp.Compare(a.Suite, b.Suite), cmp.Compare(a.Environment, b.Environment),
			cmp.Compare(verification.CellKey(a.Cell), verification.CellKey(b.Cell)))
	})
	out := make([]mcp.QualityCell, len(kept))
	for i, r := range kept {
		ref, ok := refs[r.JobID]
		if !ok {
			j, err := s.cfg.Store.Jobs().Get(ctx, accountID, r.JobID)
			if err != nil {
				return nil, fmt.Errorf("job of test run %s: %w", r.ID, err)
			}
			ref = j.Ref()
			refs[r.JobID] = ref
		}
		out[i] = mcp.QualityCell{
			Environment: r.Environment, Cell: maps.Clone(r.Cell), Suite: r.Suite, Status: r.Status, Counts: r.Counts, Job: ref,
		}
		if out[i].Cell == nil {
			out[i].Cell = map[string]string{}
		}
		var err error
		if out[i].Failures, out[i].FailuresTruncated, err = s.failures(ctx, accountID, r.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
