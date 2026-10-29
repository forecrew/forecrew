package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Commit identity of the changes the worker commits.
const (
	botName  = "forecrew-bot"
	botEmail = "forecrew-bot@users.noreply.github.com"
)

// agentBranchPrefix is the only ref namespace the worker pushes to.
const agentBranchPrefix = "agent/"

// errStaging means git could not stage the files of a worktree: the agent
// left paths git refuses (unreadable files, reserved names like .GIT/config).
// That is the agent's doing, not an infrastructure failure.
var errStaging = errors.New("cannot stage the changes")

// errRefused means the worker refused an operation before running git.
var errRefused = errors.New("refused")

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	fullSHA  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	// refChars are the characters allowed in a branch name; git's rules are
	// wider, ours are deliberately strict.
	refChars = regexp.MustCompile(`^[A-Za-z0-9._/+@-]+$`)
)

// validRepo reports whether repo is "owner/name" without path tricks.
func validRepo(repo string) bool {
	return repoName.MatchString(repo) && !strings.Contains(repo, "..")
}

// validBranch reports whether name is a safe branch name: it cannot be read
// as an option, climb out of a directory or form an invalid git ref.
func validBranch(name string) bool {
	if name == "" || !refChars.MatchString(name) || name[0] == '-' || name[0] == '/' {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// pushRef returns the full ref a branch is pushed to, or errRefused unless it
// is a valid refs/heads/agent/ branch.
func pushRef(branch string) (string, error) {
	if !strings.HasPrefix(branch, agentBranchPrefix) || !validBranch(branch) {
		return "", fmt.Errorf("push %q: only refs/heads/%s* branches are pushed: %w", branch, agentBranchPrefix, errRefused)
	}
	return "refs/heads/" + branch, nil
}

// cmdRunner runs a command and returns its stdout.
type cmdRunner func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)

// cmdError is a failed command with the tail of its stderr.
type cmdError struct {
	cmd    string
	err    error
	stderr string
}

func (e *cmdError) Error() string {
	if e.stderr == "" {
		return fmt.Sprintf("%s: %v", e.cmd, e.err)
	}
	return fmt.Sprintf("%s: %v: %s", e.cmd, e.err, e.stderr)
}

func (e *cmdError) Unwrap() error { return e.err }

const (
	stderrMax      = 1024
	cmdWaitDelay   = 5 * time.Second
	commitMessage  = "Changes by forecrew-bot"
	remoteRefsBase = "refs/remotes/origin/"
)

// execRunner is the default cmdRunner. It records the pid of each running
// process, so the reaper can tell the worker's own git processes from
// leftovers of an agent.
func (g *gitClient) execRunner(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the binary is git, the arguments are validated
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, cmdWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// The gate keeps the reaper from sweeping between the start and the
	// registration of the process.
	g.gate.RLock()
	err := cmd.Start()
	if err == nil {
		g.track(cmd.Process.Pid, true)
	}
	g.gate.RUnlock()
	if err == nil {
		err = cmd.Wait()
		g.track(cmd.Process.Pid, false)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > stderrMax {
			msg = msg[len(msg)-stderrMax:]
		}
		sub := ""
		if len(args) > 0 {
			sub = " " + args[0]
		}
		return stdout.String(), &cmdError{cmd: name + sub, err: err, stderr: strings.ToValidUTF8(msg, "")}
	}
	return stdout.String(), nil
}

func (g *gitClient) track(pid int, running bool) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	if running {
		g.pids[pid] = struct{}{}
	} else {
		delete(g.pids, pid)
	}
}

// runningPIDs returns the pids of the git processes running now.
func (g *gitClient) runningPIDs() map[int]struct{} {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return maps.Clone(g.pids)
}

// gitClient runs the worker's own git commands. The repo token reaches git
// only through the environment of a single clone, fetch or push process.
//
// Nothing the agent can write steers these commands: they never read the
// operator's or the user's git config or the GIT_* variables, they name the
// git dir and work tree explicitly instead of discovering them, and the
// config of the repo cache is rewritten before they run.
type gitClient struct {
	bin     string
	run     cmdRunner
	base    []string // the operator's environment without GIT_* and FORECREW_*
	ceiling string   // GIT_CEILING_DIRECTORIES

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	gate sync.RWMutex // read-locked while a process starts, see reaper
	pmu  sync.Mutex
	pids map[int]struct{}
}

