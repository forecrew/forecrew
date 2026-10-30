package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

// gitRemote creates a bare repo with a main branch holding README.md.
func gitRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", "--quiet", "-b", "main", remote)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return remote
}

// push commits the files on top of base (a branch of the remote, or "" for
// an orphan commit) and points branch at the result.
func push(t *testing.T, remote, base, branch string, files map[string]string) string {
	t.Helper()
	work := t.TempDir()
	index := filepath.Join(work, "index")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir=" + remote}, args...)...)
		cmd.Env = append(gitEnv(), "GIT_INDEX_FILE="+index)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	args := []string{"commit-tree"}
	if base != "" {
		run("read-tree", "refs/heads/"+base)
		args = append(args, "-p", "refs/heads/"+base)
	}
	for path, content := range files {
		f := filepath.Join(work, "blob")
		must(t, os.WriteFile(f, []byte(content), 0o600))
		blob := run("hash-object", "-w", f)
		run("update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	}
	commit := run(append(args, run("write-tree"), "-m", "commit "+branch)...)
	run("update-ref", "refs/heads/"+branch, commit)
	return commit
}

func newGitTracker(t *testing.T) (*Tracker, string) {
	t.Helper()
	tr, _ := newTracker(t)
	remote := gitRemote(t)
	push(t, remote, "", "main", map[string]string{"README.md": "hello\n"})
	must(t, tr.UseRemote("acme/app", remote))
	return tr, remote
}

func TestTrackerRemoteHeads(t *testing.T) {
	tr, remote := newGitTracker(t)
	ctx := t.Context()
	first := push(t, remote, "main", "agent/a", map[string]string{"a.txt": "a\n"})
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "A", Head: "agent/a", Base: "main"})
	must(t, err)
	if pr.HeadSHA != first {
		t.Fatalf("head = %s, want %s", pr.HeadSHA, first)
	}
	second := push(t, remote, "agent/a", "agent/a", map[string]string{"a.txt": "b\n"})
	got, err := tr.FindPR(ctx, "acme/app", "agent/a")
	if err != nil || got.HeadSHA != second {
		t.Fatalf("found head = %s, %v, want %s", got.HeadSHA, err, second)
	}
	got, err = tr.GetPR(ctx, "acme/app", pr.Number)
	if err != nil || got.HeadSHA != second {
		t.Fatalf("head = %s, %v, want %s", got.HeadSHA, err, second)
	}
	for branch, want := range map[string]bool{"agent/a": true, "main": true, "agent/none": false} {
		if ok, err := tr.BranchExists(ctx, "acme/app", branch); err != nil || ok != want {
			t.Errorf("BranchExists(%s) = %v, %v, want %v", branch, ok, err, want)
		}
	}
}

func TestTrackerRemoteFiles(t *testing.T) {
	tr, remote := newGitTracker(t)
	ctx := t.Context()
	sha := push(t, remote, "main", "agent/a", map[string]string{"docs/a.txt": "a\n"})
	for _, ref := range []string{"main", "agent/a", sha} {
		b, err := tr.GetFile(ctx, "acme/app", ref, "README.md")
		if err != nil || string(b) != "hello\n" {
			t.Errorf("README.md at %s = %q, %v", ref, b, err)
		}
	}
	_, err := tr.GetFile(ctx, "acme/app", "main", "docs/a.txt")
	wantErr(t, err, github.ErrNotFound)
	_, err = tr.GetFile(ctx, "acme/app", "agent/a", "docs")
	wantErr(t, err, github.ErrNotFound)
	// A ref git does not know can still be set by hand.
	must(t, tr.SetFile("acme/app", "synthetic", "x.txt", []byte("x")))
	if b, err := tr.GetFile(ctx, "acme/app", "synthetic", "x.txt"); err != nil || string(b) != "x" {
		t.Errorf("synthetic file = %q, %v", b, err)
	}
}

func TestTrackerRemoteMerge(t *testing.T) {
	tr, remote := newGitTracker(t)
	ctx := t.Context()
	push(t, remote, "main", "agent/a", map[string]string{"a.txt": "a\n"})
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "Add a", Head: "agent/a", Base: "main"})
	must(t, err)
	sha, err := tr.MergePR(ctx, "acme/app", pr.Number, github.MergeInput{Method: github.MergeSquash, SHA: pr.HeadSHA})
	must(t, err)
	if main, _ := git(remote, "rev-parse", "refs/heads/main"); main != sha {
		t.Fatalf("main = %s, merge commit = %s", main, sha)
	}
	if parents, _ := git(remote, "rev-list", "--parents", "-n1", sha); len(strings.Fields(parents)) != 2 {
		t.Errorf("merge commit is not a squash commit: %s", parents)
	}
	for _, ref := range []string{"main", sha} {
		if b, err := tr.GetFile(ctx, "acme/app", ref, "a.txt"); err != nil || string(b) != "a\n" {
			t.Errorf("a.txt at %s = %q, %v", ref, b, err)
		}
	}
	got, _ := tr.GetPR(ctx, "acme/app", pr.Number)
	if !got.Merged || got.State != "closed" || got.MergeSHA != sha || got.HeadSHA == sha {
		t.Errorf("merged PR = %+v", got)
	}
	_, err = tr.MergePR(ctx, "acme/app", pr.Number, github.MergeInput{})
	wantErr(t, err, github.ErrNotMergeable)
}

func TestTrackerRemoteMergeConflict(t *testing.T) {
	tr, remote := newGitTracker(t)
	ctx := t.Context()
	push(t, remote, "main", "agent/a", map[string]string{"README.md": "from a\n"})
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "A", Head: "agent/a", Base: "main"})
	must(t, err)
	push(t, remote, "main", "main", map[string]string{"README.md": "from main\n"})
	_, err = tr.MergePR(ctx, "acme/app", pr.Number, github.MergeInput{})
	wantErr(t, err, github.ErrNotMergeable)
	if got, _ := tr.GetPR(ctx, "acme/app", pr.Number); got.State != "open" {
		t.Errorf("PR state = %s", got.State)
	}

	other, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "B", Head: "agent/a", Base: "gone"})
	must(t, err)
	_, err = tr.MergePR(ctx, "acme/app", other.Number, github.MergeInput{})
	wantErr(t, err, github.ErrNotMergeable)
}
