package playbooks_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

// atHead moves the PR's head to the commit the reports are about.
func (v *verified) atHead(t *testing.T) github.PullRequest {
	t.Helper()
	must(t, v.tracker.SetHeadSHA("acme/app", v.change.Number, headSHA))
	pr, err := v.tracker.GetPR(t.Context(), "acme/app", v.change.Number)
	must(t, err)
	return pr
}

var passingBase = map[string]any{"base_check": map[string]any{"ok": true, "new_tests": []any{map[string]any{"id": "pkg::TestA", "failed_on_base": true}}}}

// report builds a report of status passed, failed or incomplete.
func (v *verified) report(t *testing.T, status string) buildOut {
	t.Helper()
	v.testerJob(t, "test-engineer", v.task, `{"files":["a_test.go"],"tests":[{"criterion":"AC-1","suite":"unit","id":"pkg::TestA","file":"a_test.go"}],`+
		`"untestable":[{"criterion":"AC-2","reason":"a human reads the page"}]}`)
	switch status {
	case "incomplete":
		v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	case "failed":
		v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
		v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
		v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	default:
		v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
		v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
		v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	}
	got := v.build(t, passingBase)
	if got.Status != status {
		t.Fatalf("report = %+v, want %s", got, status)
	}
	return got
}

func TestMergePRRequiresVerification(t *testing.T) {
	merge := func(v *verified, require bool) error {
		_, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": v.change.Number, "require_verification": require})
		return err
	}

	t.Run("no report", func(t *testing.T) {
		v := newVerified(t)
		v.atHead(t)
		wantErr(t, merge(v, true), "has no verification report")
	})
	t.Run("head moved after the report", func(t *testing.T) {
		v := newVerified(t)
		v.atHead(t)
		v.report(t, "passed")
		must(t, v.tracker.SetHeadSHA("acme/app", v.change.Number, "fedcba9"))
		wantErr(t, merge(v, true), "fedcba9 has no verification report")
	})
	t.Run("incomplete", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		v.report(t, "incomplete")
		// A neutral check run counts as passed, so the checks alone let it through.
		must(t, v.tracker.SetCheckStatus("acme/app", headSHA, github.ChecksSuccess, 1))
		wantErr(t, merge(v, true), "the verification of "+headSHA+" is incomplete: ")
		if got, _ := v.tracker.GetPR(t.Context(), "acme/app", pr.Number); got.Merged {
			t.Fatal("merged an incomplete head")
		}
		// Without the option the playbook of before is unchanged.
		must(t, merge(v, false))
	})
	t.Run("failed", func(t *testing.T) {
		v := newVerified(t)
		v.atHead(t)
		v.report(t, "failed")
		err := merge(v, true)
		wantErr(t, err, "is failed: ")
		wantErr(t, err, "unit in linux-go (os=linux) failed")
	})
	t.Run("passed", func(t *testing.T) {
		v := newVerified(t)
		v.atHead(t)
		v.report(t, "passed")
		must(t, merge(v, true))
		if pr, _ := v.tracker.GetPR(t.Context(), "acme/app", v.change.Number); !pr.Merged {
			t.Error("a verified head was not merged")
		}
	})
	t.Run("many reasons are cut", func(t *testing.T) {
		v := newVerified(t)
		v.atHead(t)
		v.testerJob(t, "test-engineer", v.task, `{"tests":[],"untestable":[]}`)
		v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "a::A", "a::B", "a::C", "a::D", "a::E", "a::F")...)
		v.build(t, map[string]any{"base_check": map[string]any{"new_tests": []any{
			map[string]any{"id": "n::A", "failed_on_base": false}, map[string]any{"id": "n::B", "failed_on_base": false}, map[string]any{"id": "n::C", "failed_on_base": false}}}})
		err := merge(v, true)
		wantErr(t, err, "; and 3 more: fix it")
		if strings.Contains(err.Error(), "n::C") {
			t.Errorf("error = %v", err)
		}
	})
}

func TestMergePRVerificationReadFails(t *testing.T) {
	v := newVerified(t)
	v.atHead(t)
	v.report(t, "passed")
	v.st = failingReports{Store: v.st}
	v.register()
	_, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": v.change.Number, "require_verification": true})
	wantErr(t, err, "get the verification report")
}