// newGitClient returns a gitClient. environ is the operator's environment;
// ceiling is a directory above the work dir that git must not search past
// (empty for none).
func newGitClient(bin string, run cmdRunner, environ []string, ceiling string) *gitClient {
	if bin == "" {
		bin = "git"
	}
	g := &gitClient{bin: bin, run: run, base: gitBaseEnv(environ), ceiling: ceiling,
		locks: map[string]*sync.Mutex{}, pids: map[int]struct{}{}}
	if run == nil {
		g.run = g.execRunner
	}
	return g
}

// gitBaseEnv drops every GIT_* variable (they can redirect git to another
// repo, change its config or turn on tracing) and every FORECREW_ variable
// (the worker token).
func gitBaseEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "FORECREW_") {
			out = append(out, kv)
		}
	}
	return out
}

// authHeader returns the extraHeader value that authenticates to GitHub over
// HTTPS with an installation token.
func authHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// env builds the environment of a git process. No config file is read except
// the repo cache's own, and the settings passed through GIT_CONFIG_* outrank
// it, so hooks, filters, attributes and credential helpers do not run and a
// url rewrite (pushInsteadOf) cannot redirect the token. The token header
// exists only for the one process that needs it. extra entries come last.
func (g *gitClient) env(token string, extra ...string) []string {
	cfg := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"core.attributesFile", os.DevNull},
		{"core.excludesFile", os.DevNull},
		{"credential.helper", ""},
		{"commit.gpgsign", "false"},
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
	}
	if token != "" {
		cfg = append(cfg, [2]string{"http.extraHeader", authHeader(token)})
	}
	env := append([]string(nil), g.base...)
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https:http:file",
		"GIT_AUTHOR_NAME="+botName, "GIT_AUTHOR_EMAIL="+botEmail,
		"GIT_COMMITTER_NAME="+botName, "GIT_COMMITTER_EMAIL="+botEmail,
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)),
	)
	if g.ceiling != "" {
		env = append(env, "GIT_CEILING_DIRECTORIES="+g.ceiling)
	}
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return append(env, extra...)
}

// version returns the output of git --version.
func (g *gitClient) version(ctx context.Context) (string, error) {
	return g.run(ctx, "", g.env(""), g.bin, "--version")
}

// inCache runs git on the repo cache, named explicitly with --git-dir.
func (g *gitClient) inCache(ctx context.Context, cache string, args ...string) (string, error) {
	return g.run(ctx, cache, g.env(""), g.bin, append([]string{"--git-dir=" + cache}, args...)...)
}

// inCacheAuth is inCache for the commands that talk to the remote.
func (g *gitClient) inCacheAuth(ctx context.Context, cache, token string, args ...string) (string, error) {
	return g.run(ctx, cache, g.env(token), g.bin, append([]string{"--git-dir=" + cache}, args...)...)
}

