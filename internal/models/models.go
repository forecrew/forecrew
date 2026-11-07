// Package models decides what a job of a role runs on: the connection, the
// tier and the model (docs/contracts/config-resources.md, Model connections).
// A project's overrides come first, then the organization, then the server
// file. Where roles.Resolver and the worker API need that answer they ask
// here, so that a job's key, endpoint and model always come from one decision
// on the job's own organization.
package models

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
)

// ReasonNoConnection is the reason_code of a job that waits for a connection.
const ReasonNoConnection = "no_connection"

// Where a decision comes from.
const (
	SourceServer       = "server"       // a runtime of the server file, with its key
	SourceOrganization = "organization" // a connection of the job's organization
)

// settingsHint is where people fix a connection.
const settingsHint = "in Organization settings, AI models"

// Request is what a decision is about.
type Request struct {
	ProjectID uuid.UUID
	Role      string
	// Pool is the job's pool; empty before the job has one.
	Pool string
	// Adapter is the job's runtime; empty before it has one.
	Adapter string
	// Pinned is the adapter of a council seat.
	Pinned string
}

// Decision is what a job runs on, or why it cannot run.
type Decision struct {
	Source     string
	Connection string // a connection, or a key of runtimes for SourceServer
	Vendor     string
	Adapter    string
	Tier       config.Tier
	// Model is empty for a role the server file does not know, on a job that
	// keeps what it was created with.
	Model string
	// Waiting says, for people, why no connection serves the job and what to
	// do. The rest of a decision that waits is empty.
	Waiting string
	// Unset means nothing at all is set up for the role, on an organization
	// that has no model settings of its own. A role that was never configured
	// is refused when a job is created, as it always was.
	Unset bool

	connection domain.ModelConnection
}

// State is an organization's model settings.
type State struct {
	Connections map[string]domain.ModelConnection
	Org         domain.ModelAssignment
	Projects    map[uuid.UUID]domain.ModelAssignment
}

// NewState builds a State from the rows of a store.
func NewState(connections []domain.ModelConnection, assignments []domain.ModelAssignment) State {
	s := State{Connections: map[string]domain.ModelConnection{}, Projects: map[uuid.UUID]domain.ModelAssignment{}}
	for _, c := range connections {
		s.Connections[c.Name] = c
	}
	for _, a := range assignments {
		if a.ProjectID == uuid.Nil() {
			s.Org = a
		} else {
			s.Projects[a.ProjectID] = a
		}
	}
	return s
}

func (s State) empty() bool {
	return len(s.Connections) == 0 && len(s.Projects) == 0 && len(s.Org.Roles) == 0 && len(s.Org.Tiers) == 0
}

// project returns the overrides that count for id.
func (s State) project(id uuid.UUID) domain.ModelAssignment {
	if !s.Org.OverridesAllowed() {
		return domain.ModelAssignment{}
	}
	return s.Projects[id]
}

// assigned returns the role's assignment in the project, or else the
// organization's. override says it is the project's.
func (s State) assigned(r Request) (role domain.ModelRole, override, ok bool) {
	if role, ok = s.project(r.ProjectID).Roles[r.Role]; ok {
		return role, true, true
	}
	role, ok = s.Org.Roles[r.Role]
	return role, false, ok
}

// model returns the model of a tier and vendor.
func (s State) model(cfg *config.Config, r Request, tier config.Tier, vendor string) string {
	t := string(tier)
	return cmp.Or(s.project(r.ProjectID).Tiers[t][vendor], s.Org.Tiers[t][vendor], cfg.ModelTiers[tier][vendor])
}

func waiting(format string, args ...any) Decision {
	return Decision{Waiting: fmt.Sprintf(format, args...)}
}

// Decide is the one decision, on what the store holds. It does not look at
// keys: Service.Decide does. An override or an assignment that cannot be used
// never falls back to the next level: a project pinned to a local model must
// not reach a cloud provider because its connection was turned off.
func (s State) Decide(cfg *config.Config, r Request) Decision {
	role, override, ok := s.assigned(r)
	if !ok {
		return s.fromFile(cfg, r)
	}
	tier := config.Tier(role.Tier)
	c, ok := s.Connections[role.Connection]
	if !ok {
		if _, file := cfg.Runtimes[role.Connection]; file && cfg.ServerKeys() {
			return s.onRuntime(cfg, r, role.Connection, tier)
		}
		return waiting("Role %s uses connection %s, which does not exist: connect it or assign another %s", r.Role, role.Connection, settingsHint)
	}
	if !c.Enabled {
		return waiting("Role %s uses connection %s, which is turned off: turn it on or assign another %s", r.Role, c.Name, settingsHint)
	}
	if r.Pinned != "" && r.Pinned != c.Adapter {
		if override {
			return waiting("Role %s has a council seat on %s, but its project keeps the role on connection %s: change the project's override %s", r.Role, r.Pinned, c.Name, settingsHint)
		}
		return s.seat(cfg, r, tier)
	}
	return s.onConnection(cfg, r, c, tier)
}

