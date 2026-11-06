package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tracker"
)

// trackers returns the tracker core, or mcp.ErrUnavailable.
func (s *Service) trackers() (*tracker.Service, error) {
	if s.cfg.Trackers == nil {
		return nil, fmt.Errorf("trackers are not configured: %w", mcp.ErrUnavailable)
	}
	return s.cfg.Trackers, nil
}

// trackerError makes the errors of the tracker core errors of the tools.
func trackerError(err error) error {
	switch {
	case errors.Is(err, tracker.ErrInvalid), errors.Is(err, tracker.ErrUnknownKind):
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	case errors.Is(err, tracker.ErrNotFound):
		return fmt.Errorf("%w: %w", err, mcp.ErrNotFound)
	case errors.Is(err, tracker.ErrNotWaiting):
		return fmt.Errorf("%w: %w", err, mcp.ErrConflict)
	}
	return err
}

// connection finds a tracker connection by name or UUID.
func (s *Service) connection(ctx context.Context, accountID uuid.UUID, ref string) (domain.TrackerConnection, error) {
	if ref == "" {
		return domain.TrackerConnection{}, fmt.Errorf("connection is required: %w", mcp.ErrInvalidInput)
	}
	var c domain.TrackerConnection
	var err error
	if id, perr := uuid.Parse(ref); perr == nil {
		c, err = s.cfg.Store.TrackerConnections().Get(ctx, accountID, id)
	} else {
		c, err = s.cfg.Store.TrackerConnections().GetByName(ctx, accountID, ref)
	}
	return c, notFound(err, "tracker connection "+ref)
}

// optionalConnection resolves a connection filter; an empty one is the zero
// ID.
func (s *Service) optionalConnection(ctx context.Context, accountID uuid.UUID, ref string) (uuid.UUID, error) {
	if ref == "" {
		return uuid.Nil(), nil
	}
	c, err := s.connection(ctx, accountID, ref)
	return c.ID, err
}

// ListTrackerConnections implements mcp.ToolService.
func (s *Service) ListTrackerConnections(ctx context.Context, by domain.Actor) ([]domain.TrackerConnection, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersRead); err != nil {
		return nil, err
	}
	return s.cfg.Store.TrackerConnections().List(ctx, by.AccountID)
}

// CreateTrackerConnection implements mcp.ToolService.
func (s *Service) CreateTrackerConnection(ctx context.Context, by domain.Actor, in mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerConnection{}, err
	}
	c, ad, err := s.trackerConnectionRow(by, in)
	if err != nil {
		return domain.TrackerConnection{}, err
	}
	core, _ := s.trackers() // trackerConnectionRow checked it
	if ad, err = prepare(ctx, core, &c, ad); err != nil {
		return domain.TrackerConnection{}, trackerError(err)
	}
	if err := ad.Verify(ctx); err != nil {
		return domain.TrackerConnection{}, trackerError(err)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TrackerConnections().Create(ctx, &c); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "tracker.connected", "tracker_connection", c.ID, map[string]any{"name": c.Name, "kind": c.Kind})
	})
	return c, conflict(err, "a tracker connection called "+c.Name+" exists")
}

