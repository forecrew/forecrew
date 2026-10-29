package domain

import (
	"time"
	"uuid"
)

// Spec is one revision of the specification written for a goal. Every
// review round that requests changes produces a new revision; approval is
// recorded by its spec_approval gate.
type Spec struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	GoalID    uuid.UUID `json:"goal_id"`
	Number    int64     `json:"number"`   // SPEC-<number>
	Revision  int       `json:"revision"` // 1, 2, ... per goal
	Body      string    `json:"body"`     // markdown
	JobID     uuid.UUID `json:"job_id,omitzero"`
	CreatedAt time.Time `json:"created_at"`
}

// Ref returns the spec's human ref, e.g. SPEC-4.
func (s Spec) Ref() string { return Ref(RefSpec, s.Number) }
