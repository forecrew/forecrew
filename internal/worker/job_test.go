package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

func edit(path, content string) runtime.ScriptEdit {
	return runtime.ScriptEdit{Path: path, Content: content}
}

func TestJobHappyPath(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{
		Events: []runtime.ScriptEvent{{Type: domain.EventMessage, Text: "adding the handler with key " + testAPIKey}},
		Edits:  []runtime.ScriptEdit{edit("src/hello.go", "package src\n")},
		Output: json.RawMessage(`{"summary":"done"}`), Usage: domain.Usage{InputTokens: 10, OutputTokens: 5}, CostUSD: 0.5,
	}))
	main := remoteSHA(t, e.remote, "main")
	s := e.spec(func(s *domain.JobSpec) { s.OutputSchema = json.RawMessage(`{"type":"object","required":["summary"]}`) })
	c := e.doComplete(s)

	if c.Status != StatusSucceeded || c.Error != "" {
		t.Fatalf("completion = %+v", c)
	}
	if string(c.Output) != `{"summary":"done"}` || c.Usage.InputTokens != 10 || c.CostUSD != 0.5 {
		t.Errorf("completion output/usage = %s %+v %v", c.Output, c.Usage, c.CostUSD)
	}
	if got := remoteSHA(t, e.remote, "agent/task-1"); got == "" || got != c.HeadSHA {
		t.Errorf("remote branch = %q, head_sha = %q", got, c.HeadSHA)
	}
	if got := runGit(t, e.remote, "show", "agent/task-1:src/hello.go"); got != "package src" {
		t.Errorf("pushed file = %q", got)
	}
	if got := runGit(t, e.remote, "log", "-1", "--format=%an <%ae>", "agent/task-1"); got != botName+" <"+botEmail+">" {
		t.Errorf("author = %q", got)
	}
	if got := remoteSHA(t, e.remote, "main"); got != main {
		t.Errorf("main moved to %s", got)
	}
	if got, want := e.api.statuses(s.JobID), []string{"cloning", "running", "checking", "pushing"}; !slices.Equal(got, want) {
		t.Errorf("status events = %v, want %v", got, want)
	}
	var sawMessage, sawDiffstat bool
	for _, ev := range e.api.eventsOf(s.JobID) {
		sawMessage = sawMessage || (ev.Type == domain.EventMessage && strings.Contains(ev.Text, "adding the handler") && strings.Contains(ev.Text, "[REDACTED]"))
		sawDiffstat = sawDiffstat || (ev.Type == domain.EventLog && strings.Contains(ev.Text, "1 file changed"))
	}
	if !sawMessage || !sawDiffstat {
		t.Errorf("message (redacted) = %v, diffstat = %v in %v", sawMessage, sawDiffstat, e.api.eventsOf(s.JobID))
	}
	if _, err := os.Stat(filepath.Join(e.workDir, "jobs", s.JobID.String())); !os.IsNotExist(err) {
		t.Errorf("job workspace still exists: %v", err)
	}
	e.noSecrets()
}

func TestJobWriteViolations(t *testing.T) {
	tests := []struct {
		name  string
		edits []runtime.ScriptEdit
		paths []string
		want  string
	}{
		{"excluded dir", []runtime.ScriptEdit{edit("ok.txt", "x"), edit(".github/workflows/ci.yml", "x")}, []string{"**", "!.github/**"}, ".github/workflows/ci.yml"},
		{"outside include", []runtime.ScriptEdit{edit("src/a.go", "x"), edit("main.go", "x")}, []string{"src/**"}, "main.go"},
		{"deleting a protected file", []runtime.ScriptEdit{{Path: "docs/guide.md", Delete: true}}, []string{"src/**"}, "docs/guide.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, scriptAdapter(t, runtime.Script{Edits: tt.edits}))
			c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.WritePaths = tt.paths }))
			if c.Status != StatusFailed || !strings.Contains(c.Error, tt.want) || c.HeadSHA != "" {
				t.Errorf("completion = %+v", c)
			}
			if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
				t.Errorf("branch was pushed: %s", got)
			}
		})
	}
}

