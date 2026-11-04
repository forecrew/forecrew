package actionsprovider

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestJobRunsOnActions(t *testing.T) {
	e := newEnv(t)
	j := e.newJob()
	e.finishesWith(j, "failure", map[string]string{
		ResultFile: resultJSON(1), "reports/unit.xml": reportXML, "coverage.out": "mode: set\n",
	})
	e.poll()

	got := e.job(j)
	if got.Status != domain.JobSucceeded || got.HeadSHA != head || got.Pool != domain.PoolActions {
		t.Fatalf("job = %s head %q pool %s error %q", got.Status, got.HeadSHA, got.Pool, got.Error)
	}
	e.checkTestRun(j)
	e.checkArtifacts(j)
	e.checkDispatch(j)
	events, _ := e.st.JobEvents().List(t.Context(), e.account.ID, j.ID, 0, 100)
	var texts []string
	for _, ev := range events {
		texts = append(texts, ev.Text)
	}
	if !slices.Contains(texts, "running") || !slices.Contains(texts, "collecting") {
		t.Errorf("job log = %q", texts)
	}
}

func (e *env) checkTestRun(j domain.Job) {
	e.t.Helper()
	runs := e.testRuns(j)
	if len(runs) != 1 {
		e.t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.Suite != "unit" || r.Status != domain.TestRunFailed || r.ExitCode != 1 || r.DurationMS != 1500 || r.SHA != head || r.Environment != "linux-go" ||
		r.Cell["os"] != "linux" || r.Counts != (domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1}) ||
		!slices.Equal(r.ReportArtifacts, []string{"reports/unit.xml"}) {
		e.t.Errorf("run = %+v", r)
	}
	wantVersions := map[string]string{"go": "go version go1.27", "runner_image": "ubuntu24", "runner_image_version": "20260301.1.0", "runner_os": "Linux X64"}
	for k, v := range wantVersions {
		if r.Versions[k] != v {
			e.t.Errorf("versions[%s] = %q, want %q (all: %v)", k, r.Versions[k], v, r.Versions)
		}
	}
	want := []string{"app/auth TestLogin passed ", "app/auth TestLogout failed want 204, got 500", "app/auth TestRefresh skipped needs redis"}
	if g := e.results(r); !slices.Equal(g, want) {
		e.t.Errorf("results = %q", g)
	}
}

func (e *env) checkArtifacts(j domain.Job) {
	e.t.Helper()
	arts, err := e.st.Artifacts().ListByJob(e.t.Context(), e.account.ID, j.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, a := range arts {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"coverage.out", "reports/unit.xml"}) {
		e.t.Errorf("artifacts = %v", names)
	}
}

func (e *env) checkDispatch(j domain.Job) {
	e.t.Helper()
	ds := e.gh.Dispatches()
	if len(ds) != 1 || ds[0].Repo != repo || ds[0].File != WorkflowFile || ds[0].Ref != "agent/task-1" {
		e.t.Fatalf("dispatches = %+v", ds)
	}
	in := ds[0].Inputs
	if in["job_id"] != j.ID.String() || in["ref"] != "" || in["runs_on"] != "ubuntu-24.04" || in["reports"] != "reports/*.xml" || in["artifacts"] != "coverage.out" {
		e.t.Errorf("inputs = %v", in)
	}
	var p payload
	if err := json.Unmarshal([]byte(in["payload"]), &p); err != nil {
		e.t.Fatal(err)
	}
	if !slices.Equal(p.Argv, []string{"go", "test", "./..."}) || p.Env["CGO_ENABLED"] != "0" || p.Env["GOFLAGS"] != "-mod=readonly" ||
		!slices.Equal(p.Setup, []string{"go version"}) || p.Versions["go"] != "go version" {
		e.t.Errorf("payload = %+v", p)
	}
}

