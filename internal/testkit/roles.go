package testkit

import (
	"context"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/roles"
)

var _ roles.Resolver = Roles(nil)

// Roles is a roles.Resolver backed by a map from role name to role; it
// ignores the account and project.
type Roles map[string]roles.Role

// Resolve implements roles.Resolver.
func (r Roles) Resolve(_ context.Context, _, _ uuid.UUID, name string) (roles.Role, error) {
	role, ok := r[name]
	if !ok {
		return roles.Role{}, fmt.Errorf("%q: %w", name, roles.ErrUnknownRole)
	}
	return role, nil
}
