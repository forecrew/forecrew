package domain

import (
	"time"
	"uuid"
)

// ChangeState mirrors the state of the pull request.
type ChangeState string

// Change states.
const (
	ChangeOpen   ChangeState = "open"
	ChangeMerged ChangeState = "merged"
	ChangeClosed ChangeState = "closed" // closed without merging
)

// Change is a pull request. It is upserted by (account, repo, number)
// whenever a PR is opened or updated.
type Change struct {
	ID        uuid.UUID   `json:"id"`
	AccountID uuid.UUID   `json:"account_id"`
	ProjectID uuid.UUID   `json:"project_id"`
	TaskID    uuid.UUID   `json:"task_id,omitzero"` // nil for PRs not opened for a task
	Repo      string      `json:"repo"`             // "owner/name"
	Number    int         `json:"number"`           // PR number
	Branch    string      `json:"branch"`           // head branch
	BaseRef   string      `json:"base_ref"`
	HeadSHA   string      `json:"head_sha"`
	State     ChangeState `json:"state"`
	URL       string      `json:"url"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}
