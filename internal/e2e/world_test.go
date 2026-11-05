//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew/internal/app"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/worker"
)

const (
	repo       = "acme/app"
	hookSecret = "hook-secret"
	// apiKey is the runtime credential of the claude runtime; no log may hold it.
	apiKey   = "sk-ant-e2e-0123456789-credential"
	apiToken = "fcm_e2e-token"
	// waitFor bounds every wait of a scenario.
	waitFor = 45 * time.Second
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// eventually polls cond until it holds.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// logBuffer is a bytes.Buffer safe for concurrent writers.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shiftClock is the real time plus an offset the test can move forward, for
// waits that GitHub and forecrewd count in minutes.
type shiftClock struct{ offset atomic.Int64 }

func (c *shiftClock) Now() time.Time { return time.Now().UTC().Add(time.Duration(c.offset.Load())) }

func (c *shiftClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

func testConfig() *config.Config {
	return &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0", PublicBaseURL: "http://localhost:8080"},
		GitHub: config.GitHub{AppID: 1, WebhookSecret: hookSecret, Allowlist: []string{"octocat"}},
		Runtimes: map[string]config.Runtime{
			"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic", Credentials: map[string]string{"ANTHROPIC_API_KEY": apiKey}},
			"codex":  {Adapter: config.AdapterCodex, Vendor: "openai"},
		},
		ModelTiers: map[config.Tier]map[string]string{
			config.TierStrong: {"anthropic": "claude-strong", "openai": "gpt-strong"},
			config.TierBest:   {"anthropic": "claude-best", "openai": "gpt-best"},
		},
		Roles: map[string]config.Role{
			"onboarding-analyst": {Runtime: "claude", Tier: config.TierStrong},
			"product-owner":      {Runtime: "claude", Tier: config.TierStrong},
			"architect":          {Runtime: "claude", Tier: config.TierBest},
			"developer":          {Runtime: "claude", Tier: config.TierStrong},
			"reviewer":           {Runtime: "codex", Tier: config.TierBest},
		},
	}
}

// world is a running forecrewd with its Postgres, its fake GitHub and the
// repo acme/app. The test plays the human; workers are started with
// startWorker.
type world struct {
	t       *testing.T
	pool    *pgxpool.Pool
	st      store.Store
	clock   *shiftClock
	tracker *testkit.Tracker
	remote  string
	url     string
	account domain.Account
	ag      *agents
	hooks   chan testkit.Event
	logs    *logBuffer
	ci      *ciSim
	// actions is the fake GitHub Actions the provider of pool actions talks to.
	actions *testkit.FakeActions
}

// newWorld starts forecrewd. The project app is registered, and onboarded
// when onboarded is set.
func newWorld(t *testing.T, onboarded bool) *world {
	t.Helper()
	return newWorldWith(t, onboarded, nil)
}

// newWorldWith is newWorld with the packs forecrewd loads (nil: the
// embedded ones), to run a playbook of the test's own.
func newWorldWith(t *testing.T, onboarded bool, files fs.FS) *world {
	t.Helper()
	return newWorldOn(t, onboarded, files, testConfig())
}

