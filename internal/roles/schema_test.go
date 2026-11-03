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
var outputSchemas = []string{"profile-draft", "spec", "task-breakdown", "developer-result", "review-verdict",
	"test-plan", "environments", "test-map", "council-brief", "council-frame", "council-proposal", "council-critique", "council-check", "triage"}

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

// TestTestRoleSchemaRejects checks that each rule the test roles' consumers
// rely on is enforced: every case breaks one rule of a valid example.
func TestTestRoleSchemaRejects(t *testing.T) {
	tests := []struct {
		schema, name string
		edit         func(doc map[string]any)
	}{
		{"test-plan", "axis without reason", func(d map[string]any) { item(d, "axes", 0)["reason"] = " " }},
		{"test-plan", "axis without values", func(d map[string]any) { item(d, "axes", 0)["values"] = []any{} }},
		{"test-plan", "bad axis name", func(d map[string]any) { item(d, "axes", 0)["name"] = "OS" }},
		{"test-plan", "unknown matrix part", func(d map[string]any) { item(d, "matrix_changes", 0)["part"] = "services" }},
		{"test-plan", "change without reason", func(d map[string]any) { delete(item(d, "matrix_changes", 0), "reason") }},
		{"test-plan", "no open questions", func(d map[string]any) { delete(d, "open_questions") }},
		{"environments", "unknown provider", func(d map[string]any) { item(d, "environments", 0)["provider"] = "vm" }},
		{"environments", "bad name", func(d map[string]any) { item(d, "environments", 0)["name"] = "Ubuntu Latest" }},
		{"environments", "no reason", func(d map[string]any) { item(d, "environments", 0)["reason"] = "" }},
		{"environments", "unknown change", func(d map[string]any) { item(d, "environments", 0)["change"] = "rebuilt" }},
		{"test-map", "bad criterion", func(d map[string]any) { item(d, "tests", 0)["criterion"] = "1" }},
		{"test-map", "bad suite", func(d map[string]any) { item(d, "tests", 0)["suite"] = "Unit" }},
		{"test-map", "empty id", func(d map[string]any) { item(d, "tests", 0)["id"] = "" }},
		{"test-map", "absolute file", func(d map[string]any) { item(d, "tests", 0)["file"] = "/src/reset_test.go" }},
		{"test-map", "extra field", func(d map[string]any) { item(d, "tests", 0)["passed"] = false }},
		{"test-map", "untestable without reason", func(d map[string]any) { delete(item(d, "untestable", 0), "reason") }},
		{"test-map", "no untestable", func(d map[string]any) { delete(d, "untestable") }},
		{"test-map", "no files", func(d map[string]any) { delete(d, "files") }},
		{"test-map", "absolute listed file", func(d map[string]any) { d["files"] = []any{"/etc/passwd"} }},
	}
	for _, tt := range tests {
		t.Run(tt.schema+"/"+tt.name, func(t *testing.T) {
			schema, err := BundleSchema(forecrew.Files, path.Join("packs", "schemas", tt.schema+".json"))
			if err != nil {
				t.Fatalf("BundleSchema: %v", err)
			}
			var doc map[string]any
			if err := json.Unmarshal(readExample(t, tt.schema+".valid.json"), &doc); err != nil {
				t.Fatal(err)
			}
			tt.edit(doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateOutput(schema, data); !errors.Is(err, ErrInvalidOutput) {
				t.Errorf("error = %v, want ErrInvalidOutput", err)
			}
		})
	}
}

// item returns the i-th object of the list doc[key].
func item(doc map[string]any, key string, i int) map[string]any {
	return doc[key].([]any)[i].(map[string]any)
}

// TestTriageSchema checks the triage output: good outputs pass and each
// broken field is refused.
func TestTriageSchema(t *testing.T) {
	schema, err := BundleSchema(forecrew.Files, path.Join("packs", "schemas", "triage.json"))
	if err != nil {
		t.Fatalf("BundleSchema: %v", err)
	}
	good := `{"impact": "high", "reversibility": "irreversible", "areas": ["internal/billing"], "reason": "Moves charges.", "assumptions": []}`
	tests := []struct {
		name string
		doc  string
		ok   bool
	}{
		{"good", good, true},
		{"good with several areas", `{"impact": "low", "reversibility": "easy", "areas": ["web", "internal/cli"], "reason": "r", "assumptions": []}`, true},
		{"missing reason", `{"impact": "high", "reversibility": "easy", "areas": ["a"], "assumptions": []}`, false},
		{"unknown impact", strings.Replace(good, `"high"`, `"huge"`, 1), false},
		{"unknown reversibility", strings.Replace(good, `"irreversible"`, `"never"`, 1), false},
		{"no areas", strings.Replace(good, `["internal/billing"]`, `[]`, 1), false},
		{"blank area", strings.Replace(good, `["internal/billing"]`, `[" "]`, 1), false},
		{"blank reason", strings.Replace(good, `"Moves charges."`, `""`, 1), false},
		{"extra field", strings.Replace(good, `"assumptions": []`, `"assumptions": [], "size": "big"`, 1), false},
		{"assumption without verify_by", `{"impact": "high", "reversibility": "easy", "areas": ["a"], "reason": "r",
			"assumptions": [{"statement": "s", "confidence": "low", "impact": "low"}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOutput(schema, []byte(tt.doc))
			if tt.ok && err != nil {
				t.Errorf("valid output rejected: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidOutput) {
				t.Errorf("error = %v, want ErrInvalidOutput", err)
			}
		})
	}
}
