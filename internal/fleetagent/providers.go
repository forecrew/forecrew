package fleetagent

import (
	"fmt"
	"sync"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

// Factory makes the provider of a kind. It finds its credentials by the
// machine's own means: files readable by the agent's user, or an instance role.
type Factory func() (fleet.Provider, error)

var (
	factoriesMu sync.RWMutex
	factories   = map[domain.ProviderKind]Factory{}
)

// Register makes a kind available in this build. Each provider calls it from
// an init function.
func Register(kind domain.ProviderKind, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[kind] = f
}

// NewProvider returns the provider of kind.
func NewProvider(kind domain.ProviderKind) (fleet.Provider, error) {
	factoriesMu.RLock()
	f, ok := factories[kind]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("this build of the fleet agent has no provider of kind %s", kind)
	}
	return f()
}
