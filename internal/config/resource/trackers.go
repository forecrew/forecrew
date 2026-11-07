package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// trackerConnectionSpec is a tracker connection (docs/contracts/tracker.md).
// Config holds references to secrets, never one: a key ending in _ref names
// a value of the secret store.
type trackerConnectionSpec struct {
	TrackerKind string                 `json:"kind"`
	Config      map[string]any         `json:"config"`
	Triggers    domain.TrackerTriggers `json:"triggers"`
}

func trackerConnectionArea() Area {
	a := base[trackerConnectionSpec](KindTrackerConn, "Tracker connection", ScopeOrganization, domain.PermTrackersRead)
	a.Setting = domain.TrackerConnection{}
	a.Hidden = []string{"id", "account_id", "name", "created_at", "updated_at"}
	a.Affects = "Tickets handed over on the tracker become goals; status goes back to them."
	a.Needs = needsOf(domain.PermTrackersManage)
	norm := a.Normalize
	a.Normalize = func(raw json.RawMessage) (json.RawMessage, error) {
		s, err := decode[trackerConnectionSpec](raw)
		if err != nil {
			return norm(raw)
		}
		if s.Config == nil {
			s.Config = map[string]any{}
		}
		return encode(s), nil
	}
	a.Read = trackerConnectionRead
	a.Validate = trackerConnectionValidate
	a.Apply = trackerConnectionApply
	a.Secrets = trackerConnectionSecrets
	a.Steps = trackerConnectionSteps
	a.Lost = func(old, want *Resource) string {
		if want == nil && old != nil {
			return "Deleting a connection drops its routes, who is who on it and how far polling got; a revert brings back the connection alone."
		}
		return ""
	}
	return a
}

// trackerConnectionRead reads the connections without the config keys their
// adapters fill in, which are no setting.
func trackerConnectionRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.TrackerConnections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, c := range rows {
		cfg := maps.Clone(c.Config)
		if e.Access != nil {
			for _, k := range e.Access.TrackerDerived(c) {
				delete(cfg, k)
			}
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		out = append(out, Resource{Kind: KindTrackerConn, Name: c.Name, Spec: encode(trackerConnectionSpec{c.Kind, cfg, c.Triggers})})
	}
	return out, nil
}

// trackerConnectionRow checks a spec as create_tracker_connection does,
// short of asking the tracker: the token may wait staged for this plan.
func trackerConnectionRow(ctx context.Context, e *Env, want Resource) (domain.TrackerConnection, []string, error) {
	s, err := decode[trackerConnectionSpec](want.Spec)
	if err != nil {
		return domain.TrackerConnection{}, nil, err
	}
	if e.Access == nil {
		return domain.TrackerConnection{}, nil, invalid("trackers are not available here")
	}
	for k, v := range s.Config {
		if !strings.HasSuffix(k, "_ref") {
			continue
		}
		if ref, _ := v.(string); !domain.ValidOrganizationSecretRef(ref) || !strings.HasPrefix(ref, domain.SecretRefStore) {
			return domain.TrackerConnection{}, nil, invalid("config.%s must name a value of the secret store: store:<name>", k)
		}
	}
	return e.Access.TrackerConnectionRow(ctx, e.Actor, mcp.CreateTrackerConnectionInput{Name: want.Name, Kind: s.TrackerKind, Config: s.Config, Triggers: s.Triggers})
}

func trackerConnectionValidate(ctx context.Context, e *Env, old *Resource, want Resource) error {
	if _, _, err := trackerConnectionRow(ctx, e, want); err != nil {
		return asInvalid(err)
	}
	if old == nil {
		return nil
	}
	o, _ := decode[trackerConnectionSpec](old.Spec)
	w, _ := decode[trackerConnectionSpec](want.Spec)
	if o.TrackerKind != w.TrackerKind {
		return invalid("a connection keeps its kind: delete %s and make a new one", want.Name)
	}
	return nil
}

