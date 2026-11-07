// Package fleet creates and destroys machines from queue demand
// (docs/contracts/fleet.md). Provider is what every place machines come from
// implements, in the fleet agent or, in direct mode, in forecrewd.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Tags every provider puts on everything it creates, where it has tags; a
// provider without them keeps the same facts its own way.
const (
	TagMachine = "forecrew:machine" // the machine ID
	TagOrg     = "forecrew:org"     // the organization's ID, when assigned; a slug can change
	TagJob     = "forecrew:job"     // the job's ref, when known
)

// ErrCapacity means the provider has no room now; the controller tries
// again or another shape, and the machine is not failed.
var ErrCapacity = errors.New("no capacity")

// Bootstrap is how the worker on a machine reaches forecrewd. It reaches
// the guest by the provider's own means, never through a file on the host or
// a log. Token is a secret.
type Bootstrap struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token"`
}

// String hides the token.
func (b Bootstrap) String() string { return "bootstrap(" + b.ServerURL + ")" }

// LogValue hides the token.
func (b Bootstrap) LogValue() slog.Value { return slog.StringValue(b.String()) }

// CreateRequest is a machine to start. It is also what the fleet agent API
// sends as a desired machine (fleet-agent-api.md).
type CreateRequest struct {
	MachineID uuid.UUID    `json:"machine_id"`
	Shape     domain.Shape `json:"shape"`
	Image     domain.Image `json:"image"`
	// Bootstrap is nil in a desired machine the agent already has.
	Bootstrap *Bootstrap        `json:"bootstrap,omitempty"`
	Tags      map[string]string `json:"tags"`
	// NetworkAllow lists hosts or CIDRs the machine may reach beyond the
	// default.
	NetworkAllow []string `json:"network_allow"`
	// Deadline is when the provider kills it, whatever it is doing; zero
	// until it has a job.
	Deadline time.Time `json:"deadline,omitzero"`
}

// UpdateRequest is what may change on a running machine, each field whole.
type UpdateRequest struct {
	Tags         map[string]string `json:"tags"`
	NetworkAllow []string          `json:"network_allow"`
	Deadline     time.Time         `json:"deadline,omitzero"` // zero: none
}

// Key is a value to compare two updates by.
func (u UpdateRequest) Key() string { return appliedKey(u) }

// Instance is a machine as a provider sees it.
type Instance struct {
	ExternalID string `json:"external_id"`
	// MachineID comes from the provider's tag; nil on something tagged as
	// Forecrew's without one.
	MachineID uuid.UUID         `json:"machine_id,omitzero"`
	Running   bool              `json:"running"`
	Tags      map[string]string `json:"tags,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitzero"`
}

// Provider starts and destroys machines in one place. Every method is safe
// for concurrent use and honours ctx. Rules in docs/contracts/fleet.md.
type Provider interface {
	// Capabilities reports what the provider can do now.
	Capabilities(ctx context.Context) (domain.ProviderCapabilities, error)
	// Create starts a machine and returns once it is booting. It is
	// idempotent for MachineID: a machine with that ID that exists already
	// is destroyed and started again, so it boots with the token given.
	// No room is ErrCapacity.
	Create(ctx context.Context, req CreateRequest) (Instance, error)
	// Update applies new tags, network allowlist and deadline to a running
	// machine without restarting it: a warm machine gets its organization's
	// tags and network when assigned, and any machine a deadline at lease.
	// Applying the same values again succeeds.
	Update(ctx context.Context, externalID string, req UpdateRequest) error
	// Destroy removes the machine with everything it owns, or resets it
	// where the provider reuses a VM. Destroying what does not exist
	// succeeds.
	Destroy(ctx context.Context, externalID string) error
	// List returns everything of Forecrew's the provider holds, running or
	// not, and nothing else.
	List(ctx context.Context) ([]Instance, error)
}

// OwnClouds are the kinds of location that put machines in the organization's
// own cloud account.
var OwnClouds = []domain.ProviderKind{domain.ProviderAWS}

// RequirePaidPlan is the gate of an own cloud on a hosted instance (config
// fleet.hosted): the organization's fleet policy must say paid (fleet.md). The
// plan is a stub until billing exists, and only instance admins set it. Every way
// of adding a location goes through it.
func RequirePaidPlan(ctx context.Context, st store.Store, hosted bool, p domain.CapacityProvider) error {
	if !hosted || p.AccountID == uuid.Nil() || !slices.Contains(OwnClouds, p.Kind) {
		return nil
	}
	policy, err := st.FleetPolicies().Get(ctx, p.AccountID)
	if err != nil {
		return err
	}
	if policy.Plan != domain.PlanPaid {
		return fmt.Errorf("a location in your own cloud needs the paid plan: %w", authz.ErrDenied)
	}
	return nil
}
