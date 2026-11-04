package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/testreport"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// junit is a report with one pass, one failure and one skip.
const junit = `<?xml version="1.0"?>
<testsuites><testsuite name="unit">
<testcase classname="pkg" name="TestPass" time="0.01"/>
<testcase classname="pkg" name="TestFail" time="0.02"><failure message="want 1, got 2"/></testcase>
<testcase classname="pkg" name="TestSkip"><skipped message="later"/></testcase>
</testsuite></testsuites>
`

// writeJUnit is a shell snippet that writes junit to path.
func writeJUnit(path string) string {
	return "mkdir -p $(dirname " + path + ") && cat > " + path + " <<'EOF'\n" + junit + "EOF\n"
}

// commandJob returns a command job on acme/app; mod may change it.
func (e *testEnv) commandJob(shell string, mod ...func(*domain.JobSpec)) domain.JobSpec {
	s := domain.JobSpec{
		JobID: domain.NewID(), AccountID: domain.NewID(), Kind: domain.JobKindCommand, Repo: "acme/app", BaseRef: "main",
		Timeout: domain.Duration(time.Minute), RepoToken: testRepoToken, Suite: "unit",
		Command: &domain.CommandSpec{Shell: shell, Reports: []string{"reports/*.xml"}},
	}
	for _, m := range mod {
		m(&s)
	}
	return s
}

func onlyRun(t *testing.T, c Completion) TestRun {
	t.Helper()
	if len(c.TestRuns) != 1 {
		t.Fatalf("test runs = %+v (completion %+v)", c.TestRuns, c)
	}
	return c.TestRuns[0]
}

func TestCommandJobReportsTestRun(t *testing.T) {
	e := newCommandEnv(t)
	e.cfg.Environ = append(os.Environ(), "FORECREW_TOKEN=worker-secret", "ANTHROPIC_API_KEY="+testAPIKey, "GITHUB_TOKEN=ghp_workerowntoken",
		"AWS_SECRET_ACCESS_KEY=aws-secret", "LC_ALL=C", "TZ=UTC")
	e.rebuild()
	main := remoteSHA(t, e.remote, "main")
	script := `echo "token=[$FORECREW_TOKEN] anthropic=[$ANTHROPIC_API_KEY] gh=[$GITHUB_TOKEN] aws=[$AWS_SECRET_ACCESS_KEY] a=$A b=$B"; echo "kept=$LC_ALL $TZ"; test -n "$PATH" && test -n "$HOME" && echo path-home; echo "oops" >&2; ` + writeJUnit("reports/unit.xml") + `echo cover > coverage.out; exit 1`
	s := e.commandJob(script, func(s *domain.JobSpec) {
		s.Command.Artifacts = []string{"coverage.out"}
		s.Command.Env = map[string]string{"B": "from-command"}
		s.Environment = &domain.Environment{Name: "host", Provider: domain.ProviderMachine, Env: map[string]string{"A": "from-env", "B": "overridden"},
			Versions: map[string]string{"sh": "echo sh 1.0; echo more"}}
	})
	c := e.doComplete(s)

	if c.Status != StatusSucceeded || c.Error != "" || c.HeadSHA != main {
		t.Fatalf("completion = %+v", c)
	}
	run := onlyRun(t, c)
	if run.Suite != "unit" || run.Status != domain.TestRunFailed || run.ExitCode != 1 || run.Versions["sh"] != "sh 1.0" ||
		run.Counts != (domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1}) || !slices.Equal(run.ReportArtifacts, []string{"reports/unit.xml"}) {
		t.Errorf("run = %+v", run)
	}
	var names []string
	for _, r := range run.Results {
		names = append(names, r.Name+":"+string(r.Status)+":"+r.Message)
	}
	if want := []string{"TestPass:passed:", "TestFail:failed:want 1, got 2", "TestSkip:skipped:later"}; !slices.Equal(names, want) {
		t.Errorf("results = %v, want %v", names, want)
	}
	up := e.api.uploaded()
	if up["reports/unit.xml"] != junit || up["coverage.out"] != "cover\n" || len(up) != 2 {
		t.Errorf("uploads = %v", up)
	}
	var logs []string
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventLog {
			logs = append(logs, ev.Text)
		}
	}
	if !slices.Contains(logs, "token=[] anthropic=[] gh=[] aws=[] a=from-env b=from-command") || !slices.Contains(logs, "kept=C UTC") || !slices.Contains(logs, "path-home") || !slices.Contains(logs, "stderr: oops") {
		t.Errorf("log events = %q", logs)
	}
	if got, want := e.api.statuses(s.JobID), []string{"cloning", "running", "collecting"}; !slices.Equal(got, want) {
		t.Errorf("status events = %v, want %v", got, want)
	}
	// A command job never commits or pushes.
	if got := remoteSHA(t, e.remote, "main"); got != main {
		t.Errorf("main moved to %s", got)
	}
	if _, err := os.Stat(filepath.Join(e.workDir, "jobs", s.JobID.String())); !os.IsNotExist(err) {
		t.Errorf("workspace still exists: %v", err)
	}
	e.noSecrets()
}

