package matrix_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/testsys"
)

var update = flag.Bool("update", false, "rewrite the golden file in testdata/golden/matrix")

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func reader(fsys fs.FS) matrix.ReadFile {
	return func(name string) ([]byte, error) { return fs.ReadFile(fsys, name) }
}

// repo returns a repo with the matrix and environment files named by base name.
func repo(matrixFile string, envs ...string) fstest.MapFS {
	m := fstest.MapFS{testsys.MatrixFile: {Data: []byte(matrixFile)}}
	for _, name := range envs {
		env := "version: 1\nname: " + name + "\nprovider: machine\nneeds: [" + name + "]\n"
		m[testsys.EnvironmentsDir+"/"+name+".yaml"] = &fstest.MapFile{Data: []byte(env)}
	}
	return m
}

const suites = `
suites:
  - {name: unit, command: [make, test], level: unit, reports: [unit.xml], artifacts: [cover.out]}
  - {name: e2e, command: make e2e, level: e2e, reports: [e2e.xml], required: false, axes: {os: [linux]}}
  - {name: paths, command: [make, paths], level: unit, reports: [paths.xml], quirks: true, axes: {os: [linux, windows]}}
`

const full = `version: 1
axes: {os: [linux, windows], db: [pg17, pg18]}
exclude: [{os: windows, db: pg17}]
include: [{os: linux, db: tip}, {os: linux, db: pg18}]
environments:
  - {match: {os: windows}, environment: win}
  - {match: {}, environment: lin}
required: [{os: linux}]` + suites

func load(t *testing.T, fsys fs.FS) (testsys.Matrix, matrix.Environments) {
	t.Helper()
	m, envs, err := matrix.Load(reader(fsys))
	must(t, err)
	return m, envs
}

func expand(t *testing.T, m testsys.Matrix, envs matrix.Environments, scope matrix.Scope) []matrix.Cell {
	t.Helper()
	cells, err := matrix.Expand(m, envs, scope)
	must(t, err)
	return cells
}

// TestDesignDocExample expands the design doc's matrix: 3 OS x 2 runtimes x 2
// databases minus the two excluded cells.
func TestDesignDocExample(t *testing.T) {
	m, envs := load(t, os.DirFS(filepath.Join("..", "..", "testdata", "matrix", "example")))
	cells := expand(t, m, envs, matrix.Scope{Set: matrix.ScopeAll})
	if len(cells) != 10 {
		t.Fatalf("cells = %d, want 10", len(cells))
	}
	got, err := json.MarshalIndent(cells, "", "  ")
	must(t, err)
	got = append(got, '\n')
	golden := filepath.Join("..", "..", "testdata", "golden", "matrix", "example.json")
	if *update {
		must(t, os.WriteFile(golden, got, 0o644))
	}
	want, err := os.ReadFile(golden)
	must(t, err)
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s; run go test ./internal/matrix -update\n%s", golden, got)
	}
	for _, c := range cells {
		var wantNeeds []string
		switch c.Cell["os"] {
		case "ubuntu-lts":
			wantNeeds = []string{"ubuntu-lts"}
		case "windows-11":
			wantNeeds = []string{"node", "runtime", "windows", "windows-11"}
		case "macos-latest":
			wantNeeds = []string{"macos", "macos-latest"}
		}
		if !slices.Equal(c.Needs, wantNeeds) || c.Pool != domain.PoolBuild {
			t.Errorf("%s: needs %v pool %s, want needs %v", c.Key, c.Needs, c.Pool, wantNeeds)
		}
		if c.Cell["os"] == "windows-11" && c.Cell["database"] == "postgres-previous" {
			t.Errorf("excluded cell %s", c.Key)
		}
	}
}

