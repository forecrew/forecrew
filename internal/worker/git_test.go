package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestValidBranch(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"agent/task-12", true},
		{"agent/goal-3-spec", true},
		{"main", true},
		{"feature/a.b_c+d@e", true},
		{"", false},
		{"-delete", false},
		{"/abs", false},
		{"a..b", false},
		{"a//b", false},
		{"a/", false},
		{"a.", false},
		{"a.lock", false},
		{"a/.hidden", false},
		{"a/b.lock/c", false},
		{"a b", false},
		{"a~1", false},
		{"a:b", false},
		{"a@{1}", false},
		{"a\nb", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validBranch(tt.name); got != tt.want {
				t.Errorf("validBranch(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestValidRepo(t *testing.T) {
	for repo, want := range map[string]bool{
		"acme/app": true, "Acme/app.js": true, "a/b-c_d": true,
		"": false, "acme": false, "acme/app/x": false, "../x": false, "a/..": false, "-x/y": true, "a b/c": false,
	} {
		if got := validRepo(repo); got != want {
			t.Errorf("validRepo(%q) = %v, want %v", repo, got, want)
		}
	}
}

func TestPushRefRefusals(t *testing.T) {
	tests := []struct {
		branch  string
		wantRef string
	}{
		{"agent/task-1", "refs/heads/agent/task-1"},
		{"main", ""},
		{"master", ""},
		{"refs/heads/main", ""},
		{"refs/heads/agent/x", ""},
		{"agent/../main", ""},
		{"agent/", ""},
		{"agent", ""},
		{"Agent/x", ""},
		{"", ""},
		{"-agent/x", ""},
		{"agent/x y", ""},
	}
	for _, tt := range tests {
		ref, err := pushRef(tt.branch)
		if ref != tt.wantRef || (tt.wantRef == "") != errors.Is(err, errRefused) {
			t.Errorf("pushRef(%q) = %q, %v; want %q", tt.branch, ref, err, tt.wantRef)
		}
	}
}

// recorder is a cmdRunner that records every call and runs nothing.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
	envs  [][]string
}

func (r *recorder) run(_ context.Context, _ string, env []string, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.envs = append(r.envs, env)
	return "", nil
}

func TestPushRefusedBeforeGitRuns(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, branch := range []string{"main", "refs/heads/main", "agent/../main", "", "release/1"} {
		rec := &recorder{}
		g := newGitClient("git", rec.run, nil, "")
		err := g.push(t.Context(), t.TempDir(), "https://example.com/a/b.git", "tok", sha, branch)
		if !errors.Is(err, errRefused) {
			t.Errorf("push(%q) error = %v, want errRefused", branch, err)
		}
		if len(rec.calls) != 0 {
			t.Errorf("push(%q) ran git: %v", branch, rec.calls)
		}
	}
	rec := &recorder{}
	g := newGitClient("git", rec.run, nil, "")
	if err := g.push(t.Context(), t.TempDir(), "https://example.com/a/b.git", "tok", "HEAD", "agent/x"); !errors.Is(err, errRefused) || len(rec.calls) != 0 {
		t.Errorf("push(HEAD) = %v after %d calls, want errRefused", err, len(rec.calls))
	}
}

func TestGitEnv(t *testing.T) {
	g := newGitClient("", nil, []string{
		"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=x", "GIT_DIR=/tmp/x", "FORECREW_WORKER_TOKEN=fcw_x", "GIT_ASKPASS=/bin/evil",
		"GIT_COMMON_DIR=/tmp/y", "GIT_TRACE=1", "GIT_TRACE_CURL=/tmp/t", "GIT_CONFIG_GLOBAL=/home/u/.gitconfig", "GIT_OBJECT_DIRECTORY=/tmp/o", "HOME=/h",
	}, "/work")
	if !slices.Equal(g.base, []string{"PATH=/bin", "HOME=/h"}) {
		t.Errorf("base env = %v", g.base)
	}
	plain := strings.Join(g.env(""), "\n")
	if strings.Contains(plain, "extraHeader") || strings.Contains(plain, "Authorization") {
		t.Errorf("env without token has an auth header:\n%s", plain)
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_CEILING_DIRECTORIES=/work"} {
		if !slices.Contains(g.env(""), want) {
			t.Errorf("env lacks %s", want)
		}
	}
	authed := g.env("ghs_token")
	joined := strings.Join(authed, "\n")
	if strings.Count(joined, "GIT_CONFIG_GLOBAL=") != 1 || strings.Contains(joined, "GIT_TRACE") || strings.Contains(joined, "/tmp/") {
		t.Errorf("env keeps operator GIT_* settings:\n%s", joined)
	}
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=9") || !strings.Contains(joined, "http.extraHeader") ||
		!strings.Contains(joined, authHeader("ghs_token")) || !strings.Contains(joined, "core.hooksPath") {
		t.Errorf("env with token:\n%s", joined)
	}
	if strings.Contains(joined, "ghs_token") {
		t.Error("the token appears in clear text in the env")
	}
}

func newTestGit(t *testing.T) *gitClient {
	t.Helper()
	return newGitClient("git", nil, os.Environ(), "")
}

func TestPrepareNewBranchFromBase(t *testing.T) {
	remote := newRemote(t)
	g := newTestGit(t)
	work := t.TempDir()
	c := checkout{URL: remote, Cache: filepath.Join(work, "cache"), Dir: filepath.Join(work, "job"), BaseRef: "main", Branch: "agent/task-1", Write: true}
	start, existed, err := g.prepare(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Error("existed = true for a branch the remote does not have")
	}
	if want := remoteSHA(t, remote, "main"); start != want {
		t.Errorf("start = %s, want %s", start, want)
	}
	if got := runGit(t, c.Dir, "branch", "--show-current"); got != "agent/task-1" {
		t.Errorf("branch = %q", got)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "docs", "guide.md")); err != nil {
		t.Error(err)
	}
	// The diff the prompts rely on works.
	if got := runGit(t, c.Dir, "diff", "--name-only", "origin/main...HEAD"); got != "" {
		t.Errorf("diff against base = %q, want empty", got)
	}
	cfg, err := os.ReadFile(filepath.Join(c.Cache, "config")) //nolint:gosec // test path
	if err != nil || string(cfg) != pristineConfig {
		t.Errorf("cache config = %q, %v", cfg, err)
	}
	g.remove(t.Context(), c.Cache, c.Dir)
	if _, err := os.Stat(c.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree still exists: %v", err)
	}
	if got := runGit(t, c.Cache, "worktree", "list", "--porcelain"); strings.Contains(got, "job") {
		t.Errorf("worktree still registered: %s", got)
	}
}

