package resource_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tools"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/jira"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
)

// trackerWorld is a world whose tools know the fake tracker and Jira.
func trackerWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	registry := tracker.NewRegistry()
	registry.Register(trackertest.Kind, trackertest.NewFake("s").Factory())
	registry.Register(jira.Kind, jira.Factory(jira.Deps{Secrets: w.vault}))
	core := tracker.New(tracker.Config{Store: w.st, Registry: registry, Authz: authz.New(w.st, nil)})
	w.engine = resource.NewEngine(resource.Config{
		Store: w.st, Access: func(st store.Store) resource.Access { return tools.New(tools.Config{Store: st, Trackers: core}) },
		Secrets: w.vault,
	})
	return w
}

func jiraSpec(base string) map[string]any {
	return map[string]any{
		"kind": "jira", "triggers": map[string]any{"label": "forecrew"},
		"config": map[string]any{"base_url": base, "projects": []string{"OPS"}, "auth": "pat", "token_ref": "store:jira-token"},
	}
}

func TestTrackerConnectionResource(t *testing.T) {
	w := trackerWorld(t)
	ctx := t.Context()
	p := w.plan(w.owner, wanted(t, "tracker_connection", "jira", jiraSpec("https://jira.acme.local/jira")))
	if !slices.Contains(p.Needs, domain.PermTrackersManage) || len(p.Steps) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	kinds := []domain.ConfigStepKind{p.Steps[0].Kind, p.Steps[1].Kind}
	if !slices.Contains(kinds, domain.ConfigStepSecret) || !slices.Contains(kinds, domain.ConfigStepConnect) {
		t.Errorf("steps = %+v", p.Steps)
	}
	_, err := w.engine.PutSecret(ctx, w.owner, p.ID, "jira-token", []byte("pat"))
	check(t, err)
	w.apply(w.owner, p)
	c, err := w.st.TrackerConnections().GetByName(ctx, w.account.ID, "jira")
	check(t, err)
	if c.Kind != "jira" || c.Config["base_url"] != "https://jira.acme.local/jira" || c.Triggers.Label != "forecrew" {
		t.Fatalf("connection = %+v", c)
	}
	// What the adapter read from Jira is no setting, and survives a change
	// of the triggers.
	c.Config["deployment"] = "data_center"
	check(t, w.st.TrackerConnections().Update(ctx, &c))
	if got := w.spec("tracker_connection", "jira"); strings.Contains(got, "deployment") {
		t.Errorf("spec = %s", got)
	}
	s := jiraSpec("https://jira.acme.local/jira")
	s["triggers"] = map[string]any{"state": "Ready"}
	p = w.plan(w.owner, wanted(t, "tracker_connection", "jira", s))
	if len(p.Changes) != 1 || p.Changes[0].Fields[0].Path != "triggers.label" {
		t.Fatalf("changes = %+v", p.Changes)
	}
	w.apply(w.owner, p)
	c, _ = w.st.TrackerConnections().GetByName(ctx, w.account.ID, "jira")
	if c.Config["deployment"] != "data_center" || c.Triggers.State != "Ready" {
		t.Errorf("updated connection = %+v", c)
	}
	// Another address is another tracker: read it again.
	p = w.plan(w.owner, wanted(t, "tracker_connection", "jira", jiraSpec("https://jira2.acme.local")))
	w.apply(w.owner, p)
	if c, _ = w.st.TrackerConnections().GetByName(ctx, w.account.ID, "jira"); c.Config["deployment"] != nil {
		t.Errorf("moved connection = %+v", c)
	}
	p = w.plan(w.owner, mcp.ConfigWanted{Kind: "tracker_connection", Name: "jira", Delete: true})
	if p.Changes[0].Op != domain.ConfigDelete || p.Reversible {
		t.Errorf("delete plan = %+v", p)
	}
	w.apply(w.owner, p)
	if _, err := w.st.TrackerConnections().GetByName(ctx, w.account.ID, "jira"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted connection: %v", err)
	}
}

