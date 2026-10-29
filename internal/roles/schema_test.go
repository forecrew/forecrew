package roles

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forecrew/forecrew"
)

// outputSchemas are the role output schemas; common.json only holds shared
// $defs.
var outputSchemas = []string{"profile-draft", "spec", "task-breakdown", "developer-result", "review-verdict"}

func TestSchemaExamples(t *testing.T) {
	for _, name := range outputSchemas {
		t.Run(name, func(t *testing.T) {
			schema, err := BundleSchema(forecrew.Files, path.Join("packs", "schemas", name+".json"))
			if err != nil {
				t.Fatalf("BundleSchema: %v", err)
			}
			if strings.Contains(string(schema), `"$ref"`) {
				t.Errorf("bundled schema still has a $ref: %s", schema)
			}
			valid := readExample(t, name+".valid.json")
			if err := ValidateOutput(schema, valid); err != nil {
				t.Errorf("valid example rejected: %v", err)
			}
			invalid := readExample(t, name+".invalid.json")
			if err := ValidateOutput(schema, invalid); !errors.Is(err, ErrInvalidOutput) {
				t.Errorf("invalid example: error = %v, want ErrInvalidOutput", err)
			}
		})
	}
}

// TestSchemasStrict checks that every object of the schemas the roles own
// requires all its properties and forbids others, which runtimes enforcing
// strict structured output (Codex) demand. The profile draft embeds the
// profile contract, which has optional fields.
func TestSchemasStrict(t *testing.T) {
	for _, name := range outputSchemas {
		if name == "profile-draft" {
			continue
		}
		schema, err := BundleSchema(forecrew.Files, path.Join("packs", "schemas", name+".json"))
		if err != nil {
			t.Fatalf("BundleSchema(%s): %v", name, err)
		}
		var v any
		if err := json.Unmarshal(schema, &v); err != nil {
			t.Fatal(err)
		}
		checkStrict(t, name, v)
	}
}

func checkStrict(t *testing.T, where string, v any) {
	t.Helper()
	switch v := v.(type) {
	case []any:
		for _, item := range v {
			checkStrict(t, where, item)
		}
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok {
			checkStrictObject(t, where, v, props)
		}
		for _, item := range v {
			checkStrict(t, where, item)
		}
	}
}

func checkStrictObject(t *testing.T, where string, obj, props map[string]any) {
	t.Helper()
	required, _ := obj["required"].([]any)
	for p := range props {
		if !slices.Contains(required, any(p)) {
			t.Errorf("%s: property %q is not required", where, p)
		}
	}
	if obj["additionalProperties"] != false {
		t.Errorf("%s: object with properties %v allows additional properties", where, required)
	}
}

func readExample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(path.Join("testdata", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBundleSchema(t *testing.T) {
	fsys := fstest.MapFS{
		"s/root.json": {Data: []byte(`{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"$id": "https://example.com/root.json",
			"type": "object",
			"properties": {
				"a": {"$ref": "#/$defs/a"},
				"b": {"$ref": "lib/common.json#/$defs/b", "description": "override"},
				"c": {"$ref": "lib/common.json#/$defs/list/1"},
				"d": {"$ref": "lib/common.json#/$defs/we~1ird~0key"},
				"e": {"$ref": "lib/whole.json"}
			},
			"$defs": {"a": {"type": "string"}}
		}`)},
		"s/lib/common.json": {Data: []byte(`{
			"$defs": {
				"b": {"type": "integer", "description": "base", "not": {"$ref": "#/$defs/zero"}},
				"zero": {"const": 0},
				"list": [{"type": "null"}, {"type": "boolean"}],
				"we/ird~key": {"type": "number"}
			}
		}`)},
		"s/lib/whole.json": {Data: []byte(`{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "x", "type": "array"}`)},
	}
	got, err := BundleSchema(fsys, "s/root.json")
	if err != nil {
		t.Fatalf("BundleSchema: %v", err)
	}
	want := `{"$schema":"https://json-schema.org/draft/2020-12/schema","properties":{` +
		`"a":{"type":"string"},` +
		`"b":{"description":"override","not":{"const":0},"type":"integer"},` +
		`"c":{"type":"boolean"},` +
		`"d":{"type":"number"},` +
		`"e":{"type":"array"}},"type":"object"}`
	if string(got) != want {
		t.Errorf("BundleSchema =\n%s\nwant\n%s", got, want)
	}
}

func TestBundleSchemaErrors(t *testing.T) {
	tests := []struct {
		name, root, want string
	}{
		{"missing file", `{"$ref": "nope.json"}`, "nope.json"},
		{"bad json", `{`, "unexpected EOF"},
		{"absolute URL", `{"$ref": "https://example.com/x.json"}`, "only relative file references"},
		{"outside", `{"$ref": "../../../x.json"}`, "outside the schema files"},
		{"not a pointer", `{"$ref": "#anchor"}`, "is not a JSON pointer"},
		{"missing key", `{"$ref": "#/$defs/nope", "$defs": {}}`, `no "nope"`},
		{"bad index", `{"$ref": "#/$defs/l/5", "$defs": {"l": []}}`, `no index "5"`},
		{"scalar parent", `{"$ref": "#/$defs/s/x", "$defs": {"s": 1}}`, "not in an object or array"},
		{"target not object", `{"$ref": "#/$defs/s", "$defs": {"s": 1}}`, "target is not a schema object"},
		{"cycle", `{"$ref": "#/$defs/a", "$defs": {"a": {"items": {"$ref": "#/$defs/a"}}}}`, "is there a cycle"},
		{"error in array", `{"allOf": [{"$ref": "#/nope"}]}`, `no "nope"`},
		{"error in sibling", `{"$ref": "#/$defs/a", "items": {"$ref": "#/nope"}, "$defs": {"a": {}}}`, `no "nope"`},
		{"meta-schema", `{"type": 5}`, "invalid schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{"a/b/root.json": {Data: []byte(tt.root)}}
			_, err := BundleSchema(fsys, "a/b/root.json")
			if !errors.Is(err, ErrInvalidSchema) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("BundleSchema error = %v, want ErrInvalidSchema containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateOutputErrors(t *testing.T) {
	if err := ValidateOutput([]byte(`{"type": 5}`), []byte(`{}`)); !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("bad schema: error = %v, want ErrInvalidSchema", err)
	}
	if err := ValidateOutput([]byte(`{`), []byte(`{}`)); !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("bad schema JSON: error = %v, want ErrInvalidSchema", err)
	}
	if err := ValidateOutput([]byte(`{"type": "object"}`), []byte(`{`)); !errors.Is(err, ErrInvalidOutput) {
		t.Errorf("bad output JSON: error = %v, want ErrInvalidOutput", err)
	}
	if err := ValidateOutput([]byte(`{"type": "object"}`), []byte(`{"a": 1}`)); err != nil {
		t.Errorf("valid output: %v", err)
	}
}
