//go:build integration

package postgres

import (
	"errors"
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestConfigRepositories(t *testing.T) {
	f := newFixture(t)
	testkit.ConfigStoreSuite(t, f.st, f.account, f.project.ID, f.newJob(t).ID)
}

// TestConfigPermissionsOfBuiltInRoles: the migration gave them to the roles
// domain.BuiltInRoles says, so organizations from before it get them too.
func TestConfigPermissionsOfBuiltInRoles(t *testing.T) {
	f := newFixture(t)
	roles, err := f.st.Roles().List(t.Context(), f.account)
	must(t, err)
	for _, r := range roles {
		want, ok := domain.BuiltInRole(r.Scope, r.Name)
		if !ok {
			continue
		}
		if !slices.Equal(r.Permissions, want.Permissions) {
			t.Errorf("%s %s = %v, want %v", r.Scope, r.Name, r.Permissions, want.Permissions)
		}
	}
}

func TestModelAssignmentOfUnknownProject(t *testing.T) {
	f := newFixture(t)
	a := domain.ModelAssignment{AccountID: f.account, ProjectID: domain.NewID(), UpdatedBy: "user:ana"}
	if err := f.st.ModelAssignments().Put(t.Context(), &a); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("put = %v", err)
	}
}
