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

// Retry backoff after a worker gives up an attempt (Job.Release): the job
// waits RetryBackoffBase * 2^(attempts-1), at most RetryBackoffMax.
const (
	RetryBackoffBase = 30 * time.Second
	RetryBackoffMax  = 10 * time.Minute
)

// RetryBackoff returns how long a job is held back after its attempts-th
// failed attempt.
func RetryBackoff(attempts int) time.Duration {
	d := RetryBackoffBase
	for range max(attempts-1, 0) {
		d *= 2
		if d >= RetryBackoffMax {
			return RetryBackoffMax
		}
	}
	return min(d, RetryBackoffMax)
}

// DefaultMaxAttempts is the MaxAttempts of a job when none is given.
const DefaultMaxAttempts = 3

// Usage counts the tokens a runtime used.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// JobKind is what a job runs.
type JobKind string

// Job kinds.
const (
	JobKindAgent   JobKind = "agent"   // an agent role through a runtime adapter
	JobKindCommand JobKind = "command" // a command, without a model
)

// JobKinds returns every job kind.
func JobKinds() []JobKind { return []JobKind{JobKindAgent, JobKindCommand} }

// OrDefault returns k, or JobKindAgent when k is empty (specs stored before
// job kinds existed).
func (k JobKind) OrDefault() JobKind {
	if k == "" {
		return JobKindAgent
	}
	return k
}

// Job is one run of an agent role or a command on a repo, executed by a
// worker.
type Job struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	Number    int64     `json:"number"` // JOB-<number>
	Status    JobStatus `json:"status"`
	Kind      JobKind   `json:"kind"` // agent or command; empty is stored as agent
	// Pool is the worker pool the job runs in; only workers of that pool
	// lease it. Empty is stored as PoolBuild.
	Pool        string    `json:"pool"`
	Role        string    `json:"role"`    // role name; for command jobs the role that asked for it, may be empty
	Runtime     string    `json:"runtime"` // runtime adapter: claude-code or codex; empty for command jobs
	Needs       []string  `json:"needs"`   // labels the worker must have, besides runtime:<runtime> for agent jobs
	GoalID      uuid.UUID `json:"goal_id,omitzero"`
	SpecID      uuid.UUID `json:"spec_id,omitzero"`
	TaskID      uuid.UUID `json:"task_id,omitzero"`
	Spec        JobSpec   `json:"spec"` // without secrets, see JobSpec.Redacted
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	WorkerID    uuid.UUID `json:"worker_id,omitzero"` // set while running
	// RunAfter holds a released job back until then; zero means no delay.
	RunAfter time.Time `json:"run_after,omitzero"`
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