func TestJobRefusesNonAgentBranchBeforeGit(t *testing.T) {
	for _, branch := range []string{"main", "refs/heads/main", "agent/../main", "", "release/1", "-x"} {
		t.Run(branch, func(t *testing.T) {
			e := newEnv(t, scriptAdapter(t, runtime.Script{Edits: []runtime.ScriptEdit{edit("a.txt", "x")}}))
			rec := &recorder{}
			e.w.git = newGitClient("git", rec.run, nil, "")
			c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.Branch = branch }))
			if c.Status != StatusFailed || !strings.Contains(c.Error, "refused") {
				t.Errorf("completion = %+v", c)
			}
			if len(rec.calls) != 0 {
				t.Errorf("git ran: %v", rec.calls)
			}
		})
	}
}

func TestJobInvalidSpec(t *testing.T) {
	for name, mod := range map[string]func(*domain.JobSpec){
		"repo with dots": func(s *domain.JobSpec) { s.Repo = "../etc" },
		"option as base": func(s *domain.JobSpec) { s.BaseRef = "--upload-pack=x" },
		"read-only option as branch": func(s *domain.JobSpec) {
			s.WritePaths, s.Branch = nil, "-x"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, scriptAdapter(t, runtime.Script{}))
			rec := &recorder{}
			e.w.git = newGitClient("git", rec.run, nil, "")
			if c := e.doComplete(e.spec(mod)); c.Status != StatusFailed || !strings.Contains(c.Error, "invalid") {
				t.Errorf("completion = %+v", c)
			}
			if len(rec.calls) != 0 {
				t.Errorf("git ran: %v", rec.calls)
			}
		})
	}
}

func TestJobReadOnly(t *testing.T) {
	tests := []struct {
		name       string
		edits      []runtime.ScriptEdit
		branch     string
		wantStatus string
		wantPath   string
	}{
		{name: "no changes", wantStatus: StatusSucceeded},
		{name: "new file", edits: []runtime.ScriptEdit{edit("notes.md", "x")}, wantStatus: StatusFailed, wantPath: "notes.md"},
		{name: "edited file", edits: []runtime.ScriptEdit{edit("README.md", "x")}, wantStatus: StatusFailed, wantPath: "README.md"},
		{name: "branch set, no changes", branch: "agent/task-9", wantStatus: StatusSucceeded},
		{name: "branch set, new file", branch: "agent/task-9", edits: []runtime.ScriptEdit{edit("notes.md", "x")}, wantStatus: StatusFailed, wantPath: "notes.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, scriptAdapter(t, runtime.Script{Edits: tt.edits, Output: json.RawMessage(`{"verdict":"ok"}`)}))
			if tt.branch != "" {
				seed := t.TempDir()
				runGit(t, seed, "clone", "--quiet", e.remote, ".")
				runGit(t, seed, "switch", "--quiet", "-c", tt.branch)
				writeFile(t, seed, "feature.txt", "f\n")
				runGit(t, seed, "add", "--all")
				runGit(t, seed, "commit", "--quiet", "-m", "feature")
				runGit(t, seed, "push", "--quiet", "origin", tt.branch)
			}
			before := runGit(t, e.remote, "for-each-ref", "--format=%(refname) %(objectname)")
			s := e.spec(func(s *domain.JobSpec) { s.WritePaths, s.Branch, s.Role = nil, tt.branch, "reviewer" })
			c := e.doComplete(s)
			if c.Status != tt.wantStatus || c.HeadSHA != "" {
				t.Fatalf("completion = %+v", c)
			}
			if tt.wantPath != "" && !strings.Contains(c.Error, tt.wantPath) {
				t.Errorf("error = %q, want the path %q", c.Error, tt.wantPath)
			}
			if after := runGit(t, e.remote, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
				t.Errorf("remote refs changed:\n%s\n--\n%s", before, after)
			}
			if statuses := e.api.statuses(s.JobID); slices.Contains(statuses, "pushing") {
				t.Errorf("a read-only job pushed: %v", statuses)
			}
		})
	}
}

func TestJobReadOnlyCommit(t *testing.T) {
	// A read-only agent with a shell commits: the checkout moved, so it fails.
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		writeFile(t, spec.WorkDir, "x.txt", "x")
		runGit(t, spec.WorkDir, "add", "--all")
		runGit(t, spec.WorkDir, "commit", "--quiet", "-m", "sneaky")
		return runtime.Result{}, nil
	}})
	c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.WritePaths = nil }))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "x.txt") {
		t.Errorf("completion = %+v", c)
	}
	if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
		t.Errorf("branch pushed: %s", got)
	}
}

