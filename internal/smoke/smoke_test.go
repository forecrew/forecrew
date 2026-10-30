package smoke

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fakeAPI is a forecrewd that onboards and delivers when its gates are
// approved.
type fakeAPI struct {
	mu          sync.Mutex
	workers     []mcp.WorkerInfo
	project     *domain.Project
	profileGate domain.GateStatus
	goalStatus  domain.GoalStatus
	goalGate    domain.GateKind // pending kind of the goal's gate, "" for none
	taskState   domain.ChangeState
	approved    []string
	jobs        []domain.Job
	// What the fake GitHub says.
	unmerged  bool // every PR is open
	noReviews bool // the task PR has no review
	noProfile bool // no merged PR from agent/onboarding
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{workers: []mcp.WorkerInfo{{Online: true}}, taskState: domain.ChangeMerged, goalGate: domain.GateSpecApproval}
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	write := func(w http.ResponseWriter, status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/workers", func(w http.ResponseWriter, _ *http.Request) { write(w, 200, f.workers) })
	mux.HandleFunc("GET /api/v1/projects/app", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.project == nil {
			write(w, 404, map[string]string{"error": "no"})
			return
		}
		write(w, 200, f.project)
	})
	mux.HandleFunc("POST /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.project, f.profileGate = &domain.Project{Name: "app", Status: domain.ProjectOnboarding}, domain.GatePending
		write(w, 202, mcp.OnboardProjectOutput{})
	})
	mux.HandleFunc("GET /api/v1/gates", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		gates := []mcp.GateInfo{}
		if f.profileGate == domain.GatePending {
			gates = append(gates, mcp.GateInfo{Ref: "GATE-1", Gate: domain.Gate{Kind: domain.GateProfileApproval, SubjectNumber: 7}})
		}
		write(w, 200, gates)
	})
	mux.HandleFunc("POST /api/v1/gates/{gate}/approve", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.approved = append(f.approved, r.PathValue("gate"))
		switch r.PathValue("gate") {
		case "GATE-1":
			f.profileGate, f.project.Status = domain.GateApproved, domain.ProjectOnboarded
		case "GATE-2":
			f.goalGate = domain.GateMergeApproval
		case "GATE-3":
			f.goalStatus = domain.GoalDone
		}
		write(w, 200, domain.Gate{})
	})
	mux.HandleFunc("POST /api/v1/goals", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.goalStatus == "" {
			f.goalStatus = domain.GoalInProgress
		}
		write(w, 201, domain.Goal{Number: 1, Status: domain.GoalDraft})
	})
	mux.HandleFunc("GET /api/v1/goals/GOAL-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		gate := mcp.GateInfo{Ref: "GATE-2", Gate: domain.Gate{Kind: f.goalGate, Status: domain.GatePending}}
		if f.goalGate == domain.GateMergeApproval {
			gate.Ref = "GATE-3"
		}
		if f.goalStatus == domain.GoalDone {
			gate.Status = domain.GateApproved
		}
		write(w, 200, mcp.GoalDetail{
			Goal: domain.Goal{Number: 1, Status: f.goalStatus}, Gates: []mcp.GateInfo{gate}, CostUSD: 1.5,
			Tasks: []mcp.TaskDetail{{Ref: "TASK-1", Changes: []mcp.ChangeInfo{{Change: domain.Change{Repo: "acme/sandbox", Number: 9, State: f.taskState}}}}},
		})
	})
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, _ *http.Request) { write(w, 200, f.jobs) })
	return mux
}

