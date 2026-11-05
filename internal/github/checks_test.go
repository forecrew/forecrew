package github_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

func TestCheckStatus(t *testing.T) {
	tests := []struct {
		name   string
		runs   []string // fixture pages
		status []string
		want   github.CheckState
		total  int
	}{
		{"nothing", []string{"check_runs_empty.json"}, []string{"status_empty.json"}, github.ChecksNone, 0},
		{"runs only, all green across pages", []string{"check_runs_page1.json", "check_runs_page2.json"}, []string{"status_empty.json"}, github.ChecksSuccess, 3},
		{"statuses only, green", []string{"check_runs_empty.json"}, []string{"status_success.json"}, github.ChecksSuccess, 1},
		{"run and status green", []string{"check_runs_page2.json"}, []string{"status_success.json"}, github.ChecksSuccess, 2},
		{"failed run wins over a pending one", []string{"check_runs_pending.json"}, []string{"status_empty.json"}, github.ChecksFailure, 2},
		{"failed run", []string{"check_runs_failed.json"}, []string{"status_success.json"}, github.ChecksFailure, 3},
		{"failed status", []string{"check_runs_page2.json"}, []string{"status_failure.json"}, github.ChecksFailure, 3},
		{"failed status wins over a pending run", []string{"check_runs_pending_only.json"}, []string{"status_failure.json"}, github.ChecksFailure, 3},
		{"pending run only", []string{"check_runs_pending_only.json"}, []string{"status_empty.json"}, github.ChecksPending, 1},
		{"pending status", []string{"check_runs_page2.json"}, []string{"status_pending.json"}, github.ChecksPending, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.servePages("GET /repos/acme/app/commits/4f1c2b9e/check-runs", tt.runs...)
			e.servePages("GET /repos/acme/app/commits/4f1c2b9e/status", tt.status...)
			got, err := e.client.CheckStatus(t.Context(), "acme/app", "4f1c2b9e")
			noErr(t, err)
			if got.State != tt.want || got.Total != tt.total || got.SHA != "4f1c2b9e" {
				t.Fatalf("got %+v, want %s/%d", got, tt.want, tt.total)
			}
		})
	}
}

func TestCheckStatusErrors(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/commits/missing/check-runs", http.StatusUnprocessableEntity, "error_validation.json")
	e.serve("GET /repos/acme/app/commits/runsok/check-runs", http.StatusOK, "check_runs_empty.json")
	e.serve("GET /repos/acme/app/commits/runsok/status", http.StatusNotFound, "error_not_found.json")
	ctx := t.Context()
	_, err := e.client.CheckStatus(ctx, "acme/app", "missing")
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.CheckStatus(ctx, "acme/app", "runsok")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.CheckStatus(ctx, "bad", "x")
	wantErr(t, err, github.ErrInvalid)
}

func TestSetCheckRun(t *testing.T) {
	in := github.CheckRunInput{Name: "forecrew/verification", SHA: "4f1c2b9e", Conclusion: github.ConclusionNeutral, Title: "Incomplete", Summary: "2 cells missing"}
	t.Run("creates", func(t *testing.T) {
		e := newEnv(t)
		e.handle("GET /repos/acme/app/commits/4f1c2b9e/check-runs", func(w http.ResponseWriter, r *http.Request) {
			if q := r.URL.Query(); q.Get("check_name") != "forecrew/verification" || q.Get("app_id") == "" {
				t.Errorf("query = %v", q)
			}
			_, _ = w.Write(fixture(t, "api", "check_runs_empty.json"))
		})
		var body map[string]any
		e.handle("POST /repos/acme/app/check-runs", func(w http.ResponseWriter, r *http.Request) {
			body = readJSON(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 1}`))
		})
		noErr(t, e.client.SetCheckRun(t.Context(), "acme/app", in))
		out, _ := body["output"].(map[string]any)
		if body["name"] != "forecrew/verification" || body["head_sha"] != "4f1c2b9e" || body["status"] != "completed" || body["conclusion"] != "neutral" ||
			out["title"] != "Incomplete" || out["summary"] != "2 cells missing" || body["completed_at"] == nil {
			t.Fatalf("body = %v", body)
		}
	})
	t.Run("updates its own run and cuts a long summary", func(t *testing.T) {
		e := newEnv(t)
		e.handle("GET /repos/acme/app/commits/4f1c2b9e/check-runs", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"total_count": 1, "check_runs": [{"id": 77, "name": "forecrew/verification", "status": "completed", "conclusion": "neutral"}]}`))
		})
		var body map[string]any
		e.handle("PATCH /repos/acme/app/check-runs/77", func(w http.ResponseWriter, r *http.Request) {
			body = readJSON(t, r)
			_, _ = w.Write([]byte(`{"id": 77}`))
		})
		long := in
		long.Conclusion, long.Summary = github.ConclusionSuccess, strings.Repeat("é", github.MaxCheckSummary)
		noErr(t, e.client.SetCheckRun(t.Context(), "acme/app", long))
		out, _ := body["output"].(map[string]any)
		if s, _ := out["summary"].(string); body["conclusion"] != "success" || len(s) > github.MaxCheckSummary || len(s) < github.MaxCheckSummary-1 {
			t.Fatalf("conclusion %v, summary of %d bytes", body["conclusion"], len(s))
		}
	})
	t.Run("errors", func(t *testing.T) {
		e := newEnv(t)
		e.serve("GET /repos/acme/app/commits/4f1c2b9e/check-runs", http.StatusForbidden, "error_forbidden.json")
		wantErr(t, e.client.SetCheckRun(t.Context(), "acme/app", in), github.ErrForbidden)
		e.serve("GET /repos/acme/app/commits/aaaa/check-runs", http.StatusOK, "check_runs_empty.json")
		e.serve("POST /repos/acme/app/check-runs", http.StatusForbidden, "error_forbidden.json")
		bad := in
		bad.SHA = "aaaa"
		wantErr(t, e.client.SetCheckRun(t.Context(), "acme/app", bad), github.ErrForbidden)
		e.serve("GET /repos/acme/app/commits/bbbb/check-runs", http.StatusOK, "check_runs_page2.json")
		e.serve("PATCH /repos/acme/app/check-runs/", http.StatusNotFound, "error_not_found.json")
		upd := in
		upd.SHA = "bbbb"
		if err := e.client.SetCheckRun(t.Context(), "acme/app", upd); err == nil {
			t.Fatal("update error swallowed")
		}
		wantErr(t, e.client.SetCheckRun(t.Context(), "bad", in), github.ErrInvalid)
	})
}
