package roles

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
)

// Sentinel errors of the catalog.
var (
	// ErrNotConfigured means forecrew.yaml assigns no runtime to the role.
	ErrNotConfigured = errors.New("role has no runtime")
	// ErrSameVendorReview means the reviewer would use the developer's
	// vendor without policy.allow_same_vendor_review.
	ErrSameVendorReview = errors.New("reviewer and developer use the same vendor")
)

var _ Resolver = (*Catalog)(nil)

// Catalog resolves roles from the role definitions and the roles,
// runtimes and model tiers of forecrew.yaml.
type Catalog struct {
	defs map[string]Definition
	cfg  *config.Config
}

// New loads the role files from fsys (see Load) and checks them against cfg:
// every role cfg configures must exist, and separation of duties must hold
// (config.Config.SameVendorReview): the reviewer uses a runtime of another
// vendor than the developer unless policy.allow_same_vendor_review is set.
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
	if v, ok := cfg.SameVendorReview(); ok {
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
// runtime and tier in forecrew.yaml (config.Config.Model). Account and
// project do not change roles yet.
func (c *Catalog) Resolve(_ context.Context, _, _ uuid.UUID, name string) (Role, error) {
	d, err := c.Definition(name)
	if err != nil {
		return Role{}, err
	}
	adapter, model, err := c.cfg.Model(name)
	if err != nil {
		return Role{}, fmt.Errorf("%q: %w: %w", name, ErrNotConfigured, err)
	}
	if adapter == "" || model == "" {
		return Role{}, fmt.Errorf("%q: %w: no runtime adapter or model in forecrew.yaml", name, ErrNotConfigured)
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
	}, nil
}
