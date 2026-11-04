//go:build e2e

package e2e

import (
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	forecrew "github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// commandPlaybook replaces the goal playbook with one command job that runs
// the fixture repo's test script.
const commandPlaybook = `description: Run the unit suite of the repo as a command job.
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
      needs: [linux]
      command:
        argv: [sh, ci/test.sh]
        reports: ['reports/*.xml']
        artifacts: [coverage.out]
      environment: {name: host, provider: machine, versions: {sh: 'echo sh e2e'}}
`

// testScript writes a JUnit report with one pass, one failure and one skip,
// like a test command does, and exits like a failing test run.
const testScript = `mkdir -p reports
cat > reports/unit.xml <<'XML'
<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="app" tests="3">
    <testcase classname="app/auth" name="TestLogin" time="0.012"/>
    <testcase classname="app/auth" name="TestLogout" time="0.030"><failure message="want 204, got 500"/></testcase>
    <testcase classname="app/auth" name="TestRefresh" time="0.002"><skipped message="needs redis"/></testcase>
  </testsuite>
</testsuites>
XML
echo "mode: set" > coverage.out
echo "1 passed, 1 failed, 1 skipped"
exit 1
`

// withPlaybook returns the embedded packs with the goal playbook replaced.
func withPlaybook(t *testing.T, playbook string) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	must(t, fs.WalkDir(forecrew.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(forecrew.Files, path)
		m[path] = &fstest.MapFile{Data: data}
		return err
	}))
	m["packs/playbooks/goal.yaml"] = &fstest.MapFile{Data: []byte(playbook)}
	return m
}

// pushToMain commits files to main of the remote and returns the new head.
func pushToMain(t *testing.T, remote string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "clone", "--quiet", remote, ".")
	must(t, writeFiles(dir, files))
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "--quiet", "-m", "add the test script")
	runGit(t, dir, "push", "--quiet", "origin", "main")
	return runGit(t, dir, "rev-parse", "HEAD")
}

// A command job runs the repo's test command on a worker; forecrewd stores
// the test run and its results, the report and the artifacts.
func TestCommandJobStoresTestResults(t *testing.T) {
	w := newWorldWith(t, true, withPlaybook(t, commandPlaybook))
	head := pushToMain(t, w.remote, map[string]string{"ci/test.sh": testScript})
	w.startWorker("box", "linux") // no runtime label: command jobs need none

	w.createGoal("Run the tests", "Run the unit suite.")
	var job domain.Job
	eventually(t, "the command job to finish", func() bool {
		js := w.jobs("")
		if len(js) != 1 || !js[0].Status.Terminal() {
			return false
		}
		job = js[0]
		return true
	})
	if job.Status != domain.JobSucceeded || job.Kind != domain.JobKindCommand || job.Pool != domain.PoolBuild || job.HeadSHA != head {
		t.Fatalf("job = %s %s pool %s head %q error %q; want succeeded command job on %s", job.Ref(), job.Status, job.Pool, job.HeadSHA, job.Error, head)
	}

	runs, err := w.st.TestRuns().List(t.Context(), w.account.ID, store.TestRunFilter{JobID: job.ID})
	must(t, err)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	run := runs[0]
	if run.Suite != "unit" || run.Status != domain.TestRunFailed || run.ExitCode != 1 || run.SHA != head || run.Environment != "host" ||
		run.Cell["os"] != "linux" || run.Counts != (domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1}) ||
		!slices.Equal(run.ReportArtifacts, []string{"reports/unit.xml"}) || run.Versions["sh"] != "sh e2e" || run.Attempt != 1 {
		t.Errorf("run = %+v", run)
	}
	results, err := w.st.TestRuns().Results(t.Context(), w.account.ID, run.ID)
	must(t, err)
	var got []string
	for _, r := range results {
		got = append(got, r.Classname+" "+r.Name+" "+string(r.Status)+" "+r.Message)
	}
	want := []string{"app/auth TestLogin passed ", "app/auth TestLogout failed want 204, got 500", "app/auth TestRefresh skipped needs redis"}
	if !slices.Equal(got, want) {
		t.Errorf("results = %q, want %q", got, want)
	}

	w.checkCommandEvidence(job)

	// Reachable over REST: job status counts, and the runs by job and sha.
	var info mcp.JobInfo
	if code := w.api(http.MethodGet, "/jobs/"+job.Ref(), nil, &info); code != http.StatusOK || info.Tests == nil ||
		info.Tests.Status != domain.TestRunFailed || info.Tests.Passed != 1 || info.Tests.Failed != 1 || info.Tests.Skipped != 1 {
		t.Errorf("job status: %d tests %+v", code, info.Tests)
	}
	for _, q := range []string{"job=" + job.Ref(), "sha=" + head} {
		var infos []mcp.TestRunInfo
		if code := w.api(http.MethodGet, "/test-runs?"+q, nil, &infos); code != http.StatusOK || len(infos) != 1 ||
			len(infos[0].Failures) != 1 || infos[0].Failures[0].Name != "TestLogout" {
			t.Errorf("test-runs?%s: %d %+v", q, code, infos)
		}
	}
	if strings.Contains(w.logs.String(), "worker-secret") {
		t.Error("forecrewd logged a secret")
	}
}

// checkCommandEvidence checks the artifacts and the log of the command job.
func (w *world) checkCommandEvidence(job domain.Job) {
	w.t.Helper()
	var arts []domain.Artifact
	if code := w.api(http.MethodGet, "/jobs/"+job.Ref()+"/artifacts", nil, &arts); code != http.StatusOK {
		w.t.Fatalf("artifacts: status %d", code)
	}
	var names []string
	for _, a := range arts {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"coverage.out", "reports/unit.xml"}) {
		w.t.Errorf("artifacts = %v", names)
	}
	logs, err := w.st.JobEvents().List(w.t.Context(), w.account.ID, job.ID, 0, 100)
	must(w.t, err)
	if !slices.ContainsFunc(logs, func(e domain.JobEvent) bool { return e.Text == "1 passed, 1 failed, 1 skipped" }) {
		w.t.Errorf("the command output is not in the job log: %+v", logs)
	}
}