func trackerConnectionApply(ctx context.Context, e *Env, old, want *Resource) error {
	repo := e.Store.TrackerConnections()
	if want == nil {
		c, err := repo.GetByName(ctx, e.Actor.AccountID, old.Name)
		if err != nil {
			return missing(err, "tracker connection "+old.Name)
		}
		err = repo.Delete(ctx, e.Actor.AccountID, c.ID)
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("tickets of %s are waiting, queued or at work: finish or dismiss them first: %w", old.Name, mcp.ErrConflict)
		}
		return err
	}
	c, derived, err := trackerConnectionRow(ctx, e, *want)
	if err != nil {
		return err
	}
	if old == nil {
		return repo.Create(ctx, &c)
	}
	stored, err := repo.GetByName(ctx, e.Actor.AccountID, old.Name)
	if err != nil {
		return missing(err, "tracker connection "+old.Name)
	}
	c.ID = stored.ID
	keepDerived(stored.Config, c.Config, derived)
	return repo.Update(ctx, &c)
}

// keepDerived keeps what the adapter read from the tracker while the rest of
// the config stays: the tracker is the same.
func keepDerived(stored, want map[string]any, derived []string) {
	rest := func(m map[string]any) map[string]any {
		out := maps.Clone(m)
		for _, k := range derived {
			delete(out, k)
		}
		return out
	}
	if !reflect.DeepEqual(rest(stored), rest(want)) {
		return
	}
	for _, k := range derived {
		if v, ok := stored[k]; ok {
			want[k] = v
		}
	}
}

func trackerConnectionSecrets(want Resource) []SecretField {
	s, _ := decode[trackerConnectionSpec](want.Spec)
	var out []SecretField
	for _, k := range slices.Sorted(maps.Keys(s.Config)) {
		if ref, ok := s.Config[k].(string); ok && strings.HasSuffix(k, "_ref") {
			out = append(out, SecretField{Ref: ref, Title: strings.TrimSuffix(k, "_ref") + " of tracker connection " + want.Name})
		}
	}
	return out
}

// trackerConnectionSteps asks for a test of a new or changed connection,
// which signs in with its token once the plan put it in place.
func trackerConnectionSteps(_, want *Resource) []domain.ConfigStep {
	if want == nil {
		return nil
	}
	return []domain.ConfigStep{{
		Kind: domain.ConfigStepConnect, Resource: want.Ref(), Title: "Test the connection",
		Detail: "Run forecrew tracker test " + want.Name + " once the plan is applied: it signs in to the tracker with the token and reads what the tracker says of itself.",
	}}
}

// trackerRouteSpec is a route; its name is <connection>/<position>.
type trackerRouteSpec struct {
	Project  string                        `json:"project"` // name
	Match    domain.TrackerMatch           `json:"match"`
	GoalKind domain.GoalKind               `json:"goal_kind,omitempty"`
	Playbook string                        `json:"playbook,omitempty"`
	States   map[domain.TicketState]string `json:"states,omitempty"`
}

