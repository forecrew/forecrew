package domain

import (
	"time"
	"uuid"
)

// Account owns projects, workers and tokens. The MVP has a single account
// created at install.
type Account struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}