func TestCommandJobArgvAndWorkdir(t *testing.T) {
	e := newCommandEnv(t)
	s := e.commandJob("", func(s *domain.JobSpec) {
		s.Command = &domain.CommandSpec{Argv: []string{"sh", "-c", "pwd; cat guide.md"}, Workdir: "docs", Reports: []string{"none.xml"}}
	})
	c := e.doComplete(s)
	var out []string
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventLog {
			out = append(out, ev.Text)
		}
	}
	if len(out) != 2 || !strings.HasSuffix(out[0], "/docs") || out[1] != "guide" {
		t.Errorf("output = %q", out)
	}
	if run := onlyRun(t, c); run.Status != domain.TestRunErrored || run.ExitCode != 0 {
		t.Errorf("run = %+v", run)
	}
}

func TestCommandJobOverlay(t *testing.T) {
	e := newCommandEnv(t)
	// A branch with a new test file and a changed README.
	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", e.remote, ".")
	runGit(t, work, "checkout", "--quiet", "-b", "agent/feature")
	writeFile(t, work, "tests/new_test.txt", "new test\n")
	writeFile(t, work, "tests/other.txt", "other\n")
	writeFile(t, work, "README.md", "changed\n")
	runGit(t, work, "add", "--all")
	runGit(t, work, "commit", "--quiet", "-m", "feature")
	runGit(t, work, "push", "--quiet", "origin", "agent/feature")
	main := remoteSHA(t, e.remote, "main")

	s := e.commandJob(`cat tests/new_test.txt; cat README.md; ls tests; ls tests/other.txt || echo no-other`, func(s *domain.JobSpec) {
		s.Command.Reports = nil
		s.Overlay = &domain.Overlay{Ref: "agent/feature", Paths: []string{"tests/new_*.txt"}}
	})
	c := e.doComplete(s)
	var out []string
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventLog {
			out = append(out, ev.Text)
		}
	}
	if !slices.Equal(out[:3], []string{"new test", "hello", "new_test.txt"}) || !slices.Contains(out, "no-other") {
		t.Errorf("output = %q: want the overlaid file on top of the base, nothing else", out)
	}
	if c.Status != StatusSucceeded || c.HeadSHA != main {
		t.Errorf("completion = %+v: head_sha is the commit tested before the overlay", c)
	}

	// An overlay by SHA works too, and one that matches nothing fails the job.
	sha := remoteSHA(t, e.remote, "agent/feature")
	s = e.commandJob(`cat tests/other.txt`, func(s *domain.JobSpec) {
		s.Command.Reports = nil
		s.Overlay = &domain.Overlay{Ref: sha, Paths: []string{"tests/other.txt"}}
	})
	e.doComplete(s)
	if ev := e.api.eventsOf(s.JobID); !slices.ContainsFunc(ev, func(x domain.JobEvent) bool { return x.Text == "other" }) {
		t.Errorf("events = %+v", ev)
	}
	bad := e.commandJob(`true`, func(s *domain.JobSpec) {
		s.Overlay = &domain.Overlay{Ref: "agent/feature", Paths: []string{"missing/**"}}
	})
	if c := e.doComplete(bad); c.Status != StatusFailed || !strings.Contains(c.Error, "overlay") || len(c.TestRuns) != 0 {
		t.Errorf("completion = %+v", c)
	}
	unknown := e.commandJob(`true`, func(s *domain.JobSpec) { s.Overlay = &domain.Overlay{Ref: "agent/nope", Paths: []string{"a"}} })
	if c := e.doComplete(unknown); c.Status != StatusRetry {
		t.Errorf("unknown ref: completion = %+v", c)
	}
	refused := e.commandJob(`true`, func(s *domain.JobSpec) { s.Overlay = &domain.Overlay{Ref: "--upload-pack=x", Paths: []string{"a"}} })
	if c := e.doComplete(refused); c.Status != StatusFailed || !strings.Contains(c.Error, "refused") {
		t.Errorf("option as ref: completion = %+v", c)
	}
}

