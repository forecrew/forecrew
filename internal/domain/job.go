package domain

import (
	"encoding/json"
	"log/slog"
	"time"
	"uuid"
)

// JobStatus is the state of a job.
type JobStatus string

// Job states.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running" // leased by a worker
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

// JobStatuses returns every job state.
func JobStatuses() []JobStatus {
	return []JobStatus{JobQueued, JobRunning, JobSucceeded, JobFailed, JobCanceled}
}

var jobTransitions = transitions[JobStatus]{
	JobQueued:  {JobRunning, JobCanceled},
	JobRunning: {JobSucceeded, JobFailed, JobCanceled, JobQueued}, // queued again on lease expiry or retry
}

// CanTransition reports whether a job may move from s to to.
func (s JobStatus) CanTransition(to JobStatus) bool {
	return jobTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s JobStatus) Transition(to JobStatus) (JobStatus, error) {
	return jobTransitions.apply("job", s, to)
}

// Terminal reports whether no transition leaves s.
func (s JobStatus) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCanceled
}

// LeaseDuration is how long a lease lasts without a heartbeat.
const LeaseDuration = 5 * time.Minute

// DefaultMaxAttempts is the MaxAttempts of a job when none is given.
const DefaultMaxAttempts = 3

// Usage counts the tokens a runtime used.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Job is one run of an agent role on a repo, executed by a worker.
type Job struct {
	ID          uuid.UUID `json:"id"`
	AccountID   uuid.UUID `json:"account_id"`
	ProjectID   uuid.UUID `json:"project_id"`
	Number      int64     `json:"number"` // JOB-<number>
	Status      JobStatus `json:"status"`
	Role        string    `json:"role"`
	Runtime     string    `json:"runtime"` // runtime adapter: claude-code or codex
	Needs       []string  `json:"needs"`   // labels the worker must have, besides runtime:<runtime>
	GoalID      uuid.UUID `json:"goal_id,omitzero"`
	SpecID      uuid.UUID `json:"spec_id,omitzero"`
	TaskID      uuid.UUID `json:"task_id,omitzero"`
	Spec        JobSpec   `json:"spec"` // without secrets, see JobSpec.Redacted
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	WorkerID    uuid.UUID `json:"worker_id,omitzero"` // set while running
	// LeaseExpiresAt is zero unless the job is running.
	LeaseExpiresAt time.Time       `json:"lease_expires_at,omitzero"`
	StartedAt      time.Time       `json:"started_at,omitzero"`  // last lease
	FinishedAt     time.Time       `json:"finished_at,omitzero"` // terminal state reached
	Output         json.RawMessage `json:"output,omitempty"`     // structured output, valid against Spec.OutputSchema
	HeadSHA        string          `json:"head_sha,omitempty"`   // commit pushed to Spec.Branch, if any
	Error          string          `json:"error,omitempty"`
	Usage          Usage           `json:"usage"`
	CostUSD        float64         `json:"cost_usd"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Ref returns the job's human ref, e.g. JOB-41.
func (j Job) Ref() string { return Ref(RefJob, j.Number) }

// RequiredLabels returns the labels a worker needs to lease the job: Needs
// plus runtime:<Runtime>.
func (j Job) RequiredLabels() []string {
	return append(append([]string(nil), j.Needs...), RuntimeLabel(j.Runtime))
}

// Lease moves a queued job to running under worker until now+LeaseDuration.
func (j *Job) Lease(workerID uuid.UUID, now time.Time) error {
	status, err := j.Status.Transition(JobRunning)
	if err != nil {
		return err
	}
	j.Status = status
	j.WorkerID = workerID
	j.StartedAt = now
	j.LeaseExpiresAt = now.Add(LeaseDuration)
	j.UpdatedAt = now
	return nil
}

// Requeue handles a lost attempt (lease expired, or the worker asked to
// retry): Attempts is incremented and the job goes back to queued, or to
// failed when Attempts reaches MaxAttempts.
func (j *Job) Requeue(reason string, now time.Time) error {
	if j.Status != JobRunning {
		return invalidTransition("job", j.Status, JobQueued)
	}
	j.Attempts++
	j.WorkerID = uuid.Nil()
	j.LeaseExpiresAt = time.Time{}
	j.UpdatedAt = now
	if j.Attempts >= j.MaxAttempts {
		j.Status = JobFailed
		j.FinishedAt = now
		j.Error = "attempts exhausted: " + reason
		return nil
	}
	j.Status = JobQueued
	return nil
}

// Finish moves the job to a terminal state.
func (j *Job) Finish(to JobStatus, now time.Time) error {
	if !to.Terminal() {
		return invalidTransition("job", j.Status, to)
	}
	status, err := j.Status.Transition(to)
	if err != nil {
		return err
	}
	j.Status = status
	j.WorkerID = uuid.Nil()
	j.LeaseExpiresAt = time.Time{}
	j.FinishedAt = now
	j.UpdatedAt = now
	return nil
}

// JobSpec is everything a worker receives to run a job
// (docs/contracts/job-spec.md). Env and RepoToken are secrets: they are
// added when the job is leased, kept in memory only and never stored or
// logged.
type JobSpec struct {
	JobID     uuid.UUID `json:"job_id"`
	AccountID uuid.UUID `json:"account_id"`
	Repo      string    `json:"repo"`     // "owner/name"
	BaseRef   string    `json:"base_ref"` // branch or SHA to start from
	Branch    string    `json:"branch"`   // agent/<...> branch to check out; writing roles push it, read-only roles may leave it empty or name one to inspect (never pushed)
	Role      string    `json:"role"`
	// WritePaths are doublestar globs relative to the repo root; a pattern
	// starting with "!" excludes. Empty means read-only.
	WritePaths       []string          `json:"write_paths"`
	Runtime          string            `json:"runtime"` // runtime adapter: claude-code or codex
	Model            string            `json:"model"`
	Prompt           string            `json:"prompt"`
	OutputSchema     json.RawMessage   `json:"output_schema,omitempty"` // inline JSON Schema, no $ref
	AllowedTools     []string          `json:"allowed_tools"`
	MaxTurns         int               `json:"max_turns"`
	Timeout          Duration          `json:"timeout"`
	IncludeRepoFacts bool              `json:"include_repo_facts"`
	Env              map[string]string `json:"env,omitempty"`
	RepoToken        string            `json:"repo_token,omitempty"`
}

// Redacted returns a copy without Env and RepoToken, the form that is stored.
func (s JobSpec) Redacted() JobSpec {
	s.Env = nil
	s.RepoToken = ""
	return s
}

// LogValue keeps secrets out of logs.
func (s JobSpec) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("job_id", s.JobID.String()),
		slog.String("repo", s.Repo),
		slog.String("role", s.Role),
		slog.String("runtime", s.Runtime),
		slog.String("model", s.Model),
	)
}

// Duration is a time.Duration encoded in JSON as a Go duration string ("30m0s").
type Duration time.Duration

// MarshalText encodes d as a Go duration string.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalText decodes a Go duration string such as "45m".
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
