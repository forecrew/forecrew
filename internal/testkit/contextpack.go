package testkit

import (
	"context"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/workflow"
)

var _ contextpack.Builder = (*ContextBuilder)(nil)

// ContextBuilder is a contextpack.Builder that renders a deterministic prompt
// from the role instructions and the request, or returns Err.
type ContextBuilder struct {
	Err error
}

// Build implements contextpack.Builder.
func (b *ContextBuilder) Build(_ context.Context, req workflow.JobRequest, role roles.Role) (contextpack.Pack, error) {
	if b.Err != nil {
		return contextpack.Pack{}, b.Err
	}
	var sb strings.Builder
	sb.WriteString(role.Instructions)
	fmt.Fprintf(&sb, "\nrole: %s\nrepo: %s\n", req.Role, req.Repo)
	for _, ref := range []struct {
		name string
		id   uuid.UUID
	}{{"goal", req.GoalID}, {"spec", req.SpecID}, {"task", req.TaskID}} {
		if ref.id != uuid.Nil() {
			fmt.Fprintf(&sb, "%s: %s\n", ref.name, ref.id)
		}
	}
	if req.Feedback != "" {
		fmt.Fprintf(&sb, "feedback: %s\n", req.Feedback)
	}
	return contextpack.Pack{Prompt: sb.String()}, nil
}