func TestWorkerRowIsReusedAndSeen(t *testing.T) {
	e := newEnv(t)
	for range 2 {
		e.newJob()
		e.finishesWith(domain.Job{}, "success", nil)
		e.poll()
	}
	ws, err := e.st.Workers().List(t.Context(), e.account.ID)
	if err != nil || len(ws) != 1 || ws[0].Name != WorkerName || ws[0].Pool != domain.PoolActions {
		t.Errorf("workers = %+v, %v", ws, err)
	}
	w, err := New(Config{Store: e.st, Jobs: e.jobs, GitHub: e.gh.Client}).worker(t.Context(), e.account.ID)
	if err != nil || w.ID != ws[0].ID {
		t.Errorf("a fresh provider did not find the row: %+v, %v", w, err)
	}
	if got, _ := e.st.Workers().Get(t.Context(), e.account.ID, w.ID); !got.Online(e.clock.Now()) {
		t.Error("the worker row is not recorded as seen, so job_status would say no worker is online")
	}
}

// The provider serves the row it created, nothing else under its name.
func TestWorkerRowIsNotAdopted(t *testing.T) {
	tests := []struct {
		name string
		row  domain.Worker
		want string
	}{
		{"made by an admin", domain.Worker{Pool: domain.PoolActions, MaxJobs: 1}, "the provider did not create it"},
		{"revoked", domain.Worker{Pool: domain.PoolActions, Version: providerVersion, RevokedAt: time.Now()}, "is revoked"},
		{"moved to another pool", domain.Worker{Pool: domain.PoolBuild, Version: providerVersion}, "is in pool build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.row.AccountID, tt.row.Name, tt.row.TokenHash = e.account.ID, WorkerName, []byte("issued")
			if err := e.st.Workers().Create(t.Context(), &tt.row); err != nil {
				t.Fatal(err)
			}
			j := e.newJob()
			e.poll()
			if _, err := e.provider.worker(t.Context(), e.account.ID); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if got := e.job(j); got.Status != domain.JobQueued || len(e.gh.Dispatches()) != 0 {
				t.Errorf("job = %s, dispatches %d; want it left queued", got.Status, len(e.gh.Dispatches()))
			}
		})
	}
}

// A worker row that LeaseNext reads from another pool than the provider
// expects never gets a job run.
func TestLeasedJobOfAnotherPoolIsGivenBack(t *testing.T) {
	e := newEnv(t)
	j := e.newJob()
	build := leaseOfPool{Store: e.st, pool: domain.PoolBuild}
	e.provider = New(Config{Store: build, Jobs: e.jobs, GitHub: e.gh.Client, Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	e.poll()
	if len(e.gh.Dispatches()) != 0 {
		t.Error("a job of another pool was dispatched")
	}
	if !strings.Contains(e.logs.String(), "leased a job of another pool") {
		t.Errorf("logs: %s", e.logs)
	}
	_ = j
}

// leaseOfPool is a store whose LeaseNext returns the job with another pool,
// as a store reading a stale worker row would.
type leaseOfPool struct {
	store.Store
	pool string
}

func (l leaseOfPool) Jobs() store.JobRepo { return leaseRepo{l.Store.Jobs(), l.pool} }

type leaseRepo struct {
	store.JobRepo
	pool string
}

func (l leaseRepo) LeaseNext(ctx context.Context, a, w uuid.UUID, labels []string, d time.Duration) (domain.Job, error) {
	j, err := l.JobRepo.LeaseNext(ctx, a, w, labels, d)
	j.Pool = l.pool
	return j, err
}

// Failures of the dispatch itself.
func TestDispatchFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string // in the error of a failed job; empty: the job is queued again
	}{
		{"workflow not installed", 404, "forecrew-matrix.yml is not installed on agent/task-1 of acme/app"},
		{"refused", 422, "GitHub refused to dispatch forecrew-matrix.yml"},
		{"forbidden", 403, "may not run workflows"},
		{"server error", 500, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			j := e.newJob()
			e.gh.DispatchStatus = tt.status
			e.poll()
			got := e.job(j)
			if tt.want == "" {
				if got.Status != domain.JobQueued || got.Attempts != 1 {
					t.Errorf("job = %s attempts %d error %q, want queued again", got.Status, got.Attempts, got.Error)
				}
				return
			}
			if got.Status != domain.JobFailed || !strings.Contains(got.Error, tt.want) {
				t.Errorf("job = %s error %q, want failed with %q", got.Status, got.Error, tt.want)
			}
		})
	}
}

