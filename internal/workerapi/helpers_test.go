package workerapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// backend is the store the tests run on (see newBackend).
type backend struct {
	st      store.Store
	clock   *testkit.Clock // clock of the job service (and of the fake store)
	account uuid.UUID
	// expireLeases makes every running lease expire.
	expireLeases func(t *testing.T)
}

const (
	repo      = "acme/app"
	apiSecret = "sk-very-secret-value"
)

var system = domain.Actor{Kind: domain.ActorSystem, Name: "test"}

type env struct {
	backend
	bus     *testkit.SignalBus
	tracker *testkit.Tracker
	jobs    *jobs.Service
	server  *Server
	http    *httptest.Server
	logs    *syncBuffer
	project domain.Project
}

type option func(*Config)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	b := newBackend(t)
	ctx := t.Context()
	p := domain.Project{AccountID: b.account, Name: "app", Repo: repo, DefaultBranch: "main", Status: domain.ProjectOnboarded}
	if err := b.st.Projects().Create(ctx, &p); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := testkit.NewSignalBus(b.st)
	tracker := testkit.NewTracker(b.clock.Now)
	tracker.Install(repo)
	svc := jobs.New(b.st, bus, nil, logger, b.clock.Now)
	cfg := Config{
		Store: b.st, Jobs: svc, Tokens: tracker, Logger: logger,
		Env: func(domain.Job) (string, map[string]string) {
			return "claude", map[string]string{"ANTHROPIC_API_KEY": apiSecret}
		},
		PollWait:    200 * time.Millisecond,
		PollRecheck: time.Hour, // wake-ups must come from job creation
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv := New(cfg)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &env{backend: b, bus: bus, tracker: tracker, jobs: svc, server: srv, http: hs, logs: logs, project: p}
}

// addWorker registers a worker of pool build and returns it with its token.
func (e *env) addWorker(t *testing.T, name string) (domain.Worker, string) {
	t.Helper()
	return e.addPoolWorker(t, name, domain.PoolBuild)
}

// addPoolWorker registers a worker of a pool and returns it with its token.
func (e *env) addPoolWorker(t *testing.T, name, pool string) (domain.Worker, string) {
	t.Helper()
	token := "fcw_" + name + "_" + domain.NewID().String()
	w := domain.Worker{AccountID: e.account, Name: name, Pool: pool, TokenHash: mcp.HashToken(token), MaxJobs: 1}
	if err := e.st.Workers().Create(t.Context(), &w); err != nil {
		t.Fatal(err)
	}
	return w, token
}

type jobOpts struct {
	needs      []string
	writePaths []string
	schema     string
	branch     string
}

func (e *env) newJob(t *testing.T, o jobOpts) domain.Job {
	t.Helper()
	spec := domain.JobSpec{Repo: repo, BaseRef: "main", Branch: o.branch, WritePaths: o.writePaths, Model: "m", Prompt: "do it"}
	if o.schema != "" {
		spec.OutputSchema = json.RawMessage(o.schema)
	}
	j, err := e.jobs.Create(t.Context(), system, domain.Job{
		AccountID: e.account, ProjectID: e.project.ID, Role: "developer", Runtime: "claude-code", Needs: o.needs, Spec: spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// call sends a request with a bearer token and returns the status and body.
func (e *env) call(t *testing.T, token, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = bytes.NewBufferString(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.http.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

// poll polls with free slots and the claude-code runtime label plus labels.
func (e *env) poll(t *testing.T, token string, labels ...string) (int, pollResponse) {
	t.Helper()
	status, body := e.call(t, token, "/worker/v1/poll", map[string]any{
		"labels": append(labels, "runtime:claude-code"), "free_slots": 1, "version": "v0.1.0",
	})
	var out pollResponse
	if status == http.StatusOK {
		decodeBody(t, body, &out)
	}
	return status, out
}

func jobPath(id uuid.UUID, action string) string {
	return "/worker/v1/jobs/" + id.String() + "/" + action
}

func decodeBody(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// errorCode returns the error code of an error response.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error apiError `json:"error"`
	}
	decodeBody(t, body, &e)
	return e.Error.Code
}

func wantStatus(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d: %s", got, want, body)
	}
}

func (e *env) job(t *testing.T, id uuid.UUID) domain.Job {
	t.Helper()
	j, err := e.st.Jobs().Get(t.Context(), e.account, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// completedSignals returns the statuses of the job.completed signals of a job.
func (e *env) completedSignals(t *testing.T, id uuid.UUID) []domain.JobStatus {
	t.Helper()
	sigs, err := e.bus.Lookup(t.Context(), e.account, domain.JobKey(id))
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.JobStatus
	for _, s := range sigs {
		p, err := s.Decode()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.(*domain.JobCompleted).Status)
	}
	return out
}

// syncBuffer is a bytes.Buffer safe for concurrent writers and readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
