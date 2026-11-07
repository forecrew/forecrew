package roles

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
)

// Sentinel errors of the catalog.
var (
	// ErrNotConfigured means forecrew.yaml assigns no runtime to the role.
	ErrNotConfigured = errors.New("role has no runtime")
	// ErrSameVendorReview means the reviewer would use the developer's
	// vendor without policy.allow_same_vendor_review.
	ErrSameVendorReview = errors.New("reviewer and developer use models of the same maker")
)

var _ Resolver = (*Catalog)(nil)

// Catalog resolves roles from the role definitions and the roles,
// runtimes and model tiers of forecrew.yaml.
type Catalog struct {
	defs   map[string]Definition
	cfg    *config.Config
	models Models
}

// Models decides a role's connection, tier and model; *models.Service is one.
type Models interface {
	Decide(ctx context.Context, accountID uuid.UUID, r models.Request) (models.Decision, error)
}

// WithModels makes Resolve ask m, which knows each organization's connections
// and each project's overrides, instead of forecrew.yaml alone.
func (c *Catalog) WithModels(m Models) *Catalog {
	c.models = m
	return c
}

// Toolbox reports whether a role runs in a toolbox, and whether it exists.
func (c *Catalog) Toolbox(name string) (toolbox, known bool) {
	d, ok := c.defs[name]
	return ok && d.Toolbox != nil, ok
}

// Writes reports whether a role writes code: it has write paths. An unknown
// role counts as writing.
func (c *Catalog) Writes(name string) bool {
	d, ok := c.defs[name]
	return !ok || len(d.WritePaths) > 0
}

// New loads the role files from fsys (see Load) and checks them against cfg:
// every role cfg configures must exist, and separation of duties must hold
// (config.Config.SameVendorReview): the reviewer uses a model of another
// maker than the developer unless policy.allow_same_vendor_review is set.
func New(fsys fs.FS, cfg *config.Config) (*Catalog, error) {
	defs, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Roles)) {
		if _, ok := defs[name]; !ok {
			return nil, fmt.Errorf("forecrew.yaml roles.%s: %w", name, ErrUnknownRole)
		}
	}
	cat, err := models.DefaultCatalog()
	if err != nil {
		return nil, err
	}
	if v, ok := cfg.SameVendorReview(cat.MakerOf); ok {
		return nil, fmt.Errorf("%w %q; set policy.allow_same_vendor_review to allow it", ErrSameVendorReview, v)
	}
	return &Catalog{defs: defs, cfg: cfg}, nil
}

// Names returns the role names, sorted.
func (c *Catalog) Names() []string {
	return slices.Sorted(maps.Keys(c.defs))
}

// Definition returns the definition of a role.
func (c *Catalog) Definition(name string) (Definition, error) {
	d, ok := c.defs[name]
	if !ok {
		return Definition{}, fmt.Errorf("%q: %w", name, ErrUnknownRole)
	}
	return d, nil
}

// Resolve implements Resolver. The runtime and model come from the role's
// runtime and tier in forecrew.yaml (config.Config.Model), or with WithModels
// from the decision of internal/models for the organization and project. A
// role with no usable connection there resolves with runtime
// domain.RuntimeNone: its jobs wait until one is connected.
func (c *Catalog) Resolve(ctx context.Context, accountID, projectID uuid.UUID, name string) (Role, error) {
	d, err := c.Definition(name)
	if err != nil {
		return Role{}, err
	}
	adapter, model, tier, err := c.model(ctx, accountID, projectID, d)
	if err != nil {
		return Role{}, err
	}
	return Role{
		Name:             d.Name,
		Runtime:          adapter,
		Model:            model,
		WritePaths:       slices.Clone(d.WritePaths),
		Instructions:     d.Instructions,
		OutputSchema:     slices.Clone(d.OutputSchema),
		AllowedTools:     d.AllowedTools(),
		MaxTurns:         d.MaxTurns,
		Timeout:          d.Timeout,
		DefinitionOfDone: slices.Clone(d.DefinitionOfDone),
		EscalateWhen:     slices.Clone(d.EscalateWhen),
		Pool:             d.Pool,
		Toolbox:          d.Toolbox,
		Resources:        d.Resources,
		Context:          slices.Clone(d.Context),
		ContextBudget:    cmp.Or(d.ContextBudget, DefaultContextBudget(tier)),
	}, nil
}

// model returns the adapter, model and tier role d runs with.
func (c *Catalog) model(ctx context.Context, accountID, projectID uuid.UUID, d Definition) (adapter, model string, tier config.Tier, err error) {
	if c.models == nil {
		adapter, model, err := c.cfg.Model(d.Name)
		if err != nil {
			return "", "", "", fmt.Errorf("%q: %w: %w", d.Name, ErrNotConfigured, err)
		}
		if adapter == "" || model == "" {
			return "", "", "", fmt.Errorf("%q: %w: no runtime adapter or model in forecrew.yaml", d.Name, ErrNotConfigured)
		}
		return adapter, model, c.cfg.Roles[d.Name].Tier, nil
	}
	dec, err := c.models.Decide(ctx, accountID, models.Request{ProjectID: projectID, Role: d.Name, Pool: d.Pool})
	switch {
	case err != nil:
		return "", "", "", fmt.Errorf("%q: %w", d.Name, err)
	case dec.Unset:
		return "", "", "", fmt.Errorf("%q: %w: no runtime adapter or model in forecrew.yaml", d.Name, ErrNotConfigured)
	case dec.Waiting != "":
		return domain.RuntimeNone, "", config.TierStrong, nil
	case dec.Model == "":
		return "", "", "", fmt.Errorf("%q: %w: no model in forecrew.yaml", d.Name, ErrNotConfigured)
	}
	return dec.Adapter, dec.Model, dec.Tier, nil
}