func TestCommandJobReportProblems(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		reports []string
		status  domain.TestRunStatus
		counts  domain.TestCounts
		message string
	}{
		{"no report written", "true", []string{"reports/*.xml"}, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "no report file matched reports/*.xml"},
		{"no reports declared", "true", nil, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "declares no reports"},
		{"broken report", "mkdir reports; echo '<testsuite><testcase' > reports/a.xml", []string{"reports/*.xml"}, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "report reports/a.xml:"},
		{"one broken of two", writeJUnit("reports/a.xml") + "echo nope > reports/b.xml", []string{"reports/*.xml"}, domain.TestRunFailed, domain.TestCounts{Passed: 1, Failed: 1, Skipped: 1, Errored: 1}, "report reports/b.xml:"},
		{"empty suite", "mkdir reports; echo '<testsuite/>' > reports/a.xml", []string{"reports/*.xml"}, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "no test cases"},
		{"suites without cases", "mkdir reports; echo '<testsuites><testsuite><!-- none --></testsuite></testsuites>' > reports/a.xml", []string{"reports/*.xml"}, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "no test cases"},
		{"comment only", "mkdir reports; echo '<!-- nothing -->' > reports/a.xml", []string{"reports/*.xml"}, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "report reports/a.xml:"},
		{"report is a directory", "mkdir -p reports/a.xml", []string{"reports/*.xml"}, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "no report file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCommandEnv(t)
			c := e.doComplete(e.commandJob(tt.script, func(s *domain.JobSpec) { s.Command.Reports = tt.reports }))
			run := onlyRun(t, c)
			if c.Status != StatusSucceeded || run.Status != tt.status || run.Counts != tt.counts {
				t.Fatalf("completion = %+v", c)
			}
			var found bool
			for _, r := range run.Results {
				found = found || (r.Status == domain.TestErrored && strings.Contains(r.Message, tt.message))
			}
			if !found {
				t.Errorf("no errored result with %q in %+v", tt.message, run.Results)
			}
		})
	}
}

func TestCommandJobCommandCannotStart(t *testing.T) {
	e := newCommandEnv(t)
	c := e.doComplete(e.commandJob("", func(s *domain.JobSpec) {
		s.Command = &domain.CommandSpec{Argv: []string{"/nonexistent/prog"}, Reports: []string{"r.xml"}}
	}))
	run := onlyRun(t, c)
	if c.Status != StatusFailed || !strings.Contains(c.Error, "could not run") || run.Status != domain.TestRunErrored || run.ExitCode != -1 ||
		!strings.Contains(run.Results[0].Message, "could not run") {
		t.Errorf("completion = %+v", c)
	}
}

