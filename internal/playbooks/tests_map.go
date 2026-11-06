package playbooks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/workflow"
)

// testsMapOutput is the output of tests.map.
type testsMapOutput struct {
	// Ready is set when the latest test engineer job did not escalate and
	// every criterion of the task has a test or an untestable reason.
	Ready bool `json:"ready"`
	// IfArchitect holds the task when the test engineer's escalation is
	// about the matrix, so that a foreach runs the test architect: inside it
	// ${item} is this element, not the task of the outer foreach.
	IfArchitect []architectJob `json:"if_architect"`
	// IfReady holds the task's ID when Ready is set, for a foreach that runs
	// steps only for tests that are ready.
	IfReady []uuid.UUID `json:"if_ready"`
	// Feedback tells the next job what is missing, when Ready is false.
	Feedback string `json:"feedback"`
	// Tests, Files and Suites are the union over the test engineer's jobs.
	Tests  []newTest `json:"tests"`
	Files  []string  `json:"files"`
	Suites []string  `json:"suites"`
}

// architectJob is what a foreach over if_architect needs to start the test
// architect on the task's branch.
type architectJob struct {
	TaskID uuid.UUID `json:"task_id"`
	Branch string    `json:"branch"`
	Needs  []string  `json:"needs"`
}

// testsMap is the action tests.map. It reads the successful test-engineer
// jobs of with.task_id (a retry reports only what it changed itself, so all
// of them count) and outputs the union of their tests, files and suites, as
// the inputs of the base check and of matrix.plan.
//
// It also says whether the task's tests are ready: the latest job did not
// escalate and every acceptance criterion in the task's body is in tests or
// in untestable. When they are not, feedback says why, and if_architect has
// the task (with its branch and with.needs, the worker labels of the task)
// when the escalation is about the test matrix or a suite, which is how a test
// engineer reports that the matrix must change first.
//
// A test mapped to a criterion the task does not have is not ready either:
// it would count for nothing. With with.repo it also reads the matrix at the
// task's branch, and a matrix that is invalid or lacks a suite is not ready and, like an escalation about the matrix,
// asks the test architect.
func (a *actions) testsMap(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID uuid.UUID `json:"task_id"`
		Needs  []string  `json:"needs"`
		// Repo, when set, makes the action check the suites of the tests
		// against the matrix at the task's branch.
		Repo string `json:"repo"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	docs, err := a.testerOutputs(ctx, t)
	if err != nil {
		return nil, err
	}
	out := testsMapOutput{IfArchitect: []architectJob{}, IfReady: []uuid.UUID{}, Tests: []newTest{}, Files: []string{}, Suites: []string{}}
	covered := unionTests(docs, &out)
	var uncovered []string
	for _, c := range taskCriteria(t.Body) {
		if !covered[c] {
			uncovered = append(uncovered, c)
		}
	}
	feedback, err := a.mapFeedback(ctx, t, docs, uncovered, with.Repo, with.Needs, &out)
	if err != nil {
		return nil, err
	}
	out.Feedback = strings.Join(feedback, "\n\n")
	out.Ready = len(feedback) == 0
	if out.Ready {
		out.IfReady = append(out.IfReady, t.ID)
	}
	return out, nil
}

// unionTests fills the tests, files and suites of out from the outputs of the
// test engineer's jobs and returns the criteria they cover: those with a test
// and those listed untestable with a reason.
func unionTests(docs []testMapDoc, out *testsMapOutput) map[string]bool {
	covered := map[string]bool{}
	for _, doc := range docs {
		out.Files = append(out.Files, doc.Files...)
		for _, x := range doc.Tests {
			covered[x.Criterion] = true
			if !slices.Contains(out.Tests, x.newTest) {
				out.Tests = append(out.Tests, x.newTest)
			}
			out.Suites = append(out.Suites, x.Suite)
		}
		for _, x := range doc.Untestable {
			if x.Reason != "" {
				covered[x.Criterion] = true
			}
		}
	}
	slices.Sort(out.Files)
	out.Files = slices.Compact(out.Files)
	slices.Sort(out.Suites)
	out.Suites = slices.Compact(out.Suites)
	return covered
}

// needsMatrix reports whether an escalation is about the test matrix or a
// suite, which the test architect answers.
func needsMatrix(escalation string) bool {
	lower := strings.ToLower(escalation)
	return strings.Contains(lower, "matrix") || strings.Contains(lower, "suite")
}

// strayTests names the tests mapped to a criterion that is not in criteria,
// each once as "suite id (criterion)". With no criteria there are none.
func strayTests(docs []testMapDoc, criteria []string) []string {
	var out []string
	if len(criteria) == 0 {
		return out
	}
	for _, doc := range docs {
		for _, x := range doc.Tests {
			if s := fmt.Sprintf("%s (%s)", contextpack.OneLine(x.ID), contextpack.OneLine(x.Criterion)); !slices.Contains(criteria, x.Criterion) && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// matrixProblem says, in one line, why the matrix of repo at ref cannot run
// the suites: it is missing or invalid, or has no such suite. It is empty when the suites can run. Only a failed read is an
// error.
func (a *actions) matrixProblem(ctx context.Context, repo, ref string, suites []string) (string, error) {
	m, envs, err := matrix.Load(a.repoReader(ctx, repo, ref))
	scope := matrix.Scope{Suites: suites}
	if err == nil {
		err = scope.Check(m)
	}
	if err == nil {
		_, err = matrix.Expand(m, envs, scope)
	}
	switch {
	case errors.Is(err, matrix.ErrInvalid):
		return contextpack.OneLine(err.Error()), nil
	case err != nil:
		return "", err
	}
	return "", nil
}

// mapFeedback says what keeps the tests of the task from being ready, and
// asks the test architect (out.IfArchitect) when the matrix must change.
func (a *actions) mapFeedback(ctx context.Context, t domain.Task, docs []testMapDoc, uncovered []string, repo string, needs []string, out *testsMapOutput) ([]string, error) {
	architect := architectJob{TaskID: t.ID, Branch: taskBranch(t), Needs: append([]string{}, needs...)}
	var feedback []string
	switch {
	case len(docs) == 0:
		feedback = append(feedback, "No test engineer job has finished for this task.")
	case docs[len(docs)-1].Escalation != nil && strings.TrimSpace(*docs[len(docs)-1].Escalation) != "":
		esc := *docs[len(docs)-1].Escalation
		feedback = append(feedback, "The test engineer stopped and asked for a decision:\n"+contextpack.WrapUntrusted("test engineer escalation", esc))
		if needsMatrix(esc) {
			out.IfArchitect = append(out.IfArchitect, architect)
		}
	}
	if len(docs) > 0 && len(uncovered) > 0 {
		feedback = append(feedback, fmt.Sprintf("Every acceptance criterion needs a test or an untestable entry with a reason. Missing: %s.", strings.Join(uncovered, ", ")))
	}
	if strays := strayTests(docs, taskCriteria(t.Body)); len(strays) > 0 {
		feedback = append(feedback, "These tests are mapped to a criterion the task does not have, so no criterion would show them: "+strings.Join(strays, ", ")+".")
	}
	if repo != "" && len(out.Suites) > 0 {
		problem, err := a.matrixProblem(ctx, repo, taskBranch(t), out.Suites)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			feedback = append(feedback, "The test matrix at the task's branch cannot run these tests: "+problem+"\n\nThe matrix must change first.")
			if len(out.IfArchitect) == 0 {
				out.IfArchitect = append(out.IfArchitect, architect)
			}
		}
	}
	return feedback, nil
}
