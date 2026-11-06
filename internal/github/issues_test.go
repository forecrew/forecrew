package github_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

func TestCreateIssue(t *testing.T) {
	e := newEnv(t)
	e.handle("POST /repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		if b["title"] != "Add login page" || b["body"] != "As a user" || fmt.Sprint(b["labels"]) != "[forecrew]" {
			t.Errorf("body = %v", b)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fixture(t, "api", "issue.json"))
	})
	got, err := e.client.CreateIssue(t.Context(), "acme/app", github.IssueInput{Title: "Add login page", Body: "As a user", Labels: []string{"forecrew"}})
	noErr(t, err)
	want := github.Issue{
		Number: 7, Title: "Add login page", Body: "As a user I want to log in.", State: "open",
		Labels: []string{"forecrew", "needs you"}, URL: "https://github.com/acme/app/issues/7",
	}
	if got.Number != want.Number || got.Title != want.Title || got.Body != want.Body || got.State != want.State ||
		got.URL != want.URL || !slices.Equal(got.Labels, want.Labels) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, a := range e.apiAuth {
		if a != "Bearer ghs_1" {
			t.Fatalf("api auth = %q", a)
		}
	}
	if len(e.tokenBodies) != 1 || e.tokenBodies[0]["permissions"] != nil {
		t.Fatalf("api token must carry the installation's permissions: %v", e.tokenBodies)
	}
}

func TestIssueErrors(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/issues/404", http.StatusNotFound, "error_not_found.json")
	e.serve("PATCH /repos/acme/app/issues/422", http.StatusUnprocessableEntity, "error_validation.json")
	e.serve("POST /repos/acme/app/issues", http.StatusUnprocessableEntity, "error_validation.json")
	e.handle("POST /repos/acme/app/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "99999999999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(fixture(t, "api", "error_rate_limit.json"))
	})
	ctx := t.Context()
	_, err := e.client.GetIssue(ctx, "acme/app", 404)
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.UpdateIssue(ctx, "acme/app", 422, github.IssueUpdate{})
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.CreateIssue(ctx, "acme/app", github.IssueInput{})
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.CreateComment(ctx, "acme/app", 7, "hi")
	wantErr(t, err, github.ErrRateLimited)

	// Repos that are malformed or not installed fail before any API call.
	_, err = e.client.GetIssue(ctx, "acme", 1)
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.GetIssue(ctx, "acme/none", 1)
	wantErr(t, err, github.ErrNotInstalled) // the mux answers 404 for the installation lookup
}

func TestUpdateIssue(t *testing.T) {
	e := newEnv(t)
	e.handle("PATCH /repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		if len(b) != 2 || b["title"] != "New" || b["state"] != "closed" {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "issue.json"))
	})
	title, state := "New", "closed"
	got, err := e.client.UpdateIssue(t.Context(), "acme/app", 7, github.IssueUpdate{Title: &title, State: &state})
	noErr(t, err)
	if got.Number != 7 {
		t.Fatalf("got %+v", got)
	}
}

func TestGetIssue(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/issues/7", http.StatusOK, "issue.json")
	got, err := e.client.GetIssue(t.Context(), "acme/app", 7)
	noErr(t, err)
	if got.Title != "Add login page" {
		t.Fatalf("got %+v", got)
	}
}

func TestAddSubIssue(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/issues/9", http.StatusOK, "issue_child.json")
	e.handle("POST /repos/acme/app/issues/7/sub_issues", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		if b["sub_issue_id"] != float64(1000009) {
			t.Errorf("body = %v (the API wants the child's ID, not its number)", b)
		}
		_, _ = w.Write(fixture(t, "api", "issue_child.json"))
	})
	noErr(t, e.client.AddSubIssue(t.Context(), "acme/app", 7, 9))
}

func TestAddSubIssueErrors(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/issues/9", http.StatusOK, "issue_child.json")
	e.serve("GET /repos/acme/app/issues/8", http.StatusNotFound, "error_not_found.json")
	e.serve("POST /repos/acme/app/issues/7/sub_issues", http.StatusUnprocessableEntity, "error_validation.json")
	ctx := t.Context()
	wantErr(t, e.client.AddSubIssue(ctx, "acme/app", 7, 8), github.ErrNotFound)
	wantErr(t, e.client.AddSubIssue(ctx, "acme/app", 7, 9), github.ErrInvalid)
	wantErr(t, e.client.AddSubIssue(ctx, "bad", 7, 9), github.ErrInvalid)
}