func TestJobsThatCannotRunOnActionsFail(t *testing.T) {
	tests := []struct {
		name string
		edit func(s *domain.JobSpec)
		want string
	}{
		{"services", func(s *domain.JobSpec) {
			s.Environment.Services = map[string]domain.Service{"db": {Image: "postgres:18"}}
		}, "services are not supported"},
		{"input too large", func(s *domain.JobSpec) { s.Command.Env = map[string]string{"BIG": strings.Repeat("x", 70_000)} }, "over the 60000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			j := e.newJob(withSpec(tt.edit))
			e.poll()
			if got := e.job(j); got.Status != domain.JobFailed || !strings.Contains(got.Error, tt.want) {
				t.Errorf("job = %s error %q, want %q", got.Status, got.Error, tt.want)
			}
			if len(e.gh.Dispatches()) != 0 {
				t.Error("an unrunnable job was dispatched")
			}
		})
	}
}

// The run ends without the command having run, or without a usable result.
func TestRunWithoutResult(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		files      map[string]string
		attempts   int
		status     domain.JobStatus
		errHas     string
		suiteHas   string // an errored test run when set
	}{
		{"failed before the command, first attempt", "failure", nil, 0, domain.JobQueued, "", ""},
		{"failed before the command, second attempt", "failure", nil, 1, domain.JobFailed, "failure before the command ran", ""},
		{"cancelled on GitHub", "cancelled", nil, 1, domain.JobFailed, "cancelled before the command ran", ""},
		{"no artifact but success", "success", nil, 0, domain.JobSucceeded, "", "the run uploaded no artifact"},
		{"artifact without result", "success", map[string]string{"x.txt": "x"}, 0, domain.JobSucceeded, "", "holds no forecrew-out/result.json"},
		{"artifact without result of a failed run", "failure", map[string]string{"x.txt": "x"}, 1, domain.JobFailed, "before the command ran", ""},
		{"result is not JSON", "success", map[string]string{ResultFile: "{"}, 0, domain.JobSucceeded, "", "is not valid"},
		{"unknown phase", "success", map[string]string{ResultFile: `{"phase":"x"}`}, 0, domain.JobSucceeded, "", `unknown phase "x"`},
		{"setup failed", "failure", map[string]string{ResultFile: `{"phase":"setup","message":"setup command failed: make"}`}, 0, domain.JobFailed, "environment setup failed: setup command failed: make", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			j := e.newJob(func(j *domain.Job) { j.Attempts = tt.attempts })
			e.finishesWith(j, tt.conclusion, tt.files)
			e.poll()
			got := e.job(j)
			if got.Status != tt.status || !strings.Contains(got.Error, tt.errHas) {
				t.Fatalf("job = %s attempts %d error %q", got.Status, got.Attempts, got.Error)
			}
			runs := e.testRuns(j)
			if tt.suiteHas == "" {
				if len(runs) != 0 {
					t.Errorf("runs = %+v", runs)
				}
				return
			}
			if len(runs) != 1 || runs[0].Status != domain.TestRunErrored || runs[0].SHA != head {
				t.Fatalf("runs = %+v", runs)
			}
			if r := e.results(runs[0]); len(r) != 1 || !strings.Contains(r[0], tt.suiteHas) {
				t.Errorf("results = %q, want %q", r, tt.suiteHas)
			}
		})
	}
}

