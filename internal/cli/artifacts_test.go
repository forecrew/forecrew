package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/verification"
)

func TestJobArtifacts(t *testing.T) {
	e := newRemoteEnv(t)
	id := domain.NewID()
	e.svc.ListArtifactsFunc = func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error) {
		return []domain.Artifact{
			{ID: id, Name: "reports/unit.xml", ContentType: "text/xml", Size: 4821, SHA256: strings.Repeat("ab", 32)},
			{Name: "raw", Size: 0, SHA256: "ab"},
		}, nil
	}
	// Both spellings of the group work.
	for _, group := range []string{"job", "jobs"} {
		out, errOut, code := e.run(t, group, "artifacts", "JOB-41")
		if code != 0 || errOut != "" {
			t.Fatalf("%s: code %d, stderr %q", group, code, errOut)
		}
		for _, want := range []string{"ID", "NAME", "SHA256", id.String() + "  reports/unit.xml  text/xml  4821  abababababab", "raw               -         0     ab"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", group, want, out)
			}
		}
	}
	if calls := e.svc.Calls(); len(calls) != 2 || calls[0].Tool != mcp.ToolListArtifacts || calls[0].Input != (mcp.JobRefInput{Job: "JOB-41"}) {
		t.Errorf("calls = %+v", calls)
	}
	e.svc.ListArtifactsFunc = nil
	if out, _, _ := e.run(t, "job", "artifacts", "JOB-1"); strings.Count(out, "\n") != 1 {
		t.Errorf("empty list prints only the header:\n%s", out)
	}
	e.svc.ListArtifactsFunc = func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error) {
		return nil, fmt.Errorf("job JOB-9: %w", mcp.ErrNotFound)
	}
	if _, errOut, code := e.run(t, "job", "artifacts", "JOB-9"); code != 1 || !strings.Contains(errOut, "JOB-9: not found") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestArtifactGet(t *testing.T) {
	e := newRemoteEnv(t)
	id := domain.NewID()
	content := "<testsuite/>\n"
	e.svc.OpenArtifactFunc = func(_ context.Context, _ domain.Actor, got string) (domain.Artifact, io.ReadCloser, error) {
		if got != id.String() {
			return domain.Artifact{}, nil, fmt.Errorf("artifact %s: %w", got, mcp.ErrNotFound)
		}
		return domain.Artifact{ID: id, Name: "unit.xml", ContentType: "text/xml", Size: int64(len(content))}, io.NopCloser(strings.NewReader(content)), nil
	}

	file := filepath.Join(t.TempDir(), "out.xml")
	if out, errOut, code := e.run(t, "artifact", "get", id.String(), "-o", file); code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != content {
		t.Fatalf("file = %q, %v", got, err)
	}
	for _, args := range [][]string{{"artifact", "get", id.String()}, {"artifact", "get", id.String(), "-o", "-"}} {
		if out, _, code := e.run(t, args...); code != 0 || out != content {
			t.Errorf("%v: code %d, stdout %q", args, code, out)
		}
	}

	// A failed request must not create the file or destroy an existing one.
	existing := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run(t, "artifact", "get", domain.NewID().String(), "-o", existing); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("unknown id: code %d, stderr %q", code, errOut)
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" {
		t.Errorf("existing file = %q", got)
	}
	missingDir := filepath.Join(t.TempDir(), "no", "such", "dir", "f")
	if _, _, code := e.run(t, "artifact", "get", id.String(), "-o", missingDir); code != 1 {
		t.Errorf("unwritable output: code %d", code)
	}
}

func TestArtifactGetCutShort(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.OpenArtifactFunc = func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error) {
		return domain.Artifact{Name: "a", Size: 100}, io.NopCloser(io.MultiReader(strings.NewReader("part"), iotest.ErrReader(errors.New("boom")))), nil
	}
	file := filepath.Join(t.TempDir(), "partial")
	if _, _, code := e.run(t, "artifact", "get", domain.NewID().String(), "-o", file); code != 1 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("a partial file was left behind: %v", err)
	}
}

