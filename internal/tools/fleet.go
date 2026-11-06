package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// List sizes of list_machines.
const (
	DefaultMachineLimit = 100
	MaxMachineLimit     = 500
)

// ListMachines implements mcp.ToolService.
func (s *Service) ListMachines(ctx context.Context, by domain.Actor, in mcp.ListMachinesInput) ([]mcp.MachineInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	visible, err := s.scope(ctx, by, domain.PermJobRead, uuid.Nil())
	if err != nil {
		return nil, err
	}
	state := domain.MachineState(in.State)
	if in.Limit < 0 || (state != "" && !slices.Contains(domain.MachineStates(), state)) {
		return nil, fmt.Errorf("state %q or limit %d: %w", in.State, in.Limit, mcp.ErrInvalidInput)
	}
	if in.Limit == 0 {
		in.Limit = DefaultMachineLimit
	}
	machines, err := s.cfg.Store.Machines().List(ctx, store.MachineFilter{
		AccountID: by.AccountID, State: state, Active: state == "", Limit: min(in.Limit, MaxMachineLimit),
	})
	if err != nil {
		return nil, err
	}
	shapes, providers, err := s.fleetNames(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.MachineInfo, len(machines))
	for i, m := range machines {
		shape := shapes[m.ShapeID]
		if shape.Ownership() == domain.OwnershipHosted {
			m.ExternalID = "" // the instance's own business
		}
		out[i] = mcp.MachineInfo{Machine: m, Name: fleet.MachineName(shape, m), Shape: shape.Name, Provider: providers[m.ProviderID].Name}
		if m.JobID == uuid.Nil() {
			continue
		}
		j, err := s.cfg.Store.Jobs().Get(ctx, by.AccountID, m.JobID)
		if err != nil {
			return nil, err
		}
		if visible.Has(j.ProjectID) {
			out[i].JobRef = j.Ref()
		} else {
			out[i].JobID = uuid.Nil() // a job of a project the caller cannot read
		}
	}
	return out, nil
}

// fleetNames returns what the organization can see of shapes and providers,
// by ID.
func (s *Service) fleetNames(ctx context.Context, accountID uuid.UUID) (map[uuid.UUID]domain.Shape, map[uuid.UUID]domain.CapacityProvider, error) {
	shapes, err := s.cfg.Store.Shapes().ListAvailable(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	byShape := make(map[uuid.UUID]domain.Shape, len(shapes))
	for _, sh := range shapes {
		byShape[sh.ID] = sh
	}
	providers, err := s.providers(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	byProvider := make(map[uuid.UUID]domain.CapacityProvider, len(providers))
	for _, p := range providers {
		byProvider[p.ID] = p
	}
	return byShape, byProvider, nil
}

// providers returns the organization's providers and the instance-wide ones.
func (s *Service) providers(ctx context.Context, accountID uuid.UUID) ([]domain.CapacityProvider, error) {
	own, err := s.cfg.Store.CapacityProviders().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	hosted, err := s.cfg.Store.CapacityProviders().List(ctx, uuid.Nil())
	if err != nil {
		return nil, err
	}
	return append(own, hosted...), nil
}

// ListShapes implements mcp.ToolService. Instance-wide shapes show as far as
// the organization's policy lets it use them, without the instance's settings.
func (s *Service) ListShapes(ctx context.Context, by domain.Actor) ([]mcp.ShapeInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	shapes, providers, err := s.fleetNames(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	policy, err := s.cfg.Store.FleetPolicies().Get(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ShapeInfo, 0, len(shapes))
	for _, sh := range shapes {
		if sh.Ownership() == domain.OwnershipHosted {
			if !policy.Allows(sh) {
				continue
			}
			sh.ProviderConfig, sh.MonthlyBudget = nil, nil
		}
		out = append(out, mcp.ShapeInfo{Shape: sh, Provider: providers[sh.ProviderID].Name, Ownership: sh.Ownership()})
	}
	slices.SortFunc(out, func(a, b mcp.ShapeInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ListProviders implements mcp.ToolService. An instance-wide provider shows
// when the organization's policy lets it use one of its shapes, with what
// its jobs need to know and nothing of how the instance is built.
func (s *Service) ListProviders(ctx context.Context, by domain.Actor) ([]mcp.ProviderInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	shapes, err := s.ListShapes(ctx, by)
	if err != nil {
		return nil, err
	}
	allowed := map[uuid.UUID]bool{}
	for _, sh := range shapes {
		allowed[sh.ProviderID] = true
	}
	providers, err := s.providers(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]mcp.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		online := p.Online(now)
		if p.Ownership() == domain.OwnershipHosted {
			if !allowed[p.ID] {
				continue
			}
			p = hostedView(p)
		}
		out = append(out, mcp.ProviderInfo{CapacityProvider: p, Online: online, Ownership: p.Ownership()})
	}
	return out, nil
}

// hostedView leaves out of an instance-wide provider what says how the
// instance is built or what it may spend.
func hostedView(p domain.CapacityProvider) domain.CapacityProvider {
	c := &p.Capabilities
	c.Host, c.CloudAccount, c.Region, c.AgentVersion, c.Checks = "", "", "", "", nil
	p.Tags, p.NetworkAllow, p.Limits = nil, nil, domain.ProviderLimits{}
	return p
}

// FleetOverview implements mcp.ToolService.
func (s *Service) FleetOverview(ctx context.Context, by domain.Actor) (fleet.Overview, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return fleet.Overview{}, err
	}
	if s.cfg.Fleet == nil {
		return fleet.Overview{}, fmt.Errorf("the fleet is not configured: %w", mcp.ErrUnavailable)
	}
	visible, err := s.scope(ctx, by, domain.PermJobRead, uuid.Nil())
	if err != nil {
		return fleet.Overview{}, err
	}
	return s.cfg.Fleet.Overview(ctx, by.AccountID, visible.Has)
}