func TestArtifactProblemsAreErroredSuites(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		e := newEnv(t)
		j := e.newJob()
		e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun {
			return &testkit.ActionsRun{Conclusion: "success", HeadSHA: head, Artifact: []byte("zip"), ArtifactName: ArtifactPrefix + j.ID.String(), Expired: true}
		}
		e.poll()
		if runs := e.testRuns(j); len(runs) != 1 || e.results(runs[0])[0] != "forecrew (report) errored the artifact of the run expired" {
			t.Errorf("runs = %+v", runs)
		}
	})
	t.Run("not a zip", func(t *testing.T) {
		e := newEnv(t)
		j := e.newJob()
		e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun {
			return &testkit.ActionsRun{Conclusion: "success", HeadSHA: head, Artifact: []byte("not a zip"), ArtifactName: ArtifactPrefix + j.ID.String()}
		}
		e.poll()
		if runs := e.testRuns(j); len(runs) != 1 || !strings.Contains(e.results(runs[0])[0], "unusable artifact") {
			t.Errorf("runs = %+v", runs)
		}
	})
}

func TestReportProblems(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		exit   int
		status domain.TestRunStatus
		counts domain.TestCounts
		last   string
	}{
		{"no report file", map[string]string{}, 0, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "no report file matched reports/*.xml"},
		{"report is not JUnit", map[string]string{"reports/a.xml": "<nope/>"}, 0, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "root element is <nope>"},
		{"exit code without failing test", map[string]string{"reports/a.xml": `<testsuite><testcase name="a"/></testsuite>`}, 2, domain.TestRunFailed,
			domain.TestCounts{Passed: 1, Errored: 1}, "the command exited with code 2 although no test failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			j := e.newJob()
			files := map[string]string{ResultFile: resultJSON(tt.exit)}
			maps.Copy(files, tt.files)
			e.finishesWith(j, "success", files)
			e.poll()
			runs := e.testRuns(j)
			if got := e.job(j); got.Status != domain.JobSucceeded || len(runs) != 1 {
				t.Fatalf("job = %s %q runs %+v", got.Status, got.Error, runs)
			}
			r := runs[0]
			rs := e.results(r)
			if r.Status != tt.status || r.Counts != tt.counts || !strings.Contains(rs[len(rs)-1], tt.last) {
				t.Errorf("run = %+v results %q", r, rs)
			}
		})
	}
}

func TestWithoutEvidenceStorage(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Evidence = nil })
	j := e.newJob()
	e.finishesWith(j, "failure", map[string]string{ResultFile: resultJSON(1), "reports/unit.xml": reportXML})
	e.poll()
	runs := e.testRuns(j)
	if len(runs) != 1 || runs[0].Counts.Failed != 1 || len(runs[0].ReportArtifacts) != 0 {
		t.Errorf("runs = %+v", runs)
	}
}

func TestEvidenceFailureRetries(t *testing.T) {
	e := newEnv(t)
	j := e.newJob()
	e.ev.PutErr = errTest
	e.finishesWith(j, "failure", map[string]string{ResultFile: resultJSON(1), "reports/unit.xml": reportXML})
	e.poll()
	if got := e.job(j); got.Status != domain.JobQueued || got.Attempts != 1 {
		t.Errorf("job = %s attempts %d", got.Status, got.Attempts)
	}
}

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "storage down" }

// Without a run id in the dispatch answer the run is found by its title.
func TestRunFoundByCorrelation(t *testing.T) {
	e := newEnv(t)
	e.gh.NoRunDetails = true
	j := e.newJob()
	e.finishesWith(j, "success", map[string]string{ResultFile: resultJSON(0), "reports/unit.xml": `<testsuite><testcase name="a"/></testsuite>`})
	e.poll()
	if got := e.job(j); got.Status != domain.JobSucceeded {
		t.Fatalf("job = %s %q", got.Status, got.Error)
	}
	if runs := e.testRuns(j); len(runs) != 1 || runs[0].Status != domain.TestRunPassed {
		t.Errorf("runs = %+v", runs)
	}
}

