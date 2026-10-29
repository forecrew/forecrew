package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/testkit"
)

// Secrets the tests hand to the worker; no log, event or file may hold them.
const (
	testRepoToken = "ghs_0123456789abcdefghijklmnopqrstuvwxyz"
	testAPIKey    = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz"
	testWorkerTok = "fcw_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg"
)

// lockedBuffer is a bytes.Buffer safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeAPI is an httptest fake of the worker API.
type fakeAPI struct {
	srv *httptest.Server

	mu          sync.Mutex
	queue       []domain.JobSpec
	polls       []int // free_slots of every poll
	pollAnswer  func(n int) int
	events      map[uuid.UUID][]domain.JobEvent
	completions map[uuid.UUID][]Completion
	completed   chan uuid.UUID
	heartbeats  map[uuid.UUID]int
	heartbeat   func(id uuid.UUID, n int) (status int, action string)
	eventStatus int
	longPoll    time.Duration
	tokens      []string // Authorization headers seen
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	a := &fakeAPI{
		events: map[uuid.UUID][]domain.JobEvent{}, completions: map[uuid.UUID][]Completion{},
		completed: make(chan uuid.UUID, 64), heartbeats: map[uuid.UUID]int{}, longPoll: 10 * time.Millisecond,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worker/v1/poll", a.poll)
	mux.HandleFunc("POST /worker/v1/jobs/{id}/heartbeat", a.beat)
	mux.HandleFunc("POST /worker/v1/jobs/{id}/events", a.sendEvents)
	mux.HandleFunc("POST /worker/v1/jobs/{id}/complete", a.complete)
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.tokens = append(a.tokens, r.Header.Get("Authorization"))
		a.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAPI) poll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels    []string `json:"labels"`
		FreeSlots int      `json:"free_slots"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	a.polls = append(a.polls, req.FreeSlots)
	n := len(a.polls)
	answer := a.pollAnswer
	var job *domain.JobSpec
	if req.FreeSlots > 0 && len(a.queue) > 0 {
		job, a.queue = &a.queue[0], a.queue[1:]
	}
	wait := a.longPoll
	a.mu.Unlock()
	if answer != nil {
		if status := answer(n); status != 0 {
			writeErr(w, status, "x", "scripted answer")
			return
		}
	}
	if job == nil {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"job": job, "heartbeat_interval_seconds": 30})
}

func (a *fakeAPI) beat(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(r.PathValue("id"))
	a.mu.Lock()
	a.heartbeats[id]++
	n, fn := a.heartbeats[id], a.heartbeat
	a.mu.Unlock()
	status, action := http.StatusOK, "continue"
	if fn != nil {
		status, action = fn(id, n)
	}
	if status != http.StatusOK {
		writeErr(w, status, "x", "scripted answer")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"action": action})
}

func (a *fakeAPI) sendEvents(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(r.PathValue("id"))
	var body struct {
		Events []domain.JobEvent `json:"events"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	status := a.eventStatus
	if status == 0 {
		a.events[id] = append(a.events[id], body.Events...)
	}
	a.mu.Unlock()
	if status != 0 {
		writeErr(w, status, "x", "scripted answer")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *fakeAPI) complete(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(r.PathValue("id"))
	var c Completion
	_ = json.NewDecoder(r.Body).Decode(&c)
	a.mu.Lock()
	a.completions[id] = append(a.completions[id], c)
	a.mu.Unlock()
	a.completed <- id
	_ = json.NewEncoder(w).Encode(map[string]string{"status": c.Status})
}

// completion returns the only completion reported for a job.
func (a *fakeAPI) completion(t *testing.T, id uuid.UUID) Completion {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.completions[id]) != 1 {
		t.Fatalf("completions of %s = %v, want exactly one", id, a.completions[id])
	}
	return a.completions[id][0]
}

func (a *fakeAPI) completionCount(id uuid.UUID) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.completions[id])
}

func (a *fakeAPI) eventsOf(id uuid.UUID) []domain.JobEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]domain.JobEvent(nil), a.events[id]...)
}

// statuses returns the status events of a job in order.
func (a *fakeAPI) statuses(id uuid.UUID) []string {
	var out []string
	for _, e := range a.eventsOf(id) {
		if e.Type == domain.EventStatus {
			out = append(out, e.Text)
		}
	}
	return out
}

