package schema

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContractSchemasCompile: every schema in docs/contracts is a valid
// JSON Schema whose $id matches its file.
func TestContractSchemasCompile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "contracts", "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("schemas: %v, %v", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// {} may fail validation, but the schema itself must compile.
		if err := Check(filepath.Base(f), string(data), map[string]any{}); err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", f, err)
		}
		if strings.Contains(string(data), `"$id"`) && !strings.Contains(string(data), `"$id": "`+Base+filepath.Base(f)+`"`) {
			t.Errorf("%s: $id does not name the file", f)
		}
	}
}

// TestCatalogFixtures checks testdata/catalog against catalog.schema.json;
// the catalog is loaded by the advisor (T50).
func TestCatalogFixtures(t *testing.T) {
	sch, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "catalog.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"valid", "invalid"} {
		files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "catalog", outcome, "*.yaml"))
		if len(files) == 0 {
			t.Fatalf("no %s catalog fixtures", outcome)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, inst, err := DecodeYAML(data)
			if err != nil {
				t.Fatal(err)
			}
			err = Check("catalog.schema.json", string(sch), inst)
			if (outcome == "valid") != (err == nil) || (err != nil && !errors.Is(err, ErrInvalid)) {
				t.Errorf("%s: %v", f, err)
			}
		}
	}
}
