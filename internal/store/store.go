// Package store defines the persistence contract of forecrewd. The Postgres
// implementation follows docs/contracts/data-model.md; internal/testkit has
// an in-memory fake.
//
// Rules shared by every repository:
//   - Every project-scoped call takes the account ID and only sees rows of
//     that account. The only unscoped lookups are the ones that discover the
//     account: Projects.ListByRepo (webhooks), Workers.GetByTokenHash and
//     MCPTokens.GetByTokenHash.
//   - Create fills ID (a new UUIDv7 when nil), Number (next human ref
//     number of the account for entities that have one), CreatedAt and
//     UpdatedAt, and writes them back into the argument.
//   - Get-style calls return ErrNotFound when no row matches.
//   - UpdateStatus is a compare-and-set: it fails with an error wrapping
//     domain.ErrInvalidTransition when from -> to is not allowed, and with
//     ErrConflict when the current status is not from.
//   - List calls return rows oldest first (by number, or created_at for
//     entities without one). A zero Limit means no limit.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Sentinel errors returned by every implementation.
var (
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a unique constraint or a compare-and-set failed.
	ErrConflict = errors.New("conflict")
	// ErrNoJob means LeaseNext found no queued job the worker can run.
	ErrNoJob = errors.New("no job available")
	// ErrLeaseLost means the job is not running under the caller's lease.
	ErrLeaseLost = errors.New("lease lost")
)

// Store gives access to every repository.
type Store interface {
	Accounts() AccountRepo
	Projects() ProjectRepo
	Goals() GoalRepo
	Specs() SpecRepo
	Tasks() TaskRepo
	Changes() ChangeRepo
	Jobs() JobRepo
	JobEvents() JobEventRepo
	Gates() GateRepo
	Signals() SignalRepo
	Workers() WorkerRepo
	WorkflowRuns() WorkflowRunRepo
	StepRuns() StepRunRepo
	WebhookDeliveries() WebhookDeliveryRepo
	MCPTokens() MCPTokenRepo
	Audit() AuditRepo
	// WithTx runs fn in a transaction: tx sees fn's writes, which are
	// committed when fn returns nil and rolled back otherwise. Calls on tx
	// must use the ctx passed to fn.
	WithTx(ctx context.Context, fn func(ctx context.Context, tx Store) error) error
}

// AccountRepo stores accounts.
type AccountRepo interface {
	Create(ctx context.Context, a *domain.Account) error
	Get(ctx context.Context, id uuid.UUID) (domain.Account, error)
	List(ctx context.Context) ([]domain.Account, error)
}

// ProjectRepo stores projects. Name and Repo are unique per account
// (case-insensitive for Repo); a duplicate fails with ErrConflict.
type ProjectRepo interface {
	Create(ctx context.Context, p *domain.Project) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Project, error)
	// GetByName finds a project by name or by repo ("owner/name"), ignoring case.
	GetByName(ctx context.Context, accountID uuid.UUID, nameOrRepo string) (domain.Project, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Project, error)
	// ListByRepo finds the projects of every account for a repo, ignoring
	// case. It is unscoped: webhooks use it to find the account.
	ListByRepo(ctx context.Context, repo string) ([]domain.Project, error)
	UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.ProjectStatus) error
	// SetProfile stores the approved profile (JSON).
	SetProfile(ctx context.Context, accountID, id uuid.UUID, profile json.RawMessage) error
}

// GoalFilter selects goals; zero fields match everything.
type GoalFilter struct {
	ProjectID uuid.UUID
	Status    domain.GoalStatus
	Limit     int
}

// GoalRepo stores goals.
type GoalRepo interface {
	Create(ctx context.Context, g *domain.Goal) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Goal, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Goal, error)
	List(ctx context.Context, accountID uuid.UUID, f GoalFilter) ([]domain.Goal, error)
	// Update writes Title, Body and IssueNumber.
	Update(ctx context.Context, g *domain.Goal) error
	UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.GoalStatus) error
}

// SpecRepo stores spec revisions. (goal_id, revision) is unique.
type SpecRepo interface {
	Create(ctx context.Context, s *domain.Spec) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Spec, error)
	// ListByGoal returns every revision of a goal's spec, oldest first.
	ListByGoal(ctx context.Context, accountID, goalID uuid.UUID) ([]domain.Spec, error)
	// Latest returns the highest revision of a goal's spec.
	Latest(ctx context.Context, accountID, goalID uuid.UUID) (domain.Spec, error)
}

