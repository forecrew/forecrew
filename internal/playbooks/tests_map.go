package playbooks

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
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
func (a *actions) testsMap(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID uuid.UUID `json:"task_id"`
		Needs  []string  `json:"needs"`
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
	out := testsMapOutput{IfArchitect: []architectJob{}, Tests: []newTest{}, Files: []string{}, Suites: []string{}}
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
	var uncovered []string
	for _, c := range taskCriteria(t.Body) {
		if !covered[c] {
			uncovered = append(uncovered, c)
		}
	}
	var feedback []string
	switch {
	case len(docs) == 0:
		feedback = append(feedback, "No test engineer job has finished for this task.")
	case docs[len(docs)-1].Escalation != nil && strings.TrimSpace(*docs[len(docs)-1].Escalation) != "":
		esc := *docs[len(docs)-1].Escalation
		feedback = append(feedback, "The test engineer stopped and asked for a decision:\n"+contextpack.WrapUntrusted("test engineer escalation", esc))
		if lower := strings.ToLower(esc); strings.Contains(lower, "matrix") || strings.Contains(lower, "suite") {
			out.IfArchitect = append(out.IfArchitect, architectJob{TaskID: t.ID, Branch: taskBranch(t), Needs: append([]string{}, with.Needs...)})
		}
	}
	if len(docs) > 0 && len(uncovered) > 0 {
		feedback = append(feedback, fmt.Sprintf("Every acceptance criterion needs a test or an untestable entry with a reason. Missing: %s.", strings.Join(uncovered, ", ")))
	}
	out.Feedback = strings.Join(feedback, "\n\n")
	out.Ready = len(feedback) == 0
	return out, nil
}