func TestPrepareContinuesRemoteBranch(t *testing.T) {
	remote := newRemote(t)
	seed := t.TempDir()
	runGit(t, seed, "clone", "--quiet", remote, ".")
	runGit(t, seed, "switch", "--quiet", "-c", "agent/task-1")
	writeFile(t, seed, "work.txt", "first\n")
	runGit(t, seed, "add", "--all")
	runGit(t, seed, "commit", "--quiet", "-m", "first")
	runGit(t, seed, "push", "--quiet", "origin", "agent/task-1")

	g := newTestGit(t)
	work := t.TempDir()
	cache := filepath.Join(work, "cache")
	for i := range 2 { // the second job reuses the cache
		c := checkout{URL: remote, Cache: cache, Dir: filepath.Join(work, "job", string(rune('a'+i))), BaseRef: "main", Branch: "agent/task-1", Write: true}
		start, existed, err := g.prepare(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		if !existed {
			t.Error("existed = false for a branch the remote has")
		}
		if want := remoteSHA(t, remote, "agent/task-1"); start != want {
			t.Errorf("start = %s, want the remote branch %s", start, want)
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "work.txt")); err != nil {
			t.Error(err)
		}
		g.remove(t.Context(), cache, c.Dir)
	}
}

func TestPrepareReadOnly(t *testing.T) {
	remote := newRemote(t)
	seed := t.TempDir()
	runGit(t, seed, "clone", "--quiet", remote, ".")
	runGit(t, seed, "switch", "--quiet", "-c", "agent/task-2")
	writeFile(t, seed, "change.txt", "x\n")
	runGit(t, seed, "add", "--all")
	runGit(t, seed, "commit", "--quiet", "-m", "change")
	runGit(t, seed, "push", "--quiet", "origin", "agent/task-2")
	baseSHA := remoteSHA(t, remote, "main")

	g := newTestGit(t)
	tests := []struct {
		name, base, branch string
		wantFile           bool
		wantChanged        string
	}{
		{name: "base only", base: "main"},
		{name: "branch to inspect", base: "main", branch: "agent/task-2", wantFile: true, wantChanged: "change.txt"},
		{name: "sha base", base: baseSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := t.TempDir()
			c := checkout{URL: remote, Cache: filepath.Join(work, "cache"), Dir: filepath.Join(work, "job"), BaseRef: tt.base, Branch: tt.branch}
			if _, _, err := g.prepare(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(c.Dir, "change.txt")); (err == nil) != tt.wantFile {
				t.Errorf("change.txt present = %v, want %v", err == nil, tt.wantFile)
			}
			if got := runGit(t, c.Dir, "branch", "--show-current"); got != "" {
				t.Errorf("read-only checkout is on branch %q, want detached HEAD", got)
			}
			if tt.branch != "" {
				if got := runGit(t, c.Dir, "diff", "--name-only", "origin/main...HEAD"); got != tt.wantChanged {
					t.Errorf("diff against base = %q, want %q", got, tt.wantChanged)
				}
			}
		})
	}
}

