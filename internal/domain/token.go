package domain

import (
	"time"
	"uuid"
)

// MCPToken is a personal or service account token for the MCP server and
// /api/v1 (docs/contracts/identity.md). Only its hash is stored; the token is
// shown once at creation. A request with it is limited to its scopes and its
// owner's current permissions in its organization (AccountID).
type MCPToken struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Name      string    `json:"name"` // unique per organization and owner
	TokenHash []byte    `json:"-"`    // SHA-256 of the token
	// OwnerKind and OwnerID say whose token it is: a user or a service
	// account. Both are empty on a token from before migration 00005 until
	// it is assigned to the first Owner.
	OwnerKind  SubjectKind `json:"owner_kind,omitempty"`
	OwnerID    uuid.UUID   `json:"owner_id,omitzero"`
	Scopes     []Scope     `json:"scopes"`
	ExpiresAt  time.Time   `json:"expires_at,omitzero"` // zero: never (tokens from before 00005 only)
	LastUsedAt time.Time   `json:"last_used_at,omitzero"`
	RevokedAt  time.Time   `json:"revoked_at,omitzero"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Usable reports whether the token may be used at now: not revoked and not
// expired. The owner's state (disabled, suspended) is checked separately.
func (t MCPToken) Usable(now time.Time) bool {
	return t.RevokedAt.IsZero() && (t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt))
}

// Validate checks the owner (both or neither of kind and ID; a user or a
// service account), at least one known scope, and the hash.
func (t MCPToken) Validate() error {
	var p problems
	if (t.OwnerKind == "") != (t.OwnerID == uuid.Nil()) {
		p.addf("owner_kind and owner_id are set together")
	}
	if t.OwnerKind != "" && t.OwnerKind != SubjectUser && t.OwnerKind != SubjectServiceAccount {
		p.addf("owner_kind must be user or service_account, got %q", t.OwnerKind)
	}
	if len(t.Scopes) == 0 {
		p.addf("scopes must not be empty")
	}
	for _, s := range t.Scopes {
		checkEnum(&p, "scopes", s, Scopes())
	}
	if len(t.TokenHash) != 32 {
		p.addf("token hash must be a SHA-256")
	}
	return p.errOf(ErrInvalidRecord, "token "+t.Name)
}

// WebhookDelivery records a processed GitHub webhook delivery so a
// redelivery is ignored.
type WebhookDelivery struct {
	DeliveryID string    `json:"delivery_id"` // X-GitHub-Delivery
	Event      string    `json:"event"`       // X-GitHub-Event
	ReceivedAt time.Time `json:"received_at"`
}