// trackerConnectionRow checks a wanted connection, short of asking the
// tracker: its name, kind and triggers, and a config its adapter takes and
// that holds no secret.
func (s *Service) trackerConnectionRow(by domain.Actor, in mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, tracker.Adapter, error) {
	core, err := s.trackers()
	if err != nil {
		return domain.TrackerConnection{}, nil, err
	}
	c := domain.TrackerConnection{AccountID: by.AccountID, Name: strings.TrimSpace(in.Name), Kind: in.Kind, Config: in.Config, Triggers: in.Triggers}
	if err := c.Validate(); err != nil {
		return domain.TrackerConnection{}, nil, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	for k := range c.Config {
		if domain.SecretLikeName(k) {
			return domain.TrackerConnection{}, nil, fmt.Errorf("config key %q looks like a secret, and the config is not a place for one: %w", k, mcp.ErrInvalidInput)
		}
	}
	ad, err := core.Registry().Build(c)
	if err != nil {
		return domain.TrackerConnection{}, nil, trackerError(err)
	}
	if err := tracker.CheckTriggers(c, ad.Capabilities()); err != nil {
		return domain.TrackerConnection{}, nil, trackerError(err)
	}
	return c, ad, nil
}

// TrackerConnectionRow implements resource.Access: the row of a wanted
// connection, checked as create_tracker_connection checks it short of asking
// the tracker, which a plan does not do. The keys its adapter derives are
// left out of Config and listed.
func (s *Service) TrackerConnectionRow(ctx context.Context, by domain.Actor, in mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, []string, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerConnection{}, nil, err
	}
	c, ad, err := s.trackerConnectionRow(by, in)
	if err != nil {
		return domain.TrackerConnection{}, nil, err
	}
	return c, derivedKeys(ad), nil
}

// TrackerDerived implements resource.Access: the config keys the adapter of
// a stored connection fills in itself.
func (s *Service) TrackerDerived(c domain.TrackerConnection) []string {
	core, err := s.trackers()
	if err != nil {
		return nil
	}
	ad, err := core.Registry().Build(c)
	if err != nil {
		return nil
	}
	return derivedKeys(ad)
}

func derivedKeys(ad tracker.Adapter) []string {
	if p, ok := ad.(tracker.Preparer); ok {
		return p.Derived()
	}
	return nil
}

// prepare stores what a tracker says of itself in the connection's config,
// and returns the adapter of the prepared connection.
func prepare(ctx context.Context, core *tracker.Service, c *domain.TrackerConnection, ad tracker.Adapter) (tracker.Adapter, error) {
	p, ok := ad.(tracker.Preparer)
	if !ok {
		return ad, nil
	}
	cfg, err := p.Prepare(ctx)
	if err != nil {
		return nil, err
	}
	c.Config = cfg
	return core.Registry().Build(*c)
}

// TestTrackerConnection implements mcp.ToolService. A connection stored
// before the tracker could be read, as a plan stores one, is prepared here.
func (s *Service) TestTrackerConnection(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionRefInput) (mcp.TrackerConnectionTest, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return mcp.TrackerConnectionTest{}, err
	}
	core, err := s.trackers()
	if err != nil {
		return mcp.TrackerConnectionTest{}, err
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return mcp.TrackerConnectionTest{}, err
	}
	ad, err := core.Registry().Build(c)
	if err != nil {
		return mcp.TrackerConnectionTest{}, trackerError(err)
	}
	out := mcp.TrackerConnectionTest{Connection: c.Name, OK: true, Checks: []mcp.TrackerCheck{}}
	checks := tracker.Test(ctx, ad)
	routes, err := s.cfg.Store.TrackerRoutes().List(ctx, by.AccountID, c.ID)
	if err != nil {
		return mcp.TrackerConnectionTest{}, err
	}
	checks = append(checks, routeStates(ctx, ad, routes, tracker.Failed(checks) == nil))
	for _, check := range checks {
		if in.WrapUntrusted && check.Detail != "" {
			check.Detail = contextpack.WrapUntrusted("tracker "+c.Name, check.Detail)
		}
		out.Checks = append(out.Checks, mcp.TrackerCheck(check))
		out.OK = out.OK && check.OK
	}
	if !out.OK {
		return out, nil
	}
	before := c.Config
	if _, err := prepare(ctx, core, &c, ad); err != nil {
		return out, trackerError(err)
	}
	if reflect.DeepEqual(before, c.Config) {
		return out, nil
	}
	return out, s.cfg.Store.TrackerConnections().Update(ctx, &c)
}

