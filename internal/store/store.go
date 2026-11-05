// Package store defines the persistence contract of forecrewd. The Postgres
// implementation follows docs/contracts/data-model.md; internal/testkit has
// an in-memory fake.
//
// Rules shared by every repository:
//   - Every project-scoped call takes the account ID and only sees rows of
//     that account. The only unscoped lookups are the ones that discover the
//     account: Projects.ListByRepo (webhooks), Workers.GetByTokenHash,
//     MCPTokens.GetByTokenHash and Jobs.GetByJobMCPTokenHash, and the
//     instance-wide calls of the fleet repositories, which say so.
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
	"slices"
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
	// ErrLastOwner means a change would leave an organization that has an
	// active Owner without one (docs/contracts/identity.md): removing or
	// demoting the last Owner, suspending or removing their membership, or
	// disabling the user.
	ErrLastOwner = errors.New("an organization must keep an active owner")
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
	TestRuns() TestRunRepo
	VerificationReports() VerificationReportRepo
	Assumptions() AssumptionRepo
	Decisions() DecisionRepo
	Knowledge() KnowledgeRepo
	KnowledgeProposals() KnowledgeProposalRepo
	Councils() CouncilRepo
	// WithTx runs fn in a transaction: tx sees fn's writes, which are
	// committed when fn returns nil and rolled back otherwise. Calls on tx
	// must use the ctx passed to fn.
	WithTx(ctx context.Context, fn func(ctx context.Context, tx Store) error) error
}

// AccountRepo stores accounts.
type AccountRepo interface {
	// Create stores the account. An empty Slug is derived from the name
	// (domain.Slugify) with the first free -2, -3, ...; a Slug that is
	// given and taken fails with ErrConflict.
	Create(ctx context.Context, a *domain.Account) error
	Get(ctx context.Context, id uuid.UUID) (domain.Account, error)
	List(ctx context.Context) ([]domain.Account, error)
	// SetMaxTokenTTL sets the organization's token cap; zero removes it.
	SetMaxTokenTTL(ctx context.Context, id uuid.UUID, ttl time.Duration) error
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
	// Update writes Title, Body and IssueNumber. It never touches the size:
	// see SetSize.
	Update(ctx context.Context, g *domain.Goal) error
	// SetSize stores the size of a goal that has none, once. A goal that has
	// a size keeps it, so a caller that read the goal earlier cannot replace
	// or clear it; read the goal again to see the stored size.
	SetSize(ctx context.Context, accountID, id uuid.UUID, size domain.GoalSize) error
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
	HeadSHA   string // lowercase, compared exactly
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
	// TestRuns are the test runs of a command job. JobRepo ignores them;
	// jobs.Service.Complete stores them with the result.
	TestRuns []TestRunReport
}

// TestRunReport is a test run with its results, as a worker reports it.
type TestRunReport struct {
	Run     domain.TestRun
	Results []domain.TestResult
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
	// SetJobMCPTokenHash stores the SHA-256 of the job MCP token of the
	// job's current lease (docs/contracts/job-mcp.md): the job must be
	// running under workerID in attempt attempts (Job.Attempts when it was
	// leased), ErrLeaseLost otherwise. The hash dies with the lease: when
	// the job stops running or is leased again it no longer matches.
	SetJobMCPTokenHash(ctx context.Context, accountID, jobID, workerID uuid.UUID, attempts int, hash []byte) error
	// GetByJobMCPTokenHash is unscoped: the token identifies the account. It
	// returns the job whose current lease holds the token, ErrNotFound when
	// none does. Callers still check the lease expiry.
	GetByJobMCPTokenHash(ctx context.Context, hash []byte) (domain.Job, error)
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
	// ExistingKeys returns those of keys that are the storage key of some
	// artifact, of any account, in one query.
	ExistingKeys(ctx context.Context, keys []string) ([]string, error)
}

