package github_test

import (
	"net/http"
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