// routeStates checks the states of a connection's routes against the
// tracker's: a plan cannot, before the connection's token is in place.
func routeStates(ctx context.Context, ad tracker.Adapter, routes []domain.TrackerRoute, ready bool) tracker.Check {
	if !ready {
		return tracker.Check{Name: "routes", Detail: "not checked: an earlier check failed"}
	}
	var errs []error
	for _, r := range routes {
		if err := checkStates(ctx, ad, r.States); err != nil {
			errs = append(errs, fmt.Errorf("route %d: %w", r.Position, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return tracker.Check{Name: "routes", Detail: err.Error()}
	}
	return tracker.Check{Name: "routes", OK: true, Detail: fmt.Sprintf("%d routes map to states the tracker has", len(routes))}
}

// DeleteTrackerConnection implements mcp.ToolService.
func (s *Service) DeleteTrackerConnection(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionRefInput) (domain.TrackerConnection, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerConnection{}, err
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return domain.TrackerConnection{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TrackerConnections().Delete(ctx, by.AccountID, c.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "tracker.disconnected", "tracker_connection", c.ID, map[string]any{"name": c.Name, "kind": c.Kind})
	})
	return c, conflict(err, "tickets of "+c.Name+" are waiting, queued or at work, or writes to it are not sent yet: finish or dismiss the tickets, and let the writes go out or discard those of ended tickets (discard_tracker_writes)")
}

// DiscardTrackerWrites implements mcp.ToolService. Only the writes of links
// that are over go: a ticket at work still needs its status.
func (s *Service) DiscardTrackerWrites(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionRefInput) (mcp.DiscardedWrites, error) {
	out := mcp.DiscardedWrites{Writes: []mcp.DiscardedWrite{}}
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return out, err
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return out, err
	}
	out.Connection = c.Name
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		writes, err := tx.TrackerOutbox().DiscardEnded(ctx, by.AccountID, c.ID)
		if err != nil {
			return err
		}
		if len(writes) == 0 {
			return nil
		}
		dropped := make([]map[string]any, len(writes))
		for i, w := range writes {
			shown := w.LastError
			if in.WrapUntrusted && shown != "" {
				shown = contextpack.WrapUntrusted("tracker error", shown)
			}
			out.Writes = append(out.Writes, mcp.DiscardedWrite{Key: w.Key, Kind: string(w.Kind), Attempts: w.Attempts, LastError: shown})
			dropped[i] = map[string]any{"key": w.Key, "kind": w.Kind, "attempts": w.Attempts, "last_error": w.LastError}
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "tracker.writes_discarded", "tracker_connection", c.ID, map[string]any{"name": c.Name, "writes": dropped})
	})
	return out, err
}

func (s *Service) routeInfo(ctx context.Context, r domain.TrackerRoute) (mcp.TrackerRouteInfo, error) {
	c, err := s.cfg.Store.TrackerConnections().Get(ctx, r.AccountID, r.ConnectionID)
	if err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	p, err := s.cfg.Store.Projects().Get(ctx, r.AccountID, r.ProjectID)
	if err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	return mcp.TrackerRouteInfo{TrackerRoute: r, Connection: c.Name, Project: p.Name}, nil
}

// ListTrackerRoutes implements mcp.ToolService.
func (s *Service) ListTrackerRoutes(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionFilterInput) ([]mcp.TrackerRouteInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersRead); err != nil {
		return nil, err
	}
	id, err := s.optionalConnection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return nil, err
	}
	routes, err := s.cfg.Store.TrackerRoutes().List(ctx, by.AccountID, id)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.TrackerRouteInfo, len(routes))
	for i, r := range routes {
		if out[i], err = s.routeInfo(ctx, r); err != nil {
			return nil, err
		}
		if in.WrapUntrusted && r.Warning != "" {
			out[i].Warning = contextpack.WrapUntrusted("tracker "+out[i].Connection, r.Warning)
		}
	}
	return out, nil
}

