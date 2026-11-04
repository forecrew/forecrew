package actionsprovider

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const repo = "acme/app"

var system = domain.Actor{Kind: domain.ActorSystem, Name: "test"}

// syncBuffer is a strings.Builder safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t        *testing.T
	st       *testkit.Store
	account  domain.Account
	project  domain.Project
	jobs     *jobs.Service
	gh       *testkit.FakeActions
	ev       *testkit.Evidence
	logs     *syncBuffer
	provider *Provider
}

type option func(*Config)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	if err := st.Accounts().Create(t.Context(), &acc); err != nil {
		t.Fatal(err)
	}
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: repo, DefaultBranch: "main", Status: domain.ProjectOnboarded}
	if err := st.Projects().Create(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &env{t: t, st: st, account: acc, project: p, gh: testkit.NewFakeActions(t, nil), ev: testkit.NewEvidence(), logs: logs}
	e.jobs = jobs.New(st, testkit.NewSignalBus(st), config.DefaultPools(), logger, func() time.Time { return time.Now().UTC() })
	cfg := Config{
		Store: st, Jobs: e.jobs, GitHub: e.gh.Client, Evidence: e.ev, Logger: logger,
		PollInterval: 5 * time.Millisecond, FindDelay: time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}
	e.provider = New(cfg)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", logs)
		}
	})
	return e
}

// jobOpts tweaks the command job newJob creates.
type jobOpts func(*domain.Job)

func withSpec(f func(s *domain.JobSpec)) jobOpts { return func(j *domain.Job) { f(&j.Spec) } }

// newJob creates a command job of pool actions that runs the unit suite.
func (e *env) newJob(opts ...jobOpts) domain.Job {
	e.t.Helper()
	j := domain.Job{
		AccountID: e.account.ID, ProjectID: e.project.ID, Kind: domain.JobKindCommand,
		Spec: domain.JobSpec{
			Kind: domain.JobKindCommand, Repo: repo, BaseRef: "main", Branch: "agent/task-1", Suite: "unit", Cell: map[string]string{"os": "linux"},
			Timeout: domain.Duration(time.Minute),
			Command: &domain.CommandSpec{Argv: []string{"go", "test", "./..."}, Env: map[string]string{"CGO_ENABLED": "0"}, Reports: []string{"reports/*.xml"}, Artifacts: []string{"coverage.out"}},
			Environment: &domain.Environment{Name: "linux-go", Provider: domain.ProviderActions, RunsOn: "ubuntu-24.04",
				Setup: []string{"go version"}, Env: map[string]string{"GOFLAGS": "-mod=readonly"}, Versions: map[string]string{"go": "go version"}},
		},
	}
	for _, o := range opts {
		o(&j)
	}
	j, err := e.jobs.Create(e.t.Context(), system, j)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) job(id domain.Job) domain.Job {
	e.t.Helper()
	j, err := e.st.Jobs().Get(e.t.Context(), e.account.ID, id.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// poll leases what is queued and waits for the runs to end.
func (e *env) poll() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 20*time.Second)
	defer cancel()
	e.provider.Poll(ctx)
	e.provider.Wait()
}

func (e *env) testRuns(j domain.Job) []domain.TestRun {
	e.t.Helper()
	runs, err := e.st.TestRuns().List(e.t.Context(), e.account.ID, store.TestRunFilter{JobID: j.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	return runs
}

func (e *env) results(r domain.TestRun) []string {
	e.t.Helper()
	rs, err := e.st.TestRuns().Results(e.t.Context(), e.account.ID, r.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, x := range rs {
		out = append(out, x.Classname+" "+x.Name+" "+string(x.Status)+" "+x.Message)
	}
	return out
}

const (
	head = "0123456789abcdef0123456789abcdef01234567"

	reportXML = `<?xml version="1.0"?>
<testsuites><testsuite name="app" tests="3">
<testcase classname="app/auth" name="TestLogin" time="0.012"/>
<testcase classname="app/auth" name="TestLogout" time="0.030"><failure message="want 204, got 500"/></testcase>
<testcase classname="app/auth" name="TestRefresh" time="0.002"><skipped message="needs redis"/></testcase>
</testsuite></testsuites>`
)

// resultJSON is the result.json of a command that ran and exited with code.
func resultJSON(code int) string {
	b, _ := json.Marshal(map[string]any{
		"phase": "command", "head_sha": head, "exit_code": code, "duration_ms": 1500, "versions": map[string]string{"go": "go version go1.27"},
		"image_os": "ubuntu24", "image_version": "20260301.1.0", "runner_os": "Linux", "runner_arch": "X64",
	})
	return string(b)
}

// finishesWith makes the dispatched run end with the artifact of files.
func (e *env) finishesWith(_ domain.Job, conclusion string, files map[string]string) {
	e.gh.OnDispatch = func(d testkit.Dispatch) *testkit.ActionsRun {
		run := &testkit.ActionsRun{Conclusion: conclusion, HeadSHA: "feed" + head[4:], Polls: 1}
		if files != nil {
			run.Artifact, run.ArtifactName = testkit.Zip(files), ArtifactPrefix+d.Inputs["job_id"]
		}
		return run
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func context_WithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}