func TestRunNotFoundIsRetried(t *testing.T) {
	e := newEnv(t)
	e.gh.NoRunDetails = true
	e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return nil }
	j := e.newJob()
	e.poll()
	if got := e.job(j); got.Status != domain.JobQueued || got.Attempts != 1 {
		t.Errorf("job = %s attempts %d", got.Status, got.Attempts)
	}
}

func TestCancelCancelsTheRun(t *testing.T) {
	e := newEnv(t)
	j := e.newJob()
	e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return &testkit.ActionsRun{Polls: 1 << 20} }
	e.provider.Poll(t.Context())
	waitFor(t, "the dispatch", func() bool { return len(e.gh.Dispatches()) == 1 })
	if _, err := e.jobs.Cancel(t.Context(), system, e.account.ID, j.ID); err != nil {
		t.Fatal(err)
	}
	e.provider.Wait()
	if c := e.gh.Canceled(); len(c) != 1 {
		t.Errorf("canceled runs = %v", c)
	}
	if got := e.job(j); got.Status != domain.JobCanceled {
		t.Errorf("job = %s", got.Status)
	}
}

func TestShutdownCancelsTheRun(t *testing.T) {
	e := newEnv(t)
	j := e.newJob()
	e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return &testkit.ActionsRun{Polls: 1 << 20} }
	ctx, cancel := contextWithCancel(t)
	e.provider.Poll(ctx)
	waitFor(t, "the run to be followed", func() bool {
		evs, _ := e.st.JobEvents().List(t.Context(), e.account.ID, j.ID, 0, 100)
		return slices.ContainsFunc(evs, func(ev domain.JobEvent) bool { return ev.Text == "running" })
	})
	cancel()
	e.provider.Wait()
	if c := e.gh.Canceled(); len(c) != 1 {
		t.Errorf("canceled runs = %v", c)
	}
	if got := e.job(j); got.Status != domain.JobRunning {
		t.Errorf("job = %s, want it left to its lease", got.Status)
	}
}

func TestTimeout(t *testing.T) {
	e := newEnv(t)
	j := e.newJob(withSpec(func(s *domain.JobSpec) { s.Timeout = domain.Duration(30 * time.Millisecond) }))
	e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun {
		e.clock.Advance(time.Hour) // the run takes longer than the timeout
		return &testkit.ActionsRun{HeadSHA: head, Polls: 1 << 20}
	}
	e.poll()
	got := e.job(j)
	if got.Status != domain.JobFailed || !strings.Contains(got.Error, "did not finish within the timeout") {
		t.Fatalf("job = %s %q", got.Status, got.Error)
	}
	if c := e.gh.Canceled(); len(c) != 1 {
		t.Errorf("canceled runs = %v", c)
	}
	if runs := e.testRuns(j); len(runs) != 1 || runs[0].Status != domain.TestRunErrored || runs[0].ExitCode != -1 {
		t.Errorf("runs = %+v", runs)
	}
}

func TestRunReadFailures(t *testing.T) {
	t.Run("run vanished", func(t *testing.T) {
		e := newEnv(t)
		j := e.newJob()
		e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return &testkit.ActionsRun{} }
		e.gh.GetRunStatus = 404
		e.poll()
		if got := e.job(j); got.Status != domain.JobQueued || got.Attempts != 1 {
			t.Errorf("job = %s attempts %d", got.Status, got.Attempts)
		}
	})
	t.Run("GitHub keeps failing", func(t *testing.T) {
		e := newEnv(t)
		j := e.newJob()
		e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return &testkit.ActionsRun{} }
		e.gh.GetRunStatus = 500
		e.poll()
		if got := e.job(j); got.Status != domain.JobQueued || got.Attempts != 1 {
			t.Errorf("job = %s attempts %d", got.Status, got.Attempts)
		}
	})
}

