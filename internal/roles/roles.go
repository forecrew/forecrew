// Package roles resolves a role (packs/roles YAML plus the roles section of
// forecrew.yaml) to everything a job of that role needs. internal/testkit
// has a fake.
package roles

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"
)

// ErrUnknownRole means no role has the requested name.
var ErrUnknownRole = errors.New("unknown role")

// Role is a resolved role.
type Role struct {
	Name    string
	Runtime string // adapter of the role's configured runtime: claude-code or codex
	Model   string // model of the role's tier for the runtime's vendor
	// WritePaths are doublestar globs; "!" excludes; empty means read-only.
	WritePaths []string
	// Instructions is the role prompt, placed first in the job prompt.
	Instructions string
	// OutputSchema is the inline JSON Schema of the role's output.
	OutputSchema json.RawMessage
	AllowedTools []string
	MaxTurns     int
	Timeout      time.Duration
}

// ReadOnly reports whether the role may not write any path.
func (r Role) ReadOnly() bool { return len(r.WritePaths) == 0 }

// Resolver looks up roles. The project lets project profiles adjust roles.
type Resolver interface {
	Resolve(ctx context.Context, accountID, projectID uuid.UUID, name string) (Role, error)
}
