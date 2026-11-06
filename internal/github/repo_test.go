package github_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

func TestTree(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") == "" {
			t.Error("the tree was not requested recursively")
		}
		_, _ = w.Write([]byte(`{"sha":"t","truncated":false,"tree":[
			{"path":"b.md","type":"blob","sha":"s2","size":5},{"path":"docs","type":"tree","sha":"d"},{"path":"a.md","type":"blob","sha":"s1","size":3}]}`))
	})
	e.handle("GET /repos/acme/app/git/trees/huge", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"t","truncated":true,"tree":[]}`))
	})
	got, err := e.client.Tree(t.Context(), "acme/app", "main")
	noErr(t, err)
	if len(got) != 2 || got[0] != (github.TreeEntry{Path: "a.md", SHA: "s1", Size: 3}) || got[1].Path != "b.md" {
		t.Errorf("tree = %+v", got)
	}
	_, err = e.client.Tree(t.Context(), "acme/app", "huge")
	wantErr(t, err, github.ErrTooLarge)
	_, err = e.client.Tree(t.Context(), "acme/app", "gone")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.Tree(t.Context(), "bad", "main")
	wantErr(t, err, github.ErrInvalid)
}

func TestCompare(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/compare/aaa...bbb", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"files":[{"filename":"c.go","status":"removed","sha":"s3"}]}`))
			return
		}
		next := *r.URL
		next.RawQuery = "page=2&per_page=100"
		w.Header().Set("Link", `<http://`+r.Host+next.RequestURI()+`>; rel="next"`)
		_, _ = w.Write([]byte(`{"files":[
			{"filename":"new.go","previous_filename":"old.go","status":"renamed","sha":"s1","patch":"@@ -1 +1 @@\n-func Old()\n+func New()"},
			{"filename":"b.go","status":"modified","sha":"s2"}]}`))
	})
	got, err := e.client.Compare(t.Context(), "acme/app", "aaa", "bbb")
	noErr(t, err)
	if len(got.Files) != 3 || got.Files[0].PreviousPath != "old.go" || got.Files[0].Status != github.FileRenamed ||
		!strings.Contains(got.Files[0].Patch, "func New()") || got.Files[2].Status != github.FileRemoved {
		t.Errorf("comparison = %+v", got)
	}
	_, err = e.client.Compare(t.Context(), "acme/app", "aaa", "gone")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.Compare(t.Context(), "bad", "a", "b")
	wantErr(t, err, github.ErrInvalid)
}

func TestLastCommit(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/commits", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("path") {
		case "bot.md":
			_, _ = w.Write([]byte(`[{"sha":"c2","author":{"login":"forecrew[bot]","type":"Bot"},"commit":{"author":{"name":"forecrew","date":"2026-03-01T12:00:00Z"}}}]`))
		case "anon.md":
			_, _ = w.Write([]byte(`[{"sha":"c3","author":null,"commit":{"author":{"name":"Ann","date":"2026-03-02T12:00:00Z"}}}]`))
		case "ok.md":
			if r.URL.Query().Get("sha") != "main" {
				t.Errorf("sha = %q", r.URL.Query().Get("sha"))
			}
			_, _ = w.Write([]byte(`[{"sha":"c1","author":{"login":"octocat","type":"User"},"commit":{"author":{"name":"Octo","date":"2026-03-01T12:00:00Z"}}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})
	c, err := e.client.LastCommit(t.Context(), "acme/app", "main", "ok.md")
	noErr(t, err)
	if c.SHA != "c1" || c.Author != "octocat" || c.Bot || !c.Time.Equal(t0) {
		t.Errorf("commit = %+v", c)
	}
	if c, err = e.client.LastCommit(t.Context(), "acme/app", "main", "bot.md"); err != nil || !c.Bot {
		t.Errorf("bot commit = %+v, %v", c, err)
	}
	if c, err = e.client.LastCommit(t.Context(), "acme/app", "main", "anon.md"); err != nil || c.Author != "Ann" {
		t.Errorf("anonymous commit = %+v, %v", c, err)
	}
	_, err = e.client.LastCommit(t.Context(), "acme/app", "main", "none.md")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.LastCommit(t.Context(), "bad", "main", "x")
	wantErr(t, err, github.ErrInvalid)
}

func TestHeadSHA(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"sha":"abc123","type":"commit"}}`))
	})
	got, err := e.client.HeadSHA(t.Context(), "acme/app", "main")
	noErr(t, err)
	if got != "abc123" {
		t.Errorf("head = %q", got)
	}
	_, err = e.client.HeadSHA(t.Context(), "acme/app", "gone")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.HeadSHA(t.Context(), "bad", "main")
	wantErr(t, err, github.ErrInvalid)
}

// A repo without commits answers 409 on the ref endpoint.
func TestHeadSHAOfAnEmptyRepo(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Git Repository is empty."}`))
	})
	_, err := e.client.HeadSHA(t.Context(), "acme/app", "main")
	wantErr(t, err, github.ErrEmptyRepo)
}

// Another 409 is no empty repo: it stays a plain conflict.
func TestHeadSHAConflictThatIsNotEmpty(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Something else is wrong."}`))
	})
	_, err := e.client.HeadSHA(t.Context(), "acme/app", "main")
	if errors.Is(err, github.ErrEmptyRepo) || !errors.Is(err, github.ErrConflict) {
		t.Errorf("err = %v, want a conflict that is not ErrEmptyRepo", err)
	}
}

func TestArchive(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/tarball/main", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/download/main.tar.gz", http.StatusFound)
	})
	e.handle("GET /repos/acme/app/tarball/gone", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/download/gone.tar.gz", http.StatusFound)
	})
	e.handle("GET /download/main.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the download link was called with credentials")
		}
		_, _ = w.Write([]byte("tarball bytes"))
	})
	e.handle("GET /download/gone.tar.gz", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	rc, err := e.client.Archive(t.Context(), "acme/app", "main")
	noErr(t, err)
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); string(b) != "tarball bytes" {
		t.Errorf("archive = %q", b)
	}
	_, err = e.client.Archive(t.Context(), "acme/app", "gone")
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.Archive(t.Context(), "bad", "main")
	wantErr(t, err, github.ErrInvalid)
}