func TestPrepareErrors(t *testing.T) {
	remote := newRemote(t)
	g := newTestGit(t)
	work := t.TempDir()
	base := checkout{URL: remote, Cache: filepath.Join(work, "cache"), Dir: filepath.Join(work, "job"), BaseRef: "main", Write: true, Branch: "agent/x"}
	tests := []struct {
		name   string
		mutate func(*checkout)
	}{
		{"missing base", func(c *checkout) { c.BaseRef = "nope" }},
		{"bad remote", func(c *checkout) { c.URL = filepath.Join(work, "missing.git") }},
		{"unreadable cache", func(c *checkout) { c.Cache = filepath.Join(work, "cache", "HEAD", "x") }},
	}
	// The third case needs a cache that exists first.
	if _, _, err := g.prepare(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	g.remove(t.Context(), base.Cache, base.Dir)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			if _, _, err := g.prepare(t.Context(), c); err == nil {
				t.Error("prepare error = nil")
			}
		})
	}
	t.Run("bad remote with branch", func(t *testing.T) {
		c := base
		c.URL = filepath.Join(work, "missing.git")
		c.Dir = filepath.Join(work, "job2")
		if _, _, err := g.prepare(t.Context(), c); err == nil || !strings.Contains(err.Error(), "list remote branch") {
			t.Errorf("prepare error = %v", err)
		}
	})
}