func TestCommandJobRefusesSymlinks(t *testing.T) {
	e := newCommandEnv(t)
	secret := tempFile(t, "secret.xml", junit)
	script := "mkdir reports real; ln -s " + secret + " reports/link.xml; ln -s ../real reports/dir; echo x > real/a.xml; ln -s " + secret + " leak.txt"
	s := e.commandJob(script, func(s *domain.JobSpec) {
		s.Command.Reports = []string{"reports/**/*.xml", "reports/*.xml"}
		s.Command.Artifacts = []string{"leak.txt"}
	})
	c := e.doComplete(s)
	run := onlyRun(t, c)
	if run.Status != domain.TestRunErrored || len(e.api.uploaded()) != 0 {
		t.Errorf("run = %+v, uploads = %v", run, e.api.uploaded())
	}
	var skipped bool
	for _, ev := range e.api.eventsOf(s.JobID) {
		skipped = skipped || strings.HasPrefix(ev.Text, "skipped symlinks:")
	}
	if !skipped {
		t.Error("the refused symlinks were not logged")
	}
}

func TestCommandJobWorkdirMustStayInTheRepo(t *testing.T) {
	for name, script := range map[string]string{"symlink": "", "missing": ""} {
		t.Run(name, func(t *testing.T) {
			e := newCommandEnv(t)
			wd := "nodir"
			if name == "symlink" {
				// docs/guide.md exists; a symlinked directory comes from the repo itself.
				work := t.TempDir()
				runGit(t, work, "clone", "--quiet", e.remote, ".")
				if err := os.Symlink("/tmp", filepath.Join(work, "link")); err != nil {
					t.Fatal(err)
				}
				runGit(t, work, "add", "--all")
				runGit(t, work, "commit", "--quiet", "-m", "link")
				runGit(t, work, "push", "--quiet", "origin", "main")
				wd = "link"
			}
			c := e.doComplete(e.commandJob("true", func(s *domain.JobSpec) { s.Command.Workdir = wd }))
			if c.Status != StatusFailed || !strings.Contains(c.Error, "workdir") {
				t.Errorf("%s: completion = %+v", script, c)
			}
		})
	}
}

func TestCommandJobSetup(t *testing.T) {
	e := newCommandEnv(t)
	env := func(setup ...string) func(*domain.JobSpec) {
		return func(s *domain.JobSpec) {
			s.Environment = &domain.Environment{Name: "host", Provider: domain.ProviderMachine, Setup: setup}
		}
	}
	c := e.doComplete(e.commandJob(`cat prepared`, env("echo ready > prepared")))
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	c = e.doComplete(e.commandJob(`true`, env("exit 7")))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "exit code 7") || len(c.TestRuns) != 0 {
		t.Errorf("failing setup: completion = %+v", c)
	}
}

func TestCommandJobUnsupportedAndInvalid(t *testing.T) {
	e := newCommandEnv(t)
	for name, mod := range map[string]func(*domain.JobSpec){
		"docker": func(s *domain.JobSpec) {
			s.Environment = &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x"}
		},
		"actions": func(s *domain.JobSpec) {
			s.Environment = &domain.Environment{Name: "a", Provider: domain.ProviderActions, RunsOn: "x"}
		},
		"no command":   func(s *domain.JobSpec) { s.Command = nil },
		"agent fields": func(s *domain.JobSpec) { s.Prompt = "hi" },
	} {
		c := e.doComplete(e.commandJob("true", mod))
		if c.Status != StatusFailed || c.Error == "" {
			t.Errorf("%s: completion = %+v", name, c)
		}
	}
}

// pidOf waits for a file holding a pid.
func pidOf(t *testing.T, path string) int {
	t.Helper()
	for range 400 {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return 0
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("process %d survived", pid)
}

func TestCommandJobTimeoutKillsTheProcessGroup(t *testing.T) {
	e := newCommandEnv(t)
	pidfile := filepath.Join(t.TempDir(), "pid")
	s := e.commandJob("sleep 300 & echo $! > "+pidfile+"; wait", func(s *domain.JobSpec) { s.Timeout = domain.Duration(500 * time.Millisecond) })
	start := time.Now()
	c := e.doComplete(s)
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %s", d)
	}
	run := onlyRun(t, c)
	if c.Status != StatusFailed || !strings.Contains(c.Error, "timeout") || run.ExitCode != -1 || run.Status != domain.TestRunErrored {
		t.Errorf("completion = %+v", c)
	}
	assertGone(t, pidOf(t, pidfile))
}

