package playbooks

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
	"github.com/forecrew/forecrew/internal/workflow"
)

// criterionItem is a criterion in the body of a task (taskBody):
// "- **AC-3** text", under the heading "## Acceptance criteria".
var criterionItem = regexp.MustCompile(`^- \*\*(AC-[1-9][0-9]*)\*\*`)

// taskCriteria returns the ids of the acceptance criteria listed in a task's
// body, in order.
func taskCriteria(body string) []string {
	var ids []string
	in := false
	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			in = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "## ")), "Acceptance criteria")
		case in:
			if m := criterionItem.FindStringSubmatch(line); m != nil {
				ids = append(ids, m[1])
			}
		}
	}
	return slices.Compact(ids)
}

// taskTestMap returns the tests and the untestable criteria of the
// successful test-engineer jobs of the task. A retry reports only what it
// changed itself, so all jobs count, as for tests.locked_paths.
func (a *actions) taskTestMap(ctx context.Context, t domain.Task) (tests []verification.MapTest, untestable []verification.Untestable, err error) {
	docs, err := a.testerOutputs(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	for _, doc := range docs {
		for _, x := range doc.Tests {
			tests = append(tests, verification.MapTest{Criterion: x.Criterion, Suite: x.Suite, ID: x.ID})
		}
		for _, x := range doc.Untestable {
			untestable = append(untestable, verification.Untestable{Criterion: x.Criterion, Reason: x.Reason})
		}
	}
	return tests, untestable, nil
}

// verificationOutput is the output of verification.build.
type verificationOutput struct {
	Status   domain.VerificationStatus `json:"status"`
	Blocking []string                  `json:"blocking"`
	Summary  string                    `json:"summary"`
	ReportID uuid.UUID                 `json:"report_id"`
}

// verificationBuild is the action verification.build. It builds the
// verification report of the head sha of the project's pull request
// with.change from the stored test runs of the sha (the runs say where they
// ran: nothing about them is taken from with), the criteria and the test map
// of the task (with.task_id; the change's task by default) and the fail-on-base
// check (with.base_check, the output of tests.base_check_result), against the
// matrix at sha. It stores the report, updates the PR comment in place and
// sets the check run forecrew/verification. Calling it again with the same
// data changes nothing, and with more test runs it replaces the report.
//
// with.baseline builds the baseline report of onboarding instead: of a commit
// on the default branch, with no criteria and no base check (task_id and
// base_check are refused), shown nowhere on GitHub (no comment, no check run:
// nothing is being merged). The report is kept under
// with.change, the pull request the commit came from; without one, under the
// project's latest merged pull request, since a report belongs to a change.
func (a *actions) verificationBuild(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Change int       `json:"change"`
		SHA    string    `json:"sha"`
		TaskID uuid.UUID `json:"task_id"`
		// BaseCheck is the output of tests.base_check_result.
		BaseCheck *struct {
			NewTests []verification.BaseTest `json:"new_tests"`
			OK       bool                    `json:"ok"`
		} `json:"base_check"`
		Baseline bool `json:"baseline"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.SHA == "" || (with.Change <= 0 && !with.Baseline) {
		return nil, errors.New("with: change and sha are required")
	}
	if with.Baseline && (with.TaskID != uuid.Nil() || with.BaseCheck != nil) {
		return nil, errors.New("with: a baseline has no task_id and no base_check")
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	ch, err := a.verificationChange(ctx, p, with.Change)
	if err != nil {
		return nil, err
	}
	var in verification.Inputs
	if with.Baseline {
		in, err = a.matrixInputs(ctx, p, with.SHA)
	} else {
		in, err = a.verificationInputs(ctx, call.Run, p, ch, with.SHA, with.TaskID)
	}
	if err != nil {
		return nil, err
	}
	if with.BaseCheck != nil {
		in.BaseCheck = with.BaseCheck.NewTests
	}
	rep, err := verification.Builder{Store: a.Store}.Build(ctx, ch, with.SHA, in)
	if err != nil {
		return nil, err
	}
	publish := a.verifier.Publish
	if with.Baseline {
		publish = a.verifier.Save
	}
	stored, err := publish(ctx, ch, rep)
	if err != nil {
		return nil, err
	}
	return verificationOutput{Status: rep.Status, Blocking: rep.Blocking, Summary: rep.Summary(), ReportID: stored.ID}, nil
}

// verificationChange returns the change a report is kept under: the project's
// pull request number, or for a baseline without one the latest merged one.
func (a *actions) verificationChange(ctx context.Context, p domain.Project, number int) (domain.Change, error) {
	if number > 0 {
		ch, err := a.Store.Changes().GetByPR(ctx, p.AccountID, p.Repo, number)
		if err != nil {
			return domain.Change{}, fmt.Errorf("get pull request %s#%d: %w", p.Repo, number, err)
		}
		return ch, nil
	}
	merged, err := a.Store.Changes().List(ctx, p.AccountID, store.ChangeFilter{ProjectID: p.ID, State: domain.ChangeMerged})
	if err != nil {
		return domain.Change{}, fmt.Errorf("list the pull requests of %s: %w", p.Name, err)
	}
	if len(merged) == 0 {
		return domain.Change{}, fmt.Errorf("%s has no merged pull request to keep the baseline under", p.Name)
	}
	return merged[len(merged)-1], nil
}

// matrixInputs returns the results the matrix at sha asks for.
func (a *actions) matrixInputs(ctx context.Context, p domain.Project, sha string) (verification.Inputs, error) {
	var in verification.Inputs
	m, envs, err := matrix.Load(a.repoReader(ctx, p.Repo, sha))
	if err != nil {
		return in, fmt.Errorf("the test matrix at %s: %w", sha, err)
	}
	all, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeAll})
	if err != nil {
		return in, err
	}
	required, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeRequired})
	if err != nil {
		return in, err
	}
	in.Expected = verification.Expect(all, required)
	return in, nil
}

// verificationInputs reads what the report needs besides the test runs:
// the results the matrix at sha asks for, and the criteria and test map of
// the task.
func (a *actions) verificationInputs(ctx context.Context, run domain.WorkflowRun, p domain.Project, ch domain.Change, sha string, taskID uuid.UUID) (verification.Inputs, error) {
	in, err := a.matrixInputs(ctx, p, sha)
	if err != nil {
		return in, err
	}
	if taskID == uuid.Nil() {
		taskID = ch.TaskID
	}
	if taskID == uuid.Nil() {
		return in, nil
	}
	t, err := a.loadTask(ctx, run, taskID)
	if err != nil {
		return in, err
	}
	in.Task, in.Criteria = true, taskCriteria(t.Body)
	in.Tests, in.Untestable, err = a.taskTestMap(ctx, t)
	return in, err
}

// rerunOutput is the output of verification.rerun_cells.
type rerunOutput struct {
	Requests []workflow.JobRequest `json:"requests"`
}

// verificationRerunCells is the action verification.rerun_cells. It returns
// the command job requests that run again the required suites of a stored
// report whose result failed or errored and that ran once (the flaky rule: a
// test that fails and then passes in a rerun of its cell is flaky, one that
// fails twice is not), for a foreach of kind: command job steps. A cell that
// was rerun already is left out, so a report is rerun once. Build the report
// again once the reruns are done.
func (a *actions) verificationRerunCells(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		ReportID uuid.UUID `json:"report_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	stored, err := a.Store.VerificationReports().Get(ctx, call.Run.AccountID, with.ReportID)
	if err != nil {
		return nil, fmt.Errorf("get verification report %s: %w", with.ReportID, err)
	}
	rep, err := verification.Parse(stored.Report)
	if err != nil {
		return nil, fmt.Errorf("verification report %s: %w", stored.ID, err)
	}
	p, err := a.Store.Projects().Get(ctx, stored.AccountID, stored.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	out := rerunOutput{Requests: []workflow.JobRequest{}}
	var failed []verification.Result
	for _, r := range rep.Results {
		if r.Required && r.Attempts == 1 && (r.Status == string(domain.TestRunFailed) || r.Status == string(domain.TestRunErrored)) {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return out, nil
	}
	m, envs, err := matrix.Load(a.repoReader(ctx, p.Repo, rep.SHA))
	if err != nil {
		return nil, fmt.Errorf("the test matrix at %s: %w", rep.SHA, err)
	}
	suites := slices.Sorted(maps.Keys(suiteSet(failed)))
	scope := matrix.Scope{Suites: suites}
	if err := scope.Check(m); err != nil {
		return nil, err
	}
	cells, err := matrix.Expand(m, envs, scope)
	if err != nil {
		return nil, err
	}
	jobs := map[string]matrix.Job{}
	for _, c := range cells {
		for _, j := range c.Jobs() {
			jobs[rerunKey(j.Environment.Name, j.Cell, j.Suite)] = j
		}
	}
	for _, r := range failed {
		j, ok := jobs[rerunKey(r.Environment, r.Cell, r.Suite)]
		if !ok {
			return nil, fmt.Errorf("the matrix at %s no longer has %s in %s", rep.SHA, r.Suite, verification.CellKey(r.Cell))
		}
		env, cmd := j.Environment, j.Command
		out.Requests = append(out.Requests, workflow.JobRequest{
			Kind: domain.JobKindCommand, Repo: p.Repo, BaseRef: rep.SHA, Needs: j.Needs, Command: &cmd, Environment: &env, Cell: j.Cell, Suite: j.Suite,
		})
	}
	return out, nil
}

func rerunKey(environment string, cell map[string]string, suite string) string {
	return environment + "\x00" + verification.CellKey(cell) + "\x00" + suite
}

func suiteSet(results []verification.Result) map[string]bool {
	out := map[string]bool{}
	for _, r := range results {
		out[r.Suite] = true
	}
	return out
}