// newWorldOn is newWorldWith with forecrewd's configuration.
func newWorldOn(t *testing.T, onboarded bool, files fs.FS, cfg *config.Config) *world {
	t.Helper()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	w := &world{t: t, pool: pool, st: postgres.New(pool), clock: &shiftClock{}, ag: &agents{scriptDir: t.TempDir()}, hooks: make(chan testkit.Event, 1024), logs: &logBuffer{}}
	ctx := t.Context()
	w.account = domain.Account{Name: "default"}
	must(t, w.st.Accounts().Create(ctx, &w.account))
	must(t, w.st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: w.account.ID, Name: "e2e", TokenHash: mcp.HashToken(apiToken)}))
	status := domain.ProjectRegistered
	if onboarded {
		status = domain.ProjectOnboarded
	}
	must(t, w.st.Projects().Create(ctx, &domain.Project{AccountID: w.account.ID, Name: "app", Repo: repo, DefaultBranch: "main", Status: status}))

	w.remote = newRemote(t)
	w.tracker = testkit.NewTracker(w.clock.Now)
	w.tracker.Install(repo)
	w.actions = testkit.NewFakeActions(t, w.clock.Now)
	must(t, w.tracker.UseRemote(repo, w.remote))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	w.url = "http://" + ln.Addr().String()
	a, err := app.New(app.Deps{
		Config: cfg, Files: files, Evidence: testkit.NewEvidence(), Store: w.st, Ping: pool.Ping, GitHub: w.tracker, Actions: w.actions.Client, Version: "e2e", Now: w.clock.Now, Listener: ln,
		Logger: slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), ReapEvery: 50 * time.Millisecond,
	})
	must(t, err)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("forecrewd: %v", err)
		}
		if t.Failed() {
			t.Logf("forecrewd logs:\n%s", w.logs)
		}
	})

	w.tracker.OnEvent(func(ev testkit.Event) { w.hooks <- ev })
	var bg sync.WaitGroup
	bg.Go(func() { w.deliverHooks(runCtx) })
	w.ci = newCISim(w)
	bg.Go(func() { w.ci.run(runCtx) })
	t.Cleanup(func() { cancel(); bg.Wait() })
	return w
}

// deliverHooks posts the events of the fake GitHub to forecrewd in order, as
// signed webhook deliveries.
func (w *world) deliverHooks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.hooks:
			mac := hmac.New(sha256.New, []byte(hookSecret))
			mac.Write(ev.Payload)
			if !w.post(ctx, mac.Sum(nil), ev) && ctx.Err() == nil {
				w.t.Errorf("webhook %s was dropped after its retries", ev.Kind)
			}
		}
	}
}

// post sends one delivery, retrying while forecrewd does not accept it. It
// reports whether it was accepted.
func (w *world) post(ctx context.Context, sum []byte, ev testkit.Event) bool {
	for range 50 {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url+"/webhooks/github", bytes.NewReader(ev.Payload))
		if err != nil {
			return false
		}
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(sum))
		req.Header.Set("X-GitHub-Delivery", uuid.New().String())
		req.Header.Set("X-GitHub-Event", ev.Kind)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// hermeticGit is the environment of the git commands of the tests.
func hermeticGit() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@example.com",
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
	}
}

func runGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir, cmd.Env = dir, hermeticGit()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRemote creates the bare repo of acme/app with one commit on main.
func newRemote(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", "--quiet", "-b", "main", remote)
	seed := t.TempDir()
	runGit(t, seed, "init", "--quiet", "-b", "main")
	must(t, writeFiles(seed, map[string]string{
		"README.md": "# app\n", "go.mod": "module example.com/app\n\ngo 1.24\n", "main.go": "package main\n\nfunc main() {}\n",
	}))
	runGit(t, seed, "add", "--all")
	runGit(t, seed, "commit", "--quiet", "-m", "initial")
	runGit(t, seed, "push", "--quiet", remote, "main")
	return remote
}

// workerProc is a running worker daemon.
type workerProc struct {
	domain.Worker
	cancel context.CancelFunc
	done   chan error
	logs   *logBuffer
}

// startWorker registers a build worker with the labels (it adds runtime labels
// for the runtimes it has) and runs it against forecrewd. It stops when the
// test ends.
func (w *world) startWorker(name string, labels ...string) *workerProc {
	return w.startWorkerIn(domain.PoolBuild, name, labels...)
}