// onConnection is the decision for connection c.
func (s State) onConnection(cfg *config.Config, r Request, c domain.ModelConnection, tier config.Tier) Decision {
	model := s.model(cfg, r, tier, c.Vendor)
	if model == "" {
		return waiting("Role %s has no %s model for vendor %s: set one %s", r.Role, tier, c.Vendor, settingsHint)
	}
	if _, _, split := domain.SplitProviderModel(model); c.Adapter == config.AdapterOpencode && !split {
		return waiting("Role %s uses model %s on opencode, which needs the form provider/model: correct it %s", r.Role, model, settingsHint)
	}
	if p, ok := cfg.Pools[r.Pool]; ok && !p.Credentials.AllowsRuntime(c.Name) {
		return waiting("Role %s runs in the %s zone, which may not receive the key of connection %s: add it to pools.%s.credentials.runtimes or assign another connection", r.Role, r.Pool, c.Name, r.Pool)
	}
	return Decision{Source: SourceOrganization, Connection: c.Name, Vendor: c.Vendor, Adapter: c.Adapter, Tier: tier, Model: model, connection: c}
}

// onRuntime is the decision for a runtime of the server file.
func (s State) onRuntime(cfg *config.Config, r Request, name string, tier config.Tier) Decision {
	rt := cfg.Runtimes[name]
	return Decision{Source: SourceServer, Connection: name, Vendor: rt.Vendor, Adapter: rt.Adapter, Tier: tier, Model: s.model(cfg, r, tier, rt.Vendor)}
}

// seat serves a council seat on another adapter than the role's: the first
// connection of that adapter, and only then the server's.
func (s State) seat(cfg *config.Config, r Request, tier config.Tier) Decision {
	for _, name := range slices.Sorted(maps.Keys(s.Connections)) {
		c := s.Connections[name]
		if c.Enabled && c.Adapter == r.Pinned && s.model(cfg, r, tier, c.Vendor) != "" {
			return s.onConnection(cfg, r, c, tier)
		}
	}
	if cfg.ServerKeys() {
		if name := s.fileRuntime(cfg, r, tier); name != "" {
			return s.onRuntime(cfg, r, name, tier)
		}
	}
	return waiting("Role %s has a council seat on %s, and no connection of that agent has a %s model: connect one %s", r.Role, r.Pinned, tier, settingsHint)
}

// fromFile is the decision for a role the organization did not assign.
func (s State) fromFile(cfg *config.Config, r Request) Decision {
	if !cfg.ServerKeys() {
		return waiting("Role %s has no model connection, and this server does not lend its keys: connect a provider and assign it to the role %s", r.Role, settingsHint)
	}
	role := cfg.Roles[r.Role]
	name := s.fileRuntime(cfg, r, role.Tier)
	if name == "" {
		d := waiting("Role %s has no model connection: connect a provider and assign it to the role %s", r.Role, settingsHint)
		d.Unset = s.empty()
		return d
	}
	return s.onRuntime(cfg, r, name, role.Tier)
}

// fileRuntime returns the runtime of the server file that serves the request:
// the role's, or for a job on another adapter the first of that adapter,
// better with a model for tier. An unpinned job on an adapter nobody has any
// more moves to the role's.
func (s State) fileRuntime(cfg *config.Config, r Request, tier config.Tier) string {
	usual := cfg.Roles[r.Role].Runtime
	_, known := cfg.Runtimes[usual]
	adapter := cmp.Or(r.Pinned, r.Adapter)
	if known && (adapter == "" || cfg.Runtimes[usual].Adapter == adapter) {
		return usual
	}
	first := ""
	for _, name := range slices.Sorted(maps.Keys(cfg.Runtimes)) {
		rt := cfg.Runtimes[name]
		if rt.Adapter != adapter {
			continue
		}
		if s.model(cfg, r, tier, rt.Vendor) != "" {
			return name
		}
		first = cmp.Or(first, name)
	}
	if first == "" && known && r.Pinned == "" {
		return usual
	}
	return first
}

