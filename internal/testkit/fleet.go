package testkit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

// FakeProvider is a fleet.Provider that keeps its machines in memory, for
// tests and e2e with no real machines. Set the exported fields before use,
// or through Configure while it runs.
type FakeProvider struct {
	mu sync.Mutex
	// BootDelay is how long a machine takes before OnBoot is called.
	BootDelay time.Duration
	// OnBoot, when set, is the guest coming up: it gets the request with
	// the bootstrap token, in its own goroutine. Nil guests never call.
	OnBoot func(fleet.CreateRequest)
	// CreateErr fails every Create; ErrCapacity is no room.
	CreateErr error
	// FailCreates fails the next n Creates.
	FailCreates int
	// ListErr fails Capabilities and List, as a provider gone away.
	ListErr error
	// UpdateErr fails every Update.
	UpdateErr error
	// Caps is what Capabilities reports.
	Caps domain.ProviderCapabilities

	instances map[string]fleet.Instance // by external ID
	requests  []fleet.CreateRequest
	updates   map[string]fleet.UpdateRequest
	destroyed []string
	next      int
}

var _ fleet.Provider = (*FakeProvider)(nil)

// NewFakeProvider returns a provider with no machines.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{instances: map[string]fleet.Instance{}, updates: map[string]fleet.UpdateRequest{}}
}

// Configure changes the fake's settings safely while it runs.
func (p *FakeProvider) Configure(fn func(p *FakeProvider)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

// Capabilities implements fleet.Provider.
func (p *FakeProvider) Capabilities(context.Context) (domain.ProviderCapabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Caps, p.ListErr
}

// Create implements fleet.Provider. A machine of the same ID is replaced.
func (p *FakeProvider) Create(_ context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.CreateErr != nil {
		return fleet.Instance{}, p.CreateErr
	}
	if p.FailCreates > 0 {
		p.FailCreates--
		return fleet.Instance{}, errors.New("the fake provider failed to create the machine")
	}
	for id, inst := range p.instances {
		if inst.MachineID == req.MachineID {
			delete(p.instances, id)
		}
	}
	p.next++
	inst := fleet.Instance{ExternalID: fmt.Sprintf("fake-%d", p.next), MachineID: req.MachineID, Running: true, Tags: maps.Clone(req.Tags)}
	p.instances[inst.ExternalID] = inst
	p.requests = append(p.requests, req)
	if p.OnBoot != nil {
		boot, delay := p.OnBoot, p.BootDelay
		go func() {
			time.Sleep(delay)
			boot(req)
		}()
	}
	return inst, nil
}

// Update implements fleet.Provider.
func (p *FakeProvider) Update(_ context.Context, externalID string, req fleet.UpdateRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.UpdateErr != nil {
		return p.UpdateErr
	}
	inst, ok := p.instances[externalID]
	if !ok {
		return fmt.Errorf("instance %s: not found", externalID)
	}
	inst.Tags = maps.Clone(req.Tags)
	p.instances[externalID] = inst
	p.updates[externalID] = req
	return nil
}

// Destroy implements fleet.Provider. Destroying what is gone succeeds.
func (p *FakeProvider) Destroy(_ context.Context, externalID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.instances[externalID]; ok {
		p.destroyed = append(p.destroyed, externalID)
	}
	delete(p.instances, externalID)
	return nil
}

// List implements fleet.Provider.
func (p *FakeProvider) List(context.Context) ([]fleet.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ListErr != nil {
		return nil, p.ListErr
	}
	return p.running(), nil
}

func (p *FakeProvider) running() []fleet.Instance {
	out := slices.Collect(maps.Values(p.instances))
	slices.SortFunc(out, func(a, b fleet.Instance) int { return compareText(a.ExternalID, b.ExternalID) })
	return out
}

func compareText(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Instances returns what the fake holds now.
func (p *FakeProvider) Instances() []fleet.Instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running()
}

// Requests returns every successful Create, oldest first.
func (p *FakeProvider) Requests() []fleet.CreateRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// Updates returns the last Update of each external ID.
func (p *FakeProvider) Updates() map[string]fleet.UpdateRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.updates)
}

// Destroyed returns the external IDs Destroy removed, oldest first.
func (p *FakeProvider) Destroyed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.destroyed)
}

// Plant adds something the controller never asked for, as a crash or a
// leftover would: an orphan unless a machine has its ID.
func (p *FakeProvider) Plant(externalID string, machine uuid.UUID, tags map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instances[externalID] = fleet.Instance{ExternalID: externalID, MachineID: machine, Running: true, Tags: tags}
}

// Vanish drops an instance without the controller asking, as a spot machine
// taken back.
func (p *FakeProvider) Vanish(externalID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.instances, externalID)
}