// startWorkerIn is startWorker for a worker of pool, registered the way an
// admin does with "forecrew worker add <name> --pool <pool>".
func (w *world) startWorkerIn(pool, name string, labels ...string) *workerProc {
	w.t.Helper()
	token := "fcw_" + name + "_" + strings.Repeat("x", 30)
	p := &workerProc{AccountID: w.account.ID, Name: name, Pool: pool, TokenHash: mcp.HashToken(token), MaxJobs: 1, done: make(chan error, 1), logs: &logBuffer{}}
	must(w.t, w.st.Workers().Create(w.t.Context(), &p.Worker))
	client := worker.NewClient(w.url, token, nil)
	client.BackoffMin, client.BackoffMax = 5*time.Millisecond, 50*time.Millisecond
	cfg := worker.Config{
		Client: client, Labels: labels, Version: "e2e", WorkDir: filepath.Join(w.t.TempDir(), "work"), MaxJobs: 1,
		Runtimes: runtime.NewRegistry(fakeRuntime{runtime.ClaudeCode, name, w.ag}, fakeRuntime{runtime.Codex, name, w.ag}, fakeRuntime{runtime.Opencode, name, w.ag}),
		Analyzer: &testkit.Analyzer{}, Logger: slog.New(slog.NewTextHandler(p.logs, nil)), Environ: hermeticGit(),
		CloneURL:          func(string) string { return w.remote },
		HeartbeatInterval: 100 * time.Millisecond, FlushInterval: 20 * time.Millisecond, GracePeriod: 100 * time.Millisecond,
	}
	var ctx context.Context
	ctx, p.cancel = context.WithCancel(context.Background())
	go func() { p.done <- worker.New(cfg).Run(ctx) }()
	w.t.Cleanup(func() { p.kill() })
	return p
}

// kill stops the worker as a crash would for its job: the job is dropped
// without a result and its lease is left to expire.
func (p *workerProc) kill() {
	p.cancel()
	err := <-p.done
	p.done <- err
}