func TestCommandJobCancelKillsTheProcessGroup(t *testing.T) {
	e := newCommandEnv(t)
	pidfile := filepath.Join(t.TempDir(), "pid")
	e.api.heartbeat = func(uuid.UUID, int) (int, string) {
		if _, err := os.Stat(pidfile); err == nil {
			return 200, "cancel"
		}
		return 200, "continue"
	}
	s := e.commandJob("sleep 300 & echo $! > " + pidfile + "; wait")
	start := time.Now()
	e.do(s)
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %s", d)
	}
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times, want not at all", n)
	}
	assertGone(t, pidOf(t, pidfile))
}

func TestCommandJobUploadFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
		runs   int
	}{
		{"evidence storage missing", http.StatusServiceUnavailable, StatusSucceeded, 1},
		{"too large", http.StatusRequestEntityTooLarge, StatusSucceeded, 1},
		{"server error", http.StatusInternalServerError, StatusRetry, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCommandEnv(t)
			e.api.artifactStatus = tt.status
			c := e.doComplete(e.commandJob(writeJUnit("reports/unit.xml")))
			if c.Status != tt.want || len(c.TestRuns) != tt.runs {
				t.Fatalf("completion = %+v", c)
			}
			if tt.runs == 1 {
				run := onlyRun(t, c)
				// The report is still parsed, but it is not an artifact.
				if run.Counts.Passed != 1 || len(run.ReportArtifacts) != 0 {
					t.Errorf("run = %+v", run)
				}
			}
		})
	}
}

func TestCommandJobLeaseLostOnUploadDropsTheResult(t *testing.T) {
	e := newCommandEnv(t)
	e.api.artifactStatus = http.StatusConflict
	s := e.commandJob(writeJUnit("reports/unit.xml"))
	e.do(s)
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times, want not at all", n)
	}
}

func TestCommandJobSkipsInvalidArtifactNames(t *testing.T) {
	e := newCommandEnv(t)
	s := e.commandJob("mkdir .hidden; echo x > .hidden/a.txt; echo y > ok.txt", func(s *domain.JobSpec) {
		s.Command.Reports = nil
		s.Command.Artifacts = []string{".hidden/*.txt", "ok.txt"}
	})
	e.doComplete(s)
	if up := e.api.uploaded(); len(up) != 1 || up["ok.txt"] != "y\n" {
		t.Errorf("uploads = %v", up)
	}
}

func TestCommandJobCapsUploads(t *testing.T) {
	e := newCommandEnv(t)
	s := e.commandJob("mkdir out; for i in $(seq 1 "+strconv.Itoa(maxArtifacts+5)+"); do echo $i > out/f$i.txt; done", func(s *domain.JobSpec) {
		s.Command.Reports = nil
		s.Command.Artifacts = []string{"out/*.txt"}
	})
	e.doComplete(s)
	if n := len(e.api.uploaded()); n != maxArtifacts {
		t.Errorf("uploaded %d files, want %d", n, maxArtifacts)
	}
}

func TestCommandJobPassingTestsUnderFailingCommand(t *testing.T) {
	e := newCommandEnv(t)
	report := `<testsuite><testcase classname="p" name="T"/></testsuite>`
	script := "mkdir reports; echo '" + report + "' > reports/a.xml; exit 3"
	run := onlyRun(t, e.doComplete(e.commandJob(script)))
	if run.Status != domain.TestRunFailed || run.ExitCode != 3 || run.Counts != (domain.TestCounts{Passed: 1, Errored: 1}) ||
		!strings.Contains(run.Results[len(run.Results)-1].Message, "exited with code 3") {
		t.Errorf("run = %+v", run)
	}
	// With exit 0 the same report passes.
	run = onlyRun(t, e.doComplete(e.commandJob("mkdir reports; echo '"+report+"' > reports/a.xml")))
	if run.Status != domain.TestRunPassed || run.Counts.Errored != 0 {
		t.Errorf("run = %+v", run)
	}
}

