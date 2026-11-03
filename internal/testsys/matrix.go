package testsys

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/domain"
)

// MatrixFile is where a repo keeps its test matrix.
const MatrixFile = ".forecrew/matrix.yaml"

// StrategyFile is where a repo keeps the test strategy the matrix follows,
// written by the test architect for humans and the other test roles.
const StrategyFile = ".forecrew/test-strategy.md"

// Cell is a matrix cell, or a partial one: axis -> value.
type Cell map[string]string

// Matches reports whether c has every value of partial.
func (c Cell) Matches(partial Cell) bool {
	for k, v := range partial {
		if got, ok := c[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Matrix is a .forecrew/matrix.yaml file (docs/contracts/matrix.schema.json).
type Matrix struct {
	Version      int                 `yaml:"version" json:"version,omitempty"`
	Axes         map[string][]string `yaml:"axes" json:"axes,omitempty"`
	Exclude      []Cell              `yaml:"exclude" json:"exclude,omitempty"`
	Include      []Cell              `yaml:"include" json:"include,omitempty"`
	Environments []EnvironmentRule   `yaml:"environments" json:"environments,omitempty"`
	Suites       []Suite             `yaml:"suites" json:"suites,omitempty"`
	// Required lists the partial cells that must pass to merge; nil means
	// every cell.
	Required []Cell `yaml:"required" json:"required,omitempty"`
}

// EnvironmentRule maps the cells that have Match to an environment.
type EnvironmentRule struct {
	Match       Cell   `yaml:"match" json:"match,omitempty"`
	Environment string `yaml:"environment" json:"environment,omitempty"`
}

// Suite is a test suite run in matrix cells.
type Suite struct {
	Name      string              `yaml:"name" json:"name,omitempty"`
	Command   Command             `yaml:"command" json:"command"`
	Level     string              `yaml:"level" json:"level,omitempty"` // unit, integration or e2e
	Reports   []string            `yaml:"reports" json:"reports,omitempty"`
	Artifacts []string            `yaml:"artifacts" json:"artifacts,omitempty"`
	Required  *bool               `yaml:"required" json:"required,omitempty"` // nil means true
	Quirks    bool                `yaml:"quirks" json:"quirks,omitempty"`
	Axes      map[string][]string `yaml:"axes" json:"axes,omitempty"` // nil means every cell
}

// IsRequired reports whether failures of the suite in a required cell
// block the merge.
func (s Suite) IsRequired() bool { return s.Required == nil || *s.Required }

// Command is a suite command: an argv list or a shell string.
type Command struct {
	Argv  []string `json:"argv,omitempty"`
	Shell string   `json:"shell,omitempty"`
}

// UnmarshalYAML reads a sequence as Argv and a scalar as Shell.
func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Shell = n.Value
		return nil
	}
	return n.Decode(&c.Argv)
}

// Spec returns the command as the command of a job spec.
func (c Command) Spec() domain.CommandSpec {
	return domain.CommandSpec{Argv: slices.Clone(c.Argv), Shell: c.Shell}
}

// ParseMatrix validates a matrix file. environments lists the environment
// names the repo has; rules naming another one are refused. A nil
// environments skips that check. Every problem wraps ErrInvalid.
func ParseMatrix(data []byte, environments []string) (Matrix, error) {
	var m Matrix
	err := validate("matrix.schema.json", matrixSchema, data,
		func(json.RawMessage) error { return decodeStrict(data, &m) },
		func() error {
			if p := m.check(environments); len(p) > 0 {
				return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(p, "; "))
			}
			return nil
		})
	if err != nil {
		return Matrix{}, fmt.Errorf("%s: %w", MatrixFile, err)
	}
	return m, nil
}

// check validates the cross-references the schema cannot: axes and values
// of partial cells, suite names and environment names.
func (m Matrix) check(environments []string) []string {
	c := matrixCheck{m: m, known: map[string][]string{}}
	c.axes()
	for i, cell := range m.Exclude {
		c.cell(fmt.Sprintf("exclude[%d]", i), cell)
	}
	for i, cell := range m.Required {
		c.cell(fmt.Sprintf("required[%d]", i), cell)
	}
	for i, r := range m.Environments {
		c.cell(fmt.Sprintf("environments[%d].match", i), r.Match)
		if environments != nil && !slices.Contains(environments, r.Environment) {
			c.addf("environments[%d]: no environment %q in %s", i, r.Environment, EnvironmentsDir)
		}
	}
	c.suites()
	return c.problems
}

// matrixCheck collects the problems of a matrix.
type matrixCheck struct {
	m        Matrix
	known    map[string][]string // axis -> values of axes and include
	problems []string
}

func (c *matrixCheck) addf(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// axes checks the axes and include, and records the known values.
func (c *matrixCheck) axes() {
	for _, axis := range slices.Sorted(maps.Keys(c.m.Axes)) {
		values := c.m.Axes[axis]
		if len(slices.Compact(slices.Sorted(slices.Values(values)))) != len(values) {
			c.addf("axes.%s: values must be unique", axis)
		}
		if slices.Contains(values, "") {
			c.addf("axes.%s: empty value", axis)
		}
		c.known[axis] = slices.Clone(values)
	}
	for i, cell := range c.m.Include {
		if len(cell) != len(c.m.Axes) {
			c.addf("include[%d]: must give a value for every axis", i)
		}
		for _, axis := range slices.Sorted(maps.Keys(cell)) {
			values, ok := c.known[axis]
			switch {
			case !ok:
				c.addf("include[%d]: unknown axis %q", i, axis)
			case !slices.Contains(values, cell[axis]):
				c.known[axis] = append(values, cell[axis])
			}
		}
	}
}

// cell checks that a partial cell uses known axes and values.
func (c *matrixCheck) cell(field string, cell Cell) {
	for _, axis := range slices.Sorted(maps.Keys(cell)) {
		values, ok := c.known[axis]
		switch {
		case !ok:
			c.addf("%s: unknown axis %q", field, axis)
		case !slices.Contains(values, cell[axis]):
			c.addf("%s: %s has no value %q", field, axis, cell[axis])
		}
	}
}

func (c *matrixCheck) suites() {
	seen := map[string]bool{}
	for i, s := range c.m.Suites {
		if seen[s.Name] {
			c.addf("suites[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		for _, g := range append(slices.Clone(s.Reports), s.Artifacts...) {
			if !domain.RelativePath(g) {
				c.addf("suites.%s: %q must be relative to the repo root", s.Name, g)
			}
		}
		for _, axis := range slices.Sorted(maps.Keys(s.Axes)) {
			for _, v := range s.Axes[axis] {
				c.cell("suites."+s.Name+".axes", Cell{axis: v})
			}
		}
	}
}