// RequiredLabels returns the labels a worker needs to lease the job: Needs,
// plus runtime:<Runtime> for agent jobs. Command jobs have no runtime.
func (j Job) RequiredLabels() []string {
	out := append([]string(nil), j.Needs...)
	if j.Kind.OrDefault() == JobKindAgent {
		out = append(out, RuntimeLabel(j.Runtime))
	}
	return out
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
	j.RunAfter = time.Time{}
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

// Release gives up the attempt like Requeue and, when the job is queued
// again, holds it back for RetryBackoff.
func (j *Job) Release(reason string, now time.Time) error {
	if err := j.Requeue(reason, now); err != nil {
		return err
	}
	if j.Status == JobQueued {
		j.RunAfter = now.Add(RetryBackoff(j.Attempts))
	}
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
//
// Agent jobs use Runtime, Model, Prompt and the fields after them; command
// jobs use Command, Environment, Cell, Suite and Overlay instead (see
// Validate).
type JobSpec struct {
	JobID     uuid.UUID `json:"job_id"`
	AccountID uuid.UUID `json:"account_id"`
	Kind      JobKind   `json:"kind,omitempty"` // absent means agent
	Repo      string    `json:"repo"`           // "owner/name"
	BaseRef   string    `json:"base_ref"`       // branch or SHA to start from
	Branch    string    `json:"branch"`         // agent/<...> branch to check out; writing roles push it, read-only roles may leave it empty or name one to inspect (never pushed)
	Role      string    `json:"role"`
	// WritePaths are doublestar globs relative to the repo root; a pattern
	// starting with "!" excludes. Empty means read-only.
	WritePaths       []string        `json:"write_paths"`
	Runtime          string          `json:"runtime"` // runtime adapter: claude-code or codex
	Model            string          `json:"model"`
	Prompt           string          `json:"prompt"`
	OutputSchema     json.RawMessage `json:"output_schema,omitempty"` // inline JSON Schema, no $ref
	AllowedTools     []string        `json:"allowed_tools"`
	MaxTurns         int             `json:"max_turns"`
	Timeout          Duration        `json:"timeout"`
	IncludeRepoFacts bool            `json:"include_repo_facts"`
	// LockedPaths are globs (like WritePaths, without "!") an agent job may
	// not change even when WritePaths allow it, e.g. tests written before
	// the developer's job.
	LockedPaths []string `json:"locked_paths,omitempty"`
	// Toolbox is the role's toolbox, resolved; agent jobs only.
	Toolbox *Toolbox `json:"toolbox,omitempty"`

	// Command is what a command job runs.
	Command *CommandSpec `json:"command,omitempty"`
	// Environment is the resolved environment a command job runs in; nil
	// means the worker's host as it is.
	Environment *Environment `json:"environment,omitempty"`
	// Cell is the matrix cell (axis -> value) the command job runs.
	Cell map[string]string `json:"cell,omitempty"`
	// Suite is the matrix suite the command job runs.
	Suite string `json:"suite,omitempty"`
	// Overlay checks out files from another ref on top of BaseRef before
	// the command runs.
	Overlay *Overlay `json:"overlay,omitempty"`

	Env       map[string]string `json:"env,omitempty"`
	RepoToken string            `json:"repo_token,omitempty"`
}

// CommandSpec is what a command job runs. Exactly one of Argv and Shell is
// set.
type CommandSpec struct {
	Argv  []string `json:"argv,omitempty"`  // program and arguments, run without a shell
	Shell string   `json:"shell,omitempty"` // run with sh -c, or cmd /c on Windows
	// Env holds non-secret variables; names that look like secrets are
	// refused (SecretLikeName).
	Env       map[string]string `json:"env,omitempty"`
	Workdir   string            `json:"workdir,omitempty"`   // relative to the repo root; empty is the root
	Reports   []string          `json:"reports,omitempty"`   // JUnit XML globs, relative to the repo root
	Artifacts []string          `json:"artifacts,omitempty"` // globs of files to upload, relative to the repo root
}

// Overlay names files taken from another ref, e.g. new tests from the change
// branch run against the base.
type Overlay struct {
	Ref   string   `json:"ref"`   // branch or SHA
	Paths []string `json:"paths"` // paths or globs relative to the repo root
}

// IsCommand reports whether the spec is of a command job.
func (s JobSpec) IsCommand() bool { return s.Kind == JobKindCommand }

// Redacted returns a copy without Env and RepoToken, the form that is stored.
func (s JobSpec) Redacted() JobSpec {
	s.Env = nil
	s.RepoToken = ""
	return s
}

// LogValue keeps secrets out of logs.
func (s JobSpec) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("job_id", s.JobID.String()),
		slog.String("kind", string(s.Kind.OrDefault())),
		slog.String("repo", s.Repo),
		slog.String("role", s.Role),
	}
	if s.IsCommand() {
		attrs = append(attrs, slog.String("suite", s.Suite))
		if s.Environment != nil {
			attrs = append(attrs, slog.String("environment", s.Environment.Name))
		}
	} else {
		attrs = append(attrs, slog.String("runtime", s.Runtime), slog.String("model", s.Model))
	}
	return slog.GroupValue(attrs...)
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