func TestJobContinuesExistingBranch(t *testing.T) {
	first := scriptAdapter(t, runtime.Script{Edits: []runtime.ScriptEdit{edit("one.txt", "1")}})
	e := newEnv(t, first)
	s := e.spec()
	c1 := e.doComplete(s)
	if c1.Status != StatusSucceeded || c1.HeadSHA == "" {
		t.Fatalf("first run = %+v", c1)
	}

	// The same job runs again (a retry) and adds more work on top.
	e.cfg.Runtimes = runtime.NewRegistry(scriptAdapter(t, runtime.Script{Edits: []runtime.ScriptEdit{edit("two.txt", "2")}}))
	e.rebuild()
	s.JobID = domain.NewID()
	c2 := e.doComplete(s)
	if c2.Status != StatusSucceeded || c2.HeadSHA == "" || c2.HeadSHA == c1.HeadSHA {
		t.Fatalf("second run = %+v", c2)
	}
	if got := runGit(t, e.remote, "rev-list", "--count", "main..agent/task-1"); got != "2" {
		t.Errorf("commits on the branch = %s, want 2", got)
	}
	if got := runGit(t, e.remote, "merge-base", "--is-ancestor", c1.HeadSHA, c2.HeadSHA); got != "" {
		t.Errorf("merge-base: %s", got)
	}
	for _, f := range []string{"one.txt", "two.txt"} {
		if got := runGit(t, e.remote, "cat-file", "-t", "agent/task-1:"+f); got != "blob" {
			t.Errorf("%s missing from the branch", f)
		}
	}

	// A third run that changes nothing pushes nothing but still reports the
	// head of the branch the remote has.
	e.cfg.Runtimes = runtime.NewRegistry(scriptAdapter(t, runtime.Script{}))
	e.rebuild()
	s.JobID = domain.NewID()
	if c3 := e.doComplete(s); c3.Status != StatusSucceeded || c3.HeadSHA != c2.HeadSHA {
		t.Errorf("third run = %+v, want head_sha %s", c3, c2.HeadSHA)
	}
	if got := remoteSHA(t, e.remote, "agent/task-1"); got != c2.HeadSHA {
		t.Errorf("branch = %s, want %s", got, c2.HeadSHA)
	}
	e.noSecrets()
}

func TestJobPublishesOneCommitOfItsOwn(t *testing.T) {
	var start string
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		start = runGit(t, spec.WorkDir, "rev-parse", "HEAD")
		writeFile(t, spec.WorkDir, "a.txt", "a")
		runGit(t, spec.WorkDir, "add", "--all")
		runGit(t, spec.WorkDir, "commit", "--quiet", "-m", "mine")
		writeFile(t, spec.WorkDir, "b.txt", "b") // left uncommitted
		return runtime.Result{}, nil
	}})
	c := e.doComplete(e.spec())
	if c.Status != StatusSucceeded || c.HeadSHA == "" {
		t.Fatalf("completion = %+v", c)
	}
	if got := runGit(t, e.remote, "rev-list", "--count", "main..agent/task-1"); got != "1" {
		t.Errorf("commits = %s, want one commit of the worker", got)
	}
	if got := runGit(t, e.remote, "log", "-1", "--format=%an %cn %P %s", "agent/task-1"); got != botName+" "+botName+" "+start+" "+commitMessage {
		t.Errorf("commit = %q", got)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if got := runGit(t, e.remote, "cat-file", "-t", "agent/task-1:"+f); got != "blob" {
			t.Errorf("%s missing from the branch", f)
		}
	}
}

// The agent commits a file outside its write paths and deletes it again in a
// later commit: the diff between the first and the last commit is clean, but
// the file would be in the pushed history.
func TestJobForbiddenContentInAgentHistoryIsNotPushed(t *testing.T) {
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		writeFile(t, spec.WorkDir, ".github/workflows/evil.yml", "evil\n")
		runGit(t, spec.WorkDir, "add", "--all")
		runGit(t, spec.WorkDir, "commit", "--quiet", "-m", "add")
		runGit(t, spec.WorkDir, "rm", "--quiet", "-r", ".github")
		runGit(t, spec.WorkDir, "commit", "--quiet", "-m", "remove")
		writeFile(t, spec.WorkDir, "ok.txt", "ok\n")
		return runtime.Result{}, nil
	}})
	c := e.doComplete(e.spec())
	if c.Status != StatusSucceeded || c.HeadSHA == "" {
		t.Fatalf("completion = %+v", c)
	}
	if got := runGit(t, e.remote, "log", "--all", "--format=%H", "--", ".github"); got != "" {
		t.Errorf("the forbidden file is in the pushed history: %s", got)
	}
	if got := runGit(t, e.remote, "rev-list", "--count", "main..agent/task-1"); got != "1" {
		t.Errorf("commits = %s, want 1", got)
	}
}