var routeNameRE = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]{0,63})/([1-9][0-9]{0,5})$`)

func trackerRouteArea() Area {
	a := base[trackerRouteSpec](KindTrackerRoute, "Tracker route", ScopeOrganization, domain.PermTrackersRead)
	a.Setting = domain.TrackerRoute{}
	a.Hidden = []string{"id", "account_id", "connection_id", "position", "project_id", "warning", "created_at", "updated_at"}
	a.Affects = "Tickets the route matches go to its project from now on; tickets at work stay where they are."
	a.Needs = needsOf(domain.PermTrackersManage)
	a.Read = trackerRouteRead
	a.Validate = trackerRouteValidate
	a.Apply = trackerRouteApply
	return a
}

func routeName(connection string, position int) string {
	return connection + "/" + strconv.Itoa(position)
}

// parseRouteName splits <connection>/<position>.
func parseRouteName(name string) (string, int, error) {
	m := routeNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", 0, invalid("route name %q is not <connection>/<position>, as in jira/1", name)
	}
	n, _ := strconv.Atoi(m[2]) // the pattern holds digits
	return m[1], n, nil
}

func trackerRouteRead(ctx context.Context, e *Env) ([]Resource, error) {
	conns, err := e.Store.TrackerConnections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	for _, c := range conns {
		names[c.ID] = c.Name
	}
	routes, err := e.Store.TrackerRoutes().List(ctx, e.Actor.AccountID, uuid.Nil())
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(routes))
	for _, r := range routes {
		p, err := e.Store.Projects().Get(ctx, e.Actor.AccountID, r.ProjectID)
		if err != nil {
			return nil, err
		}
		s := trackerRouteSpec{Project: p.Name, Match: r.Match, GoalKind: r.GoalKind, Playbook: r.Playbook, States: r.States}
		if len(s.States) == 0 {
			s.States = nil
		}
		out = append(out, Resource{Kind: KindTrackerRoute, Name: routeName(names[r.ConnectionID], r.Position), Spec: encode(s)})
	}
	return out, nil
}

// routeInput is the input of create_tracker_route a spec stands for.
func routeInput(want Resource) (mcp.CreateTrackerRouteInput, error) {
	conn, pos, err := parseRouteName(want.Name)
	if err != nil {
		return mcp.CreateTrackerRouteInput{}, err
	}
	s, err := decode[trackerRouteSpec](want.Spec)
	if err != nil {
		return mcp.CreateTrackerRouteInput{}, err
	}
	return mcp.CreateTrackerRouteInput{Connection: conn, Project: s.Project, Position: pos, Match: s.Match, GoalKind: s.GoalKind, Playbook: s.Playbook, States: s.States}, nil
}

// trackerRouteValidate checks a route as create_tracker_route does. A route
// of a connection the same plan creates is checked against the tracker's
// states only by the person testing the connection: its token is not in
// place yet.
func trackerRouteValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	in, err := routeInput(want)
	if err != nil {
		return err
	}
	if e.Access == nil {
		return invalid("trackers are not available here")
	}
	_, err = e.Store.TrackerConnections().GetByName(ctx, e.Actor.AccountID, in.Connection)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return checkRouteSpec(ctx, e, in)
	case err != nil:
		return err
	}
	_, err = e.Access.TrackerRouteRow(ctx, e.Actor, in, true)
	return asInvalid(err)
}

// asInvalid makes what a wanted resource names and does not exist bad input.
func asInvalid(err error) error {
	if errors.Is(err, mcp.ErrNotFound) {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	return err
}

// checkRouteSpec checks what it can of a route of a connection still to be
// made.
func checkRouteSpec(ctx context.Context, e *Env, in mcp.CreateTrackerRouteInput) error {
	r := domain.TrackerRoute{GoalKind: in.GoalKind, States: in.States}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if _, err := e.Store.Projects().GetByName(ctx, e.Actor.AccountID, in.Project); err != nil {
		return missing(err, "project "+in.Project)
	}
	return nil
}

func trackerRouteApply(ctx context.Context, e *Env, old, want *Resource) error {
	repo := e.Store.TrackerRoutes()
	name := cmpName(old, want)
	conn, pos, err := parseRouteName(name)
	if err != nil {
		return err
	}
	var existing *domain.TrackerRoute
	if old != nil {
		c, err := e.Store.TrackerConnections().GetByName(ctx, e.Actor.AccountID, conn)
		if err != nil {
			return missing(err, "tracker connection "+conn)
		}
		routes, err := repo.List(ctx, e.Actor.AccountID, c.ID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(routes, func(r domain.TrackerRoute) bool { return r.Position == pos })
		if i < 0 {
			return invalid("route %s does not exist", name)
		}
		existing = &routes[i]
	}
	if want == nil {
		return repo.Delete(ctx, e.Actor.AccountID, existing.ID)
	}
	in, err := routeInput(*want)
	if err != nil {
		return err
	}
	r, err := e.Access.TrackerRouteRow(ctx, e.Actor, in, false)
	if err != nil {
		return asInvalid(err)
	}
	if existing == nil {
		err = repo.Create(ctx, &r)
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("route %s exists: %w", name, mcp.ErrConflict)
		}
		return err
	}
	r.ID = existing.ID
	return repo.Update(ctx, &r)
}

func cmpName(old, want *Resource) string {
	if want != nil {
		return want.Name
	}
	return old.Name
}