func TestJobTests(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListTestRunsFunc = func(context.Context, domain.Actor, mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error) {
		return []mcp.TestRunInfo{
			{Suite: "unit", Environment: "linux-go", Cell: map[string]string{"os": "linux", "go": "1.27"}, Status: domain.TestRunFailed,
				Counts: domain.TestCounts{Passed: 40, Failed: 1, Skipped: 2}, ExitCode: 1, SHA: strings.Repeat("a", 40),
				Failures: []domain.TestResult{{Name: "TestLogout", Classname: "auth", Status: domain.TestFailed, Message: "want 204, got 500\nstack"}}},
			{Suite: "lint", Status: domain.TestRunPassed, SHA: "abc"},
		}, nil
	}
	out, errOut, code := e.run(t, "job", "tests", "JOB-41")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"SUITE", "unit   linux-go     go=1.27,os=linux  failed  40", "aaaaaaaaaaaa", "lint   -            -", "FAILED unit auth TestLogout: want 204, got 500\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stack") {
		t.Errorf("only the first line of a message is printed:\n%s", out)
	}
	if calls := e.svc.Calls(); len(calls) != 1 || calls[0].Tool != mcp.ToolListTestRuns || calls[0].Input != (mcp.ListTestRunsInput{Job: "JOB-41"}) {
		t.Errorf("calls = %+v", calls)
	}
	e.svc.ListTestRunsFunc = nil
	if out, _, _ := e.run(t, "job", "tests", "JOB-1"); strings.Count(out, "\n") != 1 {
		t.Errorf("empty list prints only the header:\n%s", out)
	}
	e.svc.ListTestRunsFunc = func(context.Context, domain.Actor, mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error) {
		return nil, fmt.Errorf("job JOB-9: %w", mcp.ErrNotFound)
	}
	if _, errOut, code := e.run(t, "job", "tests", "JOB-9"); code != 1 || !strings.Contains(errOut, "JOB-9: not found") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestJobShowPrintsTestCounts(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{Ref: "JOB-41", Tests: &mcp.TestSummary{Runs: 2, Status: domain.TestRunFailed, Passed: 3, Failed: 1}}, nil
	}
	out, _, code := e.run(t, "job", "show", "JOB-41")
	if code != 0 || !strings.Contains(out, "Tests:") || !strings.Contains(out, "failed: 3 passed, 1 failed, 0 skipped, 0 errored in 2 runs") {
		t.Errorf("code %d:\n%s", code, out)
	}
}

func TestJobShowPrintsContext(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		j := domain.Job{ContextManifest: &domain.ContextManifest{Budget: 16000, Size: 420, Sections: []domain.ContextSection{
			{Name: "assumptions", Items: []domain.ContextItem{{ID: "A-4"}}},
			{Name: "notes", Items: []domain.ContextItem{{ID: "n1"}, {ID: "n2"}}, Dropped: []domain.ContextItem{{ID: "n3"}}},
		}}}
		return mcp.JobInfo{Job: j, Ref: "JOB-41"}, nil
	}
	out, _, code := e.run(t, "job", "show", "JOB-41")
	if code != 0 || !strings.Contains(out, "Context:") || !strings.Contains(out, "420 of 16000 characters: assumptions 1, notes 2 (+1 dropped)") {
		t.Errorf("code %d:\n%s", code, out)
	}
}

func TestJobTestsStripsControlCharacters(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListTestRunsFunc = func(context.Context, domain.Actor, mcp.ListTestRunsInput) ([]mcp.TestRunInfo, error) {
		return []mcp.TestRunInfo{{TestRun: domain.TestRun{Suite: "un\x1b[2Jit", Status: domain.TestRunFailed, SHA: "abc"},
			Failures: []domain.TestResult{{Name: "T\x07est\r", Classname: "c\x1b]0;title\x07", Status: domain.TestFailed, Message: "\x1b[31mred\x1b[0m\x00"}}}}, nil
	}
	out, _, code := e.run(t, "job", "tests", "JOB-1")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07\r\x00") {
		t.Errorf("code %d, output has control characters: %q", code, out)
	}
	if !strings.Contains(out, "red") || !strings.Contains(out, "est") {
		t.Errorf("output lacks the text: %q", out)
	}
}

