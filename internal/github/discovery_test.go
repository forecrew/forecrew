package github_test

import (
	"net/http"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

func TestListAutolinks(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/autolinks", http.StatusOK, "autolinks.json")
	got, err := e.client.ListAutolinks(t.Context(), "acme/app")
	noErr(t, err)
	want := []github.Autolink{
		{KeyPrefix: "ENG-", URLTemplate: "https://acme.atlassian.net/browse/ENG-<num>"},
		{KeyPrefix: "LIN", URLTemplate: "https://linear.app/acme/issue/LIN-<num>"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("autolinks = %+v, want %+v", got, want)
	}
}

func TestListRecentIssuesLeavesPullRequestsOut(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "all" || q.Get("per_page") != "100" {
			t.Errorf("query = %v, want state=all and per_page=100", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "api", "issues_recent.json"))
	})
	got, err := e.client.ListRecentIssues(t.Context(), "acme/app", 100)
	noErr(t, err)
	if len(got) != 1 || got[0].Number != 12 || got[0].Body != "See https://acme.sentry.io/issues/9" {
		t.Fatalf("issues = %+v, want issue 12 only", got)
	}
}

func TestListRecentPulls(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/pulls", http.StatusOK, "pulls_recent.json")
	got, err := e.client.ListRecentPulls(t.Context(), "acme/app", 100)
	noErr(t, err)
	if len(got) != 1 || got[0].Number != 11 || got[0].Body != "Tracked in https://acme.atlassian.net/browse/ENG-9" {
		t.Fatalf("pulls = %+v, want PR 11 with its body", got)
	}
}

// TestDiscoveryForbidden: a missing permission is ErrForbidden on every read,
// so the discovery can skip it with a note.
func TestDiscoveryForbidden(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/autolinks", http.StatusForbidden, "error_forbidden.json")
	e.serve("GET /repos/acme/app/issues", http.StatusForbidden, "error_forbidden.json")
	e.serve("GET /repos/acme/app/pulls", http.StatusForbidden, "error_forbidden.json")
	_, err := e.client.ListAutolinks(t.Context(), "acme/app")
	wantErr(t, err, github.ErrForbidden)
	_, err = e.client.ListRecentIssues(t.Context(), "acme/app", 100)
	wantErr(t, err, github.ErrForbidden)
	_, err = e.client.ListRecentPulls(t.Context(), "acme/app", 100)
	wantErr(t, err, github.ErrForbidden)
}

func TestDiscoveryRepoErrors(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/autolinks", http.StatusNotFound, "error_not_found.json")
	_, err := e.client.ListAutolinks(t.Context(), "acme/app")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.ListAutolinks(t.Context(), "not-a-repo")
	wantErr(t, err, github.ErrInvalid)
}