// A different history is irrelevant: the worker publishes the files.
func TestJobAgentRewritingHistoryChangesNothing(t *testing.T) {
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		runGit(t, spec.WorkDir, "checkout", "--quiet", "--orphan", "other")
		writeFile(t, spec.WorkDir, "z.txt", "z")
		runGit(t, spec.WorkDir, "add", "z.txt")
		runGit(t, spec.WorkDir, "commit", "--quiet", "-m", "unrelated")
		return runtime.Result{}, nil
	}})
	c := e.doComplete(e.spec())
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if got := runGit(t, e.remote, "rev-list", "--count", "main..agent/task-1"); got != "1" {
		t.Errorf("commits = %s, want 1 on top of main", got)
	}
}

func TestJobRefusesSymlinksAndSubmodules(t *testing.T) {
	tests := []struct {
		name string
		make func(dir string)
		want string
	}{
		{"symlink", func(dir string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
				t.Skip("symlinks are not available")
			}
		}, "link"},
		{"symlink replacing a file", func(dir string) {
			_ = os.Remove(filepath.Join(dir, "README.md"))
			if err := os.Symlink("/etc/passwd", filepath.Join(dir, "README.md")); err != nil {
				t.Skip("symlinks are not available")
			}
		}, "README.md"},
		{"nested repository", func(dir string) {
			runGit(t, dir, "init", "--quiet", "sub")
			writeFile(t, dir, "sub/x.txt", "x")
			runGit(t, filepath.Join(dir, "sub"), "add", "--all")
			runGit(t, filepath.Join(dir, "sub"), "commit", "--quiet", "-m", "inner")
		}, "sub"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
				writeFile(t, spec.WorkDir, "ok.txt", "ok")
				tt.make(spec.WorkDir)
				return runtime.Result{}, nil
			}})
			c := e.doComplete(e.spec())
			if c.Status != StatusFailed || !strings.Contains(c.Error, "symlinks and submodules") || !strings.Contains(c.Error, tt.want) || c.HeadSHA != "" {
				t.Errorf("completion = %+v", c)
			}
			if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
				t.Errorf("branch pushed: %s", got)
			}
		})
	}
}

func TestJobAgentFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{"run failed", fmt.Errorf("%w: boom with %s", runtime.ErrRunFailed, testAPIKey), StatusFailed},
		{"timeout", fmt.Errorf("%w after 1m", runtime.ErrTimeout), StatusFailed},
		{"invalid output", runtime.ErrInvalidOutput, StatusFailed},
		{"cli not installed", runtime.ErrNotInstalled, StatusRetry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
				writeFile(t, spec.WorkDir, "partial.txt", "x")
				return runtime.Result{Usage: domain.Usage{InputTokens: 7}, CostUSD: 0.1}, tt.err
			}})
			c := e.doComplete(e.spec())
			if c.Status != tt.wantStatus || c.Error == "" || c.Usage.InputTokens != 7 || c.HeadSHA != "" {
				t.Errorf("completion = %+v", c)
			}
			if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
				t.Errorf("a failed job pushed: %s", got)
			}
			e.noSecrets()
		})
	}
}

func TestJobInfrastructureFailures(t *testing.T) {
	t.Run("clone fails", func(t *testing.T) {
		e := newEnv(t, scriptAdapter(t, runtime.Script{}))
		e.cfg.CloneURL = func(string) string { return filepath.Join(t.TempDir(), "gone.git") }
		e.rebuild()
		if c := e.doComplete(e.spec()); c.Status != StatusRetry || c.Error == "" {
			t.Errorf("completion = %+v", c)
		}
	})
	t.Run("unknown runtime", func(t *testing.T) {
		e := newEnv(t, scriptAdapter(t, runtime.Script{}))
		if c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.Runtime = "codex" })); c.Status != StatusRetry {
			t.Errorf("completion = %+v", c)
		}
	})
	t.Run("push fails", func(t *testing.T) {
		var e *testEnv
		e = newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
			writeFile(t, spec.WorkDir, "a.txt", "a")
			// Someone else advanced the branch meanwhile.
			seed := t.TempDir()
			runGit(t, seed, "clone", "--quiet", e.remote, ".")
			runGit(t, seed, "switch", "--quiet", "-c", "agent/task-1")
			writeFile(t, seed, "other.txt", "o")
			runGit(t, seed, "add", "--all")
			runGit(t, seed, "commit", "--quiet", "-m", "other")
			runGit(t, seed, "push", "--quiet", "origin", "agent/task-1")
			return runtime.Result{}, nil
		}})
		if c := e.doComplete(e.spec()); c.Status != StatusRetry || !strings.Contains(c.Error, "push") {
			t.Errorf("completion = %+v", c)
		}
	})
}