// The repositories below belong to the testing system
// (docs/contracts/data-model.md); TestRunRepo and VerificationReportRepo are
// in Store.

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
	// Failures returns up to limit of the failed and errored results of a
	// run, in the order of Results; a zero limit means all.
	Failures(ctx context.Context, accountID, runID uuid.UUID, limit int) ([]domain.TestResult, error)
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
// Like the testing system's, they are contracts only until they join Store
// (AssumptionRepo, DecisionRepo, CouncilRepo, KnowledgeRepo and
// KnowledgeProposalRepo are in it): ProposalRepo by T50, with the
// Postgres implementation and the testkit fake. goals.kind and goals.size
// are read and written by GoalRepo from T35 (Size) and T41 (Kind); until
// then they keep their defaults (feature, NULL). jobs.context_manifest is
// read and written by JobRepo (T38). gates.origin and jobs.job_mcp_token_hash are
// read and written by GateRepo and JobRepo (T47).

// CouncilFilter selects councils; zero fields match everything.
type CouncilFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	GateID    uuid.UUID
	Status    domain.CouncilStatus
	Limit     int
}

// CouncilRepo stores councils and their contributions.
type CouncilRepo interface {
	Create(ctx context.Context, c *domain.Council) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Council, error)
	// GetForUpdate is Get for a read-modify-write with Update inside WithTx:
	// it holds the row until the transaction ends, so a constraint a human
	// adds while an action stores the brief is not lost.
	GetForUpdate(ctx context.Context, accountID, id uuid.UUID) (domain.Council, error)
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
	// RaiseImpact stores a higher impact; a lower or equal one changes nothing.
	RaiseImpact(ctx context.Context, accountID, id uuid.UUID, impact domain.Level) error
	// Lock serializes the registrations of a goal (of the project's
	// goal-less ones when goalID is nil) until the end of the transaction
	// it is called in: a second caller waits for the first to commit, so
	// that it sees the first's items. Outside a transaction it locks
	// nothing.
	Lock(ctx context.Context, accountID, projectID, goalID uuid.UUID) error
}