func TestListSubIssues(t *testing.T) {
	e := newEnv(t)
	e.servePages("GET /repos/acme/app/issues/7/sub_issues", "sub_issues_page1.json", "sub_issues_page2.json")
	got, err := e.client.ListSubIssues(t.Context(), "acme/app", 7)
	noErr(t, err)
	if len(got) != 2 || got[0].Number != 9 || got[1].Number != 10 || got[1].State != "closed" || !slices.Equal(got[1].Labels, []string{"done"}) {
		t.Fatalf("got %+v", got)
	}
	e.serve("GET /repos/acme/app/issues/8/sub_issues", http.StatusNotFound, "error_not_found.json")
	_, err = e.client.ListSubIssues(t.Context(), "acme/app", 8)
	wantErr(t, err, github.ErrNotFound)
}

func TestListOpenIssues(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("labels") != "forecrew:baseline" || q.Get("state") != "open" {
			t.Errorf("query = %v", q)
		}
		_, _ = w.Write(fixture(t, "api", "issues_recent.json"))
	})
	got, err := e.client.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	noErr(t, err)
	if len(got) != 1 || got[0].Number != 12 { // issue 11 is a pull request
		t.Fatalf("got %+v", got)
	}
	e2 := newEnv(t)
	e2.serve("GET /repos/acme/app/issues", http.StatusNotFound, "error_not_found.json")
	_, err = e2.client.ListOpenIssues(t.Context(), "acme/app", "x")
	wantErr(t, err, github.ErrNotFound)
}

func TestComments(t *testing.T) {
	e := newEnv(t)
	e.handle("POST /repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if b := readJSON(t, r); b["body"] != "Looks good" {
			t.Errorf("body = %v", b)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fixture(t, "api", "comment.json"))
	})
	c, err := e.client.CreateComment(t.Context(), "acme/app", 7, "Looks good")
	noErr(t, err)
	if c.ID != 555 || c.Author != "octocat" || c.Body != "Looks good" || !c.CreatedAt.Equal(t0.Add(-2*time.Hour)) {
		t.Fatalf("comment = %+v", c)
	}
}

func TestListComments(t *testing.T) {
	e := newEnv(t)
	var since string
	e.handle("GET /repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		since = r.URL.Query().Get("since")
		if r.URL.Query().Get("direction") != "asc" {
			t.Errorf("query = %v", r.URL.Query())
		}
		_, _ = w.Write(fixture(t, "api", "comments.json"))
	})
	ctx := t.Context()
	all, err := e.client.ListComments(ctx, "acme/app", 7, time.Time{})
	noErr(t, err)
	if len(all) != 2 || since != "" {
		t.Fatalf("all = %+v since=%q", all, since)
	}
	// Comment 1 was created before but updated after the cutoff: GitHub returns
	// it, the client drops it.
	cut := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	got, err := e.client.ListComments(ctx, "acme/app", 7, cut)
	noErr(t, err)
	if len(got) != 1 || got[0].ID != 2 || got[0].Author != "hubot" || since != "2026-03-01T10:00:00Z" {
		t.Fatalf("got %+v since=%q", got, since)
	}
	_, err = e.client.ListComments(ctx, "acme/app", 99, cut)
	wantErr(t, err, github.ErrNotFound)
}