// github answers like the GitHub API for the PRs of acme/app.
func (f *fakeAPI) github(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("Authorization") != "Bearer ghp":
		w.WriteHeader(http.StatusUnauthorized)
	case strings.HasSuffix(r.URL.Path, "/reviews"):
		if f.noReviews {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"state":"COMMENTED"}]`))
	case strings.HasSuffix(r.URL.Path, "/pulls"):
		if r.URL.Query().Get("head") != "acme:agent/onboarding" || r.URL.Query().Get("state") != "closed" || f.noProfile {
			_, _ = w.Write([]byte(`[{"number":3,"merged_at":null}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"number":7,"merged_at":"2026-03-01T12:00:00Z"}]`))
	default:
		_, _ = fmt.Fprintf(w, `{"merged":%v}`, !f.unmerged)
	}
}

func (f *fakeAPI) config(t *testing.T) Config {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	gh := httptest.NewServer(http.HandlerFunc(f.github))
	t.Cleanup(gh.Close)
	return Config{
		ServerURL: srv.URL + "/", Token: "tok", Repo: "acme/app", GitHubToken: "ghp", GitHubAPI: gh.URL,
		Poll: time.Millisecond, Timeout: 5 * time.Second,
	}
}

func job(num int64, role string, status domain.JobStatus, cost float64) domain.Job {
	return domain.Job{Number: num, Role: role, Status: status, CostUSD: cost, StartedAt: t0, FinishedAt: t0.Add(90 * time.Second)}
}

func TestRunPasses(t *testing.T) {
	f := newFakeAPI()
	f.jobs = []domain.Job{job(2, "reviewer", domain.JobSucceeded, 0.25), job(1, "developer", domain.JobSucceeded, 0.5)}
	cfg := f.config(t)

	rep, err := Run(t.Context(), cfg)
	if err != nil || !rep.Passed() {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if got := strings.Join(f.approved, ","); got != "GATE-1,GATE-2,GATE-3" {
		t.Errorf("approved = %s", got)
	}
	md := rep.Markdown()
	for _, want := range []string{
		"# Live smoke test: PASSED", "`acme/app`", DefaultGoal, "Total cost: $0.7500", "profile PR: ok (acme/app#7)",
		"review on the PR of TASK-1: ok", "| onboarding |", "| goal |", "| JOB-1 | developer | succeeded | 1m30s | $0.5000 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
}

func TestRunAlreadyOnboarded(t *testing.T) {
	f := newFakeAPI()
	f.project = &domain.Project{Name: "app", Status: domain.ProjectOnboarded}
	f.jobs = []domain.Job{job(1, "reviewer", domain.JobSucceeded, 0)}
	rep, err := Run(t.Context(), f.config(t))
	if err != nil || !rep.Passed() || !strings.Contains(rep.Markdown(), "profile PR: ok (acme/app#7, found on GitHub") {
		t.Fatalf("report = %s, %v", rep.Markdown(), err)
	}
	if got := strings.Join(f.approved, ","); got != "GATE-2,GATE-3" {
		t.Errorf("approved = %s", got)
	}
}

func TestRunFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *fakeAPI)
		want  string
	}{
		"no worker": {func(f *fakeAPI) { f.workers = nil }, "no worker is online"},
		"needs you": {func(f *fakeAPI) { f.goalGate = domain.GateNeedsYou }, "needs a human"},
		"canceled goal": {func(f *fakeAPI) {
			f.project = &domain.Project{Status: domain.ProjectOnboarded}
			f.goalGate = ""
			f.goalStatus = domain.GoalCanceled
		}, "was canceled"},
		"unmerged PR": {func(f *fakeAPI) {
			f.taskState = domain.ChangeOpen
			f.jobs = []domain.Job{job(1, "reviewer", domain.JobSucceeded, 0)}
		}, "task PR of TASK-1: FAILED"},
		"no review": {func(f *fakeAPI) { f.jobs = []domain.Job{job(1, "developer", domain.JobSucceeded, 0)} }, "a review job succeeded: FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAPI()
			tc.setup(f)
			rep, err := Run(t.Context(), f.config(t))
			if err != nil || rep.Passed() {
				t.Fatalf("report passed: %+v, %v", rep, err)
			}
			md := rep.Markdown()
			if !strings.Contains(md, "FAILED") || !strings.Contains(md, tc.want) {
				t.Errorf("report lacks %q:\n%s", tc.want, md)
			}
		})
	}
}

func TestRunTimeoutAndErrors(t *testing.T) {
	f := newFakeAPI()
	cfg := f.config(t)
	cfg.Timeout = 30 * time.Millisecond
	f.goalGate = domain.GateMergeApproval
	f.project = &domain.Project{Status: domain.ProjectOnboarded}
	f.goalStatus = domain.GoalInProgress
	// The goal's gate is approved but the goal never finishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/workers" {
			_, _ = w.Write([]byte(`[{"online":true}]`))
			return
		}
		if r.URL.Path == "/api/v1/projects/app" {
			_, _ = w.Write([]byte(`{"status":"onboarded"}`))
			return
		}
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"number":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"goal":{"status":"in_progress"}}`))
	}))
	defer srv.Close()
	cfg.ServerURL = srv.URL
	rep, err := Run(t.Context(), cfg)
	if err != nil || rep.Passed() || rep.Err == nil {
		t.Fatalf("report = %+v, %v", rep, err)
	}

	cfg.ServerURL = "http://127.0.0.1:1"
	if rep, err = Run(t.Context(), cfg); err != nil || rep.Passed() {
		t.Fatalf("unreachable: %+v, %v", rep, err)
	}
	if rep.Markdown() == "" || len(rep.Phases) != 1 {
		t.Errorf("phases = %+v", rep.Phases)
	}
}

func TestRunBadConfig(t *testing.T) {
	full := Config{ServerURL: "http://x", Token: "t", GitHubToken: "g", Repo: "acme/app"}
	noGitHub, noRepo, noToken := full, full, full
	noGitHub.GitHubToken, noRepo.Repo, noToken.Token = "", "app", ""
	for _, cfg := range []Config{{}, noGitHub, noRepo, noToken} {
		if _, err := Run(t.Context(), cfg); err == nil {
			t.Errorf("Run(%+v) succeeded", cfg)
		}
	}
}

func TestGitHubErrors(t *testing.T) {
	f := newFakeAPI()
	f.jobs = []domain.Job{job(1, "reviewer", domain.JobSucceeded, 0)}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer gh.Close()
	cfg := f.config(t)
	cfg.GitHubToken, cfg.GitHubAPI = "ghp", gh.URL
	rep, err := Run(t.Context(), cfg)
	if err != nil || rep.Passed() || !strings.Contains(rep.Markdown(), "profile PR merged: FAILED") {
		t.Fatalf("report = %s, %v", rep.Markdown(), err)
	}
}
