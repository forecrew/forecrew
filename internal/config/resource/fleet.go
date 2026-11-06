package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// fleetPolicyName is the name of the one fleet policy resource.
const fleetPolicyName = "fleet"

// budgetNeeds is permissions.md, Budget fields: a change to a budget field
// takes billing.manage, a change to anything else worker.manage, and a new
// row counts as changing the budget fields it sets.
func budgetNeeds(budget ...string) func(context.Context, *Env, *Resource, *Resource, []domain.ConfigFieldChange) ([]Need, error) {
	return func(_ context.Context, _ *Env, _, _ *Resource, fields []domain.ConfigFieldChange) ([]Need, error) {
		var out []Need
		if touches(fields, budget...) {
			out = append(out, need(domain.PermBillingManage)...)
		}
		if !onlyTouches(fields, budget...) || len(fields) == 0 {
			out = append(out, need(domain.PermWorkerManage)...)
		}
		return out, nil
	}
}

type providerSpec struct {
	ProviderKind domain.ProviderKind   `json:"kind"`
	Mode         domain.ProviderMode   `json:"mode,omitempty"` // default agent
	Limits       domain.ProviderLimits `json:"limits"`
	NetworkAllow []string              `json:"network_allow,omitempty"`
	Tags         map[string]string     `json:"tags,omitempty"`
	Paused       bool                  `json:"paused,omitempty"`
}

func providerArea() Area {
	a := base[providerSpec](KindProvider, "Location", ScopeOrganization, domain.PermWorkerRead)
	a.Setting = domain.CapacityProvider{}
	a.Hidden = []string{"id", "account_id", "name", "mode", "status", "last_seen_at", "capabilities", "created_at", "updated_at"}
	a.Affects = "New jobs in all projects can run here. Running jobs are not touched."
	a.Needs = budgetNeeds("limits.monthly_budget")
	a.AlsoRead = domain.PermBillingManage
	a.Read = providerRead
	a.Validate = providerValidate
	a.Steps = providerSteps
	a.Normalize = providerNormalize
	a.Apply = applyProvider
	a.Protection = providerProtection
	a.Cost = func(Resource) string { return "Free. It is your machine." }
	a.Lost = providerLost
	return a
}

func providerRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.CapacityProviders().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, p := range rows {
		if p.Status == domain.ProviderRevoked {
			continue
		}
		out = append(out, Resource{Kind: KindProvider, Name: p.Name, Spec: encode(providerSpec{p.Kind, p.Mode, p.Limits, p.NetworkAllow, p.Tags, p.Status == domain.ProviderPaused})})
	}
	return out, nil
}

func providerValidate(_ context.Context, e *Env, old *Resource, want Resource) error {
	s, err := decode[providerSpec](want.Spec)
	if err != nil {
		return err
	}
	p := providerRow(e, want.Name, s)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if old != nil {
		if o, _ := decode[providerSpec](old.Spec); o.ProviderKind != s.ProviderKind {
			return invalid("a location does not change kind")
		}
	}
	return nil
}

func providerSteps(old, want *Resource) []domain.ConfigStep {
	if old != nil || want == nil {
		return nil
	}
	return []domain.ConfigStep{{Kind: domain.ConfigStepConnect, Resource: want.Ref(), Title: "Connect " + want.Name,
		Detail: "After you confirm, run the line shown on the machine. It reports what it finds, and the machine gets its token over its own connection: nobody sees or copies one."}}
}

func providerProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if want == nil {
		return nil
	}
	w, _ := decode[providerSpec](want.Spec)
	var before []string
	if old != nil {
		o, _ := decode[providerSpec](old.Spec)
		before = o.NetworkAllow
	}
	return wider("Opens network access for machines of "+want.Name, before, w.NetworkAllow)
}

func providerLost(old, want *Resource) string {
	if want == nil && old != nil {
		return "A removed location is revoked: its token stops working for good."
	}
	return ""
}