func TestJobRepoFacts(t *testing.T) {
	var prompt string
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		prompt = spec.Prompt
		return runtime.Result{}, nil
	}})
	e.analyzer.Facts = analyzer.Facts{Name: "app", Manifests: []string{"go.mod"}}
	s := e.spec(func(s *domain.JobSpec) { s.IncludeRepoFacts = true })
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if !strings.HasPrefix(prompt, "implement it") || !strings.Contains(prompt, "## Repo facts\n\n```json\n") || !strings.Contains(prompt, `"go.mod"`) {
		t.Errorf("prompt = %q", prompt)
	}
	if dirs := e.analyzer.Dirs(); len(dirs) != 1 || filepath.Base(dirs[0]) != s.JobID.String() {
		t.Errorf("analyzed dirs = %v", dirs)
	}
	if got, want := e.api.statuses(s.JobID), "cloning,analyzing,running,checking"; !strings.HasPrefix(strings.Join(got, ","), want) {
		t.Errorf("statuses = %v\n%s", got, e.logs.String())
	}

	e.analyzer.Err = errors.New("boom")
	if c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.IncludeRepoFacts = true })); c.Status != StatusRetry {
		t.Errorf("analyzer failure completion = %+v", c)
	}
}

func TestJobPassesSpecToAdapter(t *testing.T) {
	var got runtime.RunSpec
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		got = spec
		return runtime.Result{}, nil
	}})
	s := e.spec(func(s *domain.JobSpec) {
		s.OutputSchema, s.MaxTurns, s.Timeout = json.RawMessage(`{"type":"object"}`), 12, domain.Duration(90*time.Second)
	})
	e.do(s)
	if got.Model != "test-model" || got.MaxTurns != 12 || got.Timeout != 90*time.Second || string(got.OutputSchema) != `{"type":"object"}` ||
		got.Env["ANTHROPIC_API_KEY"] != testAPIKey || !slices.Equal(got.AllowedTools, []string{"Read", "Edit"}) ||
		filepath.Base(got.WorkDir) != s.JobID.String() {
		t.Errorf("RunSpec = %+v", got)
	}
}

// blockingAdapter blocks until the context ends and records that it did.
func blockingAdapter(started chan<- struct{}, stopped chan<- error) funcAdapter {
	return funcAdapter{fn: func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		writeFileQuiet(spec.WorkDir, "partial.txt")
		started <- struct{}{}
		<-ctx.Done()
		stopped <- ctx.Err()
		return runtime.Result{}, ctx.Err()
	}}
}

func writeFileQuiet(dir, name string) { _ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600) }

// answerOnceRunning answers heartbeats with continue until the agent has
// started (a value arrives on started), then with status and action.
func answerOnceRunning(started <-chan struct{}, answer func() (int, string)) func(uuid.UUID, int) (int, string) {
	var ran atomic.Bool
	return func(uuid.UUID, int) (int, string) {
		if !ran.Load() {
			select {
			case <-started:
				ran.Store(true)
			default:
				return 200, "continue"
			}
		}
		return answer()
	}
}

func TestHeartbeatStopsTheJob(t *testing.T) {
	tests := []struct {
		name   string
		answer func(domain.JobSpec) (int, string)
	}{
		{"cancel", func(domain.JobSpec) (int, string) { return 200, "cancel" }},
		{"lease lost", func(domain.JobSpec) (int, string) { return 409, "" }},
		{"job gone", func(domain.JobSpec) (int, string) { return 404, "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started, stopped := make(chan struct{}, 1), make(chan error, 1)
			e := newEnv(t, blockingAdapter(started, stopped))
			s := e.spec()
			e.api.heartbeat = answerOnceRunning(started, func() (int, string) { return tt.answer(s) })
			e.do(s)
			select {
			case err := <-stopped:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("adapter stopped with %v", err)
				}
			default:
				t.Fatal("the adapter was not stopped")
			}
			if n := e.api.completionCount(s.JobID); n != 0 {
				t.Errorf("completed %d times, want not at all", n)
			}
			if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
				t.Errorf("a stopped job pushed: %s", got)
			}
			if _, err := os.Stat(filepath.Join(e.workDir, "jobs", s.JobID.String())); !os.IsNotExist(err) {
				t.Errorf("workspace kept: %v", err)
			}
		})
	}
}

