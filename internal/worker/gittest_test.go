package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain removes the GIT_* variables the test process inherited (git hooks
// and `git rebase -x` export GIT_DIR and friends), so that no test can reach
// the repo it was started from.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

// isolateGit gives the test an empty home and no GIT_* variables, so the
// developer's own git config and environment cannot change it. It skips the
// test when git is not installed.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "") // restored when the test ends
			_ = os.Unsetenv(name)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// hermeticEnv is the environment of the git commands the tests run
// themselves: built from scratch, with no inherited GIT_* variable and no
// config file.
func hermeticEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		// A commit starts a detached `git maintenance run --auto`, which
		// writes .git/objects/maintenance.lock while a test compares trees.
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
		"GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@example.com",
	}
}

// runGit runs git in dir and returns trimmed stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes content to path below dir, creating directories.
func writeFile(t *testing.T, dir, path, content string) {
	t.Helper()
	p := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newRemote creates a bare repo with one commit on main (README.md and
// docs/guide.md) and returns its path, usable as a clone URL.
func newRemote(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", "--quiet", "-b", "main", remote)
	seed := t.TempDir()
	runGit(t, seed, "init", "--quiet", "-b", "main")
	writeFile(t, seed, "README.md", "hello\n")
	writeFile(t, seed, "docs/guide.md", "guide\n")
	runGit(t, seed, "add", "--all")
	runGit(t, seed, "commit", "--quiet", "-m", "initial")
	runGit(t, seed, "push", "--quiet", remote, "main")
	return remote
}

// remoteSHA returns the commit a branch of the remote points to, or "".
func remoteSHA(t *testing.T, remote, branch string) string {
	t.Helper()
	out := runGit(t, remote, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	return out
}
