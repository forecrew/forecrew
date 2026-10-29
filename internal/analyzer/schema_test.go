package analyzer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// profileSchemaURL is the $id of the profile schema.
const profileSchemaURL = "https://github.com/forecrew/forecrew/docs/contracts/profile.schema.json"

func TestProfileSchemaCompilesAndAcceptsMinimalProfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "profile.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(profileSchemaURL, doc); err != nil {
		t.Fatalf("add schema: %v", err)
	}
	schema, err := c.Compile(profileSchemaURL)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{"minimal profile", `{"version":1,"name":"shop","summary":"A shop."}`, false},
		{"wrong version", `{"version":2,"name":"shop","summary":"A shop."}`, true},
		{"missing name", `{"version":1,"summary":"A shop."}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(tt.json))
			if err != nil {
				t.Fatalf("parse instance: %v", err)
			}
			if err := schema.Validate(inst); (err != nil) != tt.wantErr {
				t.Errorf("Validate = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
