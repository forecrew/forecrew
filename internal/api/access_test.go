package api_test

import (
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestRouteAccessCoversEveryRoute fails when an /api/v1 route has no
// access entry (docs/contracts/permissions.md), or an entry names no route.
func TestRouteAccessCoversEveryRoute(t *testing.T) {
	st := testkit.NewStore(time.Now)
	svc := &testkit.ToolService{}
	srv := api.New(api.Config{Tools: svc, Artifacts: svc, Tokens: st.MCPTokens(), Accounts: st.Accounts(), Logger: slog.New(slog.DiscardHandler)})
	srv.Handler()
	routes := srv.Routes()
	access := api.RouteAccess()
	for _, r := range routes {
		a, ok := access[r]
		if !ok {
			t.Errorf("route %s has no entry in RouteAccess", r)
			continue
		}
		if err := mcp.CheckAccess(a); err != nil {
			t.Errorf("route %s: %v", r, err)
		}
	}
	for _, r := range slices.Sorted(maps.Keys(access)) {
		if !slices.Contains(routes, r) {
			t.Errorf("RouteAccess lists %s, which is not a route", r)
		}
	}
	if len(routes) < 20 {
		t.Fatalf("only %d routes recorded", len(routes))
	}
}