// wider returns reason when after holds something before does not.
func wider(reason string, before, after []string) []string {
	if slices.ContainsFunc(after, func(h string) bool { return !slices.Contains(before, h) }) {
		return []string{reason}
	}
	return nil
}

// providerNormalize writes the default mode, so a spec without it reads as
// what is stored.
func providerNormalize(raw json.RawMessage) (json.RawMessage, error) {
	s, err := decode[providerSpec](raw)
	if err != nil {
		return nil, err
	}
	if s.Mode == "" {
		s.Mode = domain.ProviderModeAgent
	}
	return encode(s), nil
}

func providerRow(e *Env, name string, s providerSpec) domain.CapacityProvider {
	p := domain.CapacityProvider{AccountID: e.Actor.AccountID, Name: name, Kind: s.ProviderKind, Mode: s.Mode, Status: domain.ProviderPending,
		Limits: s.Limits, NetworkAllow: s.NetworkAllow, Tags: s.Tags}
	if p.Mode == "" {
		p.Mode = domain.ProviderModeAgent
	}
	return p
}

func applyProvider(ctx context.Context, e *Env, old, want *Resource) error {
	if old == nil {
		s, err := decode[providerSpec](want.Spec)
		if err != nil {
			return err
		}
		p := providerRow(e, want.Name, s)
		return e.Store.CapacityProviders().Create(ctx, &p)
	}
	row, err := e.Store.CapacityProviders().GetByName(ctx, e.Actor.AccountID, old.Name)
	if err != nil {
		return missing(err, "location "+old.Name)
	}
	if want == nil {
		return e.Store.CapacityProviders().SetStatus(ctx, row.ID, domain.ProviderRevoked)
	}
	s, err := decode[providerSpec](want.Spec)
	if err != nil {
		return err
	}
	row.Limits, row.NetworkAllow, row.Tags = s.Limits, s.NetworkAllow, s.Tags
	if err := e.Store.CapacityProviders().Update(ctx, &row); err != nil {
		return err
	}
	switch {
	case s.Paused && row.Status == domain.ProviderActive:
		return e.Store.CapacityProviders().SetStatus(ctx, row.ID, domain.ProviderPaused)
	case !s.Paused && row.Status == domain.ProviderPaused:
		return e.Store.CapacityProviders().SetStatus(ctx, row.ID, domain.ProviderActive)
	}
	return nil
}

type shapeSpec struct {
	Provider           string             `json:"provider"`
	OS                 domain.MachineOS   `json:"os"`
	Arch               domain.MachineArch `json:"arch"`
	CPU                int                `json:"cpu"`
	MemoryMB           int                `json:"memory_mb"`
	DiskGB             int                `json:"disk_gb"`
	GPU                int                `json:"gpu,omitempty"`
	Pool               string             `json:"pool,omitempty"`
	Labels             []string           `json:"labels,omitempty"`
	ProviderConfig     map[string]any     `json:"provider_config,omitempty"`
	Image              string             `json:"image,omitempty"`
	PricePerMinute     float64            `json:"price_per_minute,omitempty"`
	MonthlyBudget      *float64           `json:"monthly_budget,omitempty"`
	WarmMin            int                `json:"warm_min,omitempty"`
	Max                int                `json:"max"`
	IdleSeconds        int                `json:"idle_seconds,omitempty"`
	BootTimeoutSeconds int                `json:"boot_timeout_seconds,omitempty"`
	MaxJobs            int                `json:"max_jobs,omitempty"`
	Note               string             `json:"note,omitempty"`
	Enabled            bool               `json:"enabled"`
}

func shapeArea() Area {
	a := base[shapeSpec](KindShape, "Machine type", ScopeOrganization, domain.PermWorkerRead)
	a.Setting = domain.Shape{}
	a.Hidden = []string{"id", "account_id", "provider_id", "name", "created_at", "updated_at"}
	a.Affects = "New jobs can get this machine type. Running jobs are not touched."
	a.Needs = budgetNeeds("monthly_budget")
	a.AlsoRead = domain.PermBillingManage
	a.Read = shapeRead
	a.Validate = shapeValidate
	a.Normalize = shapeNormalize
	a.Apply = applyShape
	a.Retire = shapeRetire
	a.Protection = shapeProtection
	a.Cost = shapeCost
	return a
}

func shapeRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.Shapes().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	providers, err := e.Store.CapacityProviders().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, s := range rows {
		spec := shapeSpec{"", s.OS, s.Arch, s.CPU, s.MemoryMB, s.DiskGB, s.GPU, s.Pool, s.Labels, s.ProviderConfig, s.Image, s.PricePerMinute, s.MonthlyBudget,
			s.WarmMin, s.Max, s.IdleSeconds, s.BootTimeoutSeconds, s.MaxJobs, s.Note, s.Enabled}
		if i := slices.IndexFunc(providers, func(p domain.CapacityProvider) bool { return p.ID == s.ProviderID }); i >= 0 {
			spec.Provider = providers[i].Name
		}
		out = append(out, Resource{Kind: KindShape, Name: s.Name, Spec: encode(spec)})
	}
	return out, nil
}

func shapeValidate(_ context.Context, e *Env, _ *Resource, want Resource) error {
	s, err := decode[shapeSpec](want.Spec)
	if err != nil {
		return err
	}
	row := shapeRow(e, want.Name, s)
	row.ProviderID = domain.NewID() // a location of the same plan has no ID yet
	if err := row.Validate(); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if s.Provider == "" {
		return invalid("provider is required")
	}
	return nil
}

func shapeRetire(spec json.RawMessage) json.RawMessage {
	s, _ := decode[shapeSpec](spec)
	s.Enabled = false
	return encode(s)
}

func shapeProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if old == nil || want == nil {
		return nil
	}
	o, _ := decode[shapeSpec](old.Spec)
	w, _ := decode[shapeSpec](want.Spec)
	if o.Pool != w.Pool {
		return []string{"Moves " + want.Name + " to another trust zone"}
	}
	return nil
}

func shapeCost(want Resource) string {
	s, _ := decode[shapeSpec](want.Spec)
	if s.PricePerMinute > 0 {
		return fmt.Sprintf("About %.2f per job minute on %s.", s.PricePerMinute, want.Name)
	}
	return ""
}

// shapeNormalize writes the defaults the row gets, so a spec without them
// reads as what is stored.
func shapeNormalize(raw json.RawMessage) (json.RawMessage, error) {
	s, err := decode[shapeSpec](raw)
	if err != nil {
		return nil, err
	}
	d := domain.Shape{Pool: s.Pool, Labels: s.Labels, BootTimeoutSeconds: s.BootTimeoutSeconds, MaxJobs: s.MaxJobs}.WithDefaults()
	s.Pool, s.Labels, s.BootTimeoutSeconds, s.MaxJobs = d.Pool, domain.NormalizeLabels(d.Labels), d.BootTimeoutSeconds, d.MaxJobs
	return encode(s), nil
}

func shapeRow(e *Env, name string, s shapeSpec) domain.Shape {
	row := domain.Shape{AccountID: e.Actor.AccountID, Name: name, OS: s.OS, Arch: s.Arch, CPU: s.CPU, MemoryMB: s.MemoryMB, DiskGB: s.DiskGB, GPU: s.GPU, Pool: s.Pool, Labels: s.Labels,
		ProviderConfig: s.ProviderConfig, Image: s.Image, PricePerMinute: s.PricePerMinute, MonthlyBudget: s.MonthlyBudget, WarmMin: s.WarmMin, Max: s.Max, IdleSeconds: s.IdleSeconds,
		BootTimeoutSeconds: s.BootTimeoutSeconds, MaxJobs: s.MaxJobs, Note: s.Note, Enabled: s.Enabled}
	return row.WithDefaults()
}

