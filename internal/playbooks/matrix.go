package playbooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"
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
// changes nothing, so it is idempotent. With with.strict, invalid files fail
// the step instead (the error quotes them as untrusted data).
func (a *actions) matrixPlan(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string   `json:"repo"`
		Ref    string   `json:"ref"`
		Scope  string   `json:"scope"`
		Suites []string `json:"suites"`
		// OnlyRunnable leaves out the jobs no online worker can run.
		OnlyRunnable bool `json:"only_runnable"`
		// Strict makes invalid files an error of the step, which a playbook
		// can escalate, instead of errors in the output.
		Strict bool `json:"strict"`
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
	case errors.Is(err, matrix.ErrInvalid) && with.Strict:
		return nil, fmt.Errorf("the test matrix of %s at %s is invalid:\n%s", with.Repo, with.Ref, contextpack.WrapUntrusted("matrix error", err.Error()))
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
		required, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeRequired})
		if err != nil {
			return nil, err
		}
		if err := a.dropUnrunnable(ctx, call.Run.AccountID, &out, required, with.Suites); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// workerCheck answers whether a worker can run a command job, for the jobs of
// one plan. It reads the account's workers once.
type workerCheck struct {
	workers []domain.Worker
	now     time.Time
}

// newWorkerCheck reads the workers of the account.
func (a *actions) newWorkerCheck(ctx context.Context, accountID uuid.UUID) (workerCheck, error) {
	workers, err := a.Store.Workers().List(ctx, accountID)
	if err != nil {
		return workerCheck{}, fmt.Errorf("list workers: %w", err)
	}
	return workerCheck{workers: workers, now: a.Now()}, nil
}

// canRun returns the pool and the labels a worker needs to lease the command
// job that runs in env with needs (as the job creator computes them: the
// job's needs, those of its environment and docker for a docker environment),
// and whether an online worker of that pool has them. Jobs of pool actions
// have no workers, forecrewd runs them, so they always can.
func (c workerCheck) canRun(env domain.Environment, needs []string) (pool string, labels []string, ok bool) {
	pool = domain.CommandPool(&env)
	needs = append(slices.Clone(needs), env.Needs...)
	if env.Provider == domain.ProviderDocker {
		needs = append(needs, "docker")
	}
	labels = domain.NormalizeLabels(needs)
	return pool, labels, pool == domain.PoolActions || slices.ContainsFunc(c.workers, func(w domain.Worker) bool {
		return w.Online(c.now) && domain.OrDefaultPool(w.Pool) == pool && domain.MatchLabels(labels, w.Labels)
	})
}

// dropUnrunnable moves the jobs of out that no online worker can lease to
// out.Unrunnable and describes them in out.Missing. A job that no required
// cell asks for is optional and nothing waits for it: without a worker it is
// dropped from the plan and not reported. The exception is a suite of
// suites that no required job runs (a suite with required: false, asked for
// by name because its tests are new): it is the only check of those tests,
// so its jobs are needed.
func (a *actions) dropUnrunnable(ctx context.Context, accountID uuid.UUID, out *matrixPlanOutput, required []matrix.Cell, suites []string) error {
	check, err := a.newWorkerCheck(ctx, accountID)
	if err != nil {
		return err
	}
	need := map[string]bool{}
	hasRequired := map[string]bool{}
	for _, c := range required {
		for _, j := range c.Jobs() {
			need[j.Key], hasRequired[j.Suite] = true, true
		}
	}
	runnable := out.Jobs[:0:0]
	for _, j := range out.Jobs {
		pool, labels, ok := check.canRun(j.Environment, j.Needs)
		switch {
		case ok:
			runnable = append(runnable, j)
		case need[j.Key] || slices.Contains(suites, j.Suite) && !hasRequired[j.Suite]:
			out.Unrunnable = append(out.Unrunnable, unrunnableJob{Key: j.Key, Pool: pool, Needs: labels})
		}
	}
	out.Jobs = runnable
	out.Missing = missingText(out.Unrunnable)
	return nil
}

// missingText says, for a person, which jobs have no worker: one line per
// pool and set of labels, with the first jobs it blocks. The names and labels
// come from the repo's matrix, so each line is one line of inline text and
// the whole is quoted as untrusted data.
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
	if len(lines) == 0 {
		return ""
	}
	return contextpack.WrapUntrusted("test matrix of the repo", strings.Join(lines, "\n"))
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
