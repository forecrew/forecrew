package domain

import (
	"time"
	"uuid"
)

// MCPToken is a bearer token for the MCP server and /api/v1. Only its hash is
// stored; the token is shown once at creation.
type MCPToken struct {
	ID         uuid.UUID `json:"id"`
	AccountID  uuid.UUID `json:"account_id"`
	Name       string    `json:"name"` // unique per account
	TokenHash  []byte    `json:"-"`    // SHA-256 of the token
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
}

// WebhookDelivery records a processed GitHub webhook delivery so a
// redelivery is ignored.
type WebhookDelivery struct {
	DeliveryID string    `json:"delivery_id"` // X-GitHub-Delivery
	Event      string    `json:"event"`       // X-GitHub-Event
	ReceivedAt time.Time `json:"received_at"`
}
