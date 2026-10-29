package worker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The tests in this file reproduce attacks on the worker's git calls: an
// operator environment or an agent that steers git to another repo, to
// another remote or to code of its own.

// treeState describes a directory tree: file names with contents.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // test walk
		if err != nil {
			return nil //nolint:nilerr // special files
		}
		rel, _ := filepath.Rel(root, path)
		state[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func sameState(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for name, content := range before {
		if got, ok := after[name]; !ok || got != content {
			t.Errorf("%s: %s changed or vanished", what, name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s: %s was created", what, name)
		}
	}
}

// newForeignRepo creates a repo the worker has no business with.
func newForeignRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "-b", "main")
	writeFile(t, dir, "mine.txt", "mine\n")
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "--quiet", "-m", "foreign")
	return dir
}

// publish runs the whole git side of a job: prepare, the agent's edits,
// snapshot, commit and push. It returns the pushed commit.
func publish(t *testing.T, g *gitClient, c checkout, agent func(dir, start string)) string {
	t.Helper()
	start, _, err := g.prepare(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	agent(c.Dir, start)
	tree, err := g.snapshot(t.Context(), c.Cache, c.Dir, start)
	if err != nil {
		t.Fatal(err)
	}
	head, err := g.commitTree(t.Context(), c.Cache, tree, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.push(t.Context(), c.Cache, c.URL, c.Token, head, c.Branch); err != nil {
		t.Fatal(err)
	}
	g.remove(t.Context(), c.Cache, c.Dir)
	return head
}

func attackCheckout(remote, work string) checkout {
	return checkout{URL: remote, Token: "ghs_attacktoken", Cache: filepath.Join(work, "cache"), Dir: filepath.Join(work, "job"),
		BaseRef: "main", Branch: "agent/task-1", Write: true}
}

// An operator environment with GIT_COMMON_DIR and friends made `init --bare`
// run on a foreign repo (core.bare=true) and worktrees land in it.
func TestOperatorGitEnvDoesNotRedirectGit(t *testing.T) {
	remote := newRemote(t)
	foreign := newForeignRepo(t)
	before := treeState(t, foreign)
	gitDir := filepath.Join(foreign, ".git")
	for k, v := range map[string]string{
		"GIT_DIR": gitDir, "GIT_COMMON_DIR": gitDir, "GIT_WORK_TREE": foreign, "GIT_INDEX_FILE": filepath.Join(gitDir, "index"),
		"GIT_OBJECT_DIRECTORY": filepath.Join(gitDir, "objects"), "GIT_NAMESPACE": "evil", "GIT_TRACE": "1",
	} {
		t.Setenv(k, v)
	}
	g := newGitClient("git", nil, os.Environ(), "")
	work := t.TempDir()
	c := attackCheckout(remote, work)
	head := publish(t, g, c, func(dir, _ string) { writeFile(t, dir, "new.txt", "n\n") })

	if got := remoteSHA(t, remote, "agent/task-1"); got != head {
		t.Errorf("remote branch = %s, want %s", got, head)
	}
	sameState(t, "foreign repo", before, treeState(t, foreign))
	if got := runGit(t, foreign, "config", "--get", "core.bare"); got != "false" {
		t.Errorf("foreign core.bare = %q", got)
	}
}

// An agent-written pushInsteadOf in the user's ~/.gitconfig sent the token to
// an attacker's server.
func TestUserGitConfigCannotRedirectThePush(t *testing.T) {
	remote := newRemote(t)
	attacker := filepath.Join(t.TempDir(), "attacker.git")
	runGit(t, t.TempDir(), "init", "--bare", "--quiet", attacker)
	home := os.Getenv("HOME")
	cfg := "[url \"" + attacker + "\"]\n\tpushInsteadOf = " + remote + "\n\tinsteadOf = " + remote + "\n[core]\n\thooksPath = /nonexistent\n"
	writeFile(t, home, ".gitconfig", cfg)
	writeFile(t, home, ".config/git/config", cfg)
	g := newGitClient("git", nil, os.Environ(), "")
	head := publish(t, g, attackCheckout(remote, t.TempDir()), func(dir, _ string) { writeFile(t, dir, "new.txt", "n\n") })

	if got := remoteSHA(t, remote, "agent/task-1"); got != head {
		t.Errorf("remote branch = %q, want %s", got, head)
	}
	if refs := runGit(t, attacker, "for-each-ref"); refs != "" {
		t.Errorf("the attacker's repo received refs:\n%s", refs)
	}
}

// With `rm .git` in the worktree, git discovered the repo above it and the
// worker committed into the host's repo.
func TestDeletedDotGitDoesNotReachTheHostRepo(t *testing.T) {
	for name, replace := range map[string]func(dir, host string){
		"deleted":   func(dir, _ string) { _ = os.Remove(filepath.Join(dir, ".git")) },
		"pointed":   func(dir, host string) { writeFile(t, dir, ".git", "gitdir: "+filepath.Join(host, ".git")+"\n") },
		"directory": func(dir, _ string) { _ = os.Remove(filepath.Join(dir, ".git")); runGit(t, dir, "init", "--quiet") },
	} {
		t.Run(name, func(t *testing.T) {
			remote := newRemote(t)
			host := t.TempDir()
			runGit(t, host, "init", "--quiet", "-b", "main")
			hostBefore := treeState(t, filepath.Join(host, ".git"))
			work := filepath.Join(host, "work") // inside the host repo, no ceiling set
			g := newGitClient("git", nil, os.Environ(), "")
			head := publish(t, g, attackCheckout(remote, work), func(dir, _ string) {
				replace(dir, host)
				writeFile(t, dir, "new.txt", "n\n")
			})

			if got := runGit(t, remote, "show", head+":new.txt"); got != "n" {
				t.Errorf("pushed file = %q", got)
			}
			if out := runGit(t, host, "for-each-ref"); out != "" {
				t.Errorf("host repo got refs:\n%s", out)
			}
			sameState(t, "host repo", hostBefore, treeState(t, filepath.Join(host, ".git")))
		})
	}
}

// Replace refs, grafts and a planted info/attributes change what git shows;
// filter drivers in the shared config run code at `git add`.
func TestAgentPlantedGitStateIsRemoved(t *testing.T) {
	remote := newRemote(t)
	g := newTestGit(t)
	work := t.TempDir()
	c := attackCheckout(remote, work)
	marker := filepath.Join(work, "pwned")
	head := publish(t, g, c, func(dir, start string) {
		// The history bypass: forbidden content in a commit that is replaced
		// for the starting commit.
		writeFile(t, dir, ".github/workflows/evil.yml", "evil\n")
		runGit(t, dir, "add", "--all")
		runGit(t, dir, "commit", "--quiet", "-m", "evil")
		evil := runGit(t, dir, "rev-parse", "HEAD")
		runGit(t, dir, "reset", "--quiet", "--hard", start)
		runGit(t, dir, "replace", start, evil)
		// Code that would run when the worker stages the files.
		runGit(t, dir, "config", "filter.evil.clean", "touch "+marker)
		runGit(t, dir, "config", "diff.evil.textconv", "touch "+marker)
		writeFile(t, dir, ".gitattributes", "* filter=evil diff=evil\n")
		writeFile(t, c.Cache, "info/attributes", "* filter=evil diff=evil\n")
		writeFile(t, c.Cache, "info/grafts", start+"\n")
		writeFile(t, dir, "ok.txt", "ok\n")
	})

	if _, err := os.Stat(marker); err == nil {
		t.Error("an agent configured filter ran")
	}
	if got := runGit(t, c.Cache, "for-each-ref", "refs/replace/"); got != "" {
		t.Errorf("replace refs survived:\n%s", got)
	}
	for _, f := range []string{"info/grafts", "info/attributes"} {
		if _, err := os.Stat(filepath.Join(c.Cache, f)); err == nil {
			t.Errorf("%s survived", f)
		}
	}
	files := strings.Fields(runGit(t, remote, "ls-tree", "-r", "--name-only", head))
	if slices.Contains(files, ".github/workflows/evil.yml") || !slices.Contains(files, "ok.txt") || !slices.Contains(files, ".gitattributes") {
		t.Errorf("pushed files = %v", files)
	}
	if got := runGit(t, remote, "rev-list", "--count", "main.."+head); got != "1" {
		t.Errorf("commits pushed = %s, want 1", got)
	}
	if got := runGit(t, remote, "log", "--all", "--format=%H", "--", ".github"); got != "" {
		t.Errorf("forbidden content is in the pushed history: %s", got)
	}
}

// A read-only agent hid a new file from the snapshot with the cache's
// info/exclude, which is shared by all worktrees.
func TestPlantedExcludeFileCannotHideChanges(t *testing.T) {
	remote := newRemote(t)
	g := newTestGit(t)
	work := t.TempDir()
	c := attackCheckout(remote, work)
	start, _, err := g.prepare(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, c.Cache, "info/exclude", "*\n")
	writeFile(t, c.Dir, "hidden.txt", "secret change\n")
	tree, err := g.snapshot(t.Context(), c.Cache, c.Dir, start)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := g.changes(t.Context(), c.Cache, start, tree)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(cs); !slices.Equal(got, []string{"hidden.txt"}) {
		t.Errorf("changes = %v, want the file the exclude list tried to hide", got)
	}
	if _, err := os.Stat(filepath.Join(c.Cache, "info", "exclude")); err == nil {
		t.Error("info/exclude survived")
	}
}

// Files git refuses to stage are the agent's doing: the error says so and
// can be told apart from a failure of the worker's own git.
func TestStagingErrorsAreRecognized(t *testing.T) {
	remote := newRemote(t)
	g := newTestGit(t)
	c := attackCheckout(remote, t.TempDir())
	start, _, err := g.prepare(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, c.Dir, ".GIT/config", "[core]\n")
	if _, err := g.snapshot(t.Context(), c.Cache, c.Dir, start); !errors.Is(err, errStaging) {
		t.Errorf("snapshot error = %v, want errStaging", err)
	}
	if _, err := g.snapshot(t.Context(), c.Cache, c.Dir, "nonsense"); err == nil || errors.Is(err, errStaging) {
		t.Errorf("snapshot of a bad start = %v, want an error that is not errStaging", err)
	}
}
