// Package roles resolves a role (packs/roles YAML plus the roles section of
// forecrew.yaml) to everything a job of that role needs. internal/testkit
// has a fake.
package roles

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
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
	// DefinitionOfDone lists what must be true when the job finishes.
	DefinitionOfDone []string
	// EscalateWhen lists when the agent must stop and ask a human.
	EscalateWhen []string
	// Pool is the worker pool the role's jobs run in.
	Pool string
	// Toolbox is the image the role's jobs run in; nil means the worker's
	// host.
	Toolbox *domain.Toolbox
}

// ReadOnly reports whether the role may not write any path.
func (r Role) ReadOnly() bool { return len(r.WritePaths) == 0 }

// Resolver looks up roles. The project lets project profiles adjust roles.
type Resolver interface {
	Resolve(ctx context.Context, accountID, projectID uuid.UUID, name string) (Role, error)
}

// WithoutWrites returns r read-only: without write paths and without the
// runtime tools of repo.write and git.commit. Council jobs of roles that
// usually write run this way.
func (r Role) WithoutWrites() Role {
	drop := map[string]bool{}
	for _, t := range writeTools {
		for _, rt := range tools[t] {
			drop[rt] = true
		}
	}
	r.WritePaths = nil
	r.AllowedTools = slices.DeleteFunc(slices.Clone(r.AllowedTools), func(t string) bool { return drop[t] })
	return r
}