// MergeDependents returns cur followed by the dependents of add that cur
// does not list, in order. Both implementations of AddDependents use it.
func MergeDependents(cur, add []domain.Dependent) []domain.Dependent {
	out := slices.Clone(cur)
	for _, d := range add {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
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
	// AllNotes returns the notes of every goal (for humans); GoalID is
	// then ignored.
	AllNotes bool
	// Embedding of Text, mixed into the ranking where the embedding column
	// exists; ignored otherwise.
	Embedding []float32
	Limit     int
}

// Ranking of a knowledge search that has an embedding (knowledge.md, Search):
// rank = (1-SemanticWeight)*ts_rank(search, query, 32) + SemanticWeight*cosine
// similarity, for the entries that have an embedding (the others rank by
// ts_rank alone). An entry that text search does not match is still a hit
// when its cosine similarity is at least SemanticFloor.
const (
	SemanticWeight = 0.5
	SemanticFloor  = 0.45
)

// KnowledgeFilter selects index rows; zero fields match everything.
type KnowledgeFilter struct {
	ProjectID     uuid.UUID
	Provider      string
	Section       string
	NeedsReverify bool
	Limit         int
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
	// SupportsEmbeddings reports whether the database keeps embeddings
	// (pgvector). Callers send no text to an embeddings provider when not.
	SupportsEmbeddings(ctx context.Context) (bool, error)
	// WithoutEmbedding returns up to limit entries of the project that
	// have no embedding, oldest first; none without pgvector. Their
	// embedding failed or was never tried, and is retried later.
	WithoutEmbedding(ctx context.Context, accountID, projectID uuid.UUID, limit int) ([]domain.KnowledgeEntry, error)
	// MarkReverify sets needs_reverify with the reason on the entries.
	MarkReverify(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID, reason string) error
	// DeleteExpired deletes the notes whose ExpiresAt is before t and
	// returns how many.
	DeleteExpired(ctx context.Context, accountID uuid.UUID, t time.Time) (int, error)
	// List returns the rows that match f, ordered by section and path.
	List(ctx context.Context, accountID uuid.UUID, f KnowledgeFilter) ([]domain.KnowledgeEntry, error)
	// ExpireGoalNotes gives the goal notes without an expiry whose goal is
	// done or canceled the expiry grace after the goal's last change, and
	// returns how many it stamped.
	ExpireGoalNotes(ctx context.Context, accountID uuid.UUID, grace time.Duration) (int, error)
}

// KnowledgeProposalFilter selects knowledge proposals; zero fields match
// everything.
type KnowledgeProposalFilter struct {
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	JobID     uuid.UUID
	ChangeID  uuid.UUID
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
	// compare-and-set of the status from from: ErrConflict when it moved,
	// domain.ErrInvalidTransition for a move the domain refuses.
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

// The repositories below belong to identity and access (migration 00005,
// docs/contracts/identity.md). Like the team's, they are contracts only,
// added to Store with the Postgres implementation and the testkit fake by
// the task that first uses them: UserRepo, IdentityRepo, MembershipRepo,
// InvitationRepo, ConnectionRepo, SessionRepo and PersonalTokenRepo by T52;
// TeamRepo, ServiceAccountRepo, RoleRepo and RoleBindingRepo by T53;
// OAuthRepo by T55; VerifiedDomainRepo and SCIMTokenRepo by T56. Lookups
// that discover who is signing in (by token hash, by provider subject, by
// verified email or domain) are unscoped; every other call takes the
// organization. Calls that may leave an organization without an active
// Owner fail with ErrLastOwner and change nothing.

// UserRepo stores users, who are global.
type UserRepo interface {
	// Create fails with ErrConflict when Email is verified and another
	// user has it verified (ignoring case).
	Create(ctx context.Context, u *domain.User) error
	Get(ctx context.Context, id uuid.UUID) (domain.User, error)
	// ListByIDs returns the users with these IDs, in no particular order;
	// unknown IDs are left out.
	ListByIDs(ctx context.Context, ids []uuid.UUID) ([]domain.User, error)
	// GetByVerifiedEmail is unscoped: it finds the user whose verified
	// email is email, ignoring case.
	GetByVerifiedEmail(ctx context.Context, email string) (domain.User, error)
	// ListByEmail is unscoped: it returns every user whose primary email is
	// email, verified or not, ignoring case. Only a verified one proves who
	// a person is, and several users may share an unverified one.
	ListByEmail(ctx context.Context, email string) ([]domain.User, error)
	// Update writes Name, Email, EmailVerified and InstanceAdmin.
	Update(ctx context.Context, u *domain.User) error
	Seen(ctx context.Context, id uuid.UUID) error // LastSeenAt = now
	// SetDisabled disables (DisabledAt = now) or enables a user.
	SetDisabled(ctx context.Context, id uuid.UUID, disabled bool) error
}

// IdentityRepo stores users' identities at their providers.
// (connection, subject) is unique; a duplicate fails with ErrConflict.
type IdentityRepo interface {
	Create(ctx context.Context, i *domain.Identity) error
	// GetBySubject is unscoped: a sign-in finds the identity of the
	// provider's subject.
	GetBySubject(ctx context.Context, connectionID uuid.UUID, subject string) (domain.Identity, error)
	// ListByLogin returns the identities of a connection with the login,
	// ignoring case (GitHub commands find the commenting user).
	ListByLogin(ctx context.Context, connectionID uuid.UUID, login string) ([]domain.Identity, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.Identity, error)
	// Seen records a sign-in: LastUsedAt = now, Login and Email as the
	// provider reported them.
	Seen(ctx context.Context, id uuid.UUID, login, email string) error
	Delete(ctx context.Context, userID, id uuid.UUID) error
}

// MembershipFilter selects memberships; zero fields match everything.
type MembershipFilter struct {
	Status domain.MembershipStatus
	Limit  int
}

// MembershipRepo stores memberships. (account, user) is unique.
type MembershipRepo interface {
	Create(ctx context.Context, m *domain.Membership) error
	Get(ctx context.Context, accountID, userID uuid.UUID) (domain.Membership, error)
	GetByExternalID(ctx context.Context, accountID uuid.UUID, externalID string) (domain.Membership, error)
	List(ctx context.Context, accountID uuid.UUID, f MembershipFilter) ([]domain.Membership, error)
	// ListByUser returns the user's memberships in every organization.
	ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.Membership, error)
	// SetStatus moves a membership to status; ErrLastOwner when it would
	// suspend the last Owner. Suspending a member who holds a role needs
	// every permission of that role, which the caller checks first
	// (permissions.md, No escalation). Ending their sessions and OAuth
	// grants and revoking their tokens are separate calls (SessionRepo,
	// OAuthRepo, PersonalTokenRepo) the caller makes in the same WithTx.
	SetStatus(ctx context.Context, accountID, userID uuid.UUID, status domain.MembershipStatus) error
	SetExternalID(ctx context.Context, accountID, userID uuid.UUID, externalID string) error
	// Remove deletes the membership with the user's team memberships and
	// role bindings in the organization, in one transaction. Removing a
	// member who holds a role needs every permission of that role, which
	// the caller checks first (permissions.md, No escalation). Ending their
	// sessions and OAuth grants and revoking their tokens are separate calls
	// (SessionRepo, OAuthRepo, PersonalTokenRepo) the caller makes in the
	// same WithTx.
	Remove(ctx context.Context, accountID, userID uuid.UUID) error
}

// InvitationRepo stores invitations.
type InvitationRepo interface {
	Create(ctx context.Context, i *domain.Invitation) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Invitation, error)
	// GetByTokenHash is unscoped: the invitation link identifies the
	// organization.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.Invitation, error)
	// List returns the organization's invitations, newest first.
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Invitation, error)
	// Accept records userID's acceptance (AcceptedBy, AcceptedAt = now) of
	// an invitation that is pending (domain.Invitation.Pending);
	// ErrConflict otherwise. The caller creates the membership and the
	// bindings in the same transaction.
	Accept(ctx context.Context, accountID, id, userID uuid.UUID) (domain.Invitation, error)
	Revoke(ctx context.Context, accountID, id uuid.UUID) error
}

