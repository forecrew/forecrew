package workerapi

import (
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func (e *env) newCommandJob(t *testing.T) domain.Job {
	t.Helper()
	j, err := e.jobs.Create(t.Context(), system, domain.Job{
		AccountID: e.account, ProjectID: e.project.ID, Kind: domain.JobKindCommand,
		Spec: domain.JobSpec{
			Repo: repo, BaseRef: "main", Suite: "unit", Cell: map[string]string{"os": "linux"},
			Command:     &domain.CommandSpec{Argv: []string{"true"}},
			Environment: &domain.Environment{Name: "host", Provider: domain.ProviderMachine},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func sampleRun() map[string]any {
	return map[string]any{
		"suite": "unit", "status": "failed", "counts": map[string]int{"passed": 1, "failed": 1}, "duration_ms": 1200,
		"exit_code": 1, "versions": map[string]string{"go": "go1.27"}, "report_artifacts": []string{"reports/unit.xml"},
		"results": []map[string]any{
			{"name": "TestA", "classname": "pkg", "status": "passed", "duration_ms": 3},
			{"name": "TestB", "classname": "pkg", "status": "failed", "duration_ms": 4, "message": "boom"},
		},
	}
}

func TestCompleteStoresTestRuns(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	change := domain.Change{AccountID: e.account, ProjectID: e.project.ID, Repo: repo, Number: 7, HeadSHA: "abc123", State: domain.ChangeOpen}
	if err := e.st.Changes().Upsert(t.Context(), &change); err != nil {
		t.Fatal(err)
	}
	j := e.newCommandJob(t)
	if st, lease := e.poll(t, token); st != http.StatusOK || lease.Job.RepoToken == "" || len(lease.Job.Env) != 0 {
		t.Fatalf("poll = %d, %+v", st, lease.Job)
	}
	st, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "succeeded", "head_sha": "abc123", "test_runs": []any{sampleRun()}})
	wantStatus(t, st, http.StatusOK, body)

	runs, err := e.st.TestRuns().List(t.Context(), e.account, store.TestRunFilter{JobID: j.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	r := runs[0]
	if r.SHA != "abc123" || r.ChangeID != change.ID || r.Suite != "unit" || r.Environment != "host" || r.Cell["os"] != "linux" || r.Attempt != 1 ||
		r.Status != domain.TestRunFailed || r.Counts.Failed != 1 || r.ExitCode != 1 || r.Versions["go"] != "go1.27" || r.ReportArtifacts[0] != "reports/unit.xml" || r.ProjectID != e.project.ID {
		t.Errorf("run = %+v", r)
	}
	rs, _ := e.st.TestRuns().Results(t.Context(), e.account, r.ID)
	if len(rs) != 2 || rs[1].Message != "boom" {
		t.Errorf("results = %+v", rs)
	}
}

func TestCompleteTestRunsWithoutChange(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newCommandJob(t)
	e.poll(t, token)
	run := sampleRun()
	run["suite"] = "" // defaults to the spec's suite
	st, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "succeeded", "head_sha": "ffff", "test_runs": []any{run}})
	wantStatus(t, st, http.StatusOK, body)
	runs, _ := e.st.TestRuns().List(t.Context(), e.account, store.TestRunFilter{SHA: "ffff"})
	if len(runs) != 1 || runs[0].ChangeID != uuid.Nil() || runs[0].Suite != "unit" {
		t.Errorf("runs = %+v", runs)
	}
}

func TestCompleteRejectsBadTestRuns(t *testing.T) {
	bad := func(mut func(map[string]any)) []any {
		r := sampleRun()
		mut(r)
		return []any{r}
	}
	tests := []struct {
		name   string
		agent  bool
		status string
		sha    string
		runs   []any
	}{
		{"agent job", true, "succeeded", "abc", []any{sampleRun()}},
		{"no head sha", false, "succeeded", "", []any{sampleRun()}},
		{"retry", false, "retry", "abc", []any{sampleRun()}},
		{"unknown run status", false, "succeeded", "abc", bad(func(r map[string]any) { r["status"] = "weird" })},
		{"negative duration", false, "succeeded", "abc", bad(func(r map[string]any) { r["duration_ms"] = -1 })},
		{"bad result", false, "succeeded", "abc", bad(func(r map[string]any) { r["results"] = []map[string]any{{"name": "", "status": "passed"}} })},
		{"bad result status", false, "succeeded", "abc", bad(func(r map[string]any) { r["results"] = []map[string]any{{"name": "t", "status": "x"}} })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			_, token := e.addWorker(t, "box")
			var j domain.Job
			if tt.agent {
				j = e.newJob(t, jobOpts{})
			} else {
				j = e.newCommandJob(t)
			}
			e.poll(t, token)
			body := map[string]any{"status": tt.status, "head_sha": tt.sha, "test_runs": tt.runs, "error": "e"}
			st, resp := e.call(t, token, jobPath(j.ID, "complete"), body)
			wantStatus(t, st, http.StatusBadRequest, resp)
			if runs, _ := e.st.TestRuns().List(t.Context(), e.account, store.TestRunFilter{}); len(runs) != 0 {
				t.Errorf("stored %+v", runs)
			}
			if got := e.job(t, j.ID); got.Status != domain.JobRunning {
				t.Errorf("job is %s: a rejected complete must change nothing", got.Status)
			}
		})
	}
}

func TestCompleteBodyLimit(t *testing.T) {
	big := sampleRun()
	big["results"] = []map[string]any{{"name": "T", "status": "failed", "message": strings.Repeat("x", 2<<20)}}

	// Over 1 MiB is fine for a complete, up to max_test_results_size.
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newCommandJob(t)
	e.poll(t, token)
	st, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "succeeded", "head_sha": "abc", "test_runs": []any{big}})
	wantStatus(t, st, http.StatusOK, body)

	// Over the limit is a 413, and changes nothing.
	e = newEnv(t, func(c *Config) { c.MaxTestResultsSize = 1 << 20 })
	_, token = e.addWorker(t, "box")
	j = e.newCommandJob(t)
	e.poll(t, token)
	st, body = e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "succeeded", "head_sha": "abc", "test_runs": []any{big}})
	wantStatus(t, st, http.StatusRequestEntityTooLarge, body)
	if got := e.job(t, j.ID); got.Status != domain.JobRunning {
		t.Errorf("job is %s", got.Status)
	}
	// An agent job's complete keeps the 1 MiB limit, and so do other endpoints.
	e = newEnv(t)
	_, token = e.addWorker(t, "box")
	agent := e.newJob(t, jobOpts{})
	e.poll(t, token)
	st, body = e.call(t, token, jobPath(agent.ID, "complete"), `{"status":"succeeded","pad":"`+strings.Repeat("x", 2<<20)+`"}`)
	wantStatus(t, st, http.StatusRequestEntityTooLarge, body)
	e = newEnv(t, func(c *Config) { c.MaxTestResultsSize = 1 << 20 })
	_, token = e.addWorker(t, "box")
	j = e.newCommandJob(t)
	e.poll(t, token)
	st, body = e.call(t, token, jobPath(j.ID, "events"), `{"events":[],"pad":"`+strings.Repeat("x", 2<<20)+`"}`)
	wantStatus(t, st, http.StatusRequestEntityTooLarge, body)
	if got := New(Config{}).cfg.MaxTestResultsSize; got != config.DefaultMaxTestResultsSize {
		t.Errorf("default = %d", got)
	}
}
