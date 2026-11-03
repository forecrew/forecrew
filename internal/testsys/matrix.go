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
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	known := map[string][]string{} // axis -> values of axes and include
	for _, axis := range slices.Sorted(maps.Keys(m.Axes)) {
		values := m.Axes[axis]
		if len(slices.Compact(slices.Sorted(slices.Values(values)))) != len(values) {
			add("axes.%s: values must be unique", axis)
		}
		if slices.Contains(values, "") {
			add("axes.%s: empty value", axis)
		}
		known[axis] = slices.Clone(values)
	}
	for i, c := range m.Include {
		if len(c) != len(m.Axes) {
			add("include[%d]: must give a value for every axis", i)
		}
		for axis, v := range c {
			if _, ok := m.Axes[axis]; ok && !slices.Contains(known[axis], v) {
				known[axis] = append(known[axis], v)
			}
		}
	}
	checkCell := func(field string, c Cell) {
		for _, axis := range slices.Sorted(maps.Keys(c)) {
			values, ok := known[axis]
			switch {
			case !ok:
				add("%s: unknown axis %q", field, axis)
			case !slices.Contains(values, c[axis]):
				add("%s: %s has no value %q", field, axis, c[axis])
			}
		}
	}
	for i, c := range m.Include {
		for axis := range c {
			if _, ok := m.Axes[axis]; !ok {
				add("include[%d]: unknown axis %q", i, axis)
			}
		}
	}
	for i, c := range m.Exclude {
		checkCell(fmt.Sprintf("exclude[%d]", i), c)
	}
	for i, c := range m.Required {
		checkCell(fmt.Sprintf("required[%d]", i), c)
	}
	for i, r := range m.Environments {
		checkCell(fmt.Sprintf("environments[%d].match", i), r.Match)
		if environments != nil && !slices.Contains(environments, r.Environment) {
			add("environments[%d]: no environment %q in %s", i, r.Environment, EnvironmentsDir)
		}
	}
	seen := map[string]bool{}
	for i, s := range m.Suites {
		if seen[s.Name] {
			add("suites[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		for _, g := range append(slices.Clone(s.Reports), s.Artifacts...) {
			if !domain.RelativePath(g) {
				add("suites.%s: %q must be relative to the repo root", s.Name, g)
			}
		}
		for _, axis := range slices.Sorted(maps.Keys(s.Axes)) {
			for _, v := range s.Axes[axis] {
				checkCell("suites."+s.Name+".axes", Cell{axis: v})
			}
		}
	}
	return p
}
