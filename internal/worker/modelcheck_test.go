package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/runtime"
)

// checkSpec is a model_check job of the models at url.
func (e *testEnv) checkSpec(url string) domain.JobSpec {
	return e.spec(func(s *domain.JobSpec) {
		s.Kind, s.Runtime, s.Model, s.Prompt, s.WritePaths, s.Branch, s.RepoToken, s.AllowedTools = domain.JobKindModelCheck, "", "", "", nil, "", "", nil
		s.ModelCheck = &domain.ModelCheckSpec{Connection: "local", Adapter: runtime.Scripted, URL: url, Auth: models.AuthBearer, Format: models.FormatOpenAI, KeyEnv: "OLLAMA_API_KEY"}
		s.Env = map[string]string{"OLLAMA_API_KEY": testAPIKey}
	})
}

func checkResult(t *testing.T, c Completion) domain.ModelCheckResult {
	t.Helper()
	var r domain.ModelCheckResult
	if c.Status != StatusSucceeded || json.Unmarshal(c.Output, &r) != nil {
		t.Fatalf("completion = %+v", c)
	}
	return r
}

// TestAModelOnTheWorkersMachineIsChecked: the key check runs on the worker,
// which reaches a model on its own machine when its operator allows it; the
// server is never asked.
func TestAModelOnTheWorkersMachineIsChecked(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-coder"}]}`))
	}))
	t.Cleanup(srv.Close)
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	got := checkResult(t, e.doComplete(e.checkSpec(srv.URL+"/v1/models")))
	if got.Status != domain.ModelCheckUnreachable || !strings.Contains(got.Detail, "local_model_endpoints") || auth != "" {
		t.Fatalf("not allowed = %+v, auth %q", got, auth)
	}
	e.cfg.LocalModelEndpoints = true
	e.rebuild()
	got = checkResult(t, e.doComplete(e.checkSpec(srv.URL+"/v1/models")))
	if got.Status != domain.ModelCheckReachable || !slices.Equal(got.Models, []string{"qwen3-coder"}) || got.AgentVersion != "scripted" || auth != "Bearer "+testAPIKey {
		t.Fatalf("allowed = %+v", got)
	}
	got = checkResult(t, e.doComplete(e.checkSpec("")))
	if got.Status != domain.ModelCheckUnsupported {
		t.Fatalf("no list = %+v", got)
	}
	bad := e.checkSpec("x")
	bad.ModelCheck = nil
	if c := e.doComplete(bad); c.Status != StatusFailed {
		t.Fatalf("no check = %+v", c)
	}
	if err := os.MkdirAll(e.workDir, 0o750); err != nil {
		t.Fatal(err)
	}
	e.noSecrets()
}

// testSpec is a suitability test whose agent is the script.
func (e *testEnv) testSpec() domain.JobSpec {
	return e.spec(func(s *domain.JobSpec) {
		s.WritePaths, s.Branch, s.RepoToken, s.Prompt = nil, "", "", models.TestPrompt
		s.Suitability = &domain.SuitabilitySpec{Connection: "c", Nonce: "fcnonce", Schema: models.TestSchema}
	})
}

func report(t *testing.T, c Completion) domain.SuitabilityReport {
	t.Helper()
	var r domain.SuitabilityReport
	if c.Status != StatusSucceeded || json.Unmarshal(c.Output, &r) != nil {
		t.Fatalf("completion = %+v", c)
	}
	return r
}