// lockRepo serializes the operations on one repo cache.
func (g *gitClient) lockRepo(cache string) func() {
	g.mu.Lock()
	l, ok := g.locks[cache]
	if !ok {
		l = &sync.Mutex{}
		g.locks[cache] = l
	}
	g.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// pristineConfig is the config of the repo cache. It is rewritten before the
// cache talks to the remote or snapshots a worktree, so a job cannot leave
// settings (url rewrites, proxies, helpers, filter or diff drivers) behind
// that would capture a token or run agent code.
const pristineConfig = "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"

// prepareCache creates the bare repo cache if needed and removes everything
// a job could have planted in it: the config, attributes, grafts, shallow
// file, exclude list (which hides files from the snapshot) and replace refs
// (which make git show other objects than the ones that are stored). The caller holds the repo lock.
func (g *gitClient) prepareCache(ctx context.Context, cache string) error {
	if _, err := os.Stat(filepath.Join(cache, "HEAD")); err != nil {
		if err := os.MkdirAll(cache, 0o750); err != nil {
			return fmt.Errorf("create repo cache: %w", err)
		}
		if _, err := g.run(ctx, cache, g.env(""), g.bin, "init", "--bare", "--quiet", cache); err != nil {
			return fmt.Errorf("init repo cache: %w", err)
		}
	}
	tmp := filepath.Join(cache, "config.forecrew")
	if err := os.WriteFile(tmp, []byte(pristineConfig), 0o600); err != nil {
		return fmt.Errorf("reset repo cache config: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(cache, "config")); err != nil {
		return fmt.Errorf("reset repo cache config: %w", err)
	}
	for _, f := range []string{"info/grafts", "info/attributes", "info/exclude", "shallow", "config.worktree"} {
		if err := os.Remove(filepath.Join(cache, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reset repo cache: %w", err)
		}
	}
	refs, err := g.inCache(ctx, cache, "for-each-ref", "--format=%(refname)", "refs/replace/")
	if err != nil {
		return fmt.Errorf("list replace refs: %w", err)
	}
	for ref := range strings.SplitSeq(refs, "\n") {
		if ref = strings.TrimSpace(ref); ref != "" {
			if _, err := g.inCache(ctx, cache, "update-ref", "-d", ref); err != nil {
				return fmt.Errorf("delete replace ref: %w", err)
			}
		}
	}
	return nil
}

// checkout describes the workspace of one job.
type checkout struct {
	URL     string // clone URL of the repo, without credentials
	Token   string // repo token, used by the worker's own git calls only
	Cache   string // bare repo cache of the repo
	Dir     string // worktree to create
	BaseRef string // branch or full SHA
	Branch  string // branch to continue or create; may be empty
	Write   bool   // a writing role: Branch is checked out as a local branch
}

// prepare fetches the refs of c into the cache and adds a fresh worktree at
// c.Dir. A writing role continues Branch when the remote has it and creates
// it from BaseRef otherwise; a read-only role gets a detached checkout of
// Branch or, without one, of BaseRef. It returns the commit checked out and
// whether Branch exists on the remote.
func (g *gitClient) prepare(ctx context.Context, c checkout) (start string, remoteBranch bool, err error) {
	unlock := g.lockRepo(c.Cache)
	defer unlock()
	if err := g.prepareCache(ctx, c.Cache); err != nil {
		return "", false, err
	}
	baseRefspec, startRef := "+refs/heads/"+c.BaseRef+":"+remoteRefsBase+c.BaseRef, remoteRefsBase+c.BaseRef
	if fullSHA.MatchString(c.BaseRef) {
		baseRefspec, startRef = c.BaseRef, c.BaseRef
	}
	refspecs := []string{baseRefspec}
	if c.Branch != "" {
		out, err := g.inCacheAuth(ctx, c.Cache, c.Token, "ls-remote", "--heads", c.URL, "refs/heads/"+c.Branch)
		if err != nil {
			return "", false, fmt.Errorf("list remote branch: %w", err)
		}
		remoteBranch = strings.TrimSpace(out) != ""
		if remoteBranch {
			refspecs = append(refspecs, "+refs/heads/"+c.Branch+":"+remoteRefsBase+c.Branch)
		}
	}
	if _, err := g.inCacheAuth(ctx, c.Cache, c.Token, append([]string{"fetch", "--quiet", "--no-tags", c.URL}, refspecs...)...); err != nil {
		return "", false, fmt.Errorf("fetch: %w", err)
	}
	if remoteBranch {
		startRef = remoteRefsBase + c.Branch
	}
	if _, err := g.inCache(ctx, c.Cache, "worktree", "prune"); err != nil {
		return "", false, fmt.Errorf("prune worktrees: %w", err)
	}
	args := []string{"worktree", "add", "--quiet", "--detach", c.Dir, startRef}
	if c.Write {
		args = []string{"worktree", "add", "--quiet", "--no-track", "-B", c.Branch, c.Dir, startRef}
	}
	if _, err := g.inCache(ctx, c.Cache, args...); err != nil {
		return "", false, fmt.Errorf("add worktree: %w", err)
	}
	out, err := g.inCache(ctx, c.Cache, "rev-parse", "--verify", startRef+"^{commit}")
	return strings.TrimSpace(out), remoteBranch, err
}

// indexPath is the private index the worker uses to snapshot a worktree; it
// lives next to the worktree, not in it.
func indexPath(dir string) string { return dir + ".index" }

// remove deletes the worktree dir of a job and its registration in the
// cache, and nothing else.
func (g *gitClient) remove(ctx context.Context, cache, dir string) {
	unlock := g.lockRepo(cache)
	defer unlock()
	_ = os.RemoveAll(dir)
	_ = os.Remove(indexPath(dir))
	_, _ = g.inCache(ctx, cache, "worktree", "prune")
}

// snapshot stages the files of the worktree on top of commit start and
// returns the resulting tree. It never looks at the worktree's own .git (the
// agent may have deleted or rewritten it, or committed anything it liked): the
// cache is the git dir, the worktree directory only the work tree, and the
// index is private. Ignored files are not included.
func (g *gitClient) snapshot(ctx context.Context, cache, dir, start string) (string, error) {
	unlock := g.lockRepo(cache)
	defer unlock()
	if err := g.prepareCache(ctx, cache); err != nil {
		return "", err
	}
	idx := indexPath(dir)
	if err := os.Remove(idx); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reset index: %w", err)
	}
	run := func(args ...string) (string, error) {
		full := append([]string{"--git-dir=" + cache, "--work-tree=" + dir}, args...)
		return g.run(ctx, dir, g.env("", "GIT_INDEX_FILE="+idx), g.bin, full...)
	}
	if _, err := run("read-tree", start); err != nil {
		return "", fmt.Errorf("read start tree: %w", err)
	}
	if _, err := run("add", "--all", "--no-warn-embedded-repo"); err != nil {
		return "", fmt.Errorf("%w: %w", errStaging, err)
	}
	out, err := run("write-tree")
	if err != nil {
		return "", fmt.Errorf("write tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// change is one path that differs between two trees.
type change struct {
	Path string
	Mode string // mode after the change; 000000 for a deletion
}

// changes lists the paths that differ between two commits or trees; renames
// count as a delete and an add.
func (g *gitClient) changes(ctx context.Context, cache, from, to string) ([]change, error) {
	out, err := g.inCache(ctx, cache, "diff-tree", "-r", "-z", "--no-renames", from, to)
	if err != nil {
		return nil, fmt.Errorf("list changed paths: %w", err)
	}
	// -z output: ":<old mode> <new mode> <old sha> <new sha> <status>\0<path>\0" ...
	fields := strings.Split(out, "\x00")
	var cs []change
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			return nil, fmt.Errorf("list changed paths: unexpected diff-tree output %q", fields[i])
		}
		cs = append(cs, change{Path: fields[i+1], Mode: meta[1]})
	}
	return cs, nil
}

// commitTree creates the commit the worker publishes: tree on top of parent,
// by the bot.
func (g *gitClient) commitTree(ctx context.Context, cache, tree, parent string) (string, error) {
	unlock := g.lockRepo(cache)
	defer unlock()
	out, err := g.inCache(ctx, cache, "commit-tree", tree, "-p", parent, "-m", commitMessage)
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// diffStat returns the one-line summary of the changes between two commits.
func (g *gitClient) diffStat(ctx context.Context, cache, from, to string) (string, error) {
	out, err := g.inCache(ctx, cache, "diff", "--shortstat", "--no-ext-diff", "--no-textconv", from, to)
	return strings.TrimSpace(out), err
}

func paths(cs []change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Path
	}
	return out
}

// push sends commit sha to refs/heads/<branch> of url. The branch must be an
// agent/ branch; anything else is refused before git runs. Pushes are fast
// forward only.
func (g *gitClient) push(ctx context.Context, cache, url, token, sha, branch string) error {
	ref, err := pushRef(branch)
	if err != nil {
		return err
	}
	if !fullSHA.MatchString(sha) {
		return fmt.Errorf("push %q: not a full commit sha: %w", sha, errRefused)
	}
	unlock := g.lockRepo(cache)
	defer unlock()
	if err := g.prepareCache(ctx, cache); err != nil {
		return err
	}
	if _, err := g.inCacheAuth(ctx, cache, token, "push", "--quiet", url, sha+":"+ref); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}
