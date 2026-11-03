package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// Artifact is a file stored as evidence: a test report, a log, a screenshot
// or a rendered verification report. Workers upload a job's artifacts
// through the worker API; forecrewd stores the ones it makes itself (no
// JobID). The content lives in object storage under StorageKey.
type Artifact struct {
	ID          uuid.UUID `json:"id"`
	AccountID   uuid.UUID `json:"account_id"`
	ProjectID   uuid.UUID `json:"project_id"`
	JobID       uuid.UUID `json:"job_id,omitzero"` // nil for artifacts forecrewd makes
	Name        string    `json:"name"`            // unique per job, see ValidArtifactName
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`   // bytes
	SHA256      string    `json:"sha256"` // lowercase hex of the content
	StorageKey  string    `json:"-"`      // object key in the evidence bucket
	CreatedAt   time.Time `json:"created_at"`
}

// TestRunStatus is the outcome of one suite in one matrix cell.
type TestRunStatus string

// Test run outcomes.
const (
	TestRunPassed TestRunStatus = "passed" // every test passed or was skipped
	TestRunFailed TestRunStatus = "failed" // at least one test failed or errored
	// TestRunErrored means no usable result: no report, a report that
	// could not be parsed, or the command could not run.
	TestRunErrored TestRunStatus = "errored"
)

// TestRunStatuses returns every test run outcome.
func TestRunStatuses() []TestRunStatus {
	return []TestRunStatus{TestRunPassed, TestRunFailed, TestRunErrored}
}

// TestStatus is the outcome of one test case.
type TestStatus string

// Test case outcomes, as in JUnit XML.
const (
	TestPassed  TestStatus = "passed"
	TestFailed  TestStatus = "failed"  // an assertion failed (<failure>)
	TestSkipped TestStatus = "skipped" // <skipped>
	TestErrored TestStatus = "errored" // the test crashed (<error>)
)

// TestStatuses returns every test case outcome.
func TestStatuses() []TestStatus {
	return []TestStatus{TestPassed, TestFailed, TestSkipped, TestErrored}
}

// TestCounts counts test cases by outcome.
type TestCounts struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Errored int `json:"errored"`
}

// TestRun is the result of one suite in one matrix cell, reported by a
// command job's complete call.
type TestRun struct {
	ID          uuid.UUID         `json:"id"`
	AccountID   uuid.UUID         `json:"account_id"`
	ProjectID   uuid.UUID         `json:"project_id"`
	JobID       uuid.UUID         `json:"job_id"`
	ChangeID    uuid.UUID         `json:"change_id,omitzero"` // the PR whose head is SHA, when there is one
	SHA         string            `json:"sha"`                // commit tested
	Suite       string            `json:"suite"`
	Environment string            `json:"environment"` // environment name; empty for the worker's host
	Cell        map[string]string `json:"cell"`        // matrix cell, axis -> value
	Status      TestRunStatus     `json:"status"`
	Counts      TestCounts        `json:"counts"`
	DurationMS  int64             `json:"duration_ms"`
	ExitCode    int               `json:"exit_code"` // -1 when the command did not exit by itself
	Versions    map[string]string `json:"versions"`  // tool -> version output
	// ReportArtifacts names the job's artifacts holding the JUnit reports.
	ReportArtifacts []string  `json:"report_artifacts"`
	Attempt         int       `json:"attempt"` // the job attempt that produced it, from 1
	CreatedAt       time.Time `json:"created_at"`
}

// TestResult is one test case of a test run.
type TestResult struct {
	ID         uuid.UUID  `json:"id"`
	AccountID  uuid.UUID  `json:"account_id"`
	RunID      uuid.UUID  `json:"run_id"`
	Name       string     `json:"name"`
	Classname  string     `json:"classname"`
	Status     TestStatus `json:"status"`
	DurationMS int64      `json:"duration_ms"`
	Message    string     `json:"message,omitempty"` // failure, error or skip message
}

// VerificationStatus is the outcome of a verification report.
type VerificationStatus string

// Verification outcomes.
const (
	VerificationPassed     VerificationStatus = "passed"
	VerificationFailed     VerificationStatus = "failed"
	VerificationIncomplete VerificationStatus = "incomplete" // required cells are still missing
)

// VerificationStatuses returns every verification outcome.
func VerificationStatuses() []VerificationStatus {
	return []VerificationStatus{VerificationPassed, VerificationFailed, VerificationIncomplete}
}

// VerificationReport is the verification of one PR head: results per
// environment, acceptance criteria to tests, new tests, flaky tests and the
// reasons that block the merge. Report follows
// docs/contracts/verification.schema.json.
type VerificationReport struct {
	ID         uuid.UUID          `json:"id"`
	AccountID  uuid.UUID          `json:"account_id"`
	ProjectID  uuid.UUID          `json:"project_id"`
	ChangeID   uuid.UUID          `json:"change_id"`
	SHA        string             `json:"sha"`
	Status     VerificationStatus `json:"status"`
	Report     json.RawMessage    `json:"report"`
	ArtifactID uuid.UUID          `json:"artifact_id,omitzero"` // rendered Markdown, when stored
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}
