package config

import (
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
)

// DefaultComponents is how many distinct areas make a goal span components
// when sizing.technical.components is not set.
const DefaultComponents = 2

// Sizing is the rule that turns a goal's triage into its councils (goal.size,
// docs/contracts/config.md). A list left out keeps its default; an empty
// list means no grade of that kind.
type Sizing struct {
	Product   SizingProduct   `yaml:"product"`
	Technical SizingTechnical `yaml:"technical"`
}

// SizingProduct gives a goal the product council when its impact is one of
// Impact or its reversibility is one of Reversibility.
type SizingProduct struct {
	Impact        []domain.Level         `yaml:"impact"`
	Reversibility []domain.Reversibility `yaml:"reversibility"`
}

// SizingTechnical gives a goal the technical council when its impact is one
// of Impact or its triage names at least Components distinct areas.
type SizingTechnical struct {
	Impact     []domain.Level `yaml:"impact"`
	Components int            `yaml:"components"`
}

// DefaultSizing is the rule of a forecrew.yaml without sizing: the product
// council when the impact is high or the reversibility is not easy, the
// technical council when the impact is medium or higher or the goal spans
// more than one component.
func DefaultSizing() Sizing {
	return Sizing{
		Product: SizingProduct{
			Impact:        []domain.Level{domain.LevelHigh},
			Reversibility: []domain.Reversibility{domain.ReversibleHard, domain.Irreversible},
		},
		Technical: SizingTechnical{
			Impact:     []domain.Level{domain.LevelMedium, domain.LevelHigh},
			Components: DefaultComponents,
		},
	}
}

// withDefaults fills the lists and the component count that are not set.
func (s Sizing) withDefaults() Sizing {
	d := DefaultSizing()
	if s.Product.Impact == nil {
		s.Product.Impact = d.Product.Impact
	}
	if s.Product.Reversibility == nil {
		s.Product.Reversibility = d.Product.Reversibility
	}
	if s.Technical.Impact == nil {
		s.Technical.Impact = d.Technical.Impact
	}
	if s.Technical.Components == 0 {
		s.Technical.Components = d.Technical.Components
	}
	return s
}

func (c *Config) applySizing() {
	c.Sizing = c.Sizing.withDefaults()
}

func (c *Config) validateSizing(p *problems) {
	for _, l := range c.Sizing.Product.Impact {
		if !slices.Contains(domain.Levels(), l) {
			p.addf("sizing.product.impact: %q must be low, medium or high", l)
		}
	}
	for _, r := range c.Sizing.Product.Reversibility {
		if !slices.Contains(domain.Reversibilities(), r) {
			p.addf("sizing.product.reversibility: %q must be easy, hard or irreversible", r)
		}
	}
	for _, l := range c.Sizing.Technical.Impact {
		if !slices.Contains(domain.Levels(), l) {
			p.addf("sizing.technical.impact: %q must be low, medium or high", l)
		}
	}
	if c.Sizing.Technical.Components < 1 {
		p.addf("sizing.technical.components must be at least 1, got %d", c.Sizing.Technical.Components)
	}
}