// checkStates refuses a mapping to a state the tracker does not have, and a
// closed state for work that goes on: Forecrew's own write would read as a
// person closing the ticket, and cancel the goal.
func checkStates(ctx context.Context, ad tracker.Adapter, states map[domain.TicketState]string) error {
	if len(states) == 0 {
		return nil
	}
	known, err := ad.States(ctx)
	if err != nil {
		return fmt.Errorf("read the states of the tracker: %w", err)
	}
	for logical, state := range states {
		i := slices.IndexFunc(known, func(k tracker.State) bool { return k.Name == state })
		if i < 0 {
			return fmt.Errorf("%s maps to %q, which the tracker does not have: %w", logical, state, mcp.ErrInvalidInput)
		}
		if known[i].Closed && logical != domain.TicketDone && logical != domain.TicketCanceled {
			return fmt.Errorf("%s maps to %q, but only done and canceled may close a ticket: %w", logical, state, mcp.ErrInvalidInput)
		}
	}
	return nil
}

// CreateTrackerRoute implements mcp.ToolService.
func (s *Service) CreateTrackerRoute(ctx context.Context, by domain.Actor, in mcp.CreateTrackerRouteInput) (mcp.TrackerRouteInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	info, err := s.trackerRouteRow(ctx, by, in, true)
	if err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TrackerRoutes().Create(ctx, &info.TrackerRoute); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, info.ProjectID, "tracker.route_created", "tracker_route", info.ID, map[string]any{"connection": info.Connection})
	})
	if err != nil {
		return mcp.TrackerRouteInfo{}, conflict(err, fmt.Sprintf("route %d of %s exists", in.Position, info.Connection))
	}
	return info, nil
}

// trackerRouteRow checks a wanted route. ask checks its states against the
// tracker's.
func (s *Service) trackerRouteRow(ctx context.Context, by domain.Actor, in mcp.CreateTrackerRouteInput, ask bool) (mcp.TrackerRouteInfo, error) {
	core, err := s.trackers()
	if err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return mcp.TrackerRouteInfo{}, err
	}
	r := domain.TrackerRoute{
		AccountID: by.AccountID, ConnectionID: c.ID, ProjectID: p.ID, Position: in.Position, Match: in.Match,
		GoalKind: in.GoalKind, Playbook: in.Playbook, States: in.States,
	}
	if err := r.Validate(); err != nil {
		return mcp.TrackerRouteInfo{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if in.Position < 0 {
		return mcp.TrackerRouteInfo{}, fmt.Errorf("position cannot be negative: %w", mcp.ErrInvalidInput)
	}
	if in.Playbook != "" && in.Playbook != PlaybookGoal && in.Playbook != PlaybookGoalTesting && in.Playbook != PlaybookBugfix {
		return mcp.TrackerRouteInfo{}, fmt.Errorf("playbook %q does not run goals: %w", in.Playbook, mcp.ErrInvalidInput)
	}
	if in.Playbook != "" && in.Playbook != PlaybookGoal && !slices.Contains(s.cfg.TestingProjects, p.Name) {
		return mcp.TrackerRouteInfo{}, fmt.Errorf("playbook %s needs the testing system: add %s to testing.projects: %w", in.Playbook, p.Name, mcp.ErrInvalidInput)
	}
	ad, err := core.Registry().Build(c)
	if err != nil {
		return mcp.TrackerRouteInfo{}, trackerError(err)
	}
	if len(in.States) > 0 && !ad.Capabilities().Transitions {
		return mcp.TrackerRouteInfo{}, fmt.Errorf("%s tickets take no state from Forecrew: %w", ad.Kind(), mcp.ErrInvalidInput)
	}
	if ask {
		if err := checkStates(ctx, ad, in.States); err != nil {
			return mcp.TrackerRouteInfo{}, err
		}
	}
	return mcp.TrackerRouteInfo{TrackerRoute: r, Connection: c.Name, Project: p.Name}, nil
}

// TrackerRouteRow implements resource.Access: the row of a wanted route,
// checked as create_tracker_route checks it. ask checks its states against
// the tracker, which a plan does only for a connection that exists already.
func (s *Service) TrackerRouteRow(ctx context.Context, by domain.Actor, in mcp.CreateTrackerRouteInput, ask bool) (domain.TrackerRoute, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerRoute{}, err
	}
	info, err := s.trackerRouteRow(ctx, by, in, ask)
	return info.TrackerRoute, err
}

