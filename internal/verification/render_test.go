package verification_test

import (
	"flag"
	"regexp"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/verification"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// sample is a report with every part filled.
func sample() verification.Report {
	return verification.Report{
		Version: 1, SHA: "abc1234def5678", Status: domain.VerificationFailed,
		Results: []verification.Result{
			{Environment: "linux-go", Cell: map[string]string{"os": "linux", "go": "1.27"}, Suite: "unit", Required: true, Status: "passed",
				Counts: domain.TestCounts{Passed: 120, Skipped: 2}, DurationMS: 41000, Job: "JOB-41", TestRun: "0192f5a0-0000-7000-8000-000000000001", Attempts: 2},
			{Environment: "mac-go", Cell: map[string]string{"os": "macos"}, Suite: "unit", Required: true, Status: "failed",
				Counts: domain.TestCounts{Passed: 118, Failed: 3, Errored: 1}, Job: "JOB-42", Attempts: 1},
			{Environment: "mac-go", Cell: map[string]string{"os": "macos"}, Suite: "e2e", Required: false, Status: "missing"},
			{Environment: "", Cell: map[string]string{}, Suite: "docs", Required: true, Status: "passed", Counts: domain.TestCounts{Passed: 4}, Attempts: 1},
		},
		Criteria: map[string]verification.Criterion{
			"AC-1":  {Status: "passed", Tests: map[string]string{"auth::TestLogin": "passed", "auth::TestLogout": "passed"}},
			"AC-2":  {Status: "failed", Tests: map[string]string{"auth::TestExpiry": "failed"}},
			"AC-10": {Status: "untested", Tests: map[string]string{}, Untestable: "a human reads the page"},
			"AC-11": {Status: "untested", Tests: map[string]string{}},
		},
		NewTests: map[string]bool{"auth::TestExpiry": true, "auth::TestLogin": false},
		Flaky:    []verification.Flaky{{Test: "auth::TestRefresh", Environments: []string{"linux-go", "mac-go"}}},
		Environments: map[string]verification.EnvironmentInfo{
			"linux-go": {Versions: map[string]string{"go": "go version go1.27 linux/amd64", "node": "v26.1.0"}},
			"mac-go":   {Versions: map[string]string{}},
		},
		Blocking: []string{"unit in mac-go (os=macos) failed (4 failed tests)", "AC-11 has no test", "new test auth::TestLogin passed on the base branch"},
	}
}

func TestRenderGolden(t *testing.T) {
	got := verification.Render(sample())
	testkit.CheckGolden(t, "testdata/golden/report.md", []byte(got), *update)
	if !strings.HasPrefix(got, verification.Marker+"\n") {
		t.Errorf("no marker: %q", got[:40])
	}
}

func TestRenderPassed(t *testing.T) {
	got := verification.Render(verification.Report{Version: 1, SHA: "abc1234", Status: domain.VerificationPassed})
	testkit.CheckGolden(t, "testdata/golden/passed.md", []byte(got), *update)
}

// TestRenderTreatsRepoTextAsData: names written by the repo cannot break the
// tables, open HTML, mention anyone or run to any length.
func TestRenderTreatsRepoTextAsData(t *testing.T) {
	hostile := "x|y`<script>alert(1)</script>@octocat\n## injected\n" + strings.Repeat("z", 500)
	rep := verification.Report{
		Version: 1, SHA: "abc1234", Status: domain.VerificationFailed,
		Results:      []verification.Result{{Environment: hostile, Cell: map[string]string{hostile: hostile}, Suite: hostile, Status: "failed", Required: true}},
		Criteria:     map[string]verification.Criterion{"AC-1": {Status: "failed", Tests: map[string]string{hostile: "failed"}, Untestable: hostile}},
		NewTests:     map[string]bool{hostile: false},
		Flaky:        []verification.Flaky{{Test: hostile, Environments: []string{hostile}}},
		Environments: map[string]verification.EnvironmentInfo{hostile: {Versions: map[string]string{hostile: hostile}}},
		Blocking:     []string{hostile},
	}
	got := verification.Render(rep)
	for _, bad := range []string{"\n## injected", strings.Repeat("z", 200)} {
		if strings.Contains(got, bad) {
			t.Errorf("render contains %q:\n%s", bad, got)
		}
	}
	// Outside code spans, where Markdown and HTML are live, nothing of the text is.
	live := regexp.MustCompile("`[^`]*`").ReplaceAllString(got, "")
	live = strings.ReplaceAll(live, "<br>", "")
	for _, bad := range []string{"<script>", "@octocat", "</script>"} {
		if strings.Contains(live, bad) {
			t.Errorf("%q is live outside a code span:\n%s", bad, live)
		}
	}
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "|") && !strings.HasSuffix(line, "|") {
			t.Errorf("a table row was broken: %q", line)
		}
	}
}

