// Package matrix expands a repo's test matrix (.forecrew/matrix.yaml, read
// by internal/testsys) into the cells to run and the command jobs of each.
package matrix

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testsys"
)

// ErrInvalid is wrapped by every problem of the repo's files or of a scope.
var ErrInvalid = testsys.ErrInvalid

// osAxis is the axis whose value a worker must have as a label.
const osAxis = "os"

// Scopes: which suites of which cells to run.
const (
	ScopeAll      = "all"      // every suite in every cell it applies to
	ScopeRequired = "required" // required suites in required cells
	ScopeQuirks   = "quirks"   // quirks suites in every cell they apply to
)

// Limits of a plan. The matrix file comes from the repo, and a few lines of
// axes can span more cells than memory holds, so sizes are checked before any
// cell is built.
const (
	// MaxCells bounds the combinations of the axes, before exclude, plus the
	// include entries.
	MaxCells = 256
	// MaxJobs bounds the jobs of one expansion.
	MaxJobs = 256
)

// Environments holds a repo's environments by name.
type Environments map[string]domain.Environment

// ReadFile reads a repo file by its path. A missing file is an error
// wrapping fs.ErrNotExist.
type ReadFile func(name string) ([]byte, error)

// Load reads and validates the matrix and every environment its rules name.
// Problems with the files wrap ErrInvalid, all of them in one error; any
// other error means a file could not be read.
func Load(read ReadFile) (testsys.Matrix, Environments, error) {
	data, err := read(testsys.MatrixFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return testsys.Matrix{}, nil, fmt.Errorf("%w: %s does not exist", ErrInvalid, testsys.MatrixFile)
	case err != nil:
		return testsys.Matrix{}, nil, fmt.Errorf("read %s: %w", testsys.MatrixFile, err)
	}
	m, err := testsys.ParseMatrix(data, nil)
	if err != nil {
		return testsys.Matrix{}, nil, err
	}
	if err := checkSize(m); err != nil {
		return testsys.Matrix{}, nil, err
	}
	envs, problems, err := loadEnvironments(m, read)
	if err != nil {
		return testsys.Matrix{}, nil, err
	}
	for _, c := range combinations(m) {
		if _, ok := environmentOf(m, c); !ok {
			problems = append(problems, fmt.Errorf("%w: %s: no environment rule matches cell %s", ErrInvalid, testsys.MatrixFile, key(c)))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return testsys.Matrix{}, nil, err
	}
	return m, envs, nil
}

// checkSize refuses a matrix of more than MaxCells cells without building any.
// The product stays at most MaxCells, so it cannot overflow.
func checkSize(m testsys.Matrix) error {
	total := 1
	for _, values := range m.Axes {
		n := len(values)
		if n == 0 {
			continue
		}
		if total > MaxCells/n {
			return fmt.Errorf("%w: %s: the axes span more than %d cells", ErrInvalid, testsys.MatrixFile, MaxCells)
		}
		total *= n
	}
	if len(m.Include) > MaxCells-total {
		return fmt.Errorf("%w: %s: the axes and include give more than %d cells", ErrInvalid, testsys.MatrixFile, MaxCells)
	}
	return nil
}

// loadEnvironments reads the environments the rules of m name. It returns the
// problems with them apart from the errors reading them.
func loadEnvironments(m testsys.Matrix, read ReadFile) (Environments, []error, error) {
	envs := Environments{}
	var problems []error
	seen := map[string]bool{}
	for _, r := range m.Environments {
		if seen[r.Environment] {
			continue
		}
		seen[r.Environment] = true
		file := path.Join(testsys.EnvironmentsDir, r.Environment+".yaml")
		data, err := read(file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, fmt.Errorf("%w: %s does not exist", ErrInvalid, file))
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("read %s: %w", file, err)
		}
		env, err := testsys.ParseEnvironment(file, data)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		envs[env.Name] = env
	}
	return envs, problems, nil
}

// Scope says what Expand selects: the suites of Set (ScopeAll, ScopeRequired,
// ScopeQuirks or empty for none) plus the named Suites, each of those in every
// cell it applies to.
type Scope struct {
	Set    string
	Suites []string
}

// Check refuses a scope that selects nothing by mistake: no set and no suites,
// an unknown set or a suite the matrix does not have.
func (s Scope) Check(m testsys.Matrix) error {
	if s.Set == "" && len(s.Suites) == 0 {
		return fmt.Errorf("%w: scope needs %s, %s, %s or suites", ErrInvalid, ScopeAll, ScopeRequired, ScopeQuirks)
	}
	if s.Set != "" && !slices.Contains([]string{ScopeAll, ScopeRequired, ScopeQuirks}, s.Set) {
		return fmt.Errorf("%w: unknown scope %q", ErrInvalid, s.Set)
	}
	for _, name := range s.Suites {
		if !slices.ContainsFunc(m.Suites, func(x testsys.Suite) bool { return x.Name == name }) {
			return fmt.Errorf("%w: %s has no suite %q", ErrInvalid, testsys.MatrixFile, name)
		}
	}
	return nil
}

func (s Scope) selects(m testsys.Matrix, c testsys.Cell, suite testsys.Suite) bool {
	if slices.Contains(s.Suites, suite.Name) {
		return true
	}
	switch s.Set {
	case ScopeAll:
		return true
	case ScopeRequired:
		return suite.IsRequired() && required(m, c)
	case ScopeQuirks:
		return suite.Quirks
	}
	return false
}