// Service answers from the store and the secret store.
type Service struct {
	st     store.Store
	cfg    *config.Config
	vault  secrets.Store
	logger *slog.Logger
}

// New returns a service. cfg is forecrew.yaml with the stored server settings;
// a nil vault leaves every organization's connection without a key.
func New(st store.Store, cfg *config.Config, vault secrets.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{st: st, cfg: cfg, vault: vault, logger: logger}
}

func (s *Service) load(ctx context.Context, accountID uuid.UUID) (State, error) {
	conns, err := s.st.ModelConnections().List(ctx, accountID)
	if err != nil {
		return State{}, fmt.Errorf("model connections: %w", err)
	}
	assigns, err := s.st.ModelAssignments().List(ctx, accountID)
	if err != nil {
		return State{}, fmt.Errorf("model assignments: %w", err)
	}
	return NewState(conns, assigns), nil
}

// key reads the key of a decision on an organization's connection, in that
// organization and nowhere else. A key that cannot be had makes the decision
// one that waits.
func (s *Service) key(ctx context.Context, accountID uuid.UUID, d Decision) (Decision, []byte, error) {
	if d.Source != SourceOrganization {
		return d, nil, nil
	}
	if s.vault == nil {
		return waiting("Connection %s has a key in the secret store, and the store is off on this server: set %s", d.Connection, config.EnvSecretKey), nil, nil
	}
	value, err := secrets.Resolve(ctx, s.vault, accountID, d.connection.SecretRef)
	switch {
	case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrInvalid):
		return waiting("Connection %s has no saved key: enter it %s", d.Connection, settingsHint), nil, nil
	case errors.Is(err, secrets.ErrCorrupt):
		return waiting("The key of connection %s does not read with this server's secret key: enter it again %s", d.Connection, settingsHint), nil, nil
	case err != nil:
		return d, nil, fmt.Errorf("key of connection %s: %w", d.Connection, err)
	}
	return d, value, nil
}

func (s *Service) decide(ctx context.Context, accountID uuid.UUID, st State, r Request) (Decision, []byte, error) {
	return s.key(ctx, accountID, st.Decide(s.cfg, r))
}

// Decide answers for one request, keys included.
func (s *Service) Decide(ctx context.Context, accountID uuid.UUID, r Request) (Decision, error) {
	st, err := s.load(ctx, accountID)
	if err != nil {
		return Decision{}, err
	}
	d, _, err := s.decide(ctx, accountID, st, r)
	return d, err
}

// requestOf is the request of job j.
func requestOf(j domain.Job) Request {
	r := Request{ProjectID: j.ProjectID, Role: j.Role, Pool: j.Pool, Adapter: j.Runtime, Pinned: j.Spec.PinnedRuntime}
	if r.Adapter == domain.RuntimeNone {
		r.Adapter = ""
	}
	return r
}

// Grant is what a leased job gets: everything from one decision, so a key
// never travels with the endpoint of another connection.
type Grant struct {
	// Connection is what the pool rule (credentials.runtimes) looks at.
	Connection string
	Env        map[string]string
	Endpoint   *domain.Endpoint
	// Model replaces the job's unless it is empty.
	Model string
	// Waiting says why the job must not run now.
	Waiting string
}

// Grant decides for a leased job of its own organization, reading the key
// now: a rotated key counts from the next lease, and nothing about it is
// stored with the job.
func (s *Service) Grant(ctx context.Context, j domain.Job) (Grant, error) {
	st, err := s.load(ctx, j.AccountID)
	if err != nil {
		return Grant{}, err
	}
	d, key, err := s.decide(ctx, j.AccountID, st, requestOf(j))
	switch {
	case err != nil:
		return Grant{}, err
	case d.Unset:
		return Grant{}, nil
	case d.Waiting != "":
		return Grant{Waiting: d.Waiting}, nil
	case d.Adapter != j.Runtime:
		return Grant{Waiting: fmt.Sprintf("Role %s moved to %s", j.Role, d.Adapter)}, nil
	case d.Source == SourceServer:
		rt := s.cfg.Runtimes[d.Connection]
		g := Grant{Connection: d.Connection, Endpoint: rt.Endpoint(), Model: d.Model}
		if len(rt.Credentials) > 0 {
			g.Env = maps.Clone(rt.Credentials)
		}
		return g, nil
	}
	c := d.connection
	name := cmp.Or(c.KeyEnv, runtime.KeyEnv(c.Adapter))
	g := Grant{Connection: c.Name, Env: map[string]string{name: string(key)}, Model: d.Model}
	if c.BaseURL != "" {
		g.Endpoint = &domain.Endpoint{BaseURL: c.BaseURL, KeyEnv: name, Organization: true}
	}
	return g, nil
}

