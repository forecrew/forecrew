// Package fleet creates and destroys machines from queue demand
// (docs/contracts/fleet.md). Provider is what every place machines come from
// implements, in the fleet agent or, in direct mode, in forecrewd.
package fleet

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Tags every provider puts on everything it creates, where it has tags; a
// provider without them keeps the same facts its own way.
const (
	TagMachine = "forecrew:machine" // the machine ID
	TagOrg     = "forecrew:org"     // the organization's slug, when assigned
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
	// Bootstrap is nil in a desired machine already started.
	Bootstrap *Bootstrap        `json:"bootstrap,omitempty"`
	Tags      map[string]string `json:"tags"`
	// NetworkAllow lists hosts or CIDRs the machine may reach beyond the
	// default.
	NetworkAllow []string `json:"network_allow"`
	// Deadline is when the provider kills it, whatever it is doing; zero
	// until it has a job.
	Deadline time.Time `json:"deadline,omitzero"`
}

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
	// Destroy removes the machine with everything it owns, or resets it
	// where the provider reuses a VM. Destroying what does not exist
	// succeeds.
	Destroy(ctx context.Context, externalID string) error
	// List returns everything of Forecrew's the provider holds, running or
	// not, and nothing else.
	List(ctx context.Context) ([]Instance, error)
}
