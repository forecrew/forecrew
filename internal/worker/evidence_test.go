package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// TestJobMCPGateway: the agent gets the gateway's URL and its token in the
// environment only, and the files it marks are uploaded after it exits.
func TestJobMCPGateway(t *testing.T) {
	var got runtime.RunSpec
	var e *testEnv
	var jobID uuid.UUID
	e = newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		got = spec
		must(t, os.MkdirAll(filepath.Join(spec.WorkDir, "shots"), 0o750))
		must(t, os.WriteFile(filepath.Join(spec.WorkDir, "shots", "home.png"), []byte("png"), 0o600))
		// Marked while running; the heartbeat after the run hands them over.
		e.api.mu.Lock()
		e.api.attach[jobID] = []Attachment{
			{Path: "shots/home.png", Name: "evidence/shots/home.png"},
			{Path: "shots", Name: "evidence/shots"}, // a directory
			{Path: "missing.png", Name: "evidence/missing.png"},
		}
		e.api.mu.Unlock()
		return runtime.Result{}, nil
	}})
	s := e.spec(func(s *domain.JobSpec) {
		s.JobMCP = &domain.JobMCP{URL: "https://forecrew.example.com/job-mcp", Token: testJobMCPTok}
	})
	jobID = s.JobID
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if got.JobMCP == nil || got.JobMCP.URL != e.api.srv.URL+"/job-mcp" || got.Env[runtime.JobMCPTokenEnv] != testJobMCPTok {
		t.Fatalf("run spec = %+v", got)
	}
	e.api.mu.Lock()
	uploads := e.api.uploads
	e.api.mu.Unlock()
	if up := uploads["evidence/shots/home.png"]; len(uploads) != 1 || string(up.body) != "png" || up.count != 1 {
		t.Fatalf("uploads = %v", uploads)
	}
	var errs []string
	for _, ev := range e.api.eventsOf(s.JobID) {
		if ev.Type == domain.EventError {
			errs = append(errs, ev.Text)
		}
	}
	if len(errs) != 2 || !strings.Contains(errs[0], "not a regular file") || !strings.Contains(errs[1], "does not exist") {
		t.Fatalf("errors = %q", errs)
	}
	e.noSecrets()
}

// TestAttachRedeliveredUntilAcknowledged: marks that keep coming, because an
// acknowledgement was lost, are held once and uploaded once.
func TestAttachRedeliveredUntilAcknowledged(t *testing.T) {
	r := &jobRun{}
	x := Attachment{Path: "x", Name: "evidence/x"}
	r.marked([]Attachment{x})
	r.marked([]Attachment{x})
	moved := Attachment{Path: "y", Name: "evidence/x"}
	r.marked([]Attachment{moved})
	if got := r.attachments(); len(got) != 1 || got[0] != moved {
		t.Fatalf("attachments = %+v", got)
	}
}

// TestRuntimeGetsTheWorkersServerURL: with a real adapter the CLI is
// pointed at the worker's server URL, whatever public URL the spec names.
func TestRuntimeGetsTheWorkersServerURL(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "args")
	cli := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + rec + "\ncat > /dev/null\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":1}'` + "\n"
	must(t, os.WriteFile(cli, []byte(script), 0o700)) // #nosec G306 -- a test CLI must be executable
	e := newEnv(t, runtime.NewClaudeCode(runtime.Config{Binary: cli, Env: os.Environ()}))
	s := e.spec(func(s *domain.JobSpec) {
		s.Runtime = runtime.ClaudeCode
		s.JobMCP = &domain.JobMCP{URL: "https://public.example.com/job-mcp", Token: testJobMCPTok}
	})
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	data, err := os.ReadFile(rec) // #nosec G304 -- a test file
	must(t, err)
	args := string(data)
	if !strings.Contains(args, `"url":"`+e.api.srv.URL+`/job-mcp"`) || strings.Contains(args, "public.example.com") || strings.Contains(args, testJobMCPTok) {
		t.Fatalf("args = %s", args)
	}
	e.noSecrets()
}

func TestNoGatewayNoToken(t *testing.T) {
	var got runtime.RunSpec
	e := newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		got = spec
		return runtime.Result{}, nil
	}})
	e.doComplete(e.spec())
	if _, ok := got.Env[runtime.JobMCPTokenEnv]; ok || got.JobMCP != nil {
		t.Fatalf("run spec = %+v", got)
	}
}

