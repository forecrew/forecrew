package playbooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
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
	// Unrunnable and Missing are set only with only_runnable: the jobs left
	// out of Jobs because no online worker can run them, and a text for a
	// person that names the pools and labels they need.
	Unrunnable []unrunnableJob `json:"unrunnable"`
	Missing    string          `json:"missing"`
}

// unrunnableJob is a job of the plan that no online worker can lease.
type unrunnableJob struct {
	Key   string   `json:"key"`
	Pool  string   `json:"pool"`
	Needs []string `json:"needs"`
}

// matrixPlan is the action matrix.plan. It reads the matrix and environments
// of with.repo at with.ref and outputs the cells and command jobs of with.scope
// and with.suites. Invalid files, a scope that does not fit the matrix or a
// matrix over the limits of internal/matrix give errors and no cells, for the playbook to act on; only reads that fail are
// errors of the step. With with.only_runnable, jobs that no online worker of
// their pool has the labels for are moved from jobs to unrunnable, because a
// queued job nobody can lease would hold its run forever. It reads and
// changes nothing, so it is idempotent.
func (a *actions) matrixPlan(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string   `json:"repo"`
		Ref    string   `json:"ref"`
		Scope  string   `json:"scope"`
		Suites []string `json:"suites"`
		// OnlyRunnable leaves out the jobs no online worker can run.
		OnlyRunnable bool `json:"only_runnable"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Repo == "" || with.Ref == "" {
		return nil, errors.New("matrix.plan needs repo and ref")
	}
	scope := matrix.Scope{Set: with.Scope, Suites: with.Suites}
	m, envs, err := matrix.Load(a.repoReader(ctx, with.Repo, with.Ref))
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
	out := matrixPlanOutput{Cells: cells, Jobs: []matrix.Job{}, Unrunnable: []unrunnableJob{}}
	for _, c := range out.Cells {
		out.Jobs = append(out.Jobs, c.Jobs()...)
	}
	if with.OnlyRunnable {
		if err := a.dropUnrunnable(ctx, call.Run.AccountID, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// dropUnrunnable moves the jobs of out that no online worker can lease to
// out.Unrunnable and describes them in out.Missing. Jobs of pool actions have
// no workers: forecrewd runs them.
func (a *actions) dropUnrunnable(ctx context.Context, accountID uuid.UUID, out *matrixPlanOutput) error {
	workers, err := a.Store.Workers().List(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	now := a.Now()
	runnable := out.Jobs[:0:0]
	for _, j := range out.Jobs {
		pool, needs := commandNeeds(j)
		if pool == domain.PoolActions || slices.ContainsFunc(workers, func(w domain.Worker) bool {
			return w.Online(now) && domain.OrDefaultPool(w.Pool) == pool && domain.MatchLabels(needs, w.Labels)
		}) {
			runnable = append(runnable, j)
			continue
		}
		out.Unrunnable = append(out.Unrunnable, unrunnableJob{Key: j.Key, Pool: pool, Needs: needs})
	}
	out.Jobs = runnable
	out.Missing = missingText(out.Unrunnable)
	return nil
}

// commandNeeds returns the pool and the labels a worker needs to lease the
// command job of a plan, as the job creator computes them: the job's needs,
// those of its environment and docker for a docker environment.
func commandNeeds(j matrix.Job) (string, []string) {
	needs := append(slices.Clone(j.Needs), j.Environment.Needs...)
	if j.Environment.Provider == domain.ProviderDocker {
		needs = append(needs, "docker")
	}
	return domain.CommandPool(&j.Environment), domain.NormalizeLabels(needs)
}

// missingText says, for a person, which jobs have no worker: one line per
// pool and set of labels, with the first jobs it blocks. The names come
// from the repo's matrix, so each is one line of inline text.
func missingText(jobs []unrunnableJob) string {
	type need struct{ pool, labels string }
	var order []need
	keys := map[need][]string{}
	for _, j := range jobs {
		n := need{j.Pool, strings.Join(j.Needs, ", ")}
		if _, ok := keys[n]; !ok {
			order = append(order, n)
		}
		keys[n] = append(keys[n], j.Key)
	}
	var lines []string
	for _, n := range order {
		ks, more := keys[n], ""
		if len(ks) > 3 {
			ks, more = ks[:3], fmt.Sprintf(" and %d more", len(keys[n])-3)
		}
		labels := "no labels"
		if n.labels != "" {
			labels = "the labels " + n.labels
		}
		lines = append(lines, contextpack.OneLine(fmt.Sprintf("- no online worker in pool %s has %s: %s%s", n.pool, labels, strings.Join(ks, ", "), more)))
	}
	return strings.Join(lines, "\n")
}

// repoReader reads the files of repo at ref through the tracker; a missing
// file wraps fs.ErrNotExist.
func (a *actions) repoReader(ctx context.Context, repo, ref string) matrix.ReadFile {
	return func(name string) ([]byte, error) {
		data, err := a.Tracker.GetFile(ctx, repo, ref, name)
		if errors.Is(err, github.ErrNotFound) {
			return nil, fmt.Errorf("%s at %s: %w", name, ref, fs.ErrNotExist)
		}
		return data, err
	}
}