func TestCouncilCommands(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.StartCouncilFunc = func(context.Context, domain.Actor, mcp.StartCouncilInput) (mcp.CouncilInfo, error) {
		return mcp.CouncilInfo{Ref: "COUNCIL-2"}, nil
	}
	out, errOut, code := e.run(t, "councils", "start", "app", "Which queue?", "--kind", "technical", "--role", "ux", "--role", "architect", "--constraint", "Keep Postgres")
	if code != 0 || errOut != "" || !strings.Contains(out, "COUNCIL-2 started") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	in := e.svc.Calls()[0].Input.(mcp.StartCouncilInput)
	if in.Project != "app" || in.Question != "Which queue?" || in.Kind != "technical" || len(in.Roles) != 2 || in.Constraints[0] != "Keep Postgres" {
		t.Fatalf("input = %+v", in)
	}

	e.svc.ListCouncilsFunc = func(context.Context, domain.Actor, mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error) {
		return []mcp.CouncilInfo{{Ref: "COUNCIL-2", Kind: domain.CouncilAdhoc, Status: domain.CouncilRunning, Question: "Which\x1b[31m queue?"}}, nil
	}
	out, errOut, code = e.run(t, "councils", "list", "app", "--status", "running")
	if code != 0 || errOut != "" || !strings.Contains(out, "COUNCIL-2") || !strings.Contains(out, "running") || strings.Contains(out, "\x1b") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if in := e.svc.Calls()[1].Input; in != (mcp.ListCouncilsInput{Project: "app", Status: "running"}) {
		t.Fatalf("input = %+v", in)
	}

	e.svc.GetCouncilFunc = func(context.Context, domain.Actor, mcp.CouncilRefInput) (mcp.CouncilDetail, error) {
		return mcp.CouncilDetail{
			Ref:  "COUNCIL-2",
			Kind: domain.CouncilTechnical, Status: domain.CouncilDecided, Question: "Which\x1b[31m queue?", Constraints: []string{"Keep Postgres"},
			Brief: &domain.CouncilBrief{Recommendation: "pg", BudgetExhausted: true, Dissent: []domain.BriefDissent{{Member: "ux@codex", Position: "No."}}},
			Contributions: []mcp.ContributionInfo{{Member: "ux@codex",
				Stage: domain.StageProposal, Round: 1, EvidenceCheck: []domain.SourceCheck{{Status: domain.SourceOK}, {Status: domain.SourceRefused}}}},
			Gate:     &mcp.GateInfo{Ref: "GATE-7", Status: domain.GateApproved},
			Decision: &domain.Decision{Number: 9, DecidedBy: "user:ana", Choice: "pg"},
		}, nil
	}
	out, errOut, code = e.run(t, "council", "show", "COUNCIL-2")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"COUNCIL-2 (technical) is decided", "Constraint: Keep Postgres", "ux@codex", "2, 1 failed", "Recommendation: pg", "budget ran out", "Dissent of ux@codex: No.", "Gate: GATE-7 (approved)", "Decision: DEC-9 by user:ana: pg"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("control characters reached the terminal: %q", out)
	}

	e.svc.AddConstraintFunc = func(context.Context, domain.Actor, mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error) {
		return mcp.CouncilInfo{Ref: "COUNCIL-2", Constraints: []string{"a", "b"}}, nil
	}
	if out, _, code := e.run(t, "councils", "constrain", "COUNCIL-2", "Stay small"); code != 0 || !strings.Contains(out, "COUNCIL-2 has 2 constraints") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	last := e.svc.Calls()[len(e.svc.Calls())-1]
	if last.Input != (mcp.AddCouncilConstraintInput{Council: "COUNCIL-2", Constraint: "Stay small"}) {
		t.Fatalf("input = %+v", last.Input)
	}
	for _, args := range [][]string{{"councils", "start", "app"}, {"councils", "list"}, {"councils", "show"}, {"councils", "constrain", "COUNCIL-2"}} {
		if _, _, code := e.run(t, args...); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestVerificationCommand(t *testing.T) {
	e := newRemoteEnv(t)
	report := verification.Report{
		Version: 1, SHA: "abc1234", Status: domain.VerificationFailed,
		Results:  []verification.Result{{Environment: "linux-go\x1b[31m", Cell: map[string]string{"os": "linux"}, Suite: "unit", Required: true, Status: "failed", Counts: domain.TestCounts{Failed: 1}, Attempts: 1}},
		Criteria: map[string]verification.Criterion{"AC-1": {Status: "untested", Tests: map[string]string{}}},
		Blocking: []string{"AC-1 has no test"},
	}
	raw, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var in mcp.VerificationReportInput
	e.svc.VerificationReportFunc = func(_ context.Context, _ domain.Actor, got mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
		in = got
		return mcp.VerificationInfo{SHA: "abc1234", Status: domain.VerificationFailed, Report: raw, Change: 9}, nil
	}
	out, errOut, code := e.run(t, "verification", "9", "--project", "app")
	if code != 0 || errOut != "" || in != (mcp.VerificationReportInput{Change: "9", Project: "app"}) {
		t.Fatalf("code %d, stderr %q, input %+v", code, errOut, in)
	}
	for _, want := range []string{"Pull request #9, report of abc1234 (not the latest commit)", "Verification of `abc1234`: failed", "AC-1 has no test", "| linux-go\\[31m | `os=linux` | unit | yes | failed |"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "forecrew:verification") {
		t.Errorf("control characters or the marker reached the terminal: %q", out)
	}

	if _, _, code := e.run(t, "verification", "TASK-3"); code != 0 || in != (mcp.VerificationReportInput{Change: "TASK-3"}) {
		t.Errorf("task ref: code %d, input %+v", code, in)
	}
	if _, _, code := e.run(t, "verification"); code == 0 {
		t.Error("accepted no change")
	}
	e.svc.VerificationReportFunc = func(context.Context, domain.Actor, mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
		return mcp.VerificationInfo{Report: []byte(`{"version":3}`)}, nil
	}
	if _, errOut, code := e.run(t, "verification", "TASK-3"); code != 1 || !strings.Contains(errOut, "bad verification report") {
		t.Errorf("bad report: code %d, stderr %q", code, errOut)
	}
	e.svc.VerificationReportFunc = func(context.Context, domain.Actor, mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
		return mcp.VerificationInfo{}, fmt.Errorf("pull request #9: %w", mcp.ErrNotFound)
	}
	if _, errOut, code := e.run(t, "verification", "9", "--project", "app"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("not found: code %d, stderr %q", code, errOut)
	}
}
