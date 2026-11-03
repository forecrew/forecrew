package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestParseSizing(t *testing.T) {
	tests := []struct {
		name   string
		sizing string // the sizing block, or empty for none
		check  func(t *testing.T, s Sizing)
	}{
		{"absent keeps the defaults", "", func(t *testing.T, s Sizing) {
			if !slices.Equal(s.Product.Impact, []domain.Level{domain.LevelHigh}) || !slices.Equal(s.Technical.Impact, []domain.Level{domain.LevelMedium, domain.LevelHigh}) || s.Technical.Components != 2 {
				t.Errorf("sizing = %+v", s)
			}
		}},
		{"lists are replaced", `
  product: {impact: [low], reversibility: [irreversible]}
  technical: {impact: [high], components: 3}`, func(t *testing.T, s Sizing) {
			if !slices.Equal(s.Product.Impact, []domain.Level{domain.LevelLow}) || !slices.Equal(s.Product.Reversibility, []domain.Reversibility{domain.Irreversible}) ||
				!slices.Equal(s.Technical.Impact, []domain.Level{domain.LevelHigh}) || s.Technical.Components != 3 {
				t.Errorf("sizing = %+v", s)
			}
		}},
		{"empty lists mean none", `
  product: {impact: [], reversibility: []}
  technical: {impact: []}`, func(t *testing.T, s Sizing) {
			if s.Product.Impact == nil || len(s.Product.Impact) != 0 || s.Product.Reversibility == nil || len(s.Product.Reversibility) != 0 ||
				s.Technical.Impact == nil || len(s.Technical.Impact) != 0 {
				t.Errorf("sizing = %+v, want empty lists", s)
			}
		}},
		{"one list left out keeps its default", `
  product: {impact: [low]}`, func(t *testing.T, s Sizing) {
			if !slices.Equal(s.Product.Reversibility, DefaultSizing().Product.Reversibility) {
				t.Errorf("reversibility = %v", s.Product.Reversibility)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := validYAML
			if tt.sizing != "" {
				y += "sizing:" + tt.sizing + "\n"
			}
			c, err := Parse([]byte(y), env(nil))
			if err != nil {
				t.Fatalf("Parse = %v", err)
			}
			tt.check(t, c.Sizing)
		})
	}
}

func TestParseSizingInvalid(t *testing.T) {
	tests := []struct {
		name   string
		sizing string
		want   string
	}{
		{"unknown impact", "\n  product: {impact: [huge]}", `sizing.product.impact: "huge"`},
		{"unknown reversibility", "\n  product: {reversibility: [never]}", `sizing.product.reversibility: "never"`},
		{"unknown technical impact", "\n  technical: {impact: [extreme]}", `sizing.technical.impact: "extreme"`},
		{"negative components", "\n  technical: {components: -1}", "sizing.technical.components must be at least 1"},
		{"unknown key", "\n  budget: 3", "field budget not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(validYAML+"sizing:"+tt.sizing+"\n"), env(nil))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse = %v, want ErrInvalid with %q", err, tt.want)
			}
		})
	}
}

func TestDefaultSizingIsParsedDefault(t *testing.T) {
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Sizing.Product.Impact, DefaultSizing().Product.Impact) || c.Sizing.Technical.Components != DefaultSizing().Technical.Components {
		t.Errorf("sizing = %+v, want %+v", c.Sizing, DefaultSizing())
	}
}