func TestCommandJobResultsTooLargeForTheServer(t *testing.T) {
	e := newCommandEnv(t)
	e.api.maxCompleteBytes = 2000
	var cases strings.Builder
	for i := range 100 {
		fmt.Fprintf(&cases, `<testcase classname="pkg" name="Test%d"/>`, i)
	}
	s := e.commandJob("mkdir reports; echo '<testsuite>" + cases.String() + "</testsuite>' > reports/a.xml")
	e.do(s)
	e.api.mu.Lock()
	comps := e.api.completions[s.JobID]
	e.api.mu.Unlock()
	if len(comps) != 2 || len(comps[0].TestRuns[0].Results) != 100 {
		t.Fatalf("completions = %d, want the refused one with 100 results and an errored one", len(comps))
	}
	c := comps[1]
	run := onlyRun(t, c)
	if c.Status != StatusSucceeded || run.Status != domain.TestRunErrored || len(run.Results) != 1 ||
		run.Results[0].Name != testreport.OversizedResult || !strings.Contains(run.Results[0].Message, "max_test_results_size") || run.Counts.Errored != 1 {
		t.Errorf("completion = %+v", c)
	}
}

// newCommandEnv is an env whose runtime fails the test when it runs: a
// command job never uses one.
func newCommandEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnv(t, funcAdapter{fn: func(context.Context, runtime.RunSpec, func(domain.JobEvent)) (runtime.Result, error) {
		t.Error("the runtime ran for a command job")
		return runtime.Result{}, nil
	}})
}

func TestCommandEnvIsAnAllowlist(t *testing.T) {
	e := newCommandEnv(t)
	e.cfg.Environ = []string{"PATH=/bin", "HOME=/home/w", "USER=w", "LANG=C", "LC_CTYPE=C", "SystemRoot=C:\\Windows", "tz=UTC",
		"ANTHROPIC_API_KEY=k", "OPENAI_API_KEY=k", "GITHUB_TOKEN=t", "GH_TOKEN=t", "AWS_ACCESS_KEY_ID=a", "SSH_AUTH_SOCK=/s", "FORECREW_TOKEN=w", "GIT_DIR=/x", "MY_PASSWORD=p"}
	e.rebuild()
	r := &jobRun{w: e.w, spec: e.commandJob("true", func(s *domain.JobSpec) {
		s.Command.Env = map[string]string{"B": "2"}
		s.Environment = &domain.Environment{Name: "h", Provider: domain.ProviderMachine, Env: map[string]string{"A": "1"}}
	})}
	got := r.commandEnv("mark")
	want := []string{"PATH=/bin", "HOME=/home/w", "USER=w", "LANG=C", "LC_CTYPE=C", "SystemRoot=C:\\Windows", "tz=UTC", "A=1", "B=2", markEnv + "=mark"}
	if !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}