func TestHeartbeatErrorsAreTolerated(t *testing.T) {
	var release sync.Once
	released := make(chan struct{})
	e := newEnv(t, funcAdapter{fn: func(ctx context.Context, _ runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		select {
		case <-released:
			return runtime.Result{}, nil
		case <-ctx.Done():
			return runtime.Result{}, ctx.Err()
		}
	}})
	s := e.spec()
	e.api.heartbeat = func(_ uuid.UUID, n int) (int, string) {
		if n >= 6 {
			release.Do(func() { close(released) })
			return 200, "continue"
		}
		return 500, ""
	}
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Errorf("completion = %+v", c)
	}
}

func TestEventsLeaseLostStopsTheJob(t *testing.T) {
	// The first events batch already gets 409: the job stops, whether it is
	// still cloning or already running, and nothing is reported.
	e := newEnv(t, funcAdapter{fn: func(ctx context.Context, _ runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		<-ctx.Done()
		return runtime.Result{}, ctx.Err()
	}})
	e.api.eventStatus = 409
	s := e.spec()
	e.do(s)
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times", n)
	}
	if !strings.Contains(e.logs.String(), "result dropped") {
		t.Error("the dropped result was not logged")
	}
}

func TestEventsAreDroppedWhenTheServerFails(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	e.api.eventStatus = 500
	if c := e.doComplete(e.spec()); c.Status != StatusSucceeded {
		t.Errorf("completion = %+v", c)
	}
	if !strings.Contains(e.logs.String(), "drop job events") {
		t.Error("the dropped events were not logged")
	}
}

func TestEventBufferBatches(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	id := domain.NewID()
	b := &eventBuffer{jobID: id, client: e.cfg.Client, red: runtime.NewRedactor(nil), now: time.Now, log: e.cfg.Logger}
	for i := range 1200 {
		b.text(domain.EventLog, fmt.Sprint(i))
	}
	b.add(domain.JobEvent{Type: domain.EventToolCall, Data: json.RawMessage(`{"input":"` + testRepoToken + `"}`)})
	b.add(domain.JobEvent{Type: domain.EventToolCall, Data: json.RawMessage(`{broken`)})
	if err := b.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := e.api.eventsOf(id)
	if len(got) != 1202 {
		t.Fatalf("events = %d, want 1202", len(got))
	}
	for i, ev := range got[:1200] {
		if ev.Text != fmt.Sprint(i) || ev.Time.IsZero() {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}
	if string(got[1200].Data) != `{"input":"[REDACTED]"}` || len(got[1201].Data) != 0 {
		t.Errorf("data = %s / %s", got[1200].Data, got[1201].Data)
	}
}

func TestJobNothingToPushOnANewBranch(t *testing.T) {
	// No changes and no branch on the remote: nothing is pushed, no head.
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	c := e.doComplete(e.spec())
	if c.Status != StatusSucceeded || c.HeadSHA != "" {
		t.Errorf("completion = %+v", c)
	}
	if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
		t.Errorf("branch pushed: %s", got)
	}
}

// A read-only agent used the cache's info/exclude to hide its change from
// the check that the checkout is unchanged.
func TestJobReadOnlyCannotHideChangesWithAnExcludeFile(t *testing.T) {
	var e *testEnv
	e = newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		writeFile(t, filepath.Join(e.workDir, "repos", "acme", "app.git"), "info/exclude", "*\n")
		writeFile(t, spec.WorkDir, "hidden.txt", "x")
		return runtime.Result{}, nil
	}})
	c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.WritePaths = nil }))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "hidden.txt") {
		t.Errorf("completion = %+v", c)
	}
}

// Files git refuses to stage fail the job; they must not make it retry until
// the attempts are used up.
// readOnlyIf makes a job spec read-only when ro is set.
func readOnlyIf(ro bool) func(*domain.JobSpec) {
	return func(s *domain.JobSpec) {
		if ro {
			s.WritePaths = nil
		}
	}
}