// TeamRepo stores teams and their members. Slug and ExternalID are unique
// per organization.
type TeamRepo interface {
	Create(ctx context.Context, t *domain.Team) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Team, error)
	GetBySlug(ctx context.Context, accountID uuid.UUID, slug string) (domain.Team, error)
	GetByExternalID(ctx context.Context, accountID uuid.UUID, externalID string) (domain.Team, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Team, error)
	// Update writes Name, Slug and ExternalID.
	Update(ctx context.Context, t *domain.Team) error
	// Delete removes the team with its members and role bindings.
	Delete(ctx context.Context, accountID, id uuid.UUID) error
	// AddMember adds or, for an existing member, keeps the member; the user
	// must be a member of the organization (ErrNotFound otherwise).
	AddMember(ctx context.Context, m *domain.TeamMember) error
	RemoveMember(ctx context.Context, accountID, teamID, userID uuid.UUID) error
	Members(ctx context.Context, accountID, teamID uuid.UUID) ([]domain.TeamMember, error)
	// TeamsOf returns the teams a user is in.
	TeamsOf(ctx context.Context, accountID, userID uuid.UUID) ([]domain.Team, error)
}

// ServiceAccountRepo stores service accounts. Name is unique per
// organization.
type ServiceAccountRepo interface {
	Create(ctx context.Context, s *domain.ServiceAccount) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.ServiceAccount, error)
	GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.ServiceAccount, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.ServiceAccount, error)
	// Update writes Name and Description.
	Update(ctx context.Context, s *domain.ServiceAccount) error
	SetDisabled(ctx context.Context, accountID, id uuid.UUID, disabled bool) error
}

// RoleRepo reads the built-in roles and stores custom roles. A call with
// an organization sees the built-in roles and that organization's custom
// roles only.
type RoleRepo interface {
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.Role, error)
	GetByName(ctx context.Context, accountID uuid.UUID, scope domain.RoleScope, name string) (domain.Role, error)
	// List returns the built-in roles, then the organization's custom ones.
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Role, error)
	// Create, Update and Delete work on custom roles only (built-in ones
	// are ErrNotFound). Update writes Name, Description and Permissions;
	// Delete fails with ErrConflict while a binding uses the role.
	Create(ctx context.Context, r *domain.Role) error
	Update(ctx context.Context, r *domain.Role) error
	Delete(ctx context.Context, accountID, id uuid.UUID) error
}

// RoleBindingFilter selects role bindings; zero fields match everything.
// ProjectID matches bindings on that project only, OrganizationOnly the
// organization-scope ones only.
type RoleBindingFilter struct {
	SubjectKind      domain.SubjectKind
	SubjectID        uuid.UUID
	RoleID           uuid.UUID
	ProjectID        uuid.UUID
	OrganizationOnly bool
}

