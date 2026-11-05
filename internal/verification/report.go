// Package verification turns the stored test runs of a PR head into one
// verification report (docs/contracts/verification.schema.json): results per
// environment, acceptance criterion to tests to result, new tests that failed
// on base, flaky tests and tool versions. It stores the report, renders it as
// Markdown and publishes it on the PR as a comment and a check run.
package verification

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testsys"
)

// Report is the verification of one PR head; its JSON is the report column of
// verification_reports. The arrays and maps are never nil.
type Report struct {
	Version      int                        `json:"version"`
	SHA          string                     `json:"sha"`
	Status       domain.VerificationStatus  `json:"status"`
	Results      []Result                   `json:"results"`
	Criteria     map[string]Criterion       `json:"criteria"`
	NewTests     map[string]bool            `json:"new_tests"` // test -> failed on base
	Flaky        []Flaky                    `json:"flaky"`
	Environments map[string]EnvironmentInfo `json:"environments"`
	Blocking     []string                   `json:"blocking"`
}

// Result statuses besides the test run statuses.
const (
	// ResultMissing means the cell has no result yet.
	ResultMissing = "missing"
	// TestMissing means a test has no result in a cell that ran.
	TestMissing = "missing"
	// CriterionUntested means no test result covers the criterion.
	CriterionUntested = "untested"
)

// Result is the latest attempt of one suite in one cell of an environment.
type Result struct {
	Environment string            `json:"environment"`
	Cell        map[string]string `json:"cell"`
	Suite       string            `json:"suite"`
	Required    bool              `json:"required"`
	// Status is the test run's status, or ResultMissing.
	Status     string            `json:"status"`
	Counts     domain.TestCounts `json:"counts"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Job        string            `json:"job,omitempty"` // JOB-41
	TestRun    string            `json:"test_run,omitempty"`
	Attempts   int               `json:"attempts"`
}

// Criterion is an acceptance criterion with the tests that cover it.
type Criterion struct {
	// Status is passed, failed or CriterionUntested.
	Status string `json:"status"`
	// Tests holds, per test, its worst result over the required results.
	Tests      map[string]string `json:"tests"`
	Untestable string            `json:"untestable,omitempty"`
}

// Flaky is a test that failed and then passed in a rerun of the same cell.
type Flaky struct {
	Test         string   `json:"test"`
	Environments []string `json:"environments"`
}

// EnvironmentInfo holds the tool versions the jobs of an environment reported.
type EnvironmentInfo struct {
	Versions map[string]string `json:"versions"`
}

// Parse decodes the report column of a verification report and checks it
// against the schema.
func Parse(raw []byte) (Report, error) {
	if err := testsys.ValidateVerification(raw); err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, fmt.Errorf("decode verification report: %w", err)
	}
	return r.normalize(), nil
}

// normalize makes the arrays and maps non-nil, so they encode as [] and {}.
func (r Report) normalize() Report {
	r.Results = nonNil(r.Results)
	r.Flaky = nonNil(r.Flaky)
	r.Blocking = nonNil(r.Blocking)
	if r.Criteria == nil {
		r.Criteria = map[string]Criterion{}
	}
	if r.NewTests == nil {
		r.NewTests = map[string]bool{}
	}
	if r.Environments == nil {
		r.Environments = map[string]EnvironmentInfo{}
	}
	for i := range r.Results {
		if r.Results[i].Cell == nil {
			r.Results[i].Cell = map[string]string{}
		}
	}
	for k, c := range r.Criteria {
		if c.Tests == nil {
			c.Tests = map[string]string{}
			r.Criteria[k] = c
		}
	}
	for k, e := range r.Environments {
		if e.Versions == nil {
			r.Environments[k] = EnvironmentInfo{Versions: map[string]string{}}
		}
	}
	return r
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// JSON returns the report as stored, checked against the schema.
func (r Report) JSON() ([]byte, error) {
	raw, err := json.Marshal(r.normalize())
	if err != nil {
		return nil, fmt.Errorf("encode verification report: %w", err)
	}
	if err := testsys.ValidateVerification(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// CellKey writes a matrix cell as text, the axis values sorted by axis
// (os=linux,go=1.27), as internal/matrix keys its cells.
func CellKey(cell map[string]string) string {
	parts := make([]string, 0, len(cell))
	for axis, v := range cell {
		parts = append(parts, axis+"="+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// Summary is a one-line description of the outcome.
func (r Report) Summary() string {
	var required, ok int
	for _, x := range r.Results {
		if x.Required {
			required++
			if x.Status == string(domain.TestRunPassed) {
				ok++
			}
		}
	}
	tested := 0
	for _, c := range r.Criteria {
		if c.Status == string(domain.TestPassed) {
			tested++
		}
	}
	line := fmt.Sprintf("%d of %d required results passed, %d of %d criteria verified", ok, required, tested, len(r.Criteria))
	if n := len(r.Flaky); n > 0 {
		line += ", " + strconv.Itoa(n) + " flaky"
	}
	switch r.Status {
	case domain.VerificationPassed:
		return "Passed: " + line
	case domain.VerificationFailed:
		return "Failed: " + line
	}
	return "Incomplete: " + line
}

// criterionNumber orders criteria ids by their number (AC-2 before AC-10).
func criterionNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "AC-"))
	if err != nil {
		return 0
	}
	return n
}

// sortedCriteria returns the criterion ids in number order.
func (r Report) sortedCriteria() []string {
	ids := make([]string, 0, len(r.Criteria))
	for id := range r.Criteria {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Or(cmp.Compare(criterionNumber(a), criterionNumber(b)), cmp.Compare(a, b))
	})
	return ids
}
