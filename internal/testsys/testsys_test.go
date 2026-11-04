package testsys

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/schema"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/golden/testsys")

var (
	fixtures = filepath.Join("..", "..", "testdata", "testsys")
	goldens  = filepath.Join("..", "..", "testdata", "golden", "testsys")
)

// TestSchemasMatchContracts keeps the embedded copies equal to
// docs/contracts.
func TestSchemasMatchContracts(t *testing.T) {
	for name, embedded := range map[string]string{
		"environment.schema.json":  environmentSchema,
		"matrix.schema.json":       matrixSchema,
		"toolbox.schema.json":      toolboxSchema,
		"verification.schema.json": verificationSchema,
	} {
		contract, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(contract) != embedded {
			t.Errorf("internal/testsys/%s differs from docs/contracts/%s; copy the contract over it", name, name)
		}
	}
}

// parsers maps a fixture directory to the function under test; each
// returns what the golden file holds.
func parsers(t *testing.T) map[string]func(file string, data []byte) (any, error) {
	envs := validEnvironmentNames(t)
	return map[string]func(string, []byte) (any, error){
		"environments": func(file string, data []byte) (any, error) { return ParseEnvironment(file, data) },
		"matrix":       func(_ string, data []byte) (any, error) { return ParseMatrix(data, envs) },
		"toolboxes":    func(file string, data []byte) (any, error) { return ParseToolbox(file, data) },
		"verification": func(_ string, data []byte) (any, error) { return nil, ValidateVerification(data) },
	}
}

func validEnvironmentNames(t *testing.T) []string {
	files, err := filepath.Glob(filepath.Join(fixtures, "environments", "valid", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("environment fixtures: %v, %v", files, err)
	}
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".yaml"))
	}
	return names
}

// TestGolden runs every fixture under testdata/testsys: valid ones must
// parse, invalid ones must fail with ErrInvalid, and the result or the
// error must equal its golden file.
func TestGolden(t *testing.T) {
	for kind, parse := range parsers(t) {
		for _, outcome := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join(fixtures, kind, outcome, "*"))
			if err != nil || len(files) == 0 {
				t.Fatalf("%s/%s fixtures: %v, %v", kind, outcome, files, err)
			}
			for _, file := range files {
				name := kind + "/" + outcome + "/" + filepath.Base(file)
				t.Run(name, func(t *testing.T) { checkFixture(t, file, outcome == "valid", parse, name) })
			}
		}
	}
}

func checkFixture(t *testing.T, file string, valid bool, parse func(string, []byte) (any, error), name string) {
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse(filepath.Base(file), data)
	var out []byte
	switch {
	case valid && err != nil:
		t.Fatalf("valid fixture refused: %v", err)
	case !valid && !errors.Is(err, ErrInvalid):
		t.Fatalf("invalid fixture: err = %v, want ErrInvalid", err)
	case err != nil:
		out = []byte(err.Error() + "\n")
	default:
		if out, err = json.MarshalIndent(got, "", "  "); err != nil {
			t.Fatal(err)
		}
		out = append(out, '\n')
	}
	checkGolden(t, filepath.Join(goldens, name+".golden"), out)
}

// TestSchemasAloneAcceptValidFixtures checks the contracts by themselves,
// as an editor or another implementation would use them.
func TestSchemasAloneAcceptValidFixtures(t *testing.T) {
	schemas := map[string]string{"environments": environmentSchema, "matrix": matrixSchema, "toolboxes": toolboxSchema, "verification": verificationSchema}
	for kind, sch := range schemas {
		files, _ := filepath.Glob(filepath.Join(fixtures, kind, "valid", "*"))
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var inst any
			if kind == "verification" {
				inst, err = schema.DecodeJSON(data)
			} else {
				_, inst, err = schema.DecodeYAML(data)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Check(kind, sch, inst); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
	}
}

func checkGolden(t *testing.T, golden string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s; run go test ./internal/testsys -update\n%s", golden, got)
	}
}