func TestRenderBounds(t *testing.T) {
	rep := verification.Report{Version: 1, SHA: "abc1234", Status: domain.VerificationFailed, Criteria: map[string]verification.Criterion{},
		NewTests: map[string]bool{}, Environments: map[string]verification.EnvironmentInfo{}}
	tests := map[string]string{}
	for i := range 300 {
		id := "pkg::Test" + strings.Repeat("N", 100) + string(rune('A'+i%26)) + strings.Repeat("x", i)
		tests[id] = "failed"
		rep.NewTests[id] = false
		rep.Blocking = append(rep.Blocking, "reason "+id)
		rep.Results = append(rep.Results, verification.Result{Environment: id, Suite: "s", Status: "passed"})
	}
	rep.Criteria["AC-1"] = verification.Criterion{Status: "failed", Tests: tests}
	got := verification.Render(rep)
	if len(got) > 65000 {
		t.Errorf("render is %d bytes", len(got))
	}
	if !strings.Contains(got, "more not shown") || !strings.Contains(got, "295 more") {
		t.Errorf("no note about what was left out:\n%s", got[:2000])
	}
}

func TestSummaryAndTitle(t *testing.T) {
	r := sample()
	if got := r.Summary(); got != "Failed: 2 of 3 required results passed, 1 of 4 criteria verified, 1 flaky" {
		t.Errorf("Summary = %q", got)
	}
	for status, want := range map[domain.VerificationStatus][2]string{
		domain.VerificationPassed:     {"Verified", "Passed: "},
		domain.VerificationFailed:     {"Verification failed", "Failed: "},
		domain.VerificationIncomplete: {"Verification incomplete", "Incomplete: "},
	} {
		r.Status = status
		if r.Title() != want[0] || !strings.HasPrefix(r.Summary(), want[1]) {
			t.Errorf("%s: title %q summary %q", status, r.Title(), r.Summary())
		}
	}
}

func TestParse(t *testing.T) {
	raw, err := sample().JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := verification.Parse(raw)
	if err != nil || got.SHA != "abc1234def5678" || len(got.Results) != 4 || got.Criteria["AC-10"].Untestable == "" {
		t.Fatalf("Parse = %+v, %v", got, err)
	}
	if _, err := verification.Parse([]byte(`{"version": 2}`)); err == nil {
		t.Error("an invalid report parsed")
	}
	if _, err := verification.Parse([]byte(`nope`)); err == nil {
		t.Error("bad JSON parsed")
	}
	bad := sample()
	bad.SHA = "NOT-HEX"
	if _, err := bad.JSON(); err == nil {
		t.Error("an invalid report was encoded")
	}
	empty, err := verification.Report{Version: 1, SHA: "abc1234", Status: domain.VerificationPassed}.JSON()
	if err != nil || !strings.Contains(string(empty), `"results":[]`) || !strings.Contains(string(empty), `"criteria":{}`) {
		t.Errorf("empty report = %s, %v", empty, err)
	}
}