func TestTrackerConnectionResourceRefuses(t *testing.T) {
	w := trackerWorld(t)
	ctx := t.Context()
	bad := map[string]map[string]any{
		"a secret value": jiraSpec("https://jira.acme.local"),
		"plain http":     jiraSpec("http://jira.acme.local"),
		"no trigger":     {"kind": "fake", "config": map[string]any{}, "triggers": map[string]any{}},
		"unknown kind":   {"kind": "linear", "config": map[string]any{}, "triggers": map[string]any{"label": "x"}},
		"env ref":        jiraSpec("https://jira.acme.local"),
		"unknown key":    {"kind": "fake", "config": map[string]any{}, "triggers": map[string]any{"label": "x"}, "verify": false},
	}
	bad["a secret value"]["config"].(map[string]any)["token_ref"] = "ATATT3xFfGF0abc"
	bad["env ref"]["config"].(map[string]any)["token_ref"] = "env:FORECREW_CONN_X"
	for name, s := range bad {
		if _, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "tracker_connection", "x", s)}}); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	w.apply(w.owner, w.plan(w.owner, wanted(t, "tracker_connection", "board", map[string]any{"kind": "fake", "config": map[string]any{}, "triggers": map[string]any{"label": "x"}})))
	other := map[string]any{"kind": "jira", "config": map[string]any{}, "triggers": map[string]any{"label": "x"}}
	if _, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "tracker_connection", "board", other)}}); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("a change of kind: %v", err)
	}
	// A viewer reads tracker connections and changes none.
	viewer := w.person(domain.RoleViewer)
	if rs, err := w.engine.Get(ctx, viewer, "tracker_connection", ""); err != nil || len(rs) != 1 {
		t.Errorf("viewer reads %+v, %v", rs, err)
	}
	// Without the tracker core, nothing is planned.
	bare := resource.NewEngine(resource.Config{Store: w.st, Access: func(st store.Store) resource.Access { return tools.New(tools.Config{Store: st}) }})
	if _, err := bare.Plan(ctx, w.owner, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "tracker_connection", "y", other)}}); err == nil {
		t.Error("planned without trackers")
	}
}

func route(project string, states map[string]string) map[string]any {
	return map[string]any{"project": project, "match": map[string]any{"labels": []string{"backend"}}, "goal_kind": "bug", "states": states}
}

func TestTrackerRouteResource(t *testing.T) {
	w := trackerWorld(t)
	ctx := t.Context()
	conn := map[string]any{"kind": "fake", "config": map[string]any{}, "triggers": map[string]any{"label": "x"}}
	// A connection and its route in one plan: the route's states wait for the
	// connection's test.
	p := w.plan(w.owner, wanted(t, "tracker_connection", "board", conn), wanted(t, "tracker_route", "board/1", route("app", map[string]string{"done": "closed"})))
	w.apply(w.owner, p)
	got := w.spec("tracker_route", "board/1")
	if !strings.Contains(got, `"project":"app"`) || !strings.Contains(got, `"done":"closed"`) || !strings.Contains(got, `"backend"`) {
		t.Fatalf("route = %s", got)
	}
	// Against a connection that exists, the states are the tracker's.
	for name, w2 := range map[string]mcp.ConfigWanted{
		"unknown state":         wanted(t, "tracker_route", "board/2", route("app", map[string]string{"done": "Shipped"})),
		"closed for work":       wanted(t, "tracker_route", "board/2", route("app", map[string]string{"in_progress": "closed"})),
		"unknown project":       wanted(t, "tracker_route", "board/2", route("nope", nil)),
		"bad name":              wanted(t, "tracker_route", "board", route("app", nil)),
		"position 0":            wanted(t, "tracker_route", "board/0", route("app", nil)),
		"testing playbook":      wanted(t, "tracker_route", "board/2", map[string]any{"project": "app", "match": map[string]any{}, "playbook": "bugfix"}),
		"unknown connection":    wanted(t, "tracker_route", "gone/1", route("nope", nil)),
		"unknown logical state": wanted(t, "tracker_route", "gone/1", route("app", map[string]string{"shipped": "x"})),
	} {
		if _, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{w2}}); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	w.apply(w.owner, w.plan(w.owner, wanted(t, "tracker_route", "board/1", route("app", map[string]string{"in_progress": "doing"}))))
	c, err := w.st.TrackerConnections().GetByName(ctx, w.account.ID, "board")
	check(t, err)
	routes, err := w.st.TrackerRoutes().List(ctx, w.account.ID, c.ID)
	check(t, err)
	if len(routes) != 1 || routes[0].States[domain.TicketInProgress] != "doing" || routes[0].Position != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	// A warning is the tracker's to set, not a setting.
	check(t, w.st.TrackerRoutes().SetWarning(ctx, w.account.ID, routes[0].ID, "no transition"))
	if got := w.spec("tracker_route", "board/1"); strings.Contains(got, "warning") {
		t.Errorf("route = %s", got)
	}
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "tracker_route", Name: "board/1", Delete: true}))
	if routes, _ := w.st.TrackerRoutes().List(ctx, w.account.ID, c.ID); len(routes) != 0 {
		t.Errorf("routes after delete = %+v", routes)
	}
	// Export and import keep routes by name.
	w.apply(w.owner, w.plan(w.owner, wanted(t, "tracker_route", "board/3", route("app", nil))))
	out, err := w.engine.Export(ctx, w.owner)
	check(t, err)
	if !strings.Contains(out, "board/3") {
		t.Errorf("export = %s", out)
	}
}