func TestMaxInFlight(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxInFlight = 1 })
	a, b := e.newJob(), e.newJob()
	e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return &testkit.ActionsRun{Polls: 1 << 20} }
	e.provider.Poll(t.Context())
	waitFor(t, "the first dispatch", func() bool { return len(e.gh.Dispatches()) == 1 })
	e.provider.Poll(t.Context()) // full: nothing more is leased
	if got := e.job(b); got.Status != domain.JobQueued {
		t.Errorf("second job = %s, want queued while the first runs", got.Status)
	}
	for _, j := range []domain.Job{a, b} {
		if _, err := e.jobs.Cancel(t.Context(), system, e.account.ID, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	e.provider.Wait()
}

func TestRunLoopAndNotify(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := contextWithCancel(t)
	done := make(chan struct{})
	go func() { e.provider.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	e.finishesWith(domain.Job{}, "success", map[string]string{ResultFile: resultJSON(0), "reports/a.xml": `<testsuite><testcase name="a"/></testsuite>`})
	j := e.newJob() // the wake-up of job creation starts the run
	waitFor(t, "the job to end", func() bool { return e.job(j).Status.Terminal() })
	if got := e.job(j); got.Status != domain.JobSucceeded {
		t.Errorf("job = %s %q", got.Status, got.Error)
	}
}

// A workflow_run webhook makes the provider read the run at once.
func TestNotifyWakesTheRun(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PollInterval = time.Hour })
	j := e.newJob()
	e.finishesWith(j, "success", map[string]string{ResultFile: resultJSON(0), "reports/a.xml": `<testsuite><testcase name="a"/></testsuite>`})
	e.provider.Poll(t.Context())
	waitFor(t, "the first read of the run", func() bool { return len(e.gh.Dispatches()) == 1 })
	e.provider.Notify(12345) // an unknown run is ignored
	for !e.job(j).Status.Terminal() {
		e.provider.Notify(1001) // the first run of the fake
		time.Sleep(time.Millisecond)
	}
	e.provider.Wait()
	if got := e.job(j); got.Status != domain.JobSucceeded {
		t.Errorf("job = %s %q", got.Status, got.Error)
	}
}

// The commit a job tested is the one the provider expects, not the one the
// repo's workflow claims.
func TestReportedCommitIsChecked(t *testing.T) {
	const asked = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name     string
		edit     func(s *domain.JobSpec)
		claimed  string
		wantSHA  string
		wantText string
	}{
		{"branch, honest", nil, head, head, ""},
		{"branch, claims another commit", nil, asked, head, "tested commit"},
		{"branch, claims none", nil, "", head, "tested commit"},
		{"commit, honest", func(s *domain.JobSpec) { s.Branch, s.BaseRef = "", asked }, asked, asked, ""},
		{"commit, claims the run's head", func(s *domain.JobSpec) { s.Branch, s.BaseRef = "", asked }, head, asked, "not the requested " + asked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			var opts []jobOpts
			if tt.edit != nil {
				opts = append(opts, withSpec(tt.edit))
			}
			j := e.newJob(opts...)
			res := strings.Replace(resultJSON(0), head, tt.claimed, 1)
			e.finishesWith(j, "success", map[string]string{ResultFile: res, "reports/a.xml": `<testsuite><testcase name="a"/></testsuite>`})
			e.poll()
			got := e.job(j)
			runs := e.testRuns(j)
			if got.Status != domain.JobSucceeded || got.HeadSHA != tt.wantSHA || len(runs) != 1 || runs[0].SHA != tt.wantSHA {
				t.Fatalf("job %s head %q runs %+v", got.Status, got.HeadSHA, runs)
			}
			if tt.wantText == "" {
				if runs[0].Status != domain.TestRunPassed {
					t.Errorf("run = %+v", runs[0])
				}
				return
			}
			if rs := e.results(runs[0]); runs[0].Status != domain.TestRunErrored || len(rs) != 1 || !strings.Contains(rs[0], tt.wantText) {
				t.Errorf("run = %+v results %q", runs[0], rs)
			}
		})
	}
}