// TaskFilter selects tasks; zero fields match everything.
type TaskFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	SpecID    uuid.UUID
	Status    domain.TaskStatus
	Limit     int
}

// TaskRepo stores tasks. List returns tasks ordered by Seq, then number.
type TaskRepo interface {
	Create(ctx context.Context, t *domain.Task) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Task, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Task, error)
	// GetByIssue finds the task mirrored by a GitHub issue of the project.
	GetByIssue(ctx context.Context, accountID, projectID uuid.UUID, issueNumber int) (domain.Task, error)
	List(ctx context.Context, accountID uuid.UUID, f TaskFilter) ([]domain.Task, error)
	// Update writes Title, Body, Seq, IssueNumber and Branch.
	Update(ctx context.Context, t *domain.Task) error
	UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.TaskStatus) error
}

// ChangeFilter selects changes; zero fields match everything.
type ChangeFilter struct {
	ProjectID uuid.UUID
	TaskID    uuid.UUID
	State     domain.ChangeState
	Limit     int
}

// ChangeRepo stores pull requests.
type ChangeRepo interface {
	// Upsert inserts or updates the change keyed by (account, repo, number),
	// writing ID and timestamps back. On update TaskID is kept when the
	// argument's is nil.
	Upsert(ctx context.Context, c *domain.Change) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Change, error)
	GetByPR(ctx context.Context, accountID uuid.UUID, repo string, number int) (domain.Change, error)
	List(ctx context.Context, accountID uuid.UUID, f ChangeFilter) ([]domain.Change, error)
}

// JobFilter selects jobs; zero fields match everything.
type JobFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	TaskID    uuid.UUID
	WorkerID  uuid.UUID
	Status    domain.JobStatus
	Limit     int
}

// JobResult is what a worker reports when it finishes a job.
type JobResult struct {
	Status  domain.JobStatus // succeeded or failed
	Output  json.RawMessage
	HeadSHA string
	Error   string
	Usage   domain.Usage
	CostUSD float64
}

// JobRepo stores jobs and leases them to workers. Lease rules are those of
// domain.Job (Lease, Requeue, Finish); lease times use the database clock.
type JobRepo interface {
	// Create stores a queued job. MaxAttempts defaults to
	// domain.DefaultMaxAttempts; Spec must already be redacted.
	Create(ctx context.Context, j *domain.Job) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Job, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Job, error)
	// List returns jobs newest first.
	List(ctx context.Context, accountID uuid.UUID, f JobFilter) ([]domain.Job, error)
	// LeaseNext leases the oldest queued job of the account whose
	// RequiredLabels are all among labels (case-insensitive) to the worker
	// for leaseFor, using SELECT ... FOR UPDATE SKIP LOCKED. It returns
	// ErrNoJob when none matches.
	LeaseNext(ctx context.Context, accountID, workerID uuid.UUID, labels []string, leaseFor time.Duration) (domain.Job, error)
	// ExtendLease moves the lease expiry to now+leaseFor. It returns
	// ErrLeaseLost unless the job is running under workerID; the returned job
	// is current either way when it exists (Status canceled means stop).
	ExtendLease(ctx context.Context, accountID, jobID, workerID uuid.UUID, leaseFor time.Duration) (domain.Job, error)
	// RequeueExpired applies domain.Job.Requeue to every running job of the
	// account whose lease expired and returns them as updated.
	RequeueExpired(ctx context.Context, accountID uuid.UUID) ([]domain.Job, error)
	// Complete finishes a running job with a worker's result. ErrLeaseLost
	// unless the job is running under workerID.
	Complete(ctx context.Context, accountID, jobID, workerID uuid.UUID, r JobResult) (domain.Job, error)
	// Release gives up the attempt (domain.Job.Release): the job is queued
	// again but not leased before domain.RetryBackoff has passed, or failed
	// when attempts are exhausted. ErrLeaseLost unless the job is running
	// under workerID.
	Release(ctx context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error)
	// Cancel moves a queued or running job to canceled.
	Cancel(ctx context.Context, accountID, jobID uuid.UUID) (domain.Job, error)
}

// JobEventRepo stores job logs.
type JobEventRepo interface {
	// Append assigns AccountID, JobID, ID and the next Seq to each event and
	// stores them in order.
	Append(ctx context.Context, accountID, jobID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error)
	// List returns up to limit events with Seq > afterSeq, by Seq.
	List(ctx context.Context, accountID, jobID uuid.UUID, afterSeq int64, limit int) ([]domain.JobEvent, error)
}