func TestVerificationRead(t *testing.T) {
	v := newVerified(t)
	read := func() map[string]any {
		out, err := v.call(t, "verification.read", map[string]any{"change": v.change.Number, "sha": headSHA})
		must(t, err)
		return toMap(t, out)
	}
	_, err := v.call(t, "verification.read", map[string]any{"change": v.change.Number, "sha": headSHA})
	wantErr(t, err, "has no verification report at "+headSHA)
	_, err = v.call(t, "verification.read", map[string]any{"change": 0, "sha": headSHA})
	wantErr(t, err, "change and sha are required")
	_, err = v.call(t, "verification.read", map[string]any{"change": 99, "sha": headSHA})
	wantErr(t, err, "has no verification report")

	v.report(t, "incomplete")
	m := read()
	if m["status"] != "incomplete" || m["complete"] != false || !slices.Equal(anyStrings(m["if_incomplete"]), []string{headSHA}) || len(anyStrings(m["blocking"])) == 0 {
		t.Errorf("incomplete = %v", m)
	}
	text := m["text"].(string)
	for _, want := range []string{"Verification report of acme/app at " + headSHA, "Incomplete: ", "<untrusted-data", "What blocks the merge"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}

	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	v.build(t, passingBase)
	m = read()
	if m["status"] != "passed" || m["complete"] != true || len(anyStrings(m["if_incomplete"])) != 0 || len(anyStrings(m["blocking"])) != 0 ||
		!strings.HasPrefix(m["summary"].(string), "Passed: ") || strings.Contains(m["text"].(string), "untrusted") {
		t.Errorf("passed = %v", m)
	}
}

func TestVerificationReadErrors(t *testing.T) {
	v := newVerified(t)
	_, err := v.call(t, "verification.read", map[string]any{"change": v.change.Number, "sha": headSHA, "x": 1})
	wantErr(t, err, "unknown field")
	v.atHead(t)
	v.report(t, "passed")
	v.st = failingReports{Store: v.st}
	v.register()
	_, err = v.call(t, "verification.read", map[string]any{"change": v.change.Number, "sha": headSHA})
	wantErr(t, err, "get the verification report")
}

func TestGoalAssessVerification(t *testing.T) {
	assess := func(v *verified, pr github.PullRequest, extra map[string]any) (map[string]any, error) {
		with := assessWith(pr, "success", approveReview())
		with["require_verification"] = true
		with["task_id"] = v.task.ID.String()
		maps.Copy(with, extra)
		out, err := v.call(t, "goal.assess", with)
		if err != nil {
			return nil, err
		}
		return toMap(t, out), nil
	}

	t.Run("passed shows the status and the link", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		v.report(t, "passed")
		m, err := assess(v, pr, nil)
		must(t, err)
		sum := m["gate_summary"].(string)
		if m["done"] != true || !strings.Contains(sum, "Verification: Passed: ") || !strings.Contains(sum, "Report: "+pr.URL) || !strings.Contains(sum, "forecrew/verification") {
			t.Errorf("assess = %v", m)
		}
		// The line comes before the review.
		if strings.Index(sum, "Verification:") > strings.Index(sum, "Review:") {
			t.Errorf("summary:\n%s", sum)
		}
	})
	t.Run("failed is rework with the reasons", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		v.report(t, "failed")
		m, err := assess(v, pr, nil)
		must(t, err)
		fb := m["feedback"].(string)
		if m["done"] != false || m["gate_summary"] != "" || len(toInts(m["if_ready"])) != 0 {
			t.Fatalf("assess = %v", m)
		}
		for _, want := range []string{"The verification of " + pr.URL + " at " + headSHA + " failed: Failed: ", "<untrusted-data", "unit in linux-go (os=linux) failed", "never the tests"} {
			if !strings.Contains(fb, want) {
				t.Errorf("feedback lacks %q:\n%s", want, fb)
			}
		}
	})
	t.Run("a failed report and failing checks give one feedback", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		v.report(t, "failed")
		m, err := assess(v, pr, map[string]any{"checks": "failure"})
		must(t, err)
		if fb := m["feedback"].(string); !strings.Contains(fb, "The checks of") || !strings.Contains(fb, "The verification of") {
			t.Errorf("feedback = %s", fb)
		}
	})
	t.Run("incomplete and missing are errors", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		_, err := assess(v, pr, nil)
		wantErr(t, err, "has no verification report at "+headSHA)
		v.report(t, "incomplete")
		_, err = assess(v, pr, nil)
		wantErr(t, err, "is incomplete")
		_, err = assess(v, pr, map[string]any{"sha": "other"})
		wantErr(t, err, "no verification report")
	})
	t.Run("without the option the report is not read", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		out, err := v.call(t, "goal.assess", assessWith(pr, "success", approveReview()))
		must(t, err)
		if m := toMap(t, out); m["done"] != true || strings.Contains(m["gate_summary"].(string), "Verification:") {
			t.Errorf("assess = %v", m)
		}
	})
	t.Run("a store that fails", func(t *testing.T) {
		v := newVerified(t)
		pr := v.atHead(t)
		v.report(t, "passed")
		v.st = failingReports{Store: v.st}
		v.register()
		_, err := assess(v, pr, nil)
		wantErr(t, err, "get the verification report")
	})
}

// failingReports is a store whose reports cannot be read.
type failingReports struct{ store.Store }

func (failingReports) VerificationReports() store.VerificationReportRepo { return brokenReports{} }

type brokenReports struct{ store.VerificationReportRepo }

func (brokenReports) GetBySHA(context.Context, uuid.UUID, uuid.UUID, string) (domain.VerificationReport, error) {
	return domain.VerificationReport{}, errors.New("db down")
}