// BoundRole is a binding with its role.
type BoundRole struct {
	Binding domain.RoleBinding
	Role    domain.Role
}

// RoleBindingRepo stores role bindings. A duplicate fails with
// ErrConflict; a subject, role or project outside the organization with
// ErrNotFound.
type RoleBindingRepo interface {
	Create(ctx context.Context, b *domain.RoleBinding) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.RoleBinding, error)
	List(ctx context.Context, accountID uuid.UUID, f RoleBindingFilter) ([]domain.RoleBinding, error)
	// Delete removes a binding; ErrLastOwner for the last Owner's.
	Delete(ctx context.Context, accountID, id uuid.UUID) error
	// Effective returns every binding that applies to a subject, with its
	// role: for a user its own and those of every team it is in; for a
	// service account its own. Only active members and enabled users and
	// service accounts have any; the others get none.
	Effective(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, id uuid.UUID) ([]BoundRole, error)
}

// ConnectionRepo stores sign-in connections and their group mappings.
type ConnectionRepo interface {
	// Create stores an organization's connection; domain.Connection.Validate
	// must pass.
	Create(ctx context.Context, c *domain.Connection) error
	// Get is unscoped: sign-in routes name the connection by ID.
	Get(ctx context.Context, id uuid.UUID) (domain.Connection, error)
	// List returns an organization's own connections, or the
	// instance-level ones for the zero ID.
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Connection, error)
	// UpsertInstance stores an instance-level connection from config by
	// name, writing ID and timestamps back.
	UpsertInstance(ctx context.Context, c *domain.Connection) error
	// Update writes Name, Issuer, ClientID, ClientSecretRef, JIT,
	// GroupClaim and Enabled.
	Update(ctx context.Context, c *domain.Connection) error
	AddGroupMapping(ctx context.Context, m *domain.GroupMapping) error
	RemoveGroupMapping(ctx context.Context, accountID, id uuid.UUID) error
	// GroupMappings returns the mappings of a connection in every
	// organization (sign-in applies each organization's own).
	GroupMappings(ctx context.Context, connectionID uuid.UUID) ([]domain.GroupMapping, error)
}

// VerifiedDomainRepo stores organizations' email domains.
type VerifiedDomainRepo interface {
	Create(ctx context.Context, d *domain.VerifiedDomain) error
	Get(ctx context.Context, accountID, id uuid.UUID) (domain.VerifiedDomain, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.VerifiedDomain, error)
	// GetVerified is unscoped: email-first sign-in finds the organization
	// that verified the domain.
	GetVerified(ctx context.Context, name string) (domain.VerifiedDomain, error)
	// SetVerified marks the domain verified (VerifiedAt = now; ErrConflict
	// when another organization verified it) or unverified, which also
	// ends EnforceSSO.
	SetVerified(ctx context.Context, accountID, id uuid.UUID, verified bool) error
	SetEnforcement(ctx context.Context, accountID, id uuid.UUID, enforce bool, connectionID uuid.UUID) error
	Delete(ctx context.Context, accountID, id uuid.UUID) error
}

// SessionRepo stores browser sessions.
type SessionRepo interface {
	Create(ctx context.Context, s *domain.Session) error
	// GetByTokenHash is unscoped: the cookie identifies the user. Revoked
	// and expired sessions are returned too; callers check Active.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.Session, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.Session, error)
	// Touch records a request: LastSeenAt = now, IdleExpiresAt as given
	// (never past ExpiresAt).
	Touch(ctx context.Context, id uuid.UUID, idleExpiresAt time.Time) error
	// SetAccount switches the session's current organization.
	SetAccount(ctx context.Context, id, accountID uuid.UUID) error
	Revoke(ctx context.Context, userID, id uuid.UUID) error
	// RevokeByUser ends the user's sessions, or only those whose current
	// organization is accountID when it is not zero.
	RevokeByUser(ctx context.Context, userID, accountID uuid.UUID) error
	// DeleteExpired deletes the sessions that ended before t.
	DeleteExpired(ctx context.Context, t time.Time) (int, error)
}