func TestCommandJobGlobsDoNotFollowSymlinks(t *testing.T) {
	e := newCommandEnv(t)
	script := "mkdir -p reports/sub; ln -s . reports/loop; ln -s .. reports/up; ln -s . loop; ln -s /nonexistent dangling;" +
		"cat > reports/sub/real.xml <<'EOF'\n" + junit + "EOF\n"
	s := e.commandJob(script, func(s *domain.JobSpec) {
		s.Command.Reports = []string{"**/*.xml"}
		s.Command.Artifacts = []string{"**/loop/**", "loop/**/*.xml"}
	})
	done := make(chan Completion, 1)
	go func() { done <- e.doComplete(s) }()
	select {
	case c := <-done:
		run := onlyRun(t, c)
		if run.Status != domain.TestRunFailed || run.Counts.Passed != 1 || !slices.Equal(run.ReportArtifacts, []string{"reports/sub/real.xml"}) {
			t.Errorf("run = %+v", run)
		}
		if up := e.api.uploaded(); len(up) != 1 || up["reports/sub/real.xml"] != junit {
			t.Errorf("uploads = %v", up)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the search for files hangs on symlink loops")
	}
}

func TestCommandJobWalkIsBounded(t *testing.T) {
	old := maxWalkEntries
	maxWalkEntries = 30
	t.Cleanup(func() { maxWalkEntries = old })
	e := newCommandEnv(t)
	c := e.doComplete(e.commandJob("mkdir many; for i in $(seq 1 100); do : > many/f$i; done", func(s *domain.JobSpec) { s.Command.Reports = []string{"**/*.xml"} }))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "too many files") {
		t.Errorf("completion = %+v", c)
	}
}

func TestCommandJobLogIsCapped(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 256 << 10
	t.Cleanup(func() { maxLogBytes = old })
	e := newCommandEnv(t)
	s := e.commandJob("head -c 20000000 /dev/zero | tr '\\0' a | fold -w 100; echo done >&2", func(s *domain.JobSpec) { s.Command.Reports = nil })
	c := e.doComplete(s)
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	total, markers := 0, 0
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventLog {
			total += len(ev.Text)
			if strings.HasPrefix(ev.Text, "[output truncated") {
				markers++
			}
		}
	}
	if int64(total) > maxLogBytes+10<<10 || markers != 1 {
		t.Errorf("logged %d bytes with %d markers, want at most %d and one marker", total, markers, maxLogBytes)
	}

	// Short lines are capped by their number: each costs a row.
	s = e.commandJob("yes | head -n 2000000; echo done >&2", func(s *domain.JobSpec) { s.Command.Reports = nil })
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	rows := 0
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventLog {
			rows++
		}
	}
	if want := int(maxLogBytes/logEventOverhead) + 2; rows > want || rows < want/2 {
		t.Errorf("%d log rows for short lines, want about %d at most", rows, want)
	}
}

func TestCommandJobNeverOpensSpecialFiles(t *testing.T) {
	e := newCommandEnv(t)
	s := e.commandJob("mkfifo r.xml fifo.out reports.xml; mkdir d; mkfifo d/x.xml", func(s *domain.JobSpec) {
		s.Command.Reports = []string{"r.xml", "d/*.xml", "**/*.xml"}
		s.Command.Artifacts = []string{"fifo.out", "reports.xml"}
	})
	done := make(chan Completion, 1)
	go func() { done <- e.doComplete(s) }()
	select {
	case c := <-done:
		run := onlyRun(t, c)
		if c.Status != StatusSucceeded || run.Status != domain.TestRunErrored || len(e.api.uploaded()) != 0 {
			t.Errorf("completion = %+v, uploads = %v", c, e.api.uploaded())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a FIFO at a report path hangs the worker")
	}
}

func TestCommandJobSetupSharesTheTimeout(t *testing.T) {
	e := newCommandEnv(t)
	s := e.commandJob("echo never", func(s *domain.JobSpec) {
		s.Timeout = domain.Duration(700 * time.Millisecond)
		s.Environment = &domain.Environment{Name: "host", Provider: domain.ProviderMachine, Setup: []string{"sleep 0.5", "sleep 30"}}
	})
	start := time.Now()
	c := e.doComplete(s)
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %s", d)
	}
	if c.Status != StatusFailed || !strings.Contains(c.Error, "timeout") || !strings.Contains(c.Error, "sleep 30") {
		t.Errorf("completion = %+v", c)
	}
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Text == "never" {
			t.Error("the command ran after its budget was used up")
		}
	}
	// The command gets what the setup left.
	s = e.commandJob("sleep 30", func(s *domain.JobSpec) {
		s.Timeout = domain.Duration(900 * time.Millisecond)
		s.Environment = &domain.Environment{Name: "host", Provider: domain.ProviderMachine, Setup: []string{"sleep 0.5"}}
	})
	start = time.Now()
	c = e.doComplete(s)
	if d := time.Since(start); d > 10*time.Second || c.Status != StatusFailed || !strings.Contains(c.Error, "timeout") {
		t.Errorf("after %s: completion = %+v", d, c)
	}
}

