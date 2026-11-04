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
	Artifacts() ArtifactRepo
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
	// AppendLeased is Append for a worker: in one transaction that locks the
	// job, it fails with ErrLeaseLost, storing nothing, unless the job is
	// running under workerID.
	AppendLeased(ctx context.Context, accountID, jobID, workerID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error)
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
	// SetPool moves a worker to pool (empty is PoolBuild). Only admins call
	// it; the worker leases from the new pool at its next lease attempt and
	// its running jobs finish.
	SetPool(ctx context.Context, accountID, id uuid.UUID, pool string) error
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

// ArtifactRepo stores artifact metadata; the content lives in evidence
// storage under Artifact.StorageKey.
type ArtifactRepo interface {
	// Put stores a, replacing the artifact of the same job and name if
	// there is one (a re-upload), and writes ID and CreatedAt back.
	Put(ctx context.Context, a *domain.Artifact) error
	// PutLeased is Put for an upload by a worker: in one transaction that
	// locks the job it checks that the job is running under workerID's lease
	// (else ErrLeaseLost) and writes the row, so a lease that ends cannot
	// slip between the check and the write. It returns the storage key of
	// the artifact it replaced, or "" when the name was new.
	PutLeased(ctx context.Context, a *domain.Artifact, workerID uuid.UUID) (replacedKey string, err error)
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Artifact, error)
	// ListByJob returns a job's artifacts by name.
	ListByJob(ctx context.Context, accountID, jobID uuid.UUID) ([]domain.Artifact, error)
	// ListOlderThan returns up to limit artifacts created before t, oldest
	// first, for retention.
	ListOlderThan(ctx context.Context, accountID uuid.UUID, t time.Time, limit int) ([]domain.Artifact, error)
	// Delete removes the row; a verification report that points at it keeps
	// its data and loses the link (ArtifactID becomes zero). Unknown IDs are
	// ErrNotFound.
	Delete(ctx context.Context, accountID, id uuid.UUID) error
}

// The repositories below belong to the testing system
// (docs/contracts/data-model.md). They are contracts only: the tasks that
// first use them add them to Store, with the Postgres implementation and the
// testkit fake (TestRunRepo with command jobs, VerificationReportRepo with
// verification reports).

// TestRunFilter selects test runs; zero fields match everything.
type TestRunFilter struct {
	ProjectID uuid.UUID
	JobID     uuid.UUID
	ChangeID  uuid.UUID
	SHA       string
	Limit     int
}

// TestRunRepo stores test runs and their results. (job, attempt, suite) is
// unique; a duplicate fails with ErrConflict.
type TestRunRepo interface {
	// Create stores a run and its results in one transaction, writing IDs,
	// AccountID and RunID of the results and CreatedAt back.
	Create(ctx context.Context, run *domain.TestRun, results []domain.TestResult) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.TestRun, error)
	List(ctx context.Context, accountID uuid.UUID, f TestRunFilter) ([]domain.TestRun, error)
	// Results returns the results of a run by classname, then name.
	Results(ctx context.Context, accountID, runID uuid.UUID) ([]domain.TestResult, error)
}

// VerificationReportRepo stores verification reports, one per change and
// SHA.
type VerificationReportRepo interface {
	// Upsert inserts the report of (change, sha) or replaces its Status,
	// Report and ArtifactID, writing ID and timestamps back.
	Upsert(ctx context.Context, r *domain.VerificationReport) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.VerificationReport, error)
	GetBySHA(ctx context.Context, accountID, changeID uuid.UUID, sha string) (domain.VerificationReport, error)
	// Latest returns the most recently updated report of a change.
	Latest(ctx context.Context, accountID, changeID uuid.UUID) (domain.VerificationReport, error)
}

// The repositories below belong to the team (councils, registers,
// knowledge, proposals; docs/contracts/data-model.md, migration 00004).
// Like the testing system's, they are contracts only: CouncilRepo is added
// to Store by T33, AssumptionRepo and DecisionRepo by T32, KnowledgeRepo by
// T36, KnowledgeProposalRepo by T37 and ProposalRepo by T50, each with the
// Postgres implementation and the testkit fake. goals.kind and goals.size
// are read and written by GoalRepo from T35 (Size) and T41 (Kind),
// jobs.context_manifest by JobRepo from T38, and gates.origin and
// jobs.job_mcp_token_hash from T47; until then they keep their defaults
// (feature, NULL, playbook, NULL).

// CouncilFilter selects councils; zero fields match everything.
type CouncilFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	Status    domain.CouncilStatus
	Limit     int
}

// CouncilRepo stores councils and their contributions.
type CouncilRepo interface {
	Create(ctx context.Context, c *domain.Council) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Council, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Council, error)
	List(ctx context.Context, accountID uuid.UUID, f CouncilFilter) ([]domain.Council, error)
	// Update writes Constraints, Brief, GateID and RunID.
	Update(ctx context.Context, c *domain.Council) error
	UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.CouncilStatus) error
	// AddContribution stores c; a second contribution of the same job, or
	// of the same role, runtime, stage and round, fails with ErrConflict.
	AddContribution(ctx context.Context, c *domain.CouncilContribution) error
	// SetEvidenceCheck stores the check of a contribution's cited sources.
	SetEvidenceCheck(ctx context.Context, accountID, contributionID uuid.UUID, checks []domain.SourceCheck) error
	// Contributions returns a council's contributions of a stage (all
	// stages when stage is empty), oldest first.
	Contributions(ctx context.Context, accountID, councilID uuid.UUID, stage domain.CouncilStage) ([]domain.CouncilContribution, error)
}