// PersonalTokenRepo is MCPTokenRepo with owners; T52 makes it the type of
// Store.MCPTokens.
type PersonalTokenRepo interface {
	MCPTokenRepo
	ListByOwner(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) ([]domain.MCPToken, error)
	// AssignOwner gives every ownerless token of the organization to the
	// owner and returns how many. A token whose name the owner already uses
	// takes the first free -2, -3, ..., so none is left ownerless.
	AssignOwner(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) (int, error)
	// RevokeByOwner revokes the owner's active tokens.
	RevokeByOwner(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) error
}

// OAuthRepo stores the OAuth clients and grants of MCP clients.
type OAuthRepo interface {
	CreateClient(ctx context.Context, c *domain.OAuthClient) error
	// GetClient finds a client by its public client_id.
	GetClient(ctx context.Context, clientID string) (domain.OAuthClient, error)
	TouchClient(ctx context.Context, id uuid.UUID) error
	// DeleteUnusedClients deletes clients without grants that were not
	// used since t.
	DeleteUnusedClients(ctx context.Context, t time.Time) (int, error)
	CreateGrant(ctx context.Context, g *domain.OAuthGrant) error
	// GetGrantByTokenHash is unscoped: the token identifies the user and
	// organization. Used, revoked and expired grants are returned too.
	GetGrantByTokenHash(ctx context.Context, hash []byte) (domain.OAuthGrant, error)
	// UseGrant sets UsedAt of an unused code or refresh token;
	// ErrConflict when it was used already (reuse: the caller revokes the
	// family).
	UseGrant(ctx context.Context, id uuid.UUID) error
	RevokeFamily(ctx context.Context, familyID uuid.UUID) error
	// ListFamilies returns one grant per active family of the user in the
	// organization (the connected apps), newest first.
	ListFamilies(ctx context.Context, accountID, userID uuid.UUID) ([]domain.OAuthGrant, error)
	// RevokeByUser revokes the user's grants in the organization.
	RevokeByUser(ctx context.Context, accountID, userID uuid.UUID) error
}

// SCIMTokenRepo stores SCIM bearer tokens. Name is unique per
// organization.
type SCIMTokenRepo interface {
	Create(ctx context.Context, t *domain.SCIMToken) error
	// GetByTokenHash is unscoped; revoked tokens are returned too.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.SCIMToken, error)
	List(ctx context.Context, accountID uuid.UUID) ([]domain.SCIMToken, error)
	Touch(ctx context.Context, accountID, id uuid.UUID) error
	Revoke(ctx context.Context, accountID, id uuid.UUID) error
}

// The fleet repositories (docs/contracts/fleet.md, data-model.md, migration
// 00007) are contracts only until they join Store, each with the Postgres
// implementation and the testkit fake: CapacityProviderRepo, ShapeRepo,
// MachineRepo, BootstrapTokenRepo and FleetPolicyRepo by T58,
// ProviderJoinCodeRepo by T59, ImageRepo by T60 and UsageRepo by T65. They
// serve a controller that works across organizations, so the calls that find
// a provider, machine or token by its ID or hash are unscoped; the zero
// account ID means the instance-wide rows.

// CapacityProviderRepo stores capacity providers. Name is unique per
// organization, and among instance-wide providers.
type CapacityProviderRepo interface {
	Create(ctx context.Context, p *domain.CapacityProvider) error
	Get(ctx context.Context, id uuid.UUID) (domain.CapacityProvider, error)
	GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.CapacityProvider, error)
	// GetByTokenHash is unscoped: the token identifies the provider.
	// Revoked providers are returned too; callers check Status.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.CapacityProvider, error)
	// List returns an organization's own providers, or the instance-wide
	// ones for the zero ID.
	List(ctx context.Context, accountID uuid.UUID) ([]domain.CapacityProvider, error)
	// ListAll returns every provider, for the controller.
	ListAll(ctx context.Context) ([]domain.CapacityProvider, error)
	// Update writes Limits, NetworkAllow and Tags.
	Update(ctx context.Context, p *domain.CapacityProvider) error
	// SetStatus moves a provider to status; ErrConflict when it is revoked.
	SetStatus(ctx context.Context, id uuid.UUID, status domain.ProviderStatus) error
	// SetToken stores the hash of a new token and makes the provider
	// active.
	SetToken(ctx context.Context, id uuid.UUID, hash []byte) error
	// Seen records contact: LastSeenAt = now and, when not nil, the
	// capabilities as reported.
	Seen(ctx context.Context, id uuid.UUID, capabilities *domain.ProviderCapabilities) error
}

