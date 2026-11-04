package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// AuditEntry is one row of the append-only audit log. Action names are
// "<subject>.<verb>", e.g. "gate.resolved", "job.created", "worker.revoked".
type AuditEntry struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id,omitzero"`
	Actor     string    `json:"actor"` // Actor.String()
	// Credential is the session, token or OAuth grant the actor used
	// (Credential.String()); empty for actors without one.
	Credential  string          `json:"credential,omitempty"`
	Action      string          `json:"action"`
	SubjectType string          `json:"subject_type"` // "job", "gate", ...
	SubjectID   uuid.UUID       `json:"subject_id,omitzero"`
	Data        json.RawMessage `json:"data,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}