// AssumptionFilter selects assumptions; zero fields match everything.
type AssumptionFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	JobID     uuid.UUID
	Status    domain.AssumptionStatus
	Impact    domain.Level
	VerifyBy  domain.VerifyBy
	Limit     int
}

// AssumptionRepo is the assumption register.
type AssumptionRepo interface {
	Create(ctx context.Context, a *domain.Assumption) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Assumption, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Assumption, error)
	List(ctx context.Context, accountID uuid.UUID, f AssumptionFilter) ([]domain.Assumption, error)
	// AddDependents appends the dependents a does not list yet.
	AddDependents(ctx context.Context, accountID, id uuid.UUID, deps []domain.Dependent) error
	// Resolve stores Status, Evidence, ResolvedBy and ResolvedAt of a after
	// domain.Assumption.Resolve, as a compare-and-set from from.
	Resolve(ctx context.Context, a *domain.Assumption, from domain.AssumptionStatus) error
}

// DecisionFilter selects decisions; zero fields match everything.
type DecisionFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	CouncilID uuid.UUID
	Limit     int
}

// DecisionRepo is the decision register; decisions are never changed.
type DecisionRepo interface {
	Create(ctx context.Context, d *domain.Decision) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Decision, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Decision, error)
	List(ctx context.Context, accountID uuid.UUID, f DecisionFilter) ([]domain.Decision, error)
}

// KnowledgeQuery is a knowledge search; zero fields match everything.
type KnowledgeQuery struct {
	ProjectID uuid.UUID
	Text      string // websearch_to_tsquery syntax; empty lists by section and path
	Sections  []string
	Kinds     []domain.KnowledgeKind
	Providers []string
	// GoalID is the searching job's goal: goal notes of other goals are
	// never returned. Nil returns project notes only.
	GoalID uuid.UUID
	// Embedding of Text, mixed into the ranking where the embedding column
	// exists; ignored otherwise.
	Embedding []float32
	Limit     int
}

// KnowledgeHit is one search result.
type KnowledgeHit struct {
	Entry   domain.KnowledgeEntry
	Rank    float64
	Snippet string // ts_headline of the body
}

// KnowledgeRepo is the knowledge index.
type KnowledgeRepo interface {
	// Upsert inserts or replaces the entry of (project, provider, path),
	// writing ID and timestamps back.
	Upsert(ctx context.Context, e *domain.KnowledgeEntry) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.KnowledgeEntry, error)
	GetByPath(ctx context.Context, accountID, projectID uuid.UUID, provider, path string) (domain.KnowledgeEntry, error)
	Delete(ctx context.Context, accountID, id uuid.UUID) error
	// Search returns the best hits first.
	Search(ctx context.Context, accountID uuid.UUID, q KnowledgeQuery) ([]KnowledgeHit, error)
	// SetEmbedding stores an entry's embedding. It fails with
	// errors.ErrUnsupported when the database has no pgvector.
	SetEmbedding(ctx context.Context, accountID, id uuid.UUID, embedding []float32) error
	// MarkReverify sets needs_reverify with the reason on the entries.
	MarkReverify(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID, reason string) error
	// DeleteExpired deletes the notes whose ExpiresAt is before t and
	// returns how many.
	DeleteExpired(ctx context.Context, accountID uuid.UUID, t time.Time) (int, error)
}

// KnowledgeProposalFilter selects knowledge proposals; zero fields match
// everything.
type KnowledgeProposalFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	Batch     domain.KnowledgeBatch
	Status    domain.KnowledgeProposalStatus
	Limit     int
}

// KnowledgeProposalRepo stores knowledge proposals.
type KnowledgeProposalRepo interface {
	Create(ctx context.Context, k *domain.KnowledgeProposal) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.KnowledgeProposal, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.KnowledgeProposal, error)
	List(ctx context.Context, accountID uuid.UUID, f KnowledgeProposalFilter) ([]domain.KnowledgeProposal, error)
	// Update writes Status, ExternalRef, ChangeID and GateID, as a
	// compare-and-set of the status from from.
	Update(ctx context.Context, k *domain.KnowledgeProposal, from domain.KnowledgeProposalStatus) error
}

// ProposalFilter selects improvement proposals; zero fields match
// everything.
type ProposalFilter struct {
	ProjectID uuid.UUID
	Status    domain.ProposalStatus
	Category  domain.ProposalCategory
	Limit     int
}

// ProposalRepo stores improvement proposals. A dedupe key is unique among
// the project's proposals that are not final; a duplicate fails with
// ErrConflict.
type ProposalRepo interface {
	Create(ctx context.Context, p *domain.Proposal) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Proposal, error)
	GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Proposal, error)
	// GetOpen returns the proposal of the project with the dedupe key that
	// is not final.
	GetOpen(ctx context.Context, accountID, projectID uuid.UUID, dedupeKey string) (domain.Proposal, error)
	List(ctx context.Context, accountID uuid.UUID, f ProposalFilter) ([]domain.Proposal, error)
	// Update writes everything but the IDs, Number and CreatedAt, as a
	// compare-and-set of the status from from.
	Update(ctx context.Context, p *domain.Proposal, from domain.ProposalStatus) error
}