func TestCommandJobStripsNULs(t *testing.T) {
	e := newCommandEnv(t)
	s := e.commandJob(`printf 'a\000b\n'; printf 'bad\377utf\n'`, func(s *domain.JobSpec) {
		s.Command.Reports = nil
		s.Environment = &domain.Environment{Name: "host", Provider: domain.ProviderMachine, Versions: map[string]string{"v": `printf 'v1\0002\n'`}}
	})
	c := e.doComplete(s)
	run := onlyRun(t, c)
	if run.Versions["v"] != "v12" {
		t.Errorf("versions = %q", run.Versions)
	}
	var lines []string
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventLog {
			lines = append(lines, ev.Text)
		}
	}
	if !slices.Contains(lines, "ab") || !slices.Contains(lines, "badutf") {
		t.Errorf("log = %q", lines)
	}
	b, _ := json.Marshal([]any{e.api.events, e.api.completions})
	if strings.Contains(string(b), `\u0000`) {
		t.Error("a NUL reached the server")
	}
}

func TestCommandJobReportsAreBoundedInTotal(t *testing.T) {
	e := newCommandEnv(t)
	gen := func(path string, n int) string {
		return fmt.Sprintf(`awk 'BEGIN{print "<testsuite>"; for(i=0;i<%d;i++) print "<testcase name=\"t\"/>"; print "</testsuite>"}' > %s; `, n, path)
	}
	// Each report is fine, together they are too many.
	half := testreport.MaxCases/2 + 10
	// The third report is broken: parsing stops before it.
	c := e.doComplete(e.commandJob("mkdir reports; " + gen("reports/a.xml", half) + gen("reports/b.xml", half) + "echo nope > reports/c.xml"))
	run := onlyRun(t, c)
	if c.Status != StatusSucceeded || run.Status != domain.TestRunErrored || len(run.Results) != 1 ||
		!strings.Contains(run.Results[0].Message, "test cases") || strings.Contains(run.Results[0].Message, "c.xml") {
		t.Errorf("run = %+v", run.Results)
	}

	// The same for the text of the cases.
	fat := func(path string) string {
		return fmt.Sprintf(`awk 'BEGIN{m=sprintf("%%4000s","x"); gsub(/ /,"y",m); print "<testsuite>"; for(i=0;i<2600;i++) print "<testcase name=\"t\"><failure message=\"" m "\"/></testcase>"; print "</testsuite>"}' > %s; `, path)
	}
	c = e.doComplete(e.commandJob("mkdir reports; " + fat("reports/a.xml") + fat("reports/b.xml") + "echo nope > reports/c.xml"))
	run = onlyRun(t, c)
	if run.Status != domain.TestRunErrored || len(run.Results) != 1 || strings.Contains(run.Results[0].Message, "c.xml") {
		t.Errorf("fat reports: run = %+v", run.Results)
	}
}

func TestCommandJobSweepsLeftoversBeforeCollecting(t *testing.T) {
	e := newCommandEnv(t)
	pidfile := filepath.Join(t.TempDir(), "pid")
	// A process that left the process group keeps rewriting the report.
	script := "mkdir reports; " + "setsid sh -c 'while :; do : > reports/late.txt; sleep 0.05; done' & echo $! > " + pidfile + "; sleep 0.3; " + writeJUnit("reports/a.xml")
	var alive atomic.Bool
	e.api.onUpload = func(string) {
		if b, err := os.ReadFile(pidfile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && syscall.Kill(pid, 0) == nil {
				alive.Store(true)
			}
		}
	}
	e.doComplete(e.commandJob(script))
	if alive.Load() {
		t.Error("a leftover process was still running when the files were uploaded")
	}
}