// Errored and timed-out runs of a job that asked for a commit land on it.
func TestCommitJobsKeepTheirCommit(t *testing.T) {
	const asked = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commit := withSpec(func(s *domain.JobSpec) { s.Branch, s.BaseRef = "", asked })
	t.Run("no artifact", func(t *testing.T) {
		e := newEnv(t)
		j := e.newJob(commit)
		e.finishesWith(j, "success", nil)
		e.poll()
		if runs := e.testRuns(j); len(runs) != 1 || runs[0].SHA != asked || e.job(j).HeadSHA != asked {
			t.Errorf("runs = %+v", runs)
		}
		if ds := e.gh.Dispatches(); len(ds) != 1 || ds[0].Ref != "main" || ds[0].Inputs["ref"] != asked {
			t.Errorf("dispatches = %+v", ds)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		e := newEnv(t)
		j := e.newJob(commit, withSpec(func(s *domain.JobSpec) { s.Timeout = domain.Duration(time.Minute) }))
		e.gh.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun {
			e.clock.Advance(time.Hour)
			return &testkit.ActionsRun{HeadSHA: head, Polls: 1 << 20}
		}
		e.poll()
		if runs := e.testRuns(j); len(runs) != 1 || runs[0].SHA != asked || e.job(j).HeadSHA != asked {
			t.Errorf("runs = %+v", runs)
		}
	})
}

func TestWorkflowMessageIsBounded(t *testing.T) {
	e := newEnv(t)
	j := e.newJob()
	res := `{"phase":"setup","message":"` + strings.Repeat("x", 100_000) + `"}`
	e.finishesWith(j, "failure", map[string]string{ResultFile: res})
	e.poll()
	got := e.job(j)
	if got.Status != domain.JobFailed || len(got.Error) > maxReason+64 || !strings.Contains(got.Error, "[truncated]") {
		t.Errorf("job = %s error of %d bytes", got.Status, len(got.Error))
	}
}

// Accounts take turns for the slots.
func TestAccountsShareTheSlots(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxInFlight = 2 })
	other := domain.Account{Name: "other"}
	if err := e.st.Accounts().Create(t.Context(), &other); err != nil {
		t.Fatal(err)
	}
	op := domain.Project{AccountID: other.ID, Name: "app", Repo: "o/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	if err := e.st.Projects().Create(t.Context(), &op); err != nil {
		t.Fatal(err)
	}
	e.newJob()
	e.newJob()
	theirs, err := e.jobs.Create(t.Context(), system, domain.Job{AccountID: other.ID, ProjectID: op.ID, Kind: domain.JobKindCommand, Spec: func() domain.JobSpec {
		s := spec()
		s.Repo = "o/app"
		return s
	}()})
	if err != nil {
		t.Fatal(err)
	}
	_, hook := e.inProgress()
	e.gh.OnDispatch = hook
	e.provider.Poll(t.Context())
	waitFor(t, "two dispatches", func() bool { return len(e.gh.Dispatches()) == 2 })
	if got, _ := e.st.Jobs().Get(t.Context(), other.ID, theirs.ID); got.Status != domain.JobRunning {
		t.Errorf("the other account's job is %s, want it to hold a slot", got.Status)
	}
	for _, a := range []uuid.UUID{e.account.ID, other.ID} {
		js, _ := e.st.Jobs().List(t.Context(), a, store.JobFilter{})
		for _, j := range js {
			_, _ = e.jobs.Cancel(t.Context(), system, a, j.ID)
		}
	}
	e.provider.Wait()
}

// inProgress returns a hook for a run that stays in progress until moved on,
// and the count of dispatches it saw.
func (e *env) inProgress() (*atomic.Int64, func(testkit.Dispatch) *testkit.ActionsRun) {
	n := new(atomic.Int64)
	return n, func(testkit.Dispatch) *testkit.ActionsRun {
		n.Add(1)
		return &testkit.ActionsRun{HeadSHA: head, Polls: 1 << 20}
	}
}