// required reports whether c must pass to merge.
func required(m testsys.Matrix, c testsys.Cell) bool {
	return m.Required == nil || slices.ContainsFunc(m.Required, func(p testsys.Cell) bool { return c.Matches(p) })
}

// Cell is one cell to run: its axis values, where it runs and its suites.
type Cell struct {
	Key         string             `json:"key"` // the axis values as text, sorted by axis
	Cell        testsys.Cell       `json:"cell"`
	Environment domain.Environment `json:"environment"`
	Pool        string             `json:"pool"`
	Needs       []string           `json:"needs"` // worker labels
	Suites      []testsys.Suite    `json:"suites"`
}

// Job is one suite of one cell: what a command job step needs, as an item of a
// foreach (with.from in docs/playbooks.md).
type Job struct {
	Key         string             `json:"key"` // <cell key>/<suite>
	Cell        testsys.Cell       `json:"cell"`
	Suite       string             `json:"suite"`
	Environment domain.Environment `json:"environment"`
	Needs       []string           `json:"needs"`
	Command     domain.CommandSpec `json:"command"`
}

// Jobs returns the command jobs of the cell, one per suite.
func (c Cell) Jobs() []Job {
	jobs := make([]Job, 0, len(c.Suites))
	for _, s := range c.Suites {
		command := s.Command.Spec()
		command.Reports, command.Artifacts = slices.Clone(s.Reports), slices.Clone(s.Artifacts)
		jobs = append(jobs, Job{
			Key: c.Key + "/" + s.Name, Cell: c.Cell, Suite: s.Name, Environment: c.Environment, Needs: c.Needs, Command: command,
		})
	}
	return jobs
}

// Expand returns the cells of m that have a suite in scope, with those suites
// in file order. Cells follow the axes sorted by name, the last one changing
// fastest and each in file order, then the included cells. envs and m come
// from Load, which checked that every cell has an environment. More than
// MaxCells cells or MaxJobs jobs wrap ErrInvalid.
func Expand(m testsys.Matrix, envs Environments, scope Scope) ([]Cell, error) {
	if err := checkSize(m); err != nil {
		return nil, err
	}
	out := []Cell{}
	jobs := 0
	for _, c := range combinations(m) {
		var suites []testsys.Suite
		for _, s := range m.Suites {
			if inAxes(c, s) && scope.selects(m, c, s) {
				suites = append(suites, s)
			}
		}
		if len(suites) == 0 {
			continue
		}
		if jobs += len(suites); jobs > MaxJobs {
			return nil, fmt.Errorf("%w: %s: the scope gives more than %d jobs", ErrInvalid, testsys.MatrixFile, MaxJobs)
		}
		name, _ := environmentOf(m, c)
		env := envs[name]
		out = append(out, Cell{Key: key(c), Cell: c, Environment: env, Pool: domain.CommandPool(&env), Needs: needs(c, env), Suites: suites})
	}
	return out, nil
}

// combinations returns every cell of m: the axes spanned, without the excluded
// cells, plus the included ones.
func combinations(m testsys.Matrix) []testsys.Cell {
	cells := []testsys.Cell{{}}
	for _, axis := range slices.Sorted(maps.Keys(m.Axes)) {
		next := make([]testsys.Cell, 0, len(cells)*len(m.Axes[axis]))
		for _, c := range cells {
			for _, v := range m.Axes[axis] {
				n := maps.Clone(c)
				n[axis] = v
				next = append(next, n)
			}
		}
		cells = next
	}
	cells = slices.DeleteFunc(cells, func(c testsys.Cell) bool {
		return slices.ContainsFunc(m.Exclude, func(p testsys.Cell) bool { return c.Matches(p) })
	})
	for _, inc := range m.Include {
		if !slices.ContainsFunc(cells, func(c testsys.Cell) bool { return maps.Equal(c, inc) }) {
			cells = append(cells, inc)
		}
	}
	return cells
}

// inAxes reports whether the suite's axes allow c.
func inAxes(c testsys.Cell, s testsys.Suite) bool {
	for axis, values := range s.Axes {
		if !slices.Contains(values, c[axis]) {
			return false
		}
	}
	return true
}

// environmentOf returns the environment of the first rule c matches.
func environmentOf(m testsys.Matrix, c testsys.Cell) (string, bool) {
	for _, r := range m.Environments {
		if c.Matches(r.Match) {
			return r.Environment, true
		}
	}
	return "", false
}

// needs returns the worker labels of a cell: the runner label on GitHub-hosted
// runners, otherwise the environment's needs and the cell's os, which any text
// without spaces can be as labels are free-form.
func needs(c testsys.Cell, env domain.Environment) []string {
	if env.Provider == domain.ProviderActions {
		return domain.NormalizeLabels([]string{env.RunsOn})
	}
	labels := slices.Clone(env.Needs)
	if os := c[osAxis]; !strings.ContainsFunc(os, unicode.IsSpace) {
		labels = append(labels, os)
	}
	return domain.NormalizeLabels(labels)
}

// key returns the axis values of c as text.
func key(c testsys.Cell) string {
	parts := make([]string, 0, len(c))
	for _, axis := range slices.Sorted(maps.Keys(c)) {
		parts = append(parts, axis+"="+c[axis])
	}
	return strings.Join(parts, ",")
}