// TestSchemasAloneRejectStructuralFixtures: the invalid fixtures whose
// problem is one of structure are refused by the schema by itself too. The
// others break cross-references only the Go rules check (file names, axes,
// environments, duplicates, paths with ..).
func TestSchemasAloneRejectStructuralFixtures(t *testing.T) {
	cases := map[string]string{
		"environments/invalid/secret-env.yaml":         environmentSchema,
		"environments/invalid/secret-service-env.yaml": environmentSchema,
		"environments/invalid/two-sources.yaml":        environmentSchema,
		"environments/invalid/no-source.yaml":          environmentSchema,
		"environments/invalid/actions-no-runner.yaml":  environmentSchema,
		"environments/invalid/machine-image.yaml":      environmentSchema,
		"environments/invalid/bad-provider.yaml":       environmentSchema,
		"environments/invalid/unknown-key.yaml":        environmentSchema,
		"matrix/invalid/bad-level.yaml":                matrixSchema,
		"matrix/invalid/no-reports.yaml":               matrixSchema,
		"matrix/invalid/no-version.yaml":               matrixSchema,
		"matrix/invalid/duplicate-value.yaml":          matrixSchema,
		"toolboxes/invalid/secret-env.yaml":            toolboxSchema,
		"toolboxes/invalid/no-image.yaml":              toolboxSchema,
	}
	for name, sch := range cases {
		data, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil {
			t.Fatal(err)
		}
		_, inst, err := schema.DecodeYAML(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Check(name, sch, inst); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: schema alone accepted it (%v)", name, err)
		}
	}
}

func TestMatrixHelpers(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures, "matrix", "valid", "full.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMatrix(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Axes["go"]; len(got) != 2 || got[0] != "1.26" {
		t.Fatalf("go axis = %q, want values as written", got)
	}
	unit, quirks := m.Suites[0], m.Suites[2]
	if !unit.IsRequired() || quirks.IsRequired() || !quirks.Quirks {
		t.Fatalf("required: unit %v, quirks %v", unit.IsRequired(), quirks.IsRequired())
	}
	if spec := unit.Command.Spec(); spec.Shell != "" || spec.Argv[0] != "gotestsum" {
		t.Fatalf("argv command = %+v", spec)
	}
	if spec := m.Suites[1].Command.Spec(); spec.Shell == "" || spec.Argv != nil {
		t.Fatalf("shell command = %+v", spec)
	}
	cell := Cell{"os": "windows", "go": "1.27", "db": "postgres-18"}
	if !cell.Matches(Cell{}) || !cell.Matches(Cell{"os": "windows"}) || cell.Matches(Cell{"os": "linux"}) || cell.Matches(Cell{"arch": "arm64"}) {
		t.Fatal("Cell.Matches")
	}
}

func TestParseErrorsNameTheFile(t *testing.T) {
	if _, err := ParseEnvironment("x.yaml", []byte("version: 1\nname: x\nprovider: machine\nports: nope\n")); !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), "x.yaml: ") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ParseMatrix([]byte(": :"), nil); !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), MatrixFile) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ParseToolbox("t.yaml", []byte("name: [")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if err := ValidateVerification([]byte("{")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	// Services need provider docker, in the schema as well as in Validate.
	for _, provider := range []string{"machine", "actions\nruns_on: ubuntu-24.04"} {
		if _, err := ParseEnvironment("p.yaml", []byte("version: 1\nname: p\nprovider: "+provider+"\nservices: {db: {image: b}}\n")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s with services: err = %v", provider, err)
		}
	}
	// A shape error the schema reports when the typed decode fails.
	if _, err := ParseEnvironment("p.yaml", []byte("version: 1\nname: p\nprovider: docker\nimage: a\nservices: {db: {image: b, ports: [x]}}\n")); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "/services/db/ports/0") {
		t.Fatalf("err = %v", err)
	}
}
