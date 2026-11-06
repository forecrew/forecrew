package resource

import (
	"fmt"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// fleetStore holds the repositories that join store.Store with the fleet
// task. Until a store has them, the fleet areas read empty and refuse changes
// as unavailable.
type fleetStore interface {
	CapacityProviders() store.CapacityProviderRepo
	Shapes() store.ShapeRepo
	FleetPolicies() store.FleetPolicyRepo
}

func fleetOf(e *Env) (fleetStore, bool) {
	s, ok := e.Store.(fleetStore)
	return s, ok
}

func unavailable(what string) error {
	return fmt.Errorf("%s are not available on this server yet: %w", what, mcp.ErrUnavailable)
}

func projectResource(e *Env, p domain.Project) authz.Resource {
	return authz.Project(e.Actor.AccountID, p.ID)
}