// funcAdapter is a runtime adapter implemented by a function.
type funcAdapter struct {
	fn func(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error)
}

func (funcAdapter) Name() string                           { return runtime.Scripted }
func (funcAdapter) Detect(context.Context) (string, error) { return "test", nil }
func (f funcAdapter) Run(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
	return f.fn(ctx, spec, emit)
}

// scriptAdapter returns the scripted adapter replaying sc.
func scriptAdapter(t *testing.T, sc runtime.Script) runtime.Adapter {
	t.Helper()
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime.NewScripted(path, runtime.Config{})
}

// testEnv is a worker wired to a fake API and a local bare repo.
type testEnv struct {
	t        *testing.T
	api      *fakeAPI
	remote   string
	workDir  string
	logs     *lockedBuffer
	analyzer *testkit.Analyzer
	cfg      Config
	w        *Worker
}

func newEnv(t *testing.T, adapter runtime.Adapter) *testEnv {
	t.Helper()
	e := &testEnv{t: t, api: newFakeAPI(t), remote: newRemote(t), workDir: filepath.Join(t.TempDir(), "work"),
		logs: &lockedBuffer{}, analyzer: &testkit.Analyzer{}}
	client := NewClient(e.api.srv.URL, testWorkerTok, nil)
	client.BackoffMin, client.BackoffMax = time.Millisecond, 5*time.Millisecond
	e.cfg = Config{
		Client: client, Labels: []string{"linux", "runtime:scripted"}, Version: "v-test", WorkDir: e.workDir, MaxJobs: 1,
		Runtimes: runtime.NewRegistry(adapter), Analyzer: e.analyzer,
		Logger:   slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Environ:  os.Environ(),
		CloneURL: func(string) string { return e.remote },
		// Short timings keep the tests fast.
		HeartbeatInterval: 20 * time.Millisecond, FlushInterval: 10 * time.Millisecond, GracePeriod: 2 * time.Second,
	}
	e.w = New(e.cfg)
	return e
}

// rebuild recreates the worker after cfg changed.
func (e *testEnv) rebuild() { e.w = New(e.cfg) }

// spec returns a writing job on acme/app; mod may change it.
func (e *testEnv) spec(mod ...func(*domain.JobSpec)) domain.JobSpec {
	s := domain.JobSpec{
		JobID: domain.NewID(), AccountID: domain.NewID(), Repo: "acme/app", BaseRef: "main", Branch: "agent/task-1",
		Role: "developer", WritePaths: []string{"**", "!.github/**", "!.forecrew/**"}, Runtime: runtime.Scripted, Model: "test-model",
		Prompt: "implement it", AllowedTools: []string{"Read", "Edit"}, Timeout: domain.Duration(time.Minute),
		Env: map[string]string{"ANTHROPIC_API_KEY": testAPIKey}, RepoToken: testRepoToken,
	}
	for _, m := range mod {
		m(&s)
	}
	return s
}

// do runs a job to its end in the calling goroutine.
func (e *testEnv) do(s domain.JobSpec) {
	e.t.Helper()
	e.w.handleJob(e.t.Context(), &Lease{Job: s})
}

// doComplete runs a job and returns the completion it reported.
func (e *testEnv) doComplete(s domain.JobSpec) Completion {
	e.t.Helper()
	e.do(s)
	return e.api.completion(e.t, s.JobID)
}

// noSecrets fails if a secret is in the logs, the events, the completions,
// the requests' bodies or any file of the work dir.
func (e *testEnv) noSecrets() {
	e.t.Helper()
	secrets := []string{testRepoToken, testAPIKey, authHeader(testRepoToken)}
	e.api.mu.Lock()
	b, _ := json.Marshal([]any{e.api.events, e.api.completions})
	e.api.mu.Unlock()
	for _, s := range secrets {
		if strings.Contains(e.logs.String(), s) {
			e.t.Errorf("a log line contains a secret %q...", s[:8])
		}
		if strings.Contains(string(b), s) {
			e.t.Errorf("events or completions contain a secret %q...", s[:8])
		}
	}
	scanTree(e.t, e.workDir, secrets...)
}
