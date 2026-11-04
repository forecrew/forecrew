//go:build e2e

package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/actionsprovider"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

// actionsPlaybook replaces the goal playbook with one command job that runs
// on a GitHub-hosted runner.
const actionsPlaybook = `description: Run the unit suite on a GitHub-hosted runner.
inputs: [repo, goal_id, platforms]
steps:
  - id: unit
    type: job
    with:
      kind: command
      repo: '${inputs.repo}'
      goal_id: '${inputs.goal_id}'
      suite: unit
      cell: {os: linux}
      command:
        argv: [sh, ci/test.sh]
        reports: ['reports/*.xml']
        artifacts: [coverage.out]
      environment: {name: hosted, provider: actions, runs_on: ubuntu-24.04, versions: {sh: 'echo sh'}}
`

// A command job of an actions environment runs on a (fake) GitHub-hosted
// runner: forecrewd dispatches the workflow, follows the run, and stores the
// results exactly as for a worker.
func TestActionsJobStoresTestResults(t *testing.T) {
	w := newWorldWith(t, true, withPlaybook(t, actionsPlaybook))
	head := pushToMain(t, w.remote, map[string]string{"ci/test.sh": testScript})
	// The run's artifact, as the workflow uploads it.
	w.actions.OnDispatch = func(d testkit.Dispatch) *testkit.ActionsRun {
		report := `<testsuites><testsuite name="app">
<testcase classname="app/auth" name="TestLogin" time="0.012"/>
<testcase classname="app/auth" name="TestLogout" time="0.030"><failure message="want 204, got 500"/></testcase>
<testcase classname="app/auth" name="TestRefresh" time="0.002"><skipped message="needs redis"/></testcase>
</testsuite></testsuites>`
		result := `{"phase":"command","head_sha":"` + head + `","exit_code":1,"duration_ms":2500,"versions":{"sh":"sh"},` +
			`"image_os":"ubuntu24","image_version":"20260301.1.0","runner_os":"Linux","runner_arch":"X64"}`
		return &testkit.ActionsRun{
			Conclusion: "failure", HeadSHA: head, Polls: 1, ArtifactName: actionsprovider.ArtifactPrefix + d.Inputs["job_id"],
			Artifact: testkit.Zip(map[string]string{actionsprovider.ResultFile: result, "reports/unit.xml": report, "coverage.out": "mode: set\n"}),
		}
	}

	w.createGoal("Run the tests", "Run the unit suite.")
	var job domain.Job
	eventually(t, "the actions job to finish", func() bool {
		js := w.jobs("")
		if len(js) != 1 || !js[0].Status.Terminal() {
			return false
		}
		job = js[0]
		return true
	})
	if job.Status != domain.JobSucceeded || job.Pool != domain.PoolActions || job.HeadSHA != head {
		t.Fatalf("job = %s %s pool %s head %q error %q", job.Ref(), job.Status, job.Pool, job.HeadSHA, job.Error)
	}

	ds := w.actions.Dispatches()
	if len(ds) != 1 || ds[0].File != actionsprovider.WorkflowFile || ds[0].Inputs["runs_on"] != "ubuntu-24.04" || ds[0].Ref != "main" {
		t.Fatalf("dispatches = %+v", ds)
	}

	var infos []mcp.TestRunInfo
	if code := w.api(http.MethodGet, "/test-runs?job="+job.Ref(), nil, &infos); code != http.StatusOK || len(infos) != 1 {
		t.Fatalf("test-runs: %d %+v", code, infos)
	}
	run := infos[0]
	if run.Suite != "unit" || run.Status != domain.TestRunFailed || run.SHA != head || run.Environment != "hosted" ||
		run.Counts != (domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1}) || run.Versions["runner_image"] != "ubuntu24" ||
		len(run.Failures) != 1 || run.Failures[0].Name != "TestLogout" {
		t.Errorf("run = %+v", run)
	}

	var arts []domain.Artifact
	if code := w.api(http.MethodGet, "/jobs/"+job.Ref()+"/artifacts", nil, &arts); code != http.StatusOK {
		t.Fatalf("artifacts: status %d", code)
	}
	var names []string
	for _, a := range arts {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"coverage.out", "reports/unit.xml"}) {
		t.Errorf("artifacts = %v", names)
	}
	logs, err := w.st.JobEvents().List(t.Context(), w.account.ID, job.ID, 0, 100)
	must(t, err)
	if !slices.ContainsFunc(logs, func(e domain.JobEvent) bool { return strings.HasPrefix(e.Text, "workflow run: ") }) {
		t.Errorf("the workflow run is not in the job log: %+v", logs)
	}
	if workers, err := w.st.Workers().List(t.Context(), w.account.ID); err != nil || len(workers) != 1 || workers[0].Pool != domain.PoolActions {
		t.Errorf("workers = %+v, %v", workers, err)
	}
}
