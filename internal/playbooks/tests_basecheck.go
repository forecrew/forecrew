package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testreport"
	"github.com/forecrew/forecrew/internal/testsys"
	"github.com/forecrew/forecrew/internal/workflow"
)

// baseCheckOutput is the output of tests.base_check_request.
type baseCheckOutput struct {
	Requests []workflow.JobRequest `json:"requests"`
	// Runnable are the requests an online worker can run: all of them
	// unless with.only_runnable is set. Missing says, for a person, which
	// requests have no worker.
	Runnable []workflow.JobRequest `json:"runnable"`
	Missing  string                `json:"missing"`
}

// testsBaseCheckRequest is the action tests.base_check_request. For every
// suite the new tests belong to and every environment the matrix at head
// runs it in, it returns one command job request that runs the suite on
// base with the test files from head laid over it. With with.only_runnable,
// runnable leaves out the requests no online worker can lease (a queued job
// nobody can run would hold the run forever), and missing names them;
// tests.base_check_result then sees fewer runs than requests.
func (a *actions) testsBaseCheckRequest(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo  string    `json:"repo"`
		Base  string    `json:"base"`
		Head  string    `json:"head"`
		Tests []newTest `json:"tests"`
		// Files are the files of the test map; with the files of tests they
		// are laid over base.
		Files        []string `json:"files"`
		OnlyRunnable bool     `json:"only_runnable"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Repo == "" || with.Base == "" || with.Head == "" {
		return nil, errors.New("with: repo, base and head are required")
	}
	files := slices.Clone(with.Files)
	suites := map[string]bool{}
	for _, t := range with.Tests {
		suites[t.Suite] = true
		files = append(files, t.File)
	}
	out := baseCheckOutput{Requests: []workflow.JobRequest{}, Runnable: []workflow.JobRequest{}}
	if len(suites) == 0 {
		return out, nil
	}
	paths, err := a.overlayPaths(ctx, call.Run, files)
	if err != nil {
		return nil, err
	}
	overlay := &domain.Overlay{Ref: with.Head, Paths: paths}
	m, envs, err := matrix.Load(a.repoReader(ctx, with.Repo, with.Head))
	if err != nil {
		return nil, err
	}
	scope := matrix.Scope{Suites: slices.Sorted(maps.Keys(suites))}
	if err := scope.Check(m); err != nil {
		return nil, fmt.Errorf("with: tests: %w", err)
	}
	cells, err := matrix.Expand(m, envs, scope)
	if err != nil {
		return nil, err
	}
	// One request per suite and environment: the first cell that has it.
	type slot struct{ suite, env string }
	done := map[slot]bool{}
	for _, c := range cells {
		for _, j := range c.Jobs() {
			if k := (slot{j.Suite, j.Environment.Name}); !done[k] {
				done[k] = true
				env, cmd := j.Environment, j.Command
				out.Requests = append(out.Requests, workflow.JobRequest{
					Kind: domain.JobKindCommand, Repo: with.Repo, BaseRef: with.Base, Needs: j.Needs, Command: &cmd, Environment: &env,
					Cell: j.Cell, Suite: j.Suite, Overlay: overlay,
				})
			}
		}
	}
	for name := range suites {
		if !slices.ContainsFunc(out.Requests, func(r workflow.JobRequest) bool { return r.Suite == name }) {
			return nil, fmt.Errorf("%s: suite %q runs in no cell", testsys.MatrixFile, name)
		}
	}
	out.Runnable = out.Requests
	if with.OnlyRunnable {
		if err := a.dropUnrunnableRequests(ctx, call.Run.AccountID, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// dropUnrunnableRequests keeps in out.Runnable the requests an online worker
// can lease and describes the others in out.Missing.
func (a *actions) dropUnrunnableRequests(ctx context.Context, accountID uuid.UUID, out *baseCheckOutput) error {
	check, err := a.newWorkerCheck(ctx, accountID)
	if err != nil {
		return err
	}
	out.Runnable = []workflow.JobRequest{}
	var missing []unrunnableJob
	for _, r := range out.Requests {
		var env domain.Environment
		if r.Environment != nil {
			env = *r.Environment
		}
		pool, labels, ok := check.canRun(env, r.Needs)
		if ok {
			out.Runnable = append(out.Runnable, r)
			continue
		}
		missing = append(missing, unrunnableJob{Key: "base check of " + r.Suite + " in " + env.Name, Pool: pool, Needs: labels})
	}
	out.Missing = missingText(missing)
	return nil
}

// overlayPaths returns the globs of files for the overlay. Only test files
// may be overlaid: the base runs whatever the head puts there, so a file the
// test engineer could not write (the Makefile, go.mod, the matrix) is
// refused.
func (a *actions) overlayPaths(ctx context.Context, run domain.WorkflowRun, files []string) ([]string, error) {
	if a.Roles == nil {
		return nil, errors.New("no role resolver")
	}
	role, err := a.Roles.Resolve(ctx, run.AccountID, run.ProjectID, testerRole)
	if err != nil {
		return nil, fmt.Errorf("resolve role %s: %w", testerRole, err)
	}
	var bad []string
	for _, f := range files {
		if !roles.PathAllowed(role.WritePaths, f) {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("with: files: not test files, the %s may not write them: %s", testerRole, strings.Join(bad, ", "))
	}
	return literalGlobs(files)
}

// baseCheckTest is one new test in the output of tests.base_check_result.
type baseCheckTest struct {
	ID           string `json:"id"`
	FailedOnBase bool   `json:"failed_on_base"`
}

// baseCheckResult is the output of tests.base_check_result.
type baseCheckResult struct {
	NewTests []baseCheckTest `json:"new_tests"`
	OK       bool            `json:"ok"`
}

// jobRefs decodes an element of runs: a job ID; the output of a job step (an
// object with job_id); or a round of a foreach over job steps (an object
// holding such outputs by step id). It holds the job IDs found.
type jobRefs []uuid.UUID

func (r *jobRefs) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		id, err := uuid.Parse(s)
		if err != nil {
			return err
		}
		*r = jobRefs{id}
		return nil
	}
	var o map[string]json.RawMessage
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	if raw, ok := o["job_id"]; ok {
		var id uuid.UUID
		if err := json.Unmarshal(raw, &id); err != nil {
			return fmt.Errorf("job_id: %w", err)
		}
		*r = jobRefs{id}
		return nil
	}
	for _, k := range slices.Sorted(maps.Keys(o)) {
		var step struct {
			JobID uuid.UUID `json:"job_id"`
		}
		if err := json.Unmarshal(o[k], &step); err == nil && step.JobID != uuid.Nil() {
			*r = append(*r, step.JobID)
		}
	}
	return nil
}

// resultID is how a test result is named in tests: classname::name, or the
// name when there is no classname.
func resultID(r domain.TestResult) string {
	if r.Classname == "" {
		return r.Name
	}
	return r.Classname + "::" + r.Name
}

// suiteRun is a stored test run of the base check.
type suiteRun struct {
	// environment is the name of the environment the run ran in.
	environment string
	// commandFailed: the command really failed, with an exit code above 0,
	// and the run is not an oversized one. Not set for a timeout or a
	// command that did not start (-1), nor for an errored run whose command
	// exited 0 (a wrong report path, an unreadable or empty report).
	commandFailed bool
	// failed holds, per test id, whether it failed or errored; a test with
	// several results failed only when all of them did.
	failed map[string]bool
}

// loadSuiteRun reads the results of a stored run.
func (a *actions) loadSuiteRun(ctx context.Context, r domain.TestRun) (suiteRun, error) {
	results, err := a.Store.TestRuns().Results(ctx, r.AccountID, r.ID)
	if err != nil {
		return suiteRun{}, fmt.Errorf("results of test run %s: %w", r.ID, err)
	}
	sr := suiteRun{environment: r.Environment, failed: map[string]bool{}}
	oversized := false
	for _, res := range results {
		id := resultID(res)
		f := res.Status == domain.TestFailed || res.Status == domain.TestErrored
		if prev, ok := sr.failed[id]; ok {
			f = f && prev
		}
		sr.failed[id] = f
		oversized = oversized || res.Name == testreport.OversizedResult
	}
	// A passing report under a failing command is not passed: the worker adds
	// a (command) result.
	sr.commandFailed = r.ExitCode > 0 && !oversized && r.Status != domain.TestRunPassed
	return sr, nil
}

// failedOnBase reports whether the test id failed in every run of its suite:
// its result is failed or errored, or it has no result and the command of
// the run failed (compile errors: the test did not run). No run, no failure.
func failedOnBase(runs []suiteRun, id string) bool {
	for _, r := range runs {
		failed, found := r.failed[id]
		if found && !failed || !found && !r.commandFailed {
			return false
		}
	}
	return len(runs) > 0
}

// loadSuiteRuns reads the stored test runs of the jobs in runs, by suite.
func (a *actions) loadSuiteRuns(ctx context.Context, accountID uuid.UUID, runs []jobRefs) (map[string][]suiteRun, error) {
	bySuite := map[string][]suiteRun{}
	for i, refs := range runs {
		if len(refs) == 0 {
			return nil, fmt.Errorf("with: runs[%d]: no job_id; give job IDs or the outputs of job steps", i)
		}
		for _, id := range refs {
			if id == uuid.Nil() {
				return nil, fmt.Errorf("with: runs[%d]: nil job_id", i)
			}
			// A nil job filter would list every run of the account.
			stored, err := a.Store.TestRuns().List(ctx, accountID, store.TestRunFilter{JobID: id})
			if err != nil {
				return nil, fmt.Errorf("list test runs of job %s: %w", id, err)
			}
			for _, r := range stored {
				sr, err := a.loadSuiteRun(ctx, r)
				if err != nil {
					return nil, err
				}
				bySuite[r.Suite] = append(bySuite[r.Suite], sr)
			}
		}
	}
	return bySuite, nil
}

// requestedRun is the part of a base check request that says which run it
// asks for.
type requestedRun struct {
	Suite       string `json:"suite"`
	Environment *struct {
		Name string `json:"name"`
	} `json:"environment"`
}

// unrun returns the suites that have fewer stored runs than requests, in an
// environment: a job that succeeded without leaving a run (or was lost) is
// no failure on base, only a check nobody did. Requests that are not objects
// are an error.
func unrun(requests []json.RawMessage, bySuite map[string][]suiteRun) (map[string]bool, error) {
	type slot struct{ suite, env string }
	want := map[slot]int{}
	for i, raw := range requests {
		var r requestedRun
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("with: requests[%d]: %w", i, err)
		}
		k := slot{suite: r.Suite}
		if r.Environment != nil {
			k.env = r.Environment.Name
		}
		want[k]++
	}
	out := map[string]bool{}
	for k, n := range want {
		have := 0
		for _, r := range bySuite[k.suite] {
			if r.environment == k.env {
				have++
			}
		}
		if have < n {
			out[k.suite] = true
		}
	}
	return out, nil
}

// testsBaseCheckResult is the action tests.base_check_result. It reads the
// test runs the base check's jobs stored and says, for each new test,
// whether it failed on base (see failedOnBase and docs/playbooks.md). ok is
// true when every new test failed on base.
//
// With with.requests, the output of tests.base_check_request, it compares the
// runs with the requests: a suite that has fewer runs than requests in an
// environment was not checked everywhere, so its tests are left out of
// new_tests (the verification then says they were not checked) and ok is
// false.
func (a *actions) testsBaseCheckResult(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Runs     []jobRefs          `json:"runs"`
		Tests    []newTest          `json:"tests"`
		Requests *[]json.RawMessage `json:"requests"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	bySuite, err := a.loadSuiteRuns(ctx, call.Run.AccountID, with.Runs)
	if err != nil {
		return nil, err
	}
	var unchecked map[string]bool
	if with.Requests != nil {
		if unchecked, err = unrun(*with.Requests, bySuite); err != nil {
			return nil, err
		}
	}
	out := baseCheckResult{NewTests: []baseCheckTest{}, OK: true}
	seen := map[newTest]bool{} // a test has one entry per criterion it checks
	for _, t := range with.Tests {
		t.File = ""
		if seen[t] {
			continue
		}
		seen[t] = true
		if unchecked[t.Suite] {
			out.OK = false
			continue
		}
		failed := failedOnBase(bySuite[t.Suite], t.ID)
		out.NewTests = append(out.NewTests, baseCheckTest{ID: t.ID, FailedOnBase: failed})
		out.OK = out.OK && failed
	}
	return out, nil
}
