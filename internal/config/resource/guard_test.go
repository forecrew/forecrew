package resource_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tools"
)

// TestAssigningAnUntestedModelNeedsTheSecondConfirmation: a model never
// tested, tested with another agent, or below the role's level makes the
// plan say why and need the browser and the word.
func TestAssigningAnUntestedModelNeedsTheSecondConfirmation(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.engine = resource.NewEngine(resource.Config{Store: w.st, Server: w.server, Getenv: env, Secrets: w.vault,
		Access:     func(st store.Store) resource.Access { return tools.New(tools.Config{Store: st}) },
		RoleWrites: func(role string) bool { return role != "reviewer" }})
	w.connect("acme")
	reviewer := func(model string) mcp.ConfigWanted {
		a := assignment("acme")
		a["tiers"] = map[string]any{"strong": map[string]any{"acme": "m1"}, "best": map[string]any{"acme": model}}
		a["roles"].(map[string]any)["reviewer"] = map[string]any{"connection": "acme", "tier": "best"}
		return wanted(t, "model_assignment", "organization", a)
	}
	reasons := func(p domain.ConfigPlan) []string {
		return slices.DeleteFunc(slices.Clone(p.LowersProtection), func(s string) bool { return !strings.Contains(s, "would run model") })
	}

	p := w.plan(w.owner, reviewer("r1"))
	if got := reasons(p); len(got) != 1 || !strings.Contains(got[0], "Role reviewer (the organization) would run model r1 on connection acme, which was never tested") {
		t.Fatalf("never tested = %v", p.LowersProtection)
	}
	_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, mcp.ErrConflict)

	// A reading role is served by a model that passed reading.
	w.tested("acme", "r2", domain.LevelRead)
	if got := reasons(w.plan(w.owner, reviewer("r2"))); len(got) != 0 {
		t.Fatalf("read level for a reader = %v", got)
	}
	// A writing role is not.
	dev := assignment("acme")
	dev["tiers"] = map[string]any{"strong": map[string]any{"acme": "r2"}}
	if got := reasons(w.plan(w.owner, wanted(t, "model_assignment", "organization", dev))); len(got) != 1 || !strings.Contains(got[0], "level read, and the role needs code") {
		t.Fatalf("read level for a writer = %v", got)
	}
	// A failed test, and a test of another agent version.
	w.tested("acme", "r3", domain.LevelNone)
	if got := reasons(w.plan(w.owner, reviewer("r3"))); len(got) != 1 || !strings.Contains(got[0], "level none") {
		t.Fatalf("failed = %v", got)
	}
	w.tested("acme", "r4", domain.LevelCode)
	rows, _ := w.st.ModelChecks().List(ctx, w.account.ID)
	for _, r := range rows {
		if r.Model == "r4" {
			r.Stale = true
			check(t, w.st.ModelChecks().Put(ctx, &r))
		}
	}
	if got := reasons(w.plan(w.owner, reviewer("r4"))); len(got) != 1 || !strings.Contains(got[0], "another version") {
		t.Fatalf("stale = %v", got)
	}
	w.tested("acme", "r5", "")
	for _, r := range func() []domain.ModelCheck { rows, _ := w.st.ModelChecks().List(ctx, w.account.ID); return rows }() {
		if r.Model == "r5" {
			r.Test, r.Status = nil, domain.SuitabilityQueued
			check(t, w.st.ModelChecks().Put(ctx, &r))
		}
	}
	if got := reasons(w.plan(w.owner, reviewer("r5"))); len(got) != 1 || !strings.Contains(got[0], "still being tested") {
		t.Fatalf("queued = %v", got)
	}
}