// DeleteTrackerRoute implements mcp.ToolService.
func (s *Service) DeleteTrackerRoute(ctx context.Context, by domain.Actor, in mcp.TrackerRouteRefInput) (domain.TrackerRoute, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerRoute{}, err
	}
	id, err := uuid.Parse(in.Route)
	if err != nil {
		return domain.TrackerRoute{}, fmt.Errorf("route %q is not a UUID: %w", in.Route, mcp.ErrInvalidInput)
	}
	r, err := s.cfg.Store.TrackerRoutes().Get(ctx, by.AccountID, id)
	if err != nil {
		return domain.TrackerRoute{}, notFound(err, "route "+in.Route)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TrackerRoutes().Delete(ctx, by.AccountID, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, r.ProjectID, "tracker.route_deleted", "tracker_route", r.ID, nil)
	})
	return r, notFound(err, "route "+in.Route)
}

// ListTrackerIdentities implements mcp.ToolService.
func (s *Service) ListTrackerIdentities(ctx context.Context, by domain.Actor, in mcp.TrackerConnectionFilterInput) ([]domain.TrackerIdentity, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersRead); err != nil {
		return nil, err
	}
	id, err := s.optionalConnection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.TrackerIdentities().List(ctx, by.AccountID, id)
}

// MapTrackerIdentity implements mcp.ToolService. Whoever is the external user
// acts as the user from then on, so the caller needs everything the user holds
// (No escalation).
func (s *Service) MapTrackerIdentity(ctx context.Context, by domain.Actor, in mcp.MapTrackerIdentityInput) (domain.TrackerIdentity, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerIdentity{}, err
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return domain.TrackerIdentity{}, err
	}
	if strings.TrimSpace(in.ExternalID) == "" {
		return domain.TrackerIdentity{}, fmt.Errorf("external_id is required: %w", mcp.ErrInvalidInput)
	}
	user, err := parseUser(in.User)
	if err != nil {
		return domain.TrackerIdentity{}, err
	}
	if _, err := s.cfg.Store.Memberships().Get(ctx, by.AccountID, user); err != nil {
		return domain.TrackerIdentity{}, notFound(err, "member "+in.User)
	}
	if err := s.authz.CoversHolder(ctx, by, domain.SubjectUser, user); err != nil {
		return domain.TrackerIdentity{}, err
	}
	i := domain.TrackerIdentity{AccountID: by.AccountID, ConnectionID: c.ID, ExternalID: in.ExternalID, ExternalName: in.ExternalName, UserID: user, CreatedBy: by.String()}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TrackerIdentities().Create(ctx, &i); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "tracker.identity_mapped", "tracker_identity", i.ID, map[string]any{"connection": c.Name, "external_id": i.ExternalID, "user": user})
	})
	return i, conflict(err, in.ExternalID+" is mapped already")
}

// UnmapTrackerIdentity implements mcp.ToolService.
func (s *Service) UnmapTrackerIdentity(ctx context.Context, by domain.Actor, in mcp.TrackerIdentityRefInput) (domain.TrackerIdentity, error) {
	if err := s.allowOrg(ctx, by, domain.PermTrackersManage); err != nil {
		return domain.TrackerIdentity{}, err
	}
	id, err := uuid.Parse(in.Identity)
	if err != nil {
		return domain.TrackerIdentity{}, fmt.Errorf("identity %q is not a UUID: %w", in.Identity, mcp.ErrInvalidInput)
	}
	i, err := s.cfg.Store.TrackerIdentities().Get(ctx, by.AccountID, id)
	if err != nil {
		return domain.TrackerIdentity{}, notFound(err, "identity "+in.Identity)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TrackerIdentities().Delete(ctx, by.AccountID, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "tracker.identity_unmapped", "tracker_identity", i.ID, map[string]any{"external_id": i.ExternalID})
	})
	return i, notFound(err, "identity "+in.Identity)
}

