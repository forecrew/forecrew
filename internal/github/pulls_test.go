package github_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

const prSHA = "4f1c2b9e0000000000000000000000000000abcd"

func TestCreatePR(t *testing.T) {
	e := newEnv(t)
	e.handle("POST /repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		if b["title"] != "Implement login" || b["head"] != "agent/task-3" || b["base"] != "main" || b["draft"] != true || b["body"] != "Closes #7" {
			t.Errorf("body = %v", b)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fixture(t, "api", "pr.json"))
	})
	pr, err := e.client.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "Implement login", Body: "Closes #7", Head: "agent/task-3", Base: "main", Draft: true})
	noErr(t, err)
	if pr.Number != 12 || pr.Head != "agent/task-3" || pr.Base != "main" || pr.HeadSHA != prSHA || pr.State != "open" ||
		pr.Merged || pr.Draft || pr.URL != "https://github.com/acme/app/pull/12" || pr.Mergeable == nil || !*pr.Mergeable {
		t.Fatalf("pr = %+v", pr)
	}
	e.serve("POST /repos/acme/other/pulls", http.StatusUnprocessableEntity, "error_validation.json")
	e.handle("GET /repos/acme/other/installation", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id": 42}`)) })
	_, err = e.client.CreatePR(t.Context(), "acme/other", github.PRInput{})
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.CreatePR(t.Context(), "bad", github.PRInput{})
	wantErr(t, err, github.ErrInvalid)
}

func TestGetPR(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/pulls/12", http.StatusOK, "pr.json")
	e.serve("GET /repos/acme/app/pulls/13", http.StatusOK, "pr_unknown_mergeable.json")
	e.serve("GET /repos/acme/app/pulls/14", http.StatusNotFound, "error_not_found.json")
	ctx := t.Context()
	pr, err := e.client.GetPR(ctx, "acme/app", 12)
	noErr(t, err)
	if pr.HeadSHA != prSHA || pr.Mergeable == nil || !*pr.Mergeable {
		t.Fatalf("pr = %+v", pr)
	}
	pr, err = e.client.GetPR(ctx, "acme/app", 13)
	noErr(t, err)
	if pr.Mergeable != nil || !pr.Merged || !pr.Draft || pr.State != "closed" {
		t.Fatalf("pr = %+v", pr)
	}
	_, err = e.client.GetPR(ctx, "acme/app", 14)
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.GetPR(ctx, "bad", 14)
	wantErr(t, err, github.ErrInvalid)
}