func applyShape(ctx context.Context, e *Env, old, want *Resource) error {
	s, err := decode[shapeSpec](want.Spec)
	if err != nil {
		return err
	}
	p, err := e.Store.CapacityProviders().GetByName(ctx, e.Actor.AccountID, s.Provider)
	if err != nil {
		return missing(err, "location "+s.Provider)
	}
	row := shapeRow(e, want.Name, s)
	row.ProviderID = p.ID
	if old == nil {
		return e.Store.Shapes().Create(ctx, &row)
	}
	cur, err := e.Store.Shapes().GetByName(ctx, e.Actor.AccountID, old.Name)
	if err != nil {
		return missing(err, "machine type "+old.Name)
	}
	if cur.ProviderID != p.ID {
		return invalid("a machine type does not move to another location")
	}
	row.ID = cur.ID
	return e.Store.Shapes().Update(ctx, &row)
}

type fleetPolicySpec struct {
	Routing       domain.FleetRouting `json:"routing"`
	AllowedShapes []string            `json:"allowed_shapes,omitempty"`
	Quotas        domain.FleetQuotas  `json:"quotas"`
	Egress        []string            `json:"egress,omitempty"`
	Plan          domain.FleetPlan    `json:"plan"`
}

func fleetPolicyArea() Area {
	a := base[fleetPolicySpec](KindFleetPolicy, "Fleet policy", ScopeOrganization, domain.PermWorkerRead)
	a.Setting = domain.FleetPolicy{}
	a.Hidden = []string{"account_id", "created_at", "updated_at"}
	a.Affects = "Applies to the next machine started. Running jobs are not touched."
	a.Needs = budgetNeeds("quotas")
	a.AlsoRead = domain.PermBillingManage
	a.NoDelete = "the fleet policy is not deleted; change it instead"
	a.Read = fleetPolicyRead
	a.Validate = fleetPolicyValidate
	a.Apply = fleetPolicyApply
	a.Protection = fleetPolicyProtection
	return a
}

func fleetPolicyRead(ctx context.Context, e *Env) ([]Resource, error) {
	p, err := e.Store.FleetPolicies().Get(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	return []Resource{{Kind: KindFleetPolicy, Name: fleetPolicyName, Spec: encode(fleetPolicySpec{p.Routing, p.AllowedShapes, p.Quotas, p.Egress, p.Plan})}}, nil
}

func fleetPolicyValidate(_ context.Context, e *Env, _ *Resource, want Resource) error {
	s, err := decode[fleetPolicySpec](want.Spec)
	if err != nil {
		return err
	}
	if want.Name != fleetPolicyName {
		return invalid("there is one fleet policy, called %s", fleetPolicyName)
	}
	p := policyRow(e, s)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	return nil
}

func fleetPolicyApply(ctx context.Context, e *Env, _, want *Resource) error {
	s, err := decode[fleetPolicySpec](want.Spec)
	if err != nil {
		return err
	}
	p := policyRow(e, s)
	return e.Store.FleetPolicies().Put(ctx, &p)
}

func fleetPolicyProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if want == nil {
		return nil
	}
	w, _ := decode[fleetPolicySpec](want.Spec)
	o := fleetPolicySpec{Routing: domain.RoutingOwnOnly}
	if old != nil {
		o, _ = decode[fleetPolicySpec](old.Spec)
	}
	out := wider("Opens network access for hosted machines", o.Egress, w.Egress)
	if o.Routing == domain.RoutingOwnOnly && w.Routing != domain.RoutingOwnOnly {
		out = append(out, "Adds Forecrew Cloud as a fallback: your code may leave your machines")
	}
	return out
}

func policyRow(e *Env, s fleetPolicySpec) domain.FleetPolicy {
	return domain.FleetPolicy{AccountID: e.Actor.AccountID, Routing: s.Routing, AllowedShapes: s.AllowedShapes, Quotas: s.Quotas, Egress: s.Egress, Plan: s.Plan}
}
