package resource

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
)

// guardModels gives, for the second confirmation, each role a plan puts on a
// model that was never tested, was tested with another agent version, or did
// not pass what the role needs: every ability for a role that writes code,
// reading, MCP and JSON for one that only reads (config-resources.md, Model
// checks).
func guardModels(ctx context.Context, e *Env, changes []domain.ConfigChange) ([]string, error) {
	if !touchesModels(changes) {
		return nil, nil
	}
	before, err := modelWorldOf(ctx, e)
	if err != nil {
		return nil, err
	}
	after, err := modelWorldOf(ctx, e)
	if err != nil {
		return nil, err
	}
	if err := after.overlay(ctx, e, changes); err != nil {
		return nil, err
	}
	checks, err := e.Store.ModelChecks().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range after.scopes() {
		for _, role := range slices.Sorted(maps.Keys(s.assignment.Roles)) {
			r := models.Request{ProjectID: s.project, Role: role}
			d := after.state.Decide(after.server, r)
			if d.Waiting != "" || d.Source != models.SourceOrganization {
				continue
			}
			if b := before.state.Decide(before.server, r); b.Connection == d.Connection && b.Model == d.Model {
				continue
			}
			if why := untested(checks, after.state.Connections[d.Connection], d.Model, roleWrites(e, role)); why != "" {
				out = append(out, fmt.Sprintf("Role %s (%s) would run model %s on connection %s, which %s", role, s.title, d.Model, d.Connection, why))
			}
		}
	}
	return out, nil
}

// roleWrites reports whether role writes code; a role nobody knows counts as one.
func roleWrites(e *Env, role string) bool {
	if e.RoleWrites == nil {
		return true
	}
	return e.RoleWrites(role)
}

// untested says why model on c is not known to serve a role, or "".
func untested(checks []domain.ModelCheck, c domain.ModelConnection, model string, writes bool) string {
	i := slices.IndexFunc(checks, func(x domain.ModelCheck) bool { return x.ConnectionID == c.ID && x.Model == model })
	switch {
	case c.ID == uuid.Nil() || i < 0 || checks[i].Test == nil:
		return "was never tested"
	case checks[i].Stale:
		return "was tested with another version of its agent"
	case !domain.Serves(checks[i].Test.Level, writes):
		need := domain.LevelRead
		if writes {
			need = domain.LevelCode
		}
		return fmt.Sprintf("passed the test at level %s, and the role needs %s", checks[i].Test.Level, need)
	}
	return ""
}