func TestFindPR(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "open" || q.Get("head") != "acme:agent/task-3" {
			t.Errorf("query = %v", q)
		}
		if q.Get("head") == "acme:agent/task-3" {
			_, _ = w.Write([]byte("[" + string(fixture(t, "api", "pr.json")) + "]"))
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	ctx := t.Context()
	pr, err := e.client.FindPR(ctx, "acme/app", "agent/task-3")
	noErr(t, err)
	if pr.Number != 12 {
		t.Fatalf("pr = %+v", pr)
	}
	e.handle("GET /repos/acme/empty/pulls", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	e.handle("GET /repos/acme/empty/installation", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id": 42}`)) })
	_, err = e.client.FindPR(ctx, "acme/empty", "x")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.FindPR(ctx, "bad", "x")
	wantErr(t, err, github.ErrInvalid)
}

func TestFindPRError(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/pulls", http.StatusUnprocessableEntity, "error_validation.json")
	_, err := e.client.FindPR(t.Context(), "acme/app", "x")
	wantErr(t, err, github.ErrInvalid)
}

func TestUpdatePR(t *testing.T) {
	e := newEnv(t)
	e.handle("PATCH /repos/acme/app/pulls/12", func(w http.ResponseWriter, r *http.Request) {
		if b := readJSON(t, r); len(b) != 1 || b["title"] != "New title" {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "pr.json"))
	})
	e.serve("PATCH /repos/acme/app/pulls/14", http.StatusNotFound, "error_not_found.json")
	title := "New title"
	pr, err := e.client.UpdatePR(t.Context(), "acme/app", 12, github.PRUpdate{Title: &title})
	noErr(t, err)
	if pr.Number != 12 {
		t.Fatalf("pr = %+v", pr)
	}
	_, err = e.client.UpdatePR(t.Context(), "acme/app", 14, github.PRUpdate{})
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.UpdatePR(t.Context(), "bad", 14, github.PRUpdate{})
	wantErr(t, err, github.ErrInvalid)
}

func TestListReviews(t *testing.T) {
	e := newEnv(t)
	e.servePages("GET /repos/acme/app/pulls/12/reviews", "reviews_page1.json", "reviews_page2.json")
	e.serve("GET /repos/acme/app/pulls/14/reviews", http.StatusNotFound, "error_not_found.json")
	got, err := e.client.ListReviews(t.Context(), "acme/app", 12)
	noErr(t, err)
	wantEvents := []github.ReviewEvent{github.ReviewApprove, github.ReviewRequestChanges, github.ReviewComment, "DISMISSED"}
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	for i, ev := range wantEvents {
		if got[i].Event != ev || got[i].ID != int64(i+1) {
			t.Fatalf("review %d = %+v, want event %s", i, got[i], ev)
		}
	}
	if got[1].Author != "hubot" || got[1].Body != "fix" || got[1].SHA != "aaa" {
		t.Fatalf("review = %+v", got[1])
	}
	_, err = e.client.ListReviews(t.Context(), "acme/app", 14)
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.ListReviews(t.Context(), "bad", 14)
	wantErr(t, err, github.ErrInvalid)
}

func TestCreateReviewWithoutComments(t *testing.T) {
	e := newEnv(t)
	e.handle("POST /repos/acme/app/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		if b["event"] != "COMMENT" || b["body"] != "Two notes" || b["comments"] != nil {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "review.json"))
	})
	r, err := e.client.CreateReview(t.Context(), "acme/app", 12, github.ReviewInput{Event: github.ReviewComment, Body: "Two notes"})
	noErr(t, err)
	if r.ID != 900 || r.Event != github.ReviewComment || r.Author != "forecrew[bot]" || r.SHA != "4f1c2b9e" {
		t.Fatalf("review = %+v", r)
	}
}

func TestCreateReviewMovesCommentsOutsideTheDiff(t *testing.T) {
	e := newEnv(t)
	e.servePages("GET /repos/acme/app/pulls/12/files", "pr_files.json")
	e.handle("POST /repos/acme/app/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		inline, _ := b["comments"].([]any)
		if len(inline) != 3 {
			t.Fatalf("inline = %v", inline)
		}
		for i, want := range []struct {
			path string
			line float64
		}{{"main.go", 2}, {"main.go", 22}, {"main.go", 1}} {
			c := inline[i].(map[string]any)
			if c["path"] != want.path || c["line"] != want.line || c["side"] != "RIGHT" {
				t.Errorf("inline[%d] = %v", i, c)
			}
		}
		body, _ := b["body"].(string)
		for _, want := range []string{"Overall fine\n\n", "- `main.go:10`: gap between hunks", "- `main.go:24`: after the last line", "- `other.go:1`: unknown file", "- `image.png:1`: binary"} {
			if !strings.Contains(body, want) {
				t.Errorf("body %q lacks %q", body, want)
			}
		}
		if strings.Contains(body, "removed line") {
			t.Errorf("inline comment leaked into the body: %q", body)
		}
		_, _ = w.Write(fixture(t, "api", "review.json"))
	})
	_, err := e.client.CreateReview(t.Context(), "acme/app", 12, github.ReviewInput{
		Event: github.ReviewComment, Body: "Overall fine",
		Comments: []github.InlineComment{
			{Path: "main.go", Line: 2, Body: "added line"},
			{Path: "main.go", Line: 22, Body: "added in second hunk"},
			{Path: "main.go", Line: 1, Body: "context line"},
			{Path: "main.go", Line: 10, Body: "gap between hunks"},
			{Path: "main.go", Line: 24, Body: "after the last line"},
			{Path: "other.go", Line: 1, Body: "unknown file"},
			{Path: "image.png", Line: 1, Body: "binary"},
		},
	})
	noErr(t, err)
}

func TestCreateReviewRetriesWithoutInlineComments(t *testing.T) {
	e := newEnv(t)
	e.servePages("GET /repos/acme/app/pulls/12/files", "pr_files.json")
	var posts int
	e.handle("POST /repos/acme/app/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		posts++
		b := readJSON(t, r)
		if posts == 1 {
			if inline, _ := b["comments"].([]any); len(inline) != 1 {
				t.Errorf("first post = %v", b)
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write(fixture(t, "api", "error_validation.json"))
			return
		}
		body, _ := b["body"].(string)
		if b["comments"] != nil || b["event"] != "COMMENT" ||
			!strings.Contains(body, "- `main.go:2`: added line") || !strings.Contains(body, "- `main.go:99`: far") || !strings.HasPrefix(body, "Fine\n\n") {
			t.Errorf("second post = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "review.json"))
	})
	r, err := e.client.CreateReview(t.Context(), "acme/app", 12, github.ReviewInput{
		Event: github.ReviewComment, Body: "Fine",
		Comments: []github.InlineComment{{Path: "main.go", Line: 2, Body: "added line"}, {Path: "main.go", Line: 99, Body: "far"}},
	})
	noErr(t, err)
	if r.ID != 900 || posts != 2 {
		t.Fatalf("review = %+v, posts = %d", r, posts)
	}
}

func TestCreateReviewDoesNotRetryOtherFailures(t *testing.T) {
	e := newEnv(t)
	e.servePages("GET /repos/acme/app/pulls/12/files", "pr_files.json")
	var posts int
	e.handle("POST /repos/acme/app/pulls/12/reviews", func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write(fixture(t, "api", "error_validation.json"))
	})
	// No inline comments: a 422 is final.
	_, err := e.client.CreateReview(t.Context(), "acme/app", 12, github.ReviewInput{Event: github.ReviewComment, Body: "x"})
	wantErr(t, err, github.ErrInvalid)
	// The retry fails too: the error is returned after exactly one retry.
	_, err = e.client.CreateReview(t.Context(), "acme/app", 12, github.ReviewInput{
		Event: github.ReviewComment, Comments: []github.InlineComment{{Path: "main.go", Line: 2, Body: "x"}},
	})
	wantErr(t, err, github.ErrInvalid)
	if posts != 3 {
		t.Fatalf("posts = %d, want 3", posts)
	}
}

func TestCreateReviewOnlyOutsideTheDiffWithEmptyBody(t *testing.T) {
	e := newEnv(t)
	e.servePages("GET /repos/acme/app/pulls/12/files", "pr_files.json")
	e.handle("POST /repos/acme/app/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		body, _ := b["body"].(string)
		if b["comments"] != nil || !strings.HasPrefix(body, "Comments outside the diff:") {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "review.json"))
	})
	_, err := e.client.CreateReview(t.Context(), "acme/app", 12, github.ReviewInput{
		Event: github.ReviewComment, Comments: []github.InlineComment{{Path: "main.go", Line: 99, Body: "far"}},
	})
	noErr(t, err)
}