// ProviderJoinCodeRepo stores join codes (fleet-agent-api.md, Joining).
type ProviderJoinCodeRepo interface {
	Create(ctx context.Context, c *domain.ProviderJoinCode) error
	// GetByCodeHash is unscoped: the code identifies the organization.
	GetByCodeHash(ctx context.Context, hash []byte) (domain.ProviderJoinCode, error)
	// Claim records the facts of the agent holding claimHash. The first
	// claim binds the code; another claim fails with ErrConflict, as does
	// an expired or used code.
	Claim(ctx context.Context, id uuid.UUID, claimHash []byte, kind domain.ProviderKind, facts domain.ProviderCapabilities) error
	// Confirm links the code to the provider a person confirmed.
	Confirm(ctx context.Context, id, providerID uuid.UUID) error
	// MarkUsed records that the token was handed over; ErrConflict when it
	// already was, so it is handed over once.
	MarkUsed(ctx context.Context, id uuid.UUID) error
}

// ShapeRepo stores shapes. Name is unique per organization, and among
// instance-wide shapes. A provider of another organization is ErrNotFound.
type ShapeRepo interface {
	Create(ctx context.Context, s *domain.Shape) error
	Get(ctx context.Context, id uuid.UUID) (domain.Shape, error)
	GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.Shape, error)
	// List returns an organization's own shapes, or the instance-wide ones
	// for the zero ID.
	List(ctx context.Context, accountID uuid.UUID) ([]domain.Shape, error)
	// ListAvailable returns what an organization's jobs can be offered,
	// for domain.SelectShapes: its own and the instance-wide shapes.
	ListAvailable(ctx context.Context, accountID uuid.UUID) ([]domain.Shape, error)
	// Update writes everything but ID, AccountID and ProviderID.
	Update(ctx context.Context, s *domain.Shape) error
}

// MachineFilter selects machines; zero fields match everything. Active
// keeps machines whose state domain.MachineState.Counts.
type MachineFilter struct {
	AccountID  uuid.UUID
	ProviderID uuid.UUID
	ShapeID    uuid.UUID
	State      domain.MachineState
	Active     bool
	Limit      int
}

// MachineRepo stores machines. The controller writes the row before it
// asks the provider for anything, so a restart finds what it asked for.
type MachineRepo interface {
	Create(ctx context.Context, m *domain.Machine) error
	Get(ctx context.Context, id uuid.UUID) (domain.Machine, error)
	// GetByWorker returns the machine of an ephemeral worker.
	GetByWorker(ctx context.Context, accountID, workerID uuid.UUID) (domain.Machine, error)
	// GetByExternalID finds a machine by the provider's ID, for
	// reconciliation.
	GetByExternalID(ctx context.Context, providerID uuid.UUID, externalID string) (domain.Machine, error)
	List(ctx context.Context, f MachineFilter) ([]domain.Machine, error)
	// UpdateState is a compare-and-set (package comment). errMsg is stored
	// when not empty; moving to booting sets BootingAt, to ready ReadyAt
	// unless it is set, to destroyed DestroyedAt.
	UpdateState(ctx context.Context, id uuid.UUID, from, to domain.MachineState, errMsg string) error
	SetExternalID(ctx context.Context, id uuid.UUID, externalID string) error
	// Assign gives an unassigned machine its organization and sets
	// AssignedAt; ErrConflict when it already has one.
	Assign(ctx context.Context, id, accountID uuid.UUID) error
	// SetWorker links the machine to its worker of the same organization.
	SetWorker(ctx context.Context, id, workerID uuid.UUID) error
	// MarkUp sets ReadyAt, once, on a booting machine whose worker called
	// and must wait; the state does not change.
	MarkUp(ctx context.Context, id uuid.UUID) error
	// TakeJob records a lease in one conditional update: JobID, the
	// deadline and one more JobsTaken, WHERE jobs_taken < (SELECT max_jobs
	// FROM shapes WHERE id = machines.shape_id), and returns the machine as
	// stored. ErrConflict when the machine has taken max_jobs already, so of
	// two concurrent polls only one gets the last job: the caller gives the
	// lease up. domain.Machine.CanTakeJob is only an early look.
	TakeJob(ctx context.Context, id, jobID uuid.UUID, deadline time.Time) (domain.Machine, error)
}

