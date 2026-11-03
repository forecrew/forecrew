// Package contextpack builds the prompt of a job from its role and the
// goal, spec, task, project profile and feedback it refers to.
// internal/testkit has a fake.
package contextpack

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Pack is a built context.
type Pack struct {
	// Prompt is the full prompt of the job, stored in its spec.
	Prompt string
	// Hash is "sha256:<hex>" of Prompt.
	Hash string
}

// Builder builds context packs.
type Builder interface {
	Build(ctx context.Context, req workflow.JobRequest, role roles.Role) (Pack, error)
}

var _ Builder = (*StoreBuilder)(nil)

// StoreBuilder is the Builder of forecrewd: it loads the project, goal,
// spec and task from the store, and for the test roles the test plan from
// the repo, and renders the prompt. The repo's own CLAUDE.md and AGENTS.md
// are not copied in: runtimes load them from the checkout.
type StoreBuilder struct {
	store   store.Store
	files   RepoFiles
	allowed func(login string) bool
	budget  int
}

// New returns a StoreBuilder. files reads the test plan of the test roles'
// jobs; nil leaves it out. allowed reports whether a GitHub login is on
// the allowlist (config.GitHub.Allowed); text from other GitHub users is
// marked untrusted. budget is the prompt size budget in bytes, DefaultBudget
// when <= 0.
func New(st store.Store, files RepoFiles, allowed func(login string) bool, budget int) *StoreBuilder {
	return &StoreBuilder{store: st, files: files, allowed: allowed, budget: budget}
}

// Build implements Builder. The spec and goal default to those of the task,
// and the goal to that of the spec. req.Prompt becomes the trusted Instructions section. req.Feedback is quoted as given: callers
// wrap text from GitHub users outside the allowlist with WrapUntrusted.
func (b *StoreBuilder) Build(ctx context.Context, req workflow.JobRequest, role roles.Role) (Pack, error) {
	in, err := b.input(ctx, req, role.Name)
	if err != nil {
		return Pack{}, fmt.Errorf("build %s context: %w", role.Name, err)
	}
	return Render(role, in, b.budget)
}

func (b *StoreBuilder) input(ctx context.Context, req workflow.JobRequest, role string) (Input, error) {
	acc := req.AccountID
	project, err := b.store.Projects().Get(ctx, acc, req.ProjectID)
	if err != nil {
		return Input{}, fmt.Errorf("load project %s: %w", req.ProjectID, err)
	}
	in := Input{Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, Profile: project.Profile}
	if in.Repo == "" {
		in.Repo = project.Repo
	}
	if in.BaseRef == "" {
		in.BaseRef = project.DefaultBranch
	}
	if b.files != nil && slices.Contains(testPlanRoles, role) {
		if in.TestPlan, err = b.testPlan(ctx, in.Repo, in.BaseRef, in.Branch); err != nil {
			return Input{}, err
		}
	}
	specID, goalID := req.SpecID, req.GoalID
	if req.TaskID != uuid.Nil() {
		task, err := b.store.Tasks().Get(ctx, acc, req.TaskID)
		if err != nil {
			return Input{}, fmt.Errorf("load task %s: %w", req.TaskID, err)
		}
		in.Task = Text{Heading: fmt.Sprintf("Task %s: %s", task.Ref(), task.Title), Body: task.Body}
		specID = cmp.Or(specID, task.SpecID)
		goalID = cmp.Or(goalID, task.GoalID)
	}
	if specID != uuid.Nil() {
		spec, err := b.store.Specs().Get(ctx, acc, specID)
		if err != nil {
			return Input{}, fmt.Errorf("load spec %s: %w", specID, err)
		}
		in.Spec = Text{Heading: fmt.Sprintf("Spec %s, revision %d", spec.Ref(), spec.Revision), Body: spec.Body}
		goalID = cmp.Or(goalID, spec.GoalID)
	}
	if goalID != uuid.Nil() {
		goal, err := b.store.Goals().Get(ctx, acc, goalID)
		if err != nil {
			return Input{}, fmt.Errorf("load goal %s: %w", goalID, err)
		}
		in.Goal = Text{Heading: fmt.Sprintf("Goal %s: %s", goal.Ref(), goal.Title), Body: goal.Body}
		in.Goal.Untrusted, in.Goal.Source = b.untrusted(goal.CreatedBy), goal.CreatedBy
	}
	if req.Prompt != "" {
		in.Instructions = Text{Heading: "Instructions", Body: req.Prompt}
	}
	if req.Feedback != "" {
		in.Feedback = Text{Heading: "Feedback to address", Body: req.Feedback}
	}
	return in, nil
}

// untrusted reports whether text by actor (Actor.String()) comes from a
// GitHub user outside the allowlist.
func (b *StoreBuilder) untrusted(actor string) bool {
	login, ok := strings.CutPrefix(actor, string(domain.ActorGitHub)+":")
	return ok && (b.allowed == nil || !b.allowed(login))
}