// TestASuitabilityTestRunsOnTheFixture: the agent works on the fixture, not
// on the job's repo, and the worker reports what it found there.
func TestASuitabilityTestRunsOnTheFixture(t *testing.T) {
	var saw []string
	all := runtime.Script{
		Edits:  []runtime.ScriptEdit{{Path: "greeting.txt", Content: "hello, forecrew\n"}},
		Output: json.RawMessage(`{"code_word":"fcnonce","command_output":"check","done":true}`), Turns: 6,
		Usage: domain.Usage{InputTokens: 100, OutputTokens: 10},
	}
	e := newEnv(t, funcAdapter{fn: func(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
		entries, _ := os.ReadDir(spec.WorkDir)
		for _, en := range entries {
			saw = append(saw, en.Name())
		}
		notes, _ := os.ReadFile(filepath.Join(spec.WorkDir, "notes.txt"))
		token, _ := os.ReadFile(filepath.Join(spec.WorkDir, "token.txt"))
		if !strings.Contains(string(notes), "code word: fcnonce") || strings.Contains(string(token), "fcnonce") || spec.MaxTurns != 15 {
			t.Errorf("notes %q, token %q, max turns %d", notes, token, spec.MaxTurns)
		}
		runCheck(t, spec)
		return scriptAdapter(t, all).Run(ctx, spec, emit)
	}})
	s := e.testSpec()
	s.MaxTurns = models.TestMaxTurns
	c := e.doComplete(s)
	r := report(t, c)
	if !r.Read || !r.Edited || !r.Ran || r.Turns != 6 || len(r.Output) == 0 || r.Error != "" || c.Usage.InputTokens != 100 {
		t.Fatalf("report = %+v, completion %+v", r, c)
	}
	if slices.Contains(saw, ".git") || !slices.Contains(saw, "check.sh") || !slices.Contains(saw, "token.txt") {
		t.Fatalf("work tree = %v", saw)
	}
	if _, err := os.Stat(filepath.Join(e.workDir, "jobs", s.JobID.String())); !os.IsNotExist(err) {
		t.Errorf("fixture left behind: %v", err)
	}

	// An answer that does not match the schema is reported, not failed.
	e2 := newEnv(t, scriptAdapter(t, runtime.Script{Output: json.RawMessage(`{"code_word":1}`)}))
	r = report(t, e2.doComplete(e2.testSpec()))
	if len(r.Output) != 0 || r.Error == "" || r.Edited || r.Ran {
		t.Fatalf("bad answer = %+v", r)
	}
	// An agent that is not installed gives the job back.
	e3 := newEnv(t, scriptAdapter(t, runtime.Script{}))
	s3 := e3.testSpec()
	s3.Runtime = "missing"
	if c := e3.doComplete(s3); c.Status != StatusRetry {
		t.Fatalf("missing agent = %+v", c)
	}
}

// runCheck runs check.sh as an agent would, with the run's environment.
func runCheck(t *testing.T, spec runtime.RunSpec) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "check.sh")
	cmd.Dir = spec.WorkDir
	cmd.Env = append(os.Environ(), CheckSaltEnv+"="+spec.Env[CheckSaltEnv])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check.sh: %v %s", err, out)
	}
}

// TestASuitabilityTestCannotBeFaked: an agent that guesses, or writes what
// the worker looks for instead of doing it, passes nothing.
func TestASuitabilityTestCannotBeFaked(t *testing.T) {
	for name, tt := range map[string]struct {
		run  func(t *testing.T, spec runtime.RunSpec) string // returns the answer
		want func(domain.SuitabilityReport) bool
	}{
		"code word from the token file": {func(_ *testing.T, spec runtime.RunSpec) string {
			token, _ := os.ReadFile(filepath.Join(spec.WorkDir, "token.txt"))
			return `{"code_word":"` + strings.TrimSpace(string(token)) + `","command_output":"x","done":true}`
		}, func(r domain.SuitabilityReport) bool { return !r.Read }},
		"check line written by hand": {func(t *testing.T, spec runtime.RunSpec) string {
			token, _ := os.ReadFile(filepath.Join(spec.WorkDir, "token.txt"))
			must(t, os.WriteFile(filepath.Join(spec.WorkDir, ".check-ran"), token, 0o600))
			return `{"code_word":"x","command_output":"x","done":true}`
		}, func(r domain.SuitabilityReport) bool { return !r.Ran }},
		"check line without the salt": {func(t *testing.T, spec runtime.RunSpec) string {
			token, _ := os.ReadFile(filepath.Join(spec.WorkDir, "token.txt"))
			must(t, os.WriteFile(filepath.Join(spec.WorkDir, ".check-ran"), []byte(CheckLine(strings.TrimSpace(string(token)), "")), 0o600))
			return `{"code_word":"x","command_output":"x","done":true}`
		}, func(r domain.SuitabilityReport) bool { return !r.Ran }},
		"greeting almost right": {func(t *testing.T, spec runtime.RunSpec) string {
			must(t, os.WriteFile(filepath.Join(spec.WorkDir, "greeting.txt"), []byte("hello, forecrew!"), 0o600))
			return `{"code_word":"x","command_output":"x","done":true}`
		}, func(r domain.SuitabilityReport) bool { return !r.Edited }},
		"answer not matching the schema": {func(_ *testing.T, _ runtime.RunSpec) string {
			return `{"code_word":"fcnonce"}`
		}, func(r domain.SuitabilityReport) bool { return len(r.Output) == 0 && r.Error != "" }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, funcAdapter{fn: func(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
				answer := tt.run(t, spec)
				return scriptAdapter(t, runtime.Script{Output: json.RawMessage(answer)}).Run(ctx, spec, emit)
			}})
			if r := report(t, e.doComplete(e.testSpec())); !tt.want(r) {
				t.Fatalf("faked = %+v", r)
			}
		})
	}
}