func TestCreateReviewErrors(t *testing.T) {
	e := newEnv(t)
	e.serve("GET /repos/acme/app/pulls/13/files", http.StatusNotFound, "error_not_found.json")
	e.serve("POST /repos/acme/app/pulls/14/reviews", http.StatusUnprocessableEntity, "error_validation.json")
	e.serve("POST /repos/acme/app/pulls/15/reviews", http.StatusNotFound, "error_not_found.json")
	ctx := t.Context()
	_, err := e.client.CreateReview(ctx, "acme/app", 13, github.ReviewInput{Comments: []github.InlineComment{{Path: "a", Line: 1}}})
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.CreateReview(ctx, "acme/app", 14, github.ReviewInput{Event: github.ReviewComment})
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.CreateReview(ctx, "acme/app", 15, github.ReviewInput{Event: github.ReviewComment})
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.CreateReview(ctx, "bad", 15, github.ReviewInput{})
	wantErr(t, err, github.ErrInvalid)
}

func TestMergePR(t *testing.T) {
	e := newEnv(t)
	e.handle("PUT /repos/acme/app/pulls/12/merge", func(w http.ResponseWriter, r *http.Request) {
		b := readJSON(t, r)
		if b["merge_method"] != "squash" || b["sha"] != prSHA {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write(fixture(t, "api", "merge.json"))
	})
	e.serve("PUT /repos/acme/app/pulls/13/merge", http.StatusMethodNotAllowed, "error_not_mergeable.json")
	e.serve("PUT /repos/acme/app/pulls/14/merge", http.StatusConflict, "error_not_mergeable.json")
	e.serve("PUT /repos/acme/app/pulls/15/merge", http.StatusNotFound, "error_not_found.json")
	e.serve("PUT /repos/acme/app/pulls/16/merge", http.StatusUnprocessableEntity, "error_validation.json")
	ctx := t.Context()
	sha, err := e.client.MergePR(ctx, "acme/app", 12, github.MergeInput{Method: github.MergeSquash, SHA: prSHA})
	noErr(t, err)
	if sha != "6dcb09b5b57875f334f61aebed695e2e4193db5e" {
		t.Fatalf("sha = %q", sha)
	}
	for _, n := range []int{13, 14} {
		_, err = e.client.MergePR(ctx, "acme/app", n, github.MergeInput{Method: github.MergeSquash})
		wantErr(t, err, github.ErrNotMergeable)
	}
	_, err = e.client.MergePR(ctx, "acme/app", 15, github.MergeInput{})
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.MergePR(ctx, "acme/app", 16, github.MergeInput{})
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.MergePR(ctx, "bad", 16, github.MergeInput{})
	wantErr(t, err, github.ErrInvalid)
}

func TestBranchExists(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/git/ref/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("branch") {
		case "agent/task-3":
			_, _ = w.Write(fixture(t, "api", "ref.json"))
		case "agent/task-4":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(fixture(t, "api", "error_not_found.json"))
		case "agent/limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "agent/invalid":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write(fixture(t, "api", "error_validation.json"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	ctx := t.Context()
	if ok, err := e.client.BranchExists(ctx, "acme/app", "agent/task-3"); err != nil || !ok {
		t.Fatalf("task-3 = %v, %v", ok, err)
	}
	if ok, err := e.client.BranchExists(ctx, "acme/app", "agent/task-4"); err != nil || ok {
		t.Fatalf("task-4 = %v, %v", ok, err)
	}
	_, err := e.client.BranchExists(ctx, "acme/app", "agent/limited")
	wantErr(t, err, github.ErrRateLimited)
	_, err = e.client.BranchExists(ctx, "acme/app", "agent/invalid")
	wantErr(t, err, github.ErrInvalid)
	_, err = e.client.BranchExists(ctx, "acme/app", "agent/boom")
	if err == nil || !strings.Contains(err.Error(), `check branch "agent/boom" of acme/app`) || errors.Unwrap(err) == nil {
		t.Fatalf("err = %v, want a wrapped error naming the branch", err)
	}
	_, err = e.client.BranchExists(ctx, "bad", "x")
	wantErr(t, err, github.ErrInvalid)
}