// ticketInfo adds the refs of a ticket.
func (s *Service) ticketInfo(ctx context.Context, l domain.TicketLink) (mcp.TicketInfo, error) {
	st := s.cfg.Store
	info := mcp.TicketInfo{TicketLink: l}
	if l.ConnectionID != uuid.Nil() { // zero: the connection was deleted
		c, err := st.TrackerConnections().Get(ctx, l.AccountID, l.ConnectionID)
		if err != nil {
			return mcp.TicketInfo{}, err
		}
		info.Connection = c.Name
	}
	if l.ProjectID != uuid.Nil() {
		p, err := st.Projects().Get(ctx, l.AccountID, l.ProjectID)
		if err != nil {
			return mcp.TicketInfo{}, err
		}
		info.Project = p.Name
	}
	if l.GateID != uuid.Nil() {
		g, err := st.Gates().Get(ctx, l.AccountID, l.GateID)
		if err != nil {
			return mcp.TicketInfo{}, err
		}
		info.GateRef = g.Ref()
	}
	if l.GoalID != uuid.Nil() {
		g, err := st.Goals().Get(ctx, l.AccountID, l.GoalID)
		if err != nil {
			return mcp.TicketInfo{}, err
		}
		info.GoalRef = g.Ref()
	}
	return info, nil
}

// goalTicket returns the ticket of a goal; nil for a goal without one.
func (s *Service) goalTicket(ctx context.Context, accountID uuid.UUID, g domain.Goal) (*mcp.TicketInfo, error) {
	l, err := s.cfg.Store.TicketLinks().GetByGoal(ctx, accountID, g.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ticket of %s: %w", g.Ref(), err)
	}
	info, err := s.ticketInfo(ctx, l)
	if err != nil {
		return nil, fmt.Errorf("ticket of %s: %w", g.Ref(), err)
	}
	return &info, nil
}

// markTicketText marks the text of a goal from a ticket as data.
func (s *Service) markTicketText(ctx context.Context, g *domain.Goal) error {
	l, err := s.cfg.Store.TicketLinks().GetByGoal(ctx, g.AccountID, g.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ticket of %s: %w", g.Ref(), err)
	}
	g.Title = contextpack.WrapUntrusted("ticket "+l.Key, g.Title)
	g.Body = contextpack.WrapUntrusted("ticket "+l.Key, g.Body)
	return nil
}

// ListTicketsWaiting implements mcp.ToolService. A ticket without a project
// shows only to who holds trackers.read on the organization.
func (s *Service) ListTicketsWaiting(ctx context.Context, by domain.Actor, in mcp.ListTicketsWaitingInput) ([]mcp.TicketInfo, error) {
	project, err := s.optionalProject(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	visible, err := s.scope(ctx, by, domain.PermTrackersRead, project)
	if err != nil {
		return nil, err
	}
	links, err := s.cfg.Store.TicketLinks().List(ctx, by.AccountID, store.TicketLinkFilter{ProjectID: project, Status: domain.LinkWaiting})
	if err != nil {
		return nil, err
	}
	out := make([]mcp.TicketInfo, 0, len(links))
	for _, l := range links {
		if (l.ProjectID == uuid.Nil() && !visible.All) || (l.ProjectID != uuid.Nil() && !visible.Has(l.ProjectID)) {
			continue
		}
		info, err := s.ticketInfo(ctx, l)
		if err != nil {
			return nil, err
		}
		if in.WrapUntrusted {
			info.Title = contextpack.WrapUntrusted("ticket "+l.Key, l.Title)
		}
		out = append(out, info)
	}
	return out, nil
}

// waitingTicket finds a ticket by its UUID.
func (s *Service) waitingTicket(ctx context.Context, accountID uuid.UUID, ref string) (domain.TicketLink, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return domain.TicketLink{}, fmt.Errorf("ticket %q is not a UUID: %w", ref, mcp.ErrInvalidInput)
	}
	l, err := s.cfg.Store.TicketLinks().Get(ctx, accountID, id)
	return l, notFound(err, "ticket "+ref)
}