// api calls /api/v1 as the human and returns the status.
func (w *world) api(method, path string, in, out any) int {
	w.t.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		must(w.t, err)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.url+"/api/v1"+path, body)
	must(w.t, err)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(w.t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	must(w.t, err)
	if out != nil && resp.StatusCode < 300 {
		must(w.t, json.Unmarshal(data, out))
	} else if resp.StatusCode >= 300 {
		w.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// gate waits for a pending gate of kind.
func (w *world) gate(kind domain.GateKind) mcp.GateInfo {
	w.t.Helper()
	var found mcp.GateInfo
	eventually(w.t, string(kind)+" gate", func() bool {
		var gates []mcp.GateInfo
		w.api(http.MethodGet, "/gates", nil, &gates)
		for _, g := range gates {
			if g.Kind == kind {
				found = g
				return true
			}
		}
		return false
	})
	return found
}

// decide answers a gate: verb is approve, request-changes or reject.
func (w *world) decide(g mcp.GateInfo, verb, feedback string) {
	w.t.Helper()
	if code := w.api(http.MethodPost, "/gates/"+g.Ref+"/"+verb, map[string]string{"feedback": feedback}, nil); code != http.StatusOK {
		w.t.Fatalf("%s %s: status %d", verb, g.Ref, code)
	}
}

func (w *world) onboard() {
	w.t.Helper()
	if code := w.api(http.MethodPost, "/projects", mcp.OnboardProjectInput{Repo: repo, Name: "app"}, nil); code != http.StatusAccepted {
		w.t.Fatalf("onboard: status %d", code)
	}
}

func (w *world) createGoal(title, body string) domain.Goal {
	w.t.Helper()
	var g domain.Goal
	if code := w.api(http.MethodPost, "/goals", mcp.CreateGoalInput{Project: "app", Title: title, Body: body, Platforms: []string{"linux"}}, &g); code != http.StatusCreated {
		w.t.Fatalf("create goal: status %d", code)
	}
	return g
}

func (w *world) project() domain.Project {
	w.t.Helper()
	p, err := w.st.Projects().GetByName(w.t.Context(), w.account.ID, "app")
	must(w.t, err)
	return p
}

func (w *world) goal(id uuid.UUID) domain.Goal {
	w.t.Helper()
	g, err := w.st.Goals().Get(w.t.Context(), w.account.ID, id)
	must(w.t, err)
	return g
}

func (w *world) tasks() []domain.Task {
	w.t.Helper()
	ts, err := w.st.Tasks().List(w.t.Context(), w.account.ID, store.TaskFilter{})
	must(w.t, err)
	return ts
}

// jobs returns the jobs of the role, oldest first.
func (w *world) jobs(role string) []domain.Job {
	w.t.Helper()
	all, err := w.st.Jobs().List(w.t.Context(), w.account.ID, store.JobFilter{})
	must(w.t, err)
	var out []domain.Job
	for _, a := range slices.Backward(all) {
		if role == "" || a.Role == role {
			out = append(out, a)
		}
	}
	return out
}

func (w *world) waitProject(status domain.ProjectStatus) {
	w.t.Helper()
	eventually(w.t, "project "+string(status), func() bool { return w.project().Status == status })
}

func (w *world) waitGoal(id uuid.UUID, status domain.GoalStatus) {
	w.t.Helper()
	eventually(w.t, "goal "+string(status), func() bool { return w.goal(id).Status == status })
}

// file returns a file of the main branch of the repo on GitHub.
func (w *world) file(path string) (string, error) {
	b, err := w.tracker.GetFile(w.t.Context(), repo, "main", path)
	return string(b), err
}

// mainFile is file for a file that must exist.
func (w *world) mainFile(path string) string {
	w.t.Helper()
	s, err := w.file(path)
	if err != nil {
		w.t.Fatalf("%s on main: %v", path, err)
	}
	return s
}

// reviews returns the review events on a PR.
func (w *world) reviews(number int) []github.ReviewEvent {
	w.t.Helper()
	rs, err := w.tracker.ListReviews(w.t.Context(), repo, number)
	must(w.t, err)
	var out []github.ReviewEvent
	for _, r := range rs {
		out = append(out, r.Event)
	}
	return out
}

// ciSim plays the checks of GitHub: for every open pull request head it
// sets a state chosen by the policy, which emits the check_suite webhook.
type ciSim struct {
	w       *world
	mu      sync.Mutex
	policy  func(branch string, attempt int) github.CheckState
	decided map[string]bool
	heads   map[string]int
}

func newCISim(w *world) *ciSim {
	return &ciSim{w: w, decided: map[string]bool{}, heads: map[string]int{},
		policy: func(string, int) github.CheckState { return github.ChecksSuccess }}
}

// setPolicy chooses the checks of the next heads: attempt counts the heads of
// the branch seen so far, from 0. github.ChecksNone means the repo has no
// checks.
func (c *ciSim) setPolicy(fn func(branch string, attempt int) github.CheckState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policy = fn
}

func (c *ciSim) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Millisecond):
		}
		changes, err := c.w.st.Changes().List(ctx, c.w.account.ID, store.ChangeFilter{State: domain.ChangeOpen})
		if err != nil {
			continue
		}
		for _, ch := range changes {
			c.check(ch)
		}
	}
}

// check decides the checks of the head of an open change once.
func (c *ciSim) check(ch domain.Change) {
	c.mu.Lock()
	if c.decided[ch.HeadSHA] {
		c.mu.Unlock()
		return
	}
	c.decided[ch.HeadSHA] = true
	state := c.policy(ch.Branch, c.heads[ch.Branch])
	c.heads[ch.Branch]++
	c.mu.Unlock()
	if state == github.ChecksNone {
		return
	}
	if err := c.w.tracker.SetCheckStatus(ch.Repo, ch.HeadSHA, state, 1); err != nil {
		c.w.t.Errorf("set checks: %v", err)
	}
}

// waitChange waits for the open change of branch and returns it.
func (w *world) waitChange(branch string) domain.Change {
	w.t.Helper()
	var found domain.Change
	eventually(w.t, "the PR of "+branch, func() bool {
		cs, err := w.st.Changes().List(w.t.Context(), w.account.ID, store.ChangeFilter{State: domain.ChangeOpen})
		must(w.t, err)
		for _, c := range cs {
			if c.Branch == branch {
				found = c
				return true
			}
		}
		return false
	})
	return found
}
