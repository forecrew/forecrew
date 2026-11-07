// Package workflow runs playbooks (packs/playbooks): the engine, the signal
// bus that wakes waiting steps and the contract of the job creator that job
// steps call. internal/testkit has a JobCreator fake.
// forecrewd runs as a single replica in the MVP: one Engine per database.
// Signals published by another process reach the engine through its
// periodic sweep (DefaultSweep, 30 s); in-process ones wake runs at once.
package workflow

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// JobRequest asks for a job. Its yaml keys are the `with:` keys of a
// playbook job step.
type JobRequest struct {
	// ID, when set, is the new job's ID. The engine sets it to the step
	// run's ID so that a step resumed after a crash finds its job.
	ID        uuid.UUID `json:"-" yaml:"-"`
	AccountID uuid.UUID `json:"account_id" yaml:"account_id"`
	ProjectID uuid.UUID `json:"project_id" yaml:"project_id"`
	Repo      string    `json:"repo" yaml:"repo"`         // "owner/name"
	BaseRef   string    `json:"base_ref" yaml:"base_ref"` // defaults to the project's default branch
	Branch    string    `json:"branch" yaml:"branch"`     // agent/<...> to check out; writing roles push it, read-only roles may leave it empty or name one to inspect (never pushed)
	Role      string    `json:"role" yaml:"role"`
	Needs     []string  `json:"needs" yaml:"needs"` // labels the worker must have
	// Pool is the worker pool to run in; empty means the role pack's pool.
	// The pool's policy must allow the role (config.md, pools).
	Pool             string `json:"pool,omitempty" yaml:"pool,omitempty"`
	IncludeRepoFacts bool   `json:"include_repo_facts" yaml:"include_repo_facts"`
	// Optional refs the context pack draws on.
	GoalID uuid.UUID `json:"goal_id,omitzero" yaml:"goal_id,omitempty"`
	SpecID uuid.UUID `json:"spec_id,omitzero" yaml:"spec_id,omitempty"`
	TaskID uuid.UUID `json:"task_id,omitzero" yaml:"task_id,omitempty"`
	// Feedback is added to the prompt of reruns and rework.
	Feedback string `json:"feedback,omitempty" yaml:"feedback,omitempty"`
	// Prompt is extra instructions for the job, added to its prompt (ad-hoc
	// jobs from dispatch_job).
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	// LockedPaths are globs an agent job may not change even when its role's
	// write paths allow it (the tests written from the spec). They are put
	// in the job spec.
	LockedPaths []string `json:"locked_paths,omitempty" yaml:"locked_paths,omitempty"`
	// WritePaths are globs added to the role's write paths for this job, so
	// that a read-only role writes the files they name (the architect's ADR). Tasks, council turns and proposal reviews stay read-only.
	// They are for playbook authors: take them from an action, never from a
	// job's output. The job needs a pool whose policy gives a write token.
	WritePaths []string `json:"write_paths,omitempty" yaml:"write_paths,omitempty"`
	// ProposalReview makes the job the review of a knowledge proposal by the
	// section's owner role: read-only, with the reviewer's verdict as output.
	ProposalReview bool `json:"proposal_review,omitempty" yaml:"proposal_review,omitempty"`
	// Task makes the job a task of the role instead of its usual work (a
	// packs/tasks file, roles.Task): the knowledge curator's retro or
	// curate. Read-only, with the task's instructions and output schema.
	Task string `json:"task,omitempty" yaml:"task,omitempty"`
	// Council makes the job a turn of a council: the creator replaces the
	// role's job with the council step's (councils.Set.Job).
	Council *CouncilTurn `json:"council,omitempty" yaml:"council,omitempty"`

	// Kind is agent (the default) or command. A command job has no role,
	// prompt or context pack: it runs Command, optionally in Environment,
	// for one matrix Cell and Suite, with Overlay applied on top of
	// BaseRef. Its pool follows from its environment, and Pool must be empty.
	Kind        domain.JobKind      `json:"kind,omitempty" yaml:"kind,omitempty"`
	Command     *domain.CommandSpec `json:"command,omitempty" yaml:"command,omitempty"`
	Environment *domain.Environment `json:"environment,omitempty" yaml:"environment,omitempty"`
	Cell        map[string]string   `json:"cell,omitempty" yaml:"cell,omitempty"`
	Suite       string              `json:"suite,omitempty" yaml:"suite,omitempty"`
	Overlay     *domain.Overlay     `json:"overlay,omitempty" yaml:"overlay,omitempty"`
	// Timeout bounds a command job; zero means the creator's default.
	Timeout domain.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// CouncilTurn says which turn of a council a job is (docs/councils.md). The
// council playbook fills it from the outputs of its actions.
type CouncilTurn struct {
	CouncilID uuid.UUID `json:"council_id" yaml:"council_id"`
	Step      string    `json:"step" yaml:"step"` // a councils.Step
	Round     int       `json:"round" yaml:"round"`
	// Priority names the member's seat; empty for the facilitator.
	Priority string `json:"priority,omitempty" yaml:"priority,omitempty"`
	// Runtime is the adapter a member is pinned to; empty for the facilitator,
	// which runs on its role's runtime.
	Runtime string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	// Roles are the members a council of its own chose instead of its pack's.
	Roles []string `json:"roles,omitempty" yaml:"roles,omitempty"`
	// BudgetExhausted tells a synthesis that the budget ran out.
	BudgetExhausted bool `json:"budget_exhausted,omitempty" yaml:"budget_exhausted,omitempty"`
	// Framing is the facilitator's framing, the output of the frame job.
	// It is agent text: the prompt quotes it as untrusted data.
	Framing json.RawMessage `json:"framing,omitempty" yaml:"framing,omitempty"`
}

// JobCreator creates jobs. The implementation resolves the role, builds the
// prompt with the context pack, stores the job (queued) with req.ID when it
// is set, and returns its ID.
type JobCreator interface {
	Create(ctx context.Context, req JobRequest) (uuid.UUID, error)
	// Cancel cancels a queued or running job, as forecrewd. Errors wrap
	// store.ErrNotFound for an unknown job and domain.ErrInvalidTransition
	// for a finished one.
	Cancel(ctx context.Context, accountID, jobID uuid.UUID) error
}

// SignalBus stores signals and wakes the engine.
type SignalBus interface {
	// Publish stores s unless its DedupeKey was seen in the account, then
	// notifies subscribers. It reports whether s was new.
	Publish(ctx context.Context, s domain.Signal) (bool, error)
	// Lookup returns the stored signals with a correlation key, oldest
	// first, so a step that starts waiting sees facts that arrived before.
	Lookup(ctx context.Context, accountID uuid.UUID, key string) ([]domain.Signal, error)
	// Subscribe delivers every signal published after the call until ctx is
	// done, when the channel is closed. Slow subscribers may miss signals and
	// must recover with Lookup.
	Subscribe(ctx context.Context) <-chan domain.Signal
}