func TestEnsureLabel(t *testing.T) {
	l := github.Label{Name: "needs you", Color: "d93f0b", Description: "Waiting for a human"}
	t.Run("creates", func(t *testing.T) {
		e := newEnv(t)
		e.handle("POST /repos/acme/app/labels", func(w http.ResponseWriter, r *http.Request) {
			b := readJSON(t, r)
			if b["name"] != "needs you" || b["color"] != "d93f0b" || b["description"] != "Waiting for a human" {
				t.Errorf("body = %v", b)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"needs you"}`))
		})
		noErr(t, e.client.EnsureLabel(t.Context(), "acme/app", l))
	})
	t.Run("updates an existing label", func(t *testing.T) {
		e := newEnv(t)
		e.serve("POST /repos/acme/app/labels", http.StatusUnprocessableEntity, "error_validation.json")
		var patched bool
		e.handle("PATCH /repos/acme/app/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
			patched = r.PathValue("name") == "needs you"
			if b := readJSON(t, r); b["color"] != "d93f0b" {
				t.Errorf("body = %v", b)
			}
			_, _ = w.Write([]byte(`{"name":"needs you"}`))
		})
		noErr(t, e.client.EnsureLabel(t.Context(), "acme/app", l))
		if !patched {
			t.Fatal("label not updated")
		}
	})
	t.Run("empty color and description are left out", func(t *testing.T) {
		e := newEnv(t)
		e.handle("POST /repos/acme/app/labels", func(w http.ResponseWriter, r *http.Request) {
			if b := readJSON(t, r); len(b) != 1 {
				t.Errorf("body = %v", b)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		})
		noErr(t, e.client.EnsureLabel(t.Context(), "acme/app", github.Label{Name: "x"}))
	})
	t.Run("errors", func(t *testing.T) {
		e := newEnv(t)
		e.serve("POST /repos/acme/app/labels", http.StatusUnprocessableEntity, "error_validation.json")
		e.serve("PATCH /repos/acme/app/labels/{name}", http.StatusNotFound, "error_not_found.json")
		wantErr(t, e.client.EnsureLabel(t.Context(), "acme/app", l), github.ErrNotFound)

		e = newEnv(t)
		e.serve("POST /repos/acme/app/labels", http.StatusNotFound, "error_not_found.json")
		wantErr(t, e.client.EnsureLabel(t.Context(), "acme/app", l), github.ErrNotFound)
		wantErr(t, e.client.EnsureLabel(t.Context(), "bad", l), github.ErrInvalid)
	})
}

func TestAddAndRemoveLabels(t *testing.T) {
	e := newEnv(t)
	e.handle("POST /repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		b, _ := readAll(r)
		if strings.TrimSpace(string(b)) != `["a","b"]` {
			t.Errorf("body = %s", b)
		}
		_, _ = w.Write([]byte(`[{"name":"a"},{"name":"b"}]`))
	})
	var removed string
	e.handle("DELETE /repos/acme/app/issues/7/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		removed = r.PathValue("name")
		_, _ = w.Write([]byte(`[]`))
	})
	e.serve("DELETE /repos/acme/app/issues/8/labels/{name}", http.StatusNotFound, "error_not_found.json")
	e.serve("DELETE /repos/acme/app/issues/9/labels/{name}", http.StatusUnprocessableEntity, "error_validation.json")
	e.serve("POST /repos/acme/app/issues/8/labels", http.StatusNotFound, "error_not_found.json")
	ctx := t.Context()
	noErr(t, e.client.AddLabels(ctx, "acme/app", 7, "a", "b"))
	wantErr(t, e.client.AddLabels(ctx, "acme/app", 8, "a"), github.ErrNotFound)
	wantErr(t, e.client.AddLabels(ctx, "bad", 8, "a"), github.ErrInvalid)

	noErr(t, e.client.RemoveLabel(ctx, "acme/app", 7, "needs you"))
	if removed != "needs you" {
		t.Fatalf("removed %q", removed)
	}
	noErr(t, e.client.RemoveLabel(ctx, "acme/app", 8, "gone")) // not on the issue: fine
	wantErr(t, e.client.RemoveLabel(ctx, "acme/app", 9, "x"), github.ErrInvalid)
	wantErr(t, e.client.RemoveLabel(ctx, "bad", 9, "x"), github.ErrInvalid)
}

func TestOwnTokenIsCachedUntilNearExpiry(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/issues/7", http.StatusOK, "issue.json")
	ctx := t.Context()
	get := func() {
		t.Helper()
		_, err := e.client.GetIssue(ctx, "acme/app", 7)
		noErr(t, err)
	}
	get()
	e.clock.Advance(54 * time.Minute) // 6 minutes left: still cached
	get()
	if len(e.tokenBodies) != 1 {
		t.Fatalf("mints = %d, want 1", len(e.tokenBodies))
	}
	e.clock.Advance(2 * time.Minute) // 4 minutes left: replaced
	get()
	if len(e.tokenBodies) != 2 || e.apiAuth[2] != "Bearer ghs_2" {
		t.Fatalf("mints = %d, auth = %v", len(e.tokenBodies), e.apiAuth)
	}
	// Job tokens do not feed the client's own cache, nor the other way round.
	_, err := e.client.InstallationToken(ctx, "acme/app", github.AccessRead)
	noErr(t, err)
	get()
	if len(e.tokenBodies) != 3 || e.apiAuth[3] != "Bearer ghs_2" {
		t.Fatalf("mints = %d, auth = %v", len(e.tokenBodies), e.apiAuth)
	}
	if e.lookups != 1 {
		t.Fatalf("installation looked up %d times, want 1", e.lookups)
	}
}

func TestUpdateComment(t *testing.T) {
	e := newEnv(t)
	e.handle("PATCH /repos/acme/app/issues/comments/555", func(w http.ResponseWriter, r *http.Request) {
		if b := readJSON(t, r); b["body"] != "Edited" {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "comment.json"))
	})
	c, err := e.client.UpdateComment(t.Context(), "acme/app", 555, "Edited")
	noErr(t, err)
	if c.ID != 555 {
		t.Fatalf("comment = %+v", c)
	}
	e.serve("PATCH /repos/acme/app/issues/comments/9", http.StatusNotFound, "error_not_found.json")
	_, err = e.client.UpdateComment(t.Context(), "acme/app", 9, "x")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.UpdateComment(t.Context(), "bad", 9, "x")
	wantErr(t, err, github.ErrInvalid)
}