// GateFilter selects gates; zero fields match everything.
type GateFilter struct {
	ProjectID uuid.UUID
	Kind      domain.GateKind
	Status    domain.GateStatus
	Limit     int
}

// GateRepo stores gates.
type GateRepo interface {
	// Create stores g as a pending gate.
	Create(ctx context.Context, g *domain.Gate) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Gate, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Gate, error)
	List(ctx context.Context, accountID uuid.UUID, f GateFilter) ([]domain.Gate, error)
	// ListPendingBySubject returns the pending gates about an issue or PR.
	ListPendingBySubject(ctx context.Context, accountID uuid.UUID, repo string, number int) ([]domain.Gate, error)
	// Resolve stores the decision fields (Status, ResolvedBy, Feedback,
	// ResolvedAt) of g, which must have been pending; ErrConflict otherwise.
	Resolve(ctx context.Context, g *domain.Gate) error
}

// SignalRepo stores signals.
type SignalRepo interface {
	// Insert stores s unless a signal with the same DedupeKey exists in the
	// account. It reports whether s was inserted.
	Insert(ctx context.Context, s *domain.Signal) (bool, error)
	// ListByKey returns the signals with a correlation key, oldest first.
	ListByKey(ctx context.Context, accountID uuid.UUID, key string) ([]domain.Signal, error)
}

// WorkerRepo stores workers. Name is unique per account.
type WorkerRepo interface {
	Create(ctx context.Context, w *domain.Worker) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Worker, error)
	// GetByTokenHash is unscoped: the token identifies the account. Revoked
	// workers are returned too; callers check Revoked.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.Worker, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Worker, error)
	// Seen records a poll or heartbeat: LastSeenAt = now, and labels and
	// version as reported.
	Seen(ctx context.Context, accountID, id uuid.UUID, labels []string, version string) error
	Revoke(ctx context.Context, accountID, id uuid.UUID) error
}

// WorkflowRunRepo stores workflow runs.
type WorkflowRunRepo interface {
	Create(ctx context.Context, r *domain.WorkflowRun) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.WorkflowRun, error)
	// ListActive returns the runs that are running or waiting.
	ListActive(ctx context.Context, accountID uuid.UUID) ([]domain.WorkflowRun, error)
	// Update writes Status, State, Error and FinishedAt.
	Update(ctx context.Context, r *domain.WorkflowRun) error
}

// StepRunRepo stores step runs.
type StepRunRepo interface {
	Create(ctx context.Context, s *domain.StepRun) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.StepRun, error)
	ListByRun(ctx context.Context, accountID, runID uuid.UUID) ([]domain.StepRun, error)
	// ListWaiting returns the waiting steps whose WaitKey is key.
	ListWaiting(ctx context.Context, accountID uuid.UUID, key string) ([]domain.StepRun, error)
	// Update writes Status, WaitKey, JobID, GateID, SignalID, Output, Error,
	// StartedAt and FinishedAt.
	Update(ctx context.Context, s *domain.StepRun) error
}

// WebhookDeliveryRepo deduplicates GitHub webhook deliveries.
type WebhookDeliveryRepo interface {
	// Record stores the delivery and reports whether it was new.
	Record(ctx context.Context, d domain.WebhookDelivery) (bool, error)
}

// MCPTokenRepo stores MCP and API tokens. Name is unique per account.
type MCPTokenRepo interface {
	Create(ctx context.Context, t *domain.MCPToken) error
	// GetByTokenHash is unscoped: the token identifies the account. Revoked
	// tokens are returned too; callers check RevokedAt.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.MCPToken, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.MCPToken, error)
	Touch(ctx context.Context, accountID, id uuid.UUID) error // LastUsedAt = now
	Revoke(ctx context.Context, accountID, id uuid.UUID) error
}

// AuditFilter selects audit entries; zero fields match everything.
type AuditFilter struct {
	ProjectID uuid.UUID
	SubjectID uuid.UUID
	Limit     int
}

// AuditRepo is the append-only audit log.
type AuditRepo interface {
	Append(ctx context.Context, e *domain.AuditEntry) error
	// List returns entries newest first.
	List(ctx context.Context, accountID uuid.UUID, f AuditFilter) ([]domain.AuditEntry, error)
}
