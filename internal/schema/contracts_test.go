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

// checkDef validates instance against one $def of a contract schema.
func checkDef(t *testing.T, file, def string, instance any) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", file))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["$ref"] = "#/$defs/" + def
	sch, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return Check(file, string(sch), instance)
}

// TestFleetFixtures checks testdata/fleet against fleet.schema.json: a
// file <def>-<case>.yaml is an instance of that $def.
func TestFleetFixtures(t *testing.T) {
	seen := map[string]bool{}
	for _, outcome := range []string{"valid", "invalid"} {
		files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "fleet", outcome, "*.yaml"))
		for _, f := range files {
			def, _, _ := strings.Cut(filepath.Base(f), "-")
			seen[outcome+" "+def] = true
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, inst, err := DecodeYAML(data)
			if err != nil {
				t.Fatal(err)
			}
			err = checkDef(t, "fleet.schema.json", def, inst)
			if (outcome == "valid") != (err == nil) || (err != nil && !errors.Is(err, ErrInvalid)) {
				t.Errorf("%s: %v", f, err)
			}
		}
	}
	for _, def := range []string{"shape", "provider", "fleet_policy", "image", "resources"} {
		if !seen["valid "+def] || !seen["invalid "+def] {
			t.Errorf("%s lacks valid or invalid fixtures", def)
		}
	}
}

// TestFleetAgentSchemas: the messages of fleet-agent-api.md accept and
// refuse sample bodies.
func TestFleetAgentSchemas(t *testing.T) {
	id := `"018f4e2a-7b1c-7000-8000-000000000001"`
	desired := `{"machine_id": ` + id + `, "shape": {}, "image": {}, "tags": {"forecrew:machine": "x"}, "network_allow": []`
	build := `{"id": ` + id + `, "state": "requested", "version": "2026.11.1", "iso": "D:\\isos\\server.iso", "iso_checksum": "sha256:` + strings.Repeat("a", 64) +
		`", "license_confirmed": true, "chocolatey_sha256": "` + strings.Repeat("b", 64) + `"`
	tests := []struct {
		def, instance string
		ok            bool
	}{
		{"join_request", `{"code": "K7PQ-2M4X", "claim": "` + strings.Repeat("a", 43) + `", "kind": "microvm", "facts": {"host": "nas"}}`, true},
		{"join_request", `{"code": "K7PQ-2M4X", "claim": "short", "kind": "microvm", "facts": {}}`, false},
		{"join_response", `{"status": "waiting", "retry_after_seconds": 5}`, true},
		{"join_response", `{"status": "waiting", "retry_after_seconds": 5, "token": "fcp_x"}`, false},
		{"join_response", `{"status": "confirmed", "token": "fcp_x"}`, true},
		{"join_response", `{"status": "confirmed"}`, false},
		{"register_request", `{"capabilities": {"os": ["linux"]}}`, true},
		{"register_request", `{}`, false},
		{"register_response", `{"id": ` + id + `, "name": "home", "kind": "microvm", "status": "paused", "limits": {}, "tags": {}, "poll_timeout_seconds": 30}`, true},
		{"poll_request", `{"generation": "7", "have": [` + id + `]}`, true},
		{"poll_request", `{"generation": 7}`, false},
		{"poll_request", `{"have": ["not-a-machine", 7]}`, false},
		{"poll_response", `{"generation": "8", "machines": [` + desired + `}]}`, true},
		{"poll_response", `{"generation": "8", "machines": [` + desired + `, "bootstrap": {"server_url": "https://f.example.com", "token": "fcb_x"}}]}`, true},
		{"poll_response", `{"generation": "8", "machines": [` + desired + `, "bootstrap": {"server_url": "https://f.example.com", "token": "fcw_x"}}]}`, false},
		{"poll_response", `{"machines": []}`, false},
		{"state_report", `{"state": "booting", "external_id": "vm-1"}`, true},
		{"state_report", `{"state": "booting"}`, false},
		{"state_report", `{"state": "failed", "error": "no KVM"}`, true},
		{"state_report", `{"state": "ready"}`, false},
		{"state_report", `{"state": "failed", "error": "stratum", "abuse": "mining"}`, true},
		{"state_report", `{"state": "destroyed", "abuse": "mining"}`, false},
		{"state_report", `{"state": "failed", "abuse": "spam"}`, false},
		{"poll_response", `{"generation": "8", "machines": [], "image_builds": [` + build + `}]}`, true},
		{"poll_response", `{"generation": "8", "machines": [], "image_builds": [` + build + `, "x": 1}]}`, false},
		{"poll_response", `{"generation": "8", "machines": [], "image_builds": [{"id": ` + id + `, "state": "failed"}]}`, false},
		{"image_build_report", `{"state": "running", "step": "checking the ISO", "percent": 10}`, true},
		{"image_build_report", `{"state": "done", "step": "done", "percent": 100, "manifest": "D:\\b\\m.json", "digest": "sha256:` + strings.Repeat("c", 64) + `"}`, true},
		{"image_build_report", `{"state": "done", "step": "done", "percent": 100}`, false},
		{"image_build_report", `{"state": "failed", "step": "failed", "percent": 15, "error": "the ISO is not the one you named"}`, true},
		{"image_build_report", `{"state": "failed", "step": "failed", "percent": 15}`, false},
		{"image_build_report", `{"state": "requested", "step": "x", "percent": 0}`, false},
		{"image_build_report", `{"state": "running", "step": "x", "percent": 101}`, false},
		{"reconcile_request", `{"instances": [{"external_id": "vm-1", "machine_id": ` + id + `, "running": true}]}`, true},
		{"reconcile_request", `{"instances": [{"machine_id": ` + id + `, "running": true}]}`, false},
		{"reconcile_response", `{"destroy": ["vm-1"]}`, true},
	}
	for _, tt := range tests {
		inst, err := DecodeJSON([]byte(tt.instance))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkDef(t, "fleet-agent-api.schema.json", tt.def, inst); (err == nil) != tt.ok {
			t.Errorf("%s %s: %v", tt.def, tt.instance, err)
		}
	}
}

// jobMCPTools are the tools of docs/contracts/job-mcp.md.
var jobMCPTools = []string{"kb_search", "kb_propose", "request_access", "assumption_record", "evidence_attach",
	"notes_write", "ask_human", "report_progress", "council_read", "config_read", "config_plan"}

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