func TestExpandScopes(t *testing.T) {
	m, envs := load(t, repo(full, "win", "lin"))
	tests := []struct {
		name  string
		scope matrix.Scope
		want  []string // <cell key>/<suite>
	}{
		{"all", matrix.Scope{Set: matrix.ScopeAll}, []string{
			"db=pg17,os=linux/unit", "db=pg17,os=linux/e2e", "db=pg17,os=linux/paths",
			"db=pg18,os=linux/unit", "db=pg18,os=linux/e2e", "db=pg18,os=linux/paths",
			"db=pg18,os=windows/unit", "db=pg18,os=windows/paths",
			"db=tip,os=linux/unit", "db=tip,os=linux/e2e", "db=tip,os=linux/paths",
		}},
		{"required", matrix.Scope{Set: matrix.ScopeRequired}, []string{
			"db=pg17,os=linux/unit", "db=pg17,os=linux/paths",
			"db=pg18,os=linux/unit", "db=pg18,os=linux/paths",
			"db=tip,os=linux/unit", "db=tip,os=linux/paths",
		}},
		{"quirks", matrix.Scope{Set: matrix.ScopeQuirks}, []string{
			"db=pg17,os=linux/paths", "db=pg18,os=linux/paths", "db=pg18,os=windows/paths", "db=tip,os=linux/paths",
		}},
		{"suite names", matrix.Scope{Suites: []string{"e2e"}}, []string{
			"db=pg17,os=linux/e2e", "db=pg18,os=linux/e2e", "db=tip,os=linux/e2e",
		}},
		{"required plus a suite", matrix.Scope{Set: matrix.ScopeRequired, Suites: []string{"paths", "e2e"}}, []string{
			"db=pg17,os=linux/unit", "db=pg17,os=linux/e2e", "db=pg17,os=linux/paths",
			"db=pg18,os=linux/unit", "db=pg18,os=linux/e2e", "db=pg18,os=linux/paths",
			"db=pg18,os=windows/paths",
			"db=tip,os=linux/unit", "db=tip,os=linux/e2e", "db=tip,os=linux/paths",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must(t, tt.scope.Check(m))
			var got []string
			for _, c := range expand(t, m, envs, tt.scope) {
				for _, j := range c.Jobs() {
					got = append(got, j.Key)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("jobs = %q\nwant   %q", got, tt.want)
			}
		})
	}
}

// Without required in the file every cell is required; suites still decide
// for themselves.
func TestExpandRequiredByDefault(t *testing.T) {
	m, envs := load(t, repo(`version: 1
axes: {os: [linux, windows]}
environments: [{match: {}, environment: lin}]
suites:
  - {name: unit, command: [t], level: unit, reports: [r.xml]}
  - {name: extra, command: [t], level: unit, reports: [r.xml], required: false}
`, "lin"))
	var got []string
	for _, c := range expand(t, m, envs, matrix.Scope{Set: matrix.ScopeRequired}) {
		for _, j := range c.Jobs() {
			got = append(got, j.Key)
		}
	}
	if want := []string{"os=linux/unit", "os=windows/unit"}; !slices.Equal(got, want) {
		t.Fatalf("jobs = %q, want %q", got, want)
	}
}

func TestExpandIsStable(t *testing.T) {
	m, envs := load(t, repo(full, "win", "lin"))
	first, err := json.Marshal(expand(t, m, envs, matrix.Scope{Set: matrix.ScopeAll}))
	must(t, err)
	for range 20 {
		again, err := json.Marshal(expand(t, m, envs, matrix.Scope{Set: matrix.ScopeAll}))
		must(t, err)
		if !bytes.Equal(first, again) {
			t.Fatal("the order changed between expansions")
		}
	}
}

func TestExpandNoAxes(t *testing.T) {
	m, envs := load(t, repo("version: 1\nenvironments: [{match: {}, environment: lin}]\nsuites:\n- {name: unit, command: [t], level: unit, reports: [r.xml]}\n", "lin"))
	cells := expand(t, m, envs, matrix.Scope{Set: matrix.ScopeAll})
	if len(cells) != 1 || cells[0].Key != "" || len(cells[0].Cell) != 0 || !slices.Equal(cells[0].Needs, []string{"lin"}) {
		t.Fatalf("cells = %+v", cells)
	}
}

func TestExpandNothingInScope(t *testing.T) {
	m, envs := load(t, repo(full, "win", "lin"))
	m.Suites = m.Suites[:1]
	cells := expand(t, m, envs, matrix.Scope{Set: matrix.ScopeQuirks})
	if cells == nil || len(cells) != 0 {
		t.Fatalf("cells = %#v, want empty", cells)
	}
}

func TestEnvironmentPoolAndNeeds(t *testing.T) {
	fsys := repo(`version: 1
axes: {os: [linux, "mac os"]}
environments:
  - {match: {os: linux}, environment: gh}
  - {match: {}, environment: lab}
suites:
  - {name: unit, command: [t], level: unit, reports: [r.xml]}
`, "lab")
	fsys[testsys.EnvironmentsDir+"/gh.yaml"] = &fstest.MapFile{Data: []byte("version: 1\nname: gh\nprovider: actions\nruns_on: ubuntu-24.04\n")}
	m, envs := load(t, fsys)
	cells := expand(t, m, envs, matrix.Scope{Set: matrix.ScopeAll})
	if len(cells) != 2 {
		t.Fatalf("cells = %+v", cells)
	}
	if c := cells[0]; c.Pool != domain.PoolActions || c.Environment.Name != "gh" || !slices.Equal(c.Needs, []string{"ubuntu-24.04"}) {
		t.Errorf("actions cell = %+v", c)
	}
	// A value with a space cannot be a worker label.
	if c := cells[1]; c.Pool != domain.PoolBuild || !slices.Equal(c.Needs, []string{"lab"}) {
		t.Errorf("machine cell = %+v", c)
	}
}

func TestJobs(t *testing.T) {
	m, envs := load(t, repo(full, "win", "lin"))
	jobs := expand(t, m, envs, matrix.Scope{Suites: []string{"unit", "e2e"}})[0].Jobs()
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	unit, e2e := jobs[0], jobs[1]
	if unit.Suite != "unit" || unit.Cell["db"] != "pg17" || unit.Environment.Name != "lin" || !slices.Equal(unit.Needs, []string{"lin", "linux"}) ||
		!slices.Equal(unit.Command.Argv, []string{"make", "test"}) || !slices.Equal(unit.Command.Reports, []string{"unit.xml"}) || !slices.Equal(unit.Command.Artifacts, []string{"cover.out"}) {
		t.Errorf("unit job = %+v", unit)
	}
	if e2e.Command.Shell != "make e2e" || len(e2e.Command.Argv) != 0 {
		t.Errorf("e2e job = %+v", e2e)
	}
}

func TestScopeCheck(t *testing.T) {
	m, _ := load(t, repo(full, "win", "lin"))
	for name, tt := range map[string]struct {
		scope matrix.Scope
		want  string
	}{
		"empty":         {matrix.Scope{}, "scope needs"},
		"unknown":       {matrix.Scope{Set: "some"}, `unknown scope "some"`},
		"unknown suite": {matrix.Scope{Set: matrix.ScopeAll, Suites: []string{"nope"}}, `no suite "nope"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := tt.scope.Check(m)
			if !errors.Is(err, matrix.ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// axes writes a matrix with n axes of size values each.
func axes(n, size int) string {
	var b strings.Builder
	b.WriteString("version: 1\naxes:\n")
	for i := range n {
		fmt.Fprintf(&b, "  a%d: [", i)
		for v := range size {
			fmt.Fprintf(&b, "v%d,", v)
		}
		b.WriteString("]\n")
	}
	b.WriteString("environments: [{match: {}, environment: lin}]\n")
	b.WriteString("suites:\n  - {name: unit, command: [t], level: unit, reports: [r.xml]}\n")
	return b.String()
}

// includes writes a matrix of one axis with n more include entries.
func includes(values, n int) string {
	var b strings.Builder
	b.WriteString("version: 1\naxes:\n  os: [")
	for v := range values {
		fmt.Fprintf(&b, "v%d,", v)
	}
	b.WriteString("]\ninclude:\n")
	for i := range n {
		fmt.Fprintf(&b, "  - {os: i%d}\n", i)
	}
	b.WriteString("environments: [{match: {}, environment: lin}]\n")
	b.WriteString("suites:\n  - {name: unit, command: [t], level: unit, reports: [r.xml]}\n")
	return b.String()
}

func TestLoadRefusesHugeMatrices(t *testing.T) {
	tests := map[string]struct {
		file string
		want string
	}{
		"nine axes of ten":   {axes(9, 10), "the axes span more than 256 cells"},
		"overflows an int":   {axes(70, 10), "the axes span more than 256 cells"},
		"just over":          {axes(1, matrix.MaxCells+1), "the axes span more than 256 cells"},
		"too many includes":  {includes(1, matrix.MaxCells), "the axes and include give more than 256 cells"},
		"includes on a full": {includes(matrix.MaxCells, 1), "the axes and include give more than 256 cells"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := matrix.Load(reader(repo(tt.file, "lin")))
			if !errors.Is(err, matrix.ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLoadAcceptsTheLimit(t *testing.T) {
	for name, file := range map[string]string{"axes": axes(8, 2), "include": includes(matrix.MaxCells-1, 1)} {
		t.Run(name, func(t *testing.T) {
			m, envs := load(t, repo(file, "lin"))
			if cells := expand(t, m, envs, matrix.Scope{Set: matrix.ScopeAll}); len(cells) != matrix.MaxCells {
				t.Fatalf("cells = %d", len(cells))
			}
		})
	}
}

func TestExpandBoundsJobs(t *testing.T) {
	file := axes(8, 2) + "  - {name: more, command: [t], level: unit, reports: [r.xml], axes: {a0: [v0]}}\n"
	m, envs := load(t, repo(file, "lin"))
	_, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeAll})
	if !errors.Is(err, matrix.ErrInvalid) || !strings.Contains(err.Error(), "more than 256 jobs") {
		t.Fatalf("err = %v", err)
	}
	if cells := expand(t, m, envs, matrix.Scope{Suites: []string{"more"}}); len(cells) != 128 {
		t.Fatalf("cells = %d", len(cells))
	}
}

// Expand also holds for a matrix that did not come from Load.
func TestExpandRefusesHugeMatrices(t *testing.T) {
	m := testsys.Matrix{Axes: map[string][]string{}}
	for i := range 70 {
		m.Axes[fmt.Sprint("a", i)] = slices.Repeat([]string{"v"}, 10)
	}
	if _, err := matrix.Expand(m, nil, matrix.Scope{Set: matrix.ScopeAll}); !errors.Is(err, matrix.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadProblems(t *testing.T) {
	badEnv := repo(full, "lin")
	badEnv[testsys.EnvironmentsDir+"/win.yaml"] = &fstest.MapFile{Data: []byte("version: 1\nname: other\nprovider: machine\n")}
	tests := map[string]struct {
		fsys fstest.MapFS
		want []string
	}{
		"no matrix":          {fstest.MapFS{}, []string{".forecrew/matrix.yaml does not exist"}},
		"bad matrix":         {repo("version: 2"), []string{".forecrew/matrix.yaml"}},
		"missing envs":       {repo(full), []string{"environments/win.yaml does not exist", "environments/lin.yaml does not exist"}},
		"bad env":            {badEnv, []string{`name "other" must match the file name "win"`}},
		"no rule for a cell": {repo("version: 1\naxes: {os: [linux, mac]}\nenvironments: [{match: {os: linux}, environment: lin}]\n"+suites[:strings.Index(suites, "  - {name: e2e")], "lin"), []string{"no environment rule matches cell os=mac"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := matrix.Load(reader(tt.fsys))
			if !errors.Is(err, matrix.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want %q", err, want)
				}
			}
		})
	}
}

func TestLoadReadErrors(t *testing.T) {
	boom := errors.New("boom")
	failOn := func(file string) matrix.ReadFile {
		return func(name string) ([]byte, error) {
			if name == file {
				return nil, boom
			}
			return fs.ReadFile(repo(full, "win", "lin"), name)
		}
	}
	for _, file := range []string{testsys.MatrixFile, testsys.EnvironmentsDir + "/lin.yaml"} {
		_, _, err := matrix.Load(failOn(file))
		if !errors.Is(err, boom) || errors.Is(err, matrix.ErrInvalid) {
			t.Errorf("%s: err = %v", file, err)
		}
	}
}