// Settle brings the queued agent jobs of an organization in line with the
// decisions of now: a job moves to the runtime and model of its role, and one
// with no usable connection gets domain.RuntimeNone, which keeps it queued and
// starts no machine. Nothing else remembers this, so it survives a restart.
func (s *Service) Settle(ctx context.Context, accountID uuid.UUID) error {
	classes, err := s.st.Jobs().QueuedClasses(ctx, accountID)
	if err != nil || len(classes) == 0 {
		return err
	}
	st, err := s.load(ctx, accountID)
	if err != nil {
		return err
	}
	for _, c := range classes {
		j := domain.Job{ProjectID: c.ProjectID, Role: c.Role, Pool: c.Pool, Runtime: c.Runtime, Spec: domain.JobSpec{PinnedRuntime: c.Pinned}}
		d, _, err := s.decide(ctx, accountID, st, requestOf(j))
		if err != nil {
			return err
		}
		if d.Unset {
			continue
		}
		to, model := cmp.Or(d.Adapter, domain.RuntimeNone), cmp.Or(d.Model, c.Model)
		if to == c.Runtime && model == c.Model {
			continue
		}
		if err := s.st.Jobs().Retarget(ctx, accountID, c, to, model); err != nil {
			return fmt.Errorf("retarget %s jobs: %w", c.Role, err)
		}
		s.logger.InfoContext(ctx, "queued jobs moved", "account_id", accountID, "role", c.Role, "from", c.Runtime, "to", to, "connection", d.Connection)
	}
	return nil
}

// Waiter says why a queued job waits; jobs.Waiting has the same method.
type Waiter interface {
	WaitReason(ctx context.Context, j domain.Job) (code, sentence string, ok bool, err error)
}

// WaitReason implements Waiter: ok only for a job with no usable connection.
func (s *Service) WaitReason(ctx context.Context, j domain.Job) (code, sentence string, ok bool, err error) {
	if j.Kind.OrDefault() != domain.JobKindAgent {
		return "", "", false, nil
	}
	st, err := s.load(ctx, j.AccountID)
	if err != nil {
		return "", "", false, err
	}
	d, _, err := s.decide(ctx, j.AccountID, st, requestOf(j))
	if err != nil || d.Unset || d.Waiting == "" {
		return "", "", false, err
	}
	return ReasonNoConnection, d.Waiting, true, nil
}

type before struct {
	first *Service
	next  Waiter
}

func (b before) WaitReason(ctx context.Context, j domain.Job) (string, string, bool, error) {
	code, sentence, ok, err := b.first.WaitReason(ctx, j)
	if ok || err != nil {
		return code, sentence, ok, err
	}
	return b.next.WaitReason(ctx, j)
}

// Before returns a Waiter that asks s first and then next: a machine does not
// help a job that has no key to run with.
func (s *Service) Before(next Waiter) Waiter { return before{s, next} }

// ErrNoModel means a role has no model on an adapter.
var ErrNoModel = errors.New("no model")

// ModelOn returns the model a role runs with in a project on a council seat of
// adapter.
func (s *Service) ModelOn(ctx context.Context, accountID, projectID uuid.UUID, role, adapter string) (string, error) {
	d, err := s.Decide(ctx, accountID, Request{ProjectID: projectID, Role: role, Pinned: adapter})
	switch {
	case err != nil:
		return "", err
	case d.Waiting != "" && !d.Unset:
		return "", fmt.Errorf("%s: %w", d.Waiting, ErrNoModel)
	case d.Model == "" || d.Adapter != adapter:
		return "", fmt.Errorf("role %q: no runtime of adapter %q has a %s model: %w", role, adapter, d.Tier, ErrNoModel)
	}
	return d.Model, nil
}

// Usual returns the adapter a role runs on in a project, empty when it has no
// usable connection.
func (s *Service) Usual(ctx context.Context, accountID, projectID uuid.UUID, role string) (string, error) {
	d, err := s.Decide(ctx, accountID, Request{ProjectID: projectID, Role: role})
	return d.Adapter, err
}
