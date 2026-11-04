package playbooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/workflow"
)

// matrixPlanOutput is the output of matrix.plan. Cells and Jobs are never
// nil: a foreach takes them as they are.
type matrixPlanOutput struct {
	Cells  []matrix.Cell `json:"cells"`
	Jobs   []matrix.Job  `json:"jobs"` // the suites of the cells, one element per command job
	Errors string        `json:"errors"`
}

// matrixPlan is the action matrix.plan. It reads the matrix and environments
// of with.repo at with.ref and outputs the cells and command jobs of with.scope
// and with.suites. Invalid files, a scope that does not fit the matrix or a
// matrix over the limits of internal/matrix give errors and no cells, for the playbook to act on; only reads that fail are
// errors of the step. It reads and changes nothing else, so it is idempotent.
func (a *actions) matrixPlan(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string   `json:"repo"`
		Ref    string   `json:"ref"`
		Scope  string   `json:"scope"`
		Suites []string `json:"suites"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Repo == "" || with.Ref == "" {
		return nil, errors.New("matrix.plan needs repo and ref")
	}
	scope := matrix.Scope{Set: with.Scope, Suites: with.Suites}
	m, envs, err := matrix.Load(func(name string) ([]byte, error) {
		data, err := a.Tracker.GetFile(ctx, with.Repo, with.Ref, name)
		if errors.Is(err, github.ErrNotFound) {
			return nil, fmt.Errorf("%s at %s: %w", name, with.Ref, fs.ErrNotExist)
		}
		return data, err
	})
	if err == nil {
		err = scope.Check(m)
	}
	var cells []matrix.Cell
	if err == nil {
		cells, err = matrix.Expand(m, envs, scope)
	}
	switch {
	case errors.Is(err, matrix.ErrInvalid):
		return matrixPlanOutput{Cells: []matrix.Cell{}, Jobs: []matrix.Job{}, Errors: err.Error()}, nil
	case err != nil:
		return nil, err
	}
	out := matrixPlanOutput{Cells: cells, Jobs: []matrix.Job{}}
	for _, c := range out.Cells {
		out.Jobs = append(out.Jobs, c.Jobs()...)
	}
	return out, nil
}
