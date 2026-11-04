//go:build e2e

package e2e

import (
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// matrixPlaybook replaces the goal playbook with a run of the repo's matrix.
const matrixPlaybook = `description: Run the test matrix of the repo.
inputs: [repo, goal_id, platforms]
steps:
  - {id: plan, type: action, action: matrix.plan, with: {repo: '${inputs.repo}', ref: main, scope: all}}
  - id: cells
    type: foreach
    parallel: 2
    items: '${steps.plan.output.jobs}'
    steps:
      - {id: run, type: job, with: {kind: command, repo: '${inputs.repo}', goal_id: '${inputs.goal_id}', from: '${item}'}}
`

const testMatrix = `version: 1
axes: {os: [linux, plan9]}
environments:
  - {match: {}, environment: host}
suites:
  - {name: unit, command: [sh, ci/test.sh], level: unit, reports: ['reports/*.xml'], artifacts: [coverage.out]}
`

const hostEnvironment = `version: 1
name: host
provider: machine
versions: {sh: echo sh e2e}
`

// A playbook plans the matrix of the repo and runs its two cells as command
// jobs on the workers that have the labels of each.
func TestMatrixRunsItsCells(t *testing.T) {
	w := newWorldWith(t, true, withPlaybook(t, matrixPlaybook))
	head := pushToMain(t, w.remote, map[string]string{
		"ci/test.sh":                       testScript,
		".forecrew/matrix.yaml":            testMatrix,
		".forecrew/environments/host.yaml": hostEnvironment,
	})
	w.startWorker("box", "linux")
	w.startWorker("lab", "plan9")

	w.createGoal("Run the matrix", "Run every cell.")
	w.runStatus(domain.RunSucceeded)

	js := w.jobs("")
	if len(js) != 2 {
		t.Fatalf("jobs = %+v", js)
	}
	var cells []string
	for _, j := range js {
		if j.Status != domain.JobSucceeded || j.Kind != domain.JobKindCommand || j.Pool != domain.PoolBuild || j.HeadSHA != head || j.Spec.Suite != "unit" {
			t.Errorf("job = %s %s pool %s head %q suite %q error %q", j.Ref(), j.Status, j.Pool, j.HeadSHA, j.Spec.Suite, j.Error)
		}
		if !slices.Equal(j.Needs, []string{j.Spec.Cell["os"]}) {
			t.Errorf("%s needs %v for cell %v", j.Ref(), j.Needs, j.Spec.Cell)
		}
		runs, err := w.st.TestRuns().List(t.Context(), w.account.ID, store.TestRunFilter{JobID: j.ID})
		must(t, err)
		if len(runs) != 1 || runs[0].Environment != "host" || runs[0].Suite != "unit" || runs[0].Counts.Passed != 1 {
			t.Errorf("test runs of %s = %+v", j.Ref(), runs)
			continue
		}
		cells = append(cells, runs[0].Cell["os"])
	}
	slices.Sort(cells)
	if !slices.Equal(cells, []string{"linux", "plan9"}) {
		t.Errorf("cells = %v", cells)
	}
}
