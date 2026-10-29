// Package contextpack builds the prompt of a job from its role and the
// goal, spec, task, project profile and feedback it refers to.
// internal/testkit has a fake.
package contextpack

import (
	"context"

	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Pack is a built context.
type Pack struct {
	Prompt string
}

// Builder builds context packs.
type Builder interface {
	Build(ctx context.Context, req workflow.JobRequest, role roles.Role) (Pack, error)
}
