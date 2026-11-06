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
	refs := map[uuid.UUID]string{}
	out := make([]mcp.MachineInfo, len(machines))
	for i, m := range machines {
		shape := shapes[m.ShapeID]
		out[i] = mcp.MachineInfo{Machine: m, Name: fleet.MachineName(shape, m), Shape: shape.Name, Provider: providers[m.ProviderID].Name}
		if m.JobID != uuid.Nil() {
			out[i].JobRef, err = memo(refs, m.JobID, func() (string, error) {
				j, err := s.cfg.Store.Jobs().Get(ctx, by.AccountID, m.JobID)
				return j.Ref(), err
			})
			if err != nil {
				return nil, err
			}
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

// ListShapes implements mcp.ToolService.
func (s *Service) ListShapes(ctx context.Context, by domain.Actor) ([]mcp.ShapeInfo, error) {
	shapes, providers, err := s.fleetNames(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ShapeInfo, 0, len(shapes))
	for _, sh := range shapes {
		if sh.Ownership() == domain.OwnershipHosted {
			sh.ProviderConfig = nil
		}
		out = append(out, mcp.ShapeInfo{Shape: sh, Provider: providers[sh.ProviderID].Name, Ownership: sh.Ownership()})
	}
	slices.SortFunc(out, func(a, b mcp.ShapeInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ListProviders implements mcp.ToolService.
func (s *Service) ListProviders(ctx context.Context, by domain.Actor) ([]mcp.ProviderInfo, error) {
	providers, err := s.providers(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]mcp.ProviderInfo, len(providers))
	for i, p := range providers {
		online := p.Online(now)
		if p.Ownership() == domain.OwnershipHosted {
			p.Tags, p.NetworkAllow = nil, nil
			p.Capabilities.Host, p.Capabilities.CloudAccount, p.Capabilities.Checks = "", "", nil
		}
		out[i] = mcp.ProviderInfo{CapacityProvider: p, Online: online, Ownership: p.Ownership()}
	}
	return out, nil
}

// FleetOverview implements mcp.ToolService.
func (s *Service) FleetOverview(ctx context.Context, by domain.Actor) (fleet.Overview, error) {
	if s.cfg.Fleet == nil {
		return fleet.Overview{}, fmt.Errorf("the fleet is not configured: %w", mcp.ErrUnavailable)
	}
	return s.cfg.Fleet.Overview(ctx, by.AccountID)
}