func TestJobStagingErrorsFailInsteadOfRetrying(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, dir string)
	}{
		{"reserved git dir name", func(t *testing.T, dir string) { writeFile(t, dir, ".GIT/config", "[core]\n") }},
		{"unreadable file", func(t *testing.T, dir string) {
			if os.Geteuid() == 0 {
				t.Skip("root can read everything")
			}
			writeFile(t, dir, "secret.txt", "x")
			if err := os.Chmod(filepath.Join(dir, "secret.txt"), 0); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
				tt.make(t, spec.WorkDir)
				return runtime.Result{}, nil
			}})
			for _, readOnly := range []bool{false, true} {
				c := e.doComplete(e.spec(readOnlyIf(readOnly)))
				if c.Status != StatusFailed || !strings.Contains(c.Error, "cannot stage") {
					t.Errorf("read-only=%v: completion = %+v", readOnly, c)
				}
			}
			if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
				t.Errorf("branch pushed: %s", got)
			}
		})
	}
}

func TestJobInvalidWritePaths(t *testing.T) {
	for _, patterns := range [][]string{{"**", "![.github/**"}, {"[a"}, {"**", "!{a,b"}} {
		e := newEnv(t, scriptAdapter(t, runtime.Script{Edits: []runtime.ScriptEdit{edit(".github/workflows/ci.yml", "x")}}))
		rec := &recorder{}
		e.w.git = newGitClient("git", rec.run, nil, "")
		c := e.doComplete(e.spec(func(s *domain.JobSpec) { s.WritePaths = patterns }))
		if c.Status != StatusFailed || !strings.Contains(c.Error, "invalid write_paths pattern") {
			t.Errorf("%v: completion = %+v", patterns, c)
		}
		if len(rec.calls) != 0 {
			t.Errorf("%v: git ran: %v", patterns, rec.calls)
		}
	}
}

// The cancel arrives after the agent is done and before the next periodic
// heartbeat: the heartbeat right before the push must still stop the job.
func TestCancelBeforePushStopsThePush(t *testing.T) {
	e := newEnv(t, editAndWait(t, 0))
	e.w.cfg.HeartbeatInterval = time.Hour // only the heartbeat before the push can see the cancel
	e.api.heartbeat = func(uuid.UUID, int) (int, string) { return 200, "cancel" }
	s := e.spec()
	e.do(s)
	if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
		t.Errorf("a canceled job pushed %s", got)
	}
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times, want not at all", n)
	}
	if !strings.Contains(e.logs.String(), "canceled by forecrewd") {
		t.Errorf("the cancellation was not logged:\n%s", e.logs)
	}
}

// A heartbeat that fails before the push does not stop a job whose lease is
// still good; one that answers lease lost does.
func TestHeartbeatBeforePushErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		pushed bool
	}{{"server error", 500, true}, {"lease lost", 409, false}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, editAndWait(t, 0))
			e.w.cfg.HeartbeatInterval = time.Hour
			e.api.heartbeat = func(uuid.UUID, int) (int, string) { return tt.status, "" }
			e.do(e.spec())
			if got := remoteSHA(t, e.remote, "agent/task-1"); (got != "") != tt.pushed {
				t.Errorf("pushed = %q, want pushed %v", got, tt.pushed)
			}
		})
	}
}

// A canceled job is gone for the server: its events would be refused, so the
// worker does not flush them.
func TestCanceledJobDoesNotFlushEvents(t *testing.T) {
	started := make(chan struct{}, 1)
	e := newEnv(t, funcAdapter{fn: func(ctx context.Context, _ runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
		emit(domain.JobEvent{Type: domain.EventLog, Text: "working", Time: time.Now()})
		started <- struct{}{}
		<-ctx.Done()
		return runtime.Result{}, ctx.Err()
	}})
	e.w.cfg.FlushInterval = time.Hour // only a final flush would send the event
	e.api.heartbeat = answerOnceRunning(started, func() (int, string) { return 200, "cancel" })
	s := e.spec()
	e.do(s)
	e.api.mu.Lock()
	n := len(e.api.events[s.JobID])
	e.api.mu.Unlock()
	if n != 0 {
		t.Errorf("%d events sent for a canceled job", n)
	}
	if !stopsJob(errJobCanceled) {
		t.Error("errJobCanceled does not stop the job")
	}
}
