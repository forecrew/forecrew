package schema

import (
	"encoding/json"
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

// jobMCPTools are the tools of docs/contracts/job-mcp.md.
var jobMCPTools = []string{"kb_search", "kb_propose", "request_access", "assumption_record", "evidence_attach",
	"notes_write", "ask_human", "report_progress", "council_read"}

// TestJobMCPSchemas: every gateway tool has an input and an output schema,
// and they accept and refuse sample calls.
func TestJobMCPSchemas(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "job-mcp.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	defs := doc["$defs"].(map[string]any)
	for _, tool := range jobMCPTools {
		if defs[tool+"_input"] == nil || defs[tool+"_output"] == nil {
			t.Errorf("%s lacks an input or output schema", tool)
		}
	}
	assumption := `"assumption": {"statement": "Nobody uses IE", "confidence": "medium", "impact": "low", "verify_by": "ask-human"}`
	tests := []struct {
		def, instance string
		ok            bool
	}{
		{"kb_search_input", `{"query": "lease expiry", "kinds": ["note"], "limit": 5}`, true},
		{"kb_search_input", `{"query": " ", "limit": 50}`, false},
		{"ask_human_input", `{"question": "Which browsers?", "timeout": "10m", ` + assumption + `}`, true},
		{"ask_human_input", `{"question": "Which browsers?", "timeout": "10m"}`, false},
		{"ask_human_output", `{"status": "no_answer", "gate": "GATE-4", "assumption": "A-7"}`, true},
		{"notes_write_input", `{"scope": "team", "title": "t", "body": "b"}`, false},
		{"council_read_input", `{}`, true},
		{"council_read_output", `{"council": "COUNCIL-2", "question": "q", "constraints": [], "stage": "critique", "round": 1,
			"contributions": [{"member": "architect", "runtime": "codex", "stage": "proposal", "round": 1, "content": {},
			"evidence_check": [{"source": "http://10.0.0.1/x", "kind": "url", "status": "refused"}]}]}`, true},
		{"council_read_output", `{"council": "COUNCIL-2", "question": "q", "constraints": [], "stage": "critique", "round": 1,
			"contributions": [{"member": "architect", "runtime": "codex", "stage": "proposal", "round": 1, "content": {}}]}`, false},
		{"kb_propose_output", `{"proposal": "KB-1", "status": "queued", "batch": "per_goal", "message": "waits for the learn PR"}`, true},
	}
	for _, tt := range tests {
		doc["$ref"] = "#/$defs/" + tt.def
		sch, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := DecodeJSON([]byte(tt.instance))
		if err != nil {
			t.Fatal(err)
		}
		if err := Check("job-mcp.schema.json", string(sch), inst); (err == nil) != tt.ok {
			t.Errorf("%s %s: %v, want ok %v", tt.def, tt.instance, err, tt.ok)
		}
	}
}
