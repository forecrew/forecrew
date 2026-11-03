package schema

import (
	"errors"
	"strings"
	"testing"
)

const person = `{"type": "object", "additionalProperties": false, "required": ["name"],
  "properties": {"name": {"type": "string"}, "age": {"type": "integer"}}}`

func TestCheck(t *testing.T) {
	tests := []struct {
		name, doc string
		want      string // substring of the error; empty means valid
	}{
		{"valid", "name: ann\nage: 3\n", ""},
		{"missing and extra", "age: x\nnick: a\n", "at '': additional properties 'nick' not allowed; at '': missing property 'name'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, inst, err := DecodeYAML([]byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			err = Check("person.json", person, inst)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Check = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Check = %v, want ErrInvalid with %q", err, tt.want)
			}
		})
	}
}

func TestCheckBadSchema(t *testing.T) {
	for name, s := range map[string]string{"not json": "{", "not a schema": `{"type": 3}`} {
		if err := Check("bad.json", s, map[string]any{}); err == nil || errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want a schema error", name, err)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, err := DecodeJSON([]byte("{")); !errors.Is(err, ErrInvalid) {
		t.Errorf("DecodeJSON = %v", err)
	}
	if _, _, err := DecodeYAML([]byte("a: [")); !errors.Is(err, ErrInvalid) {
		t.Errorf("DecodeYAML = %v", err)
	}
	if _, _, err := DecodeYAML([]byte("{1: [a]}\n")); err != nil {
		t.Errorf("DecodeYAML of a map with a number key = %v", err)
	}
	if _, _, err := DecodeYAML([]byte("? [a]\n: b\n")); !errors.Is(err, ErrInvalid) {
		t.Errorf("DecodeYAML of a list key = %v", err)
	}
}

func TestStrict(t *testing.T) {
	var out struct {
		Name string `json:"name"`
	}
	if err := Strict([]byte(`{"name": "ann"}`), &out); err != nil || out.Name != "ann" {
		t.Fatalf("Strict = %v, %+v", err, out)
	}
	if err := Strict([]byte(`{"nick": "a"}`), &out); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Strict unknown field = %v", err)
	}
}

func TestCheckContract(t *testing.T) {
	_, inst, err := DecodeYAML([]byte("version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckContract("matrix.schema.json", inst); !errors.Is(err, ErrInvalid) {
		t.Errorf("incomplete matrix = %v, want ErrInvalid", err)
	}
	if err := CheckContract("nope.schema.json", inst); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("unknown contract = %v", err)
	}
}
