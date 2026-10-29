// Package workflow runs playbooks (packs/playbooks): the engine, the signal
// bus that wakes waiting steps and the contract of the job creator that job
// steps call. internal/testkit has a JobCreator fake.
// forecrewd runs as a single replica in the MVP: one Engine per database.
// Signals published by another process reach the engine through its
// periodic sweep (DefaultSweep, 30 s); in-process ones wake runs at once.
package workflow

import (
	"context"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// JobRequest asks for a job. Its yaml keys are the `with:` keys of a
// playbook job step.
type JobRequest struct {
	// ID, when set, is the new job's ID. The engine sets it to the step
	// run's ID so that a step resumed after a crash finds its job.
	ID               uuid.UUID `json:"-" yaml:"-"`
	AccountID        uuid.UUID `json:"account_id" yaml:"account_id"`
	ProjectID        uuid.UUID `json:"project_id" yaml:"project_id"`
	Repo             string    `json:"repo" yaml:"repo"`         // "owner/name"
	BaseRef          string    `json:"base_ref" yaml:"base_ref"` // defaults to the project's default branch
	Branch           string    `json:"branch" yaml:"branch"`     // agent/<...> to check out; writing roles push it, read-only roles may leave it empty or name one to inspect (never pushed)
	Role             string    `json:"role" yaml:"role"`
	Needs            []string  `json:"needs" yaml:"needs"` // labels the worker must have
	IncludeRepoFacts bool      `json:"include_repo_facts" yaml:"include_repo_facts"`
	// Optional refs the context pack draws on.
	GoalID uuid.UUID `json:"goal_id,omitzero" yaml:"goal_id,omitempty"`
	SpecID uuid.UUID `json:"spec_id,omitzero" yaml:"spec_id,omitempty"`
	TaskID uuid.UUID `json:"task_id,omitzero" yaml:"task_id,omitempty"`
	// Feedback is added to the prompt of reruns and rework.
	Feedback string `json:"feedback,omitempty" yaml:"feedback,omitempty"`
}

// JobCreator creates jobs. The implementation resolves the role, builds the
// prompt with the context pack, stores the job (queued) with req.ID when it
// is set, and returns its ID.
type JobCreator interface {
	Create(ctx context.Context, req JobRequest) (uuid.UUID, error)
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