func TestSnapshotCommitPush(t *testing.T) {
	remote := newRemote(t)
	g := newTestGit(t)
	work := t.TempDir()
	c := checkout{URL: remote, Cache: filepath.Join(work, "cache"), Dir: filepath.Join(work, "job"), BaseRef: "main", Branch: "agent/task-1", Write: true}
	start, _, err := g.prepare(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := g.snapshot(t.Context(), c.Cache, c.Dir, start)
	if err != nil {
		t.Fatal(err)
	}
	if cs, err := g.changes(t.Context(), c.Cache, start, tree); err != nil || len(cs) != 0 {
		t.Fatalf("changes on a clean tree = %v, %v", cs, err)
	}

	writeFile(t, c.Dir, "src/new.go", "package src\n")
	writeFile(t, c.Dir, "README.md", "changed\n")
	if err := os.Remove(filepath.Join(c.Dir, "docs", "guide.md")); err != nil {
		t.Fatal(err)
	}
	if tree, err = g.snapshot(t.Context(), c.Cache, c.Dir, start); err != nil {
		t.Fatal(err)
	}
	cs, err := g.changes(t.Context(), c.Cache, start, tree)
	if err != nil {
		t.Fatal(err)
	}
	got := paths(cs)
	slices.Sort(got)
	if want := []string{"README.md", "docs/guide.md", "src/new.go"}; !slices.Equal(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
	for _, ch := range cs {
		if want := map[string]string{"README.md": "100644", "docs/guide.md": "000000", "src/new.go": "100644"}[ch.Path]; ch.Mode != want {
			t.Errorf("mode of %s = %s, want %s", ch.Path, ch.Mode, want)
		}
	}
	head, err := g.commitTree(t.Context(), c.Cache, tree, start)
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, c.Cache, "log", "-1", "--format=%an <%ae> %cn %P", head); got != botName+" <"+botEmail+"> "+botName+" "+start {
		t.Errorf("commit = %q", got)
	}
	if stat, err := g.diffStat(t.Context(), c.Cache, start, head); err != nil || !strings.Contains(stat, "3 files changed") {
		t.Errorf("diffStat = %q, %v", stat, err)
	}
	if err := g.push(t.Context(), c.Cache, remote, "", head, "agent/task-1"); err != nil {
		t.Fatal(err)
	}
	if got := remoteSHA(t, remote, "agent/task-1"); got != head {
		t.Errorf("remote branch = %s, want %s", got, head)
	}
	if got := remoteSHA(t, remote, "main"); got != start {
		t.Errorf("main moved to %s", got)
	}
	// A commit that does not descend from the remote branch is refused by the
	// fast-forward rule.
	if err := g.push(t.Context(), c.Cache, remote, "", start, "agent/task-1"); err == nil {
		t.Error("non fast-forward push succeeded")
	}
	if _, err := g.changes(t.Context(), c.Cache, "nonsense", tree); err == nil {
		t.Error("changes(nonsense) error = nil")
	}
}

func TestTokenOnlyInAuthedCalls(t *testing.T) {
	remote := newRemote(t)
	rec := &recorder{}
	var g *gitClient
	g = newGitClient("git", func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
		_, _ = rec.run(ctx, dir, env, name, args...)
		return g.execRunner(ctx, dir, env, name, args...)
	}, os.Environ(), "")
	work := t.TempDir()
	c := checkout{URL: remote, Token: "ghs_supersecret", Cache: filepath.Join(work, "cache"), Dir: filepath.Join(work, "job"), BaseRef: "main", Branch: "agent/x", Write: true}
	start, _, err := g.prepare(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, c.Dir, "a.txt", "a\n")
	tree, err := g.snapshot(t.Context(), c.Cache, c.Dir, start)
	if err != nil {
		t.Fatal(err)
	}
	head, err := g.commitTree(t.Context(), c.Cache, tree, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.push(t.Context(), c.Cache, remote, c.Token, head, c.Branch); err != nil {
		t.Fatal(err)
	}
	header := authHeader(c.Token)
	for i, call := range rec.calls {
		withToken := slices.ContainsFunc(rec.envs[i], func(e string) bool {
			return strings.HasPrefix(e, "GIT_CONFIG_VALUE_") && strings.HasSuffix(e, header)
		})
		remote := slices.Contains(call, "ls-remote") || slices.Contains(call, "fetch") || slices.Contains(call, "push")
		if withToken != remote {
			t.Errorf("git %v: token in env = %v, want %v", call, withToken, remote)
		}
		if strings.Contains(strings.Join(call, " "), c.Token) {
			t.Errorf("git %v has the token in its arguments", call)
		}
	}
	scanTree(t, work, c.Token, header)
}

// scanTree fails if any file below root contains one of the secrets.
func scanTree(t *testing.T, root string, secrets ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // test walk
		if err != nil {
			return nil //nolint:nilerr // unreadable special files are skipped
		}
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("%s contains a secret", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecRunnerError(t *testing.T) {
	isolateGit(t)
	g := newTestGit(t)
	if v, err := g.version(t.Context()); err != nil || !strings.HasPrefix(v, "git version") {
		t.Errorf("version = %q, %v", v, err)
	}
	if pids := g.runningPIDs(); len(pids) != 0 {
		t.Errorf("running git processes = %v after the runs ended", pids)
	}
	_, err := g.execRunner(t.Context(), t.TempDir(), g.env(""), "git", "rev-parse", "HEAD")
	var ce *cmdError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %v", err)
	}
	if _, err := g.execRunner(t.Context(), t.TempDir(), g.env(""), "git", "--version"); err != nil {
		t.Error(err)
	}
	if _, err := g.execRunner(t.Context(), t.TempDir(), g.env(""), "git"); err == nil {
		t.Error("bare git succeeded")
	}
}
