package testkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// UseRemote backs repo with the bare git repository at dir, so the fake
// behaves like GitHub for what is pushed there:
//
//   - the head of an open PR is the commit its branch points to in dir;
//   - GetFile and BranchExists read dir (files set with SetFile still count
//     for refs git does not know);
//   - MergePR squash-merges the PR into its base branch in dir and returns
//     the new commit, or fails with github.ErrNotMergeable on a conflict.
//
// It needs git on PATH.
func (t *Tracker) UseRemote(repo, dir string) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.remote = dir
		return nil
	})
}

// gitEnv is the environment of the git commands run on a remote: no user or
// system config, a fixed identity, and no background maintenance that would
// write into the repo.
func gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.DevNull,
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=github", "GIT_AUTHOR_EMAIL=noreply@github.com",
		"GIT_COMMITTER_NAME=github", "GIT_COMMITTER_EMAIL=noreply@github.com",
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
	}
}

// gitRaw runs git on the bare repository at dir and returns its stdout.
func gitRaw(dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"--git-dir=" + dir}, args...)...) //nolint:gosec // test fake
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// git is gitRaw with the output trimmed.
func git(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	return strings.TrimSpace(string(out)), err
}

// branchSHA returns the commit a branch of the remote points to.
func (r *fakeRepo) branchSHA(branch string) (string, bool) {
	if r.remote == "" {
		return "", false
	}
	sha, err := git(r.remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return sha, err == nil && sha != ""
}

// squashMerge adds the changes of head to base in the remote as one commit
// and returns it.
func (r *fakeRepo) squashMerge(base, head, message string) (string, error) {
	baseSHA, ok := r.branchSHA(base)
	if !ok {
		return "", fmt.Errorf("base branch %s of the remote: %w", base, errNoBranch)
	}
	tree, err := git(r.remote, "merge-tree", "--write-tree", "--no-messages", baseSHA, head)
	if err != nil {
		return "", err
	}
	commit, err := git(r.remote, "commit-tree", tree, "-p", baseSHA, "-m", message)
	if err != nil {
		return "", err
	}
	if _, err := git(r.remote, "update-ref", "refs/heads/"+base, commit, baseSHA); err != nil {
		return "", err
	}
	return commit, nil
}

var errNoBranch = errors.New("no such branch")