func TestEvidenceFile(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o750))
	must(t, os.WriteFile(filepath.Join(dir, "a", "b", "f.txt"), nil, 0o600))
	// A real file: only the .git rule may refuse it.
	must(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o750))
	must(t, os.WriteFile(filepath.Join(dir, ".git", "config"), nil, 0o600))
	must(t, os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "via")))
	if p, err := evidenceFile(dir, "", "a/b/f.txt"); err != nil || p != filepath.Join(dir, "a", "b", "f.txt") {
		t.Fatalf("evidenceFile = %q, %v", p, err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../a/b/f.txt", "a\\b", "via/b/f.txt", "a/b", "a/b/f.txt/x", ".git/config", "nope"} {
		if _, err := evidenceFile(dir, "", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEvidenceFileScratch(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(scratch, "shots"), 0o750))
	must(t, os.WriteFile(filepath.Join(scratch, "shots", "a.png"), nil, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "a.png"), nil, 0o600))
	must(t, os.Symlink(filepath.Join(dir, "a.png"), filepath.Join(scratch, "link.png")))
	must(t, os.Symlink(dir, filepath.Join(scratch, "out")))
	if p, err := evidenceFile(dir, scratch, "evidence:shots/a.png"); err != nil || p != filepath.Join(scratch, "shots", "a.png") {
		t.Fatalf("evidenceFile = %q, %v", p, err)
	}
	for _, bad := range []string{"evidence:", "evidence:/etc/passwd", "evidence:../a.png", "evidence:shots/../shots/a.png",
		"evidence:link.png", "evidence:out/a.png", "evidence:shots", "evidence:nope.png", "evidence:.git/config"} {
		if _, err := evidenceFile(dir, scratch, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	badRoot := t.TempDir() + "/root"
	must(t, os.Symlink(scratch, badRoot))
	if _, err := evidenceFile(dir, badRoot, "evidence:shots/a.png"); err == nil {
		t.Error("symlinked scratch root accepted")
	}
	if _, err := evidenceFile(dir, scratch+"-missing", "evidence:shots/a.png"); err == nil {
		t.Error("missing scratch root accepted")
	}
	if _, err := evidenceFile(dir, "", "evidence:shots/a.png"); err == nil {
		t.Error("no scratch directory: accepted")
	}
}

// A read-only role writes its evidence to the scratch directory: the job
// succeeds and the file is uploaded; the directory is gone afterwards.
func TestReadOnlyRoleAttachesScratchFile(t *testing.T) {
	var e *testEnv
	var jobID uuid.UUID
	var scratch string
	e = newEnv(t, funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		scratch = spec.EvidenceDir
		if scratch == "" || spec.Env[runtime.EvidenceDirEnv] != scratch || strings.HasPrefix(scratch, spec.WorkDir+string(filepath.Separator)) {
			t.Errorf("evidence dir = %q, env = %q, work dir = %q", scratch, spec.Env[runtime.EvidenceDirEnv], spec.WorkDir)
		}
		must(t, os.WriteFile(filepath.Join(scratch, "home.png"), []byte("png"), 0o600))
		must(t, os.Symlink("/etc/passwd", filepath.Join(scratch, "passwd")))
		e.api.mu.Lock()
		e.api.attach[jobID] = []Attachment{
			{Path: "evidence:home.png", Name: "evidence/home.png"},
			{Path: "evidence:passwd", Name: "evidence/passwd"},
		}
		e.api.mu.Unlock()
		return runtime.Result{}, nil
	}})
	s := e.spec(func(s *domain.JobSpec) {
		s.WritePaths, s.Role = nil, "reviewer"
		s.JobMCP = &domain.JobMCP{URL: "https://forecrew.example.com/job-mcp", Token: testJobMCPTok}
	})
	jobID = s.JobID
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	e.api.mu.Lock()
	uploads := e.api.uploads
	e.api.mu.Unlock()
	if up := uploads["evidence/home.png"]; len(uploads) != 1 || string(up.body) != "png" {
		t.Fatalf("uploads = %v", uploads)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch directory left behind: %v", err)
	}
}

// A scratch directory that cannot be created is a local problem: retry.
func TestScratchDirectoryFailureRetries(t *testing.T) {
	e := newEnv(t, funcAdapter{fn: func(context.Context, runtime.RunSpec, func(domain.JobEvent)) (runtime.Result, error) {
		t.Error("agent ran")
		return runtime.Result{}, nil
	}})
	s := e.spec()
	must(t, os.MkdirAll(filepath.Join(e.workDir, "jobs"), 0o750))
	must(t, os.WriteFile(filepath.Join(e.workDir, "jobs", s.JobID.String()+".evidence"), nil, 0o600))
	if c := e.doComplete(s); c.Status != StatusRetry || !strings.Contains(c.Error, "evidence directory") {
		t.Fatalf("completion = %+v", c)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