// BootstrapTokenRepo stores machines' one-time tokens, one per machine.
type BootstrapTokenRepo interface {
	// Issue stores t, replacing the machine's token unless that one was
	// used (ErrConflict). Only hashes are kept, so delivering a machine to
	// a provider mints a token, and it is called for that and nothing else:
	// a machine the agent already has keeps its token (fleet-agent-api.md,
	// poll).
	Issue(ctx context.Context, t *domain.BootstrapToken) error
	// GetByTokenHash is unscoped: the token identifies the machine. Used
	// and expired tokens are returned too; callers check Usable.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.BootstrapToken, error)
	// Assign binds an unassigned token to its organization, with an expiry
	// counted from now. The machine is assigned first, in the same
	// transaction.
	Assign(ctx context.Context, machineID, accountID uuid.UUID, expiresAt time.Time) error
	// Consume marks the token used; ErrConflict when it is not
	// domain.BootstrapToken.Usable or its machine is not booting, so two
	// callers cannot both use it and a finished machine gets no worker.
	Consume(ctx context.Context, hash []byte) (domain.BootstrapToken, error)
}

// FleetPolicyRepo stores fleet policies, one per organization.
type FleetPolicyRepo interface {
	// Get returns domain.DefaultFleetPolicy when the organization has no
	// row.
	Get(ctx context.Context, accountID uuid.UUID) (domain.FleetPolicy, error)
	// Put creates or replaces the row.
	Put(ctx context.Context, p *domain.FleetPolicy) error
}

// ImageFilter selects images; zero fields match everything.
type ImageFilter struct {
	Name   string
	OS     domain.MachineOS
	Arch   domain.MachineArch
	Format domain.ImageFormat
	Limit  int
}

// ImageRepo stores images. (name, version, arch, format) is unique per
// organization, and among instance-wide images.
type ImageRepo interface {
	Create(ctx context.Context, i *domain.Image) error
	Get(ctx context.Context, id uuid.UUID) (domain.Image, error)
	// List returns an organization's own images and the instance-wide
	// ones, or only the latter for the zero ID.
	List(ctx context.Context, accountID uuid.UUID, f ImageFilter) ([]domain.Image, error)
	// Resolve returns what a shape's image reference names for an
	// organization: the version asked for, or the newest by BuiltAt, among
	// images of os, arch and one of formats. The organization's own image
	// wins over an instance-wide one of the same version.
	Resolve(ctx context.Context, accountID uuid.UUID, ref string, os domain.MachineOS, arch domain.MachineArch, formats []domain.ImageFormat) (domain.Image, error)
	// Delete removes an image by retention; ErrConflict while a live
	// machine's shape pins it.
	Delete(ctx context.Context, id uuid.UUID) error
}

// UsageFilter selects usage records by EndedAt in [From, To); zero fields
// match everything.
type UsageFilter struct {
	ShapeID   uuid.UUID
	Ownership domain.Ownership
	From, To  time.Time
}

// UsageTotal sums usage records.
type UsageTotal struct {
	Minutes float64
	Cost    float64
}

// UsageRepo stores usage records, one per machine.
type UsageRepo interface {
	// Create stores a record; a machine that has one fails with
	// ErrConflict, so metering can be retried.
	Create(ctx context.Context, u *domain.UsageRecord) error
	List(ctx context.Context, accountID uuid.UUID, f UsageFilter) ([]domain.UsageRecord, error)
	Sum(ctx context.Context, accountID uuid.UUID, f UsageFilter) (UsageTotal, error)
	// SumByProvider sums a provider's records of every organization, for
	// its monthly budget.
	SumByProvider(ctx context.Context, providerID uuid.UUID, from, to time.Time) (UsageTotal, error)
}