// allowIntakeGate checks by's right to decide the pending gate of a ticket, if
// it has one: only people resolve gates.
func (s *Service) allowIntakeGate(ctx context.Context, by domain.Actor, l domain.TicketLink) error {
	if l.GateID == uuid.Nil() {
		return nil
	}
	g, err := s.cfg.Store.Gates().Get(ctx, l.AccountID, l.GateID)
	if err != nil {
		return err
	}
	if g.Status != domain.GatePending {
		return nil
	}
	return s.authz.AuthorizeGate(ctx, by, g)
}

// AcceptTicket implements mcp.ToolService.
func (s *Service) AcceptTicket(ctx context.Context, by domain.Actor, in mcp.AcceptTicketInput) (mcp.TicketInfo, error) {
	core, err := s.trackers()
	if err != nil {
		return mcp.TicketInfo{}, err
	}
	l, err := s.waitingTicket(ctx, by.AccountID, in.Ticket)
	if err != nil {
		return mcp.TicketInfo{}, err
	}
	project := l.ProjectID
	if in.Project != "" {
		p, err := s.project(ctx, by.AccountID, in.Project)
		if err != nil {
			return mcp.TicketInfo{}, err
		}
		if project != uuid.Nil() && project != p.ID {
			return mcp.TicketInfo{}, fmt.Errorf("the route of ticket %s goes to another project: %w", l.Key, mcp.ErrInvalidInput)
		}
		project = p.ID
	}
	if project == uuid.Nil() {
		return mcp.TicketInfo{}, fmt.Errorf("no route gave ticket %s a project: name one: %w", l.Key, mcp.ErrInvalidInput)
	}
	if err := s.allow(ctx, by, domain.PermGoalCreate, project); err != nil {
		return mcp.TicketInfo{}, err
	}
	p, err := s.cfg.Store.Projects().Get(ctx, by.AccountID, project)
	if err != nil {
		return mcp.TicketInfo{}, err
	}
	if p.Status != domain.ProjectOnboarded {
		return mcp.TicketInfo{}, fmt.Errorf("project %s is %s, onboard it first: %w", p.Name, p.Status, mcp.ErrInvalidInput)
	}
	if err := s.allowIntakeGate(ctx, by, l); err != nil {
		return mcp.TicketInfo{}, err
	}
	got, err := core.Accept(ctx, by, l.ID, project)
	if err != nil {
		return mcp.TicketInfo{}, trackerError(err)
	}
	if err := s.audit(ctx, s.cfg.Store, by, project, "ticket.accepted", "ticket", got.ID, map[string]any{"key": got.Key}); err != nil {
		return mcp.TicketInfo{}, err
	}
	return s.ticketInfo(ctx, got)
}

// DismissTicket implements mcp.ToolService.
func (s *Service) DismissTicket(ctx context.Context, by domain.Actor, in mcp.DismissTicketInput) (mcp.TicketInfo, error) {
	core, err := s.trackers()
	if err != nil {
		return mcp.TicketInfo{}, err
	}
	l, err := s.waitingTicket(ctx, by.AccountID, in.Ticket)
	if err != nil {
		return mcp.TicketInfo{}, err
	}
	if l.ProjectID == uuid.Nil() {
		err = s.allowOrg(ctx, by, domain.PermGoalCreate)
	} else {
		err = s.allow(ctx, by, domain.PermGoalCreate, l.ProjectID)
	}
	if err != nil {
		return mcp.TicketInfo{}, err
	}
	if err := s.allowIntakeGate(ctx, by, l); err != nil {
		return mcp.TicketInfo{}, err
	}
	got, err := core.Dismiss(ctx, by, l.ID, in.Reason)
	if err != nil {
		return mcp.TicketInfo{}, trackerError(err)
	}
	return s.ticketInfo(ctx, got)
}
