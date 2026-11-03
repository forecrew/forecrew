// Package schema checks files and JSON documents against the JSON Schemas
// of docs/contracts. Packages that own a contract embed a copy of its
// schema (go:embed cannot reach docs/) and keep it equal with a test; they
// call Check, then apply the rules a schema cannot express.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew"
)

// ErrInvalid is wrapped by every error about the checked document (not
// about the schema itself).
var ErrInvalid = errors.New("invalid file")

// Base is the URL prefix of the contract schemas' $id.
const Base = "https://github.com/forecrew/forecrew/docs/contracts/"

// Check validates instance, decoded JSON (DecodeJSON, DecodeYAML), against
// the schema named name. Problems are sorted and joined with "; ", each as
// "at '<location>': <message>", wrapping ErrInvalid.
func Check(name, schema string, instance any) error {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(Base+name, doc); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	s, err := c.Compile(Base + name)
	if err != nil {
		return fmt.Errorf("compile %s: %w", name, err)
	}
	err = s.Validate(instance)
	if ve, ok := errors.AsType[*jsonschema.ValidationError](err); ok {
		return fmt.Errorf("%w: %s", ErrInvalid, leaves(ve))
	}
	return err
}

// CheckContract validates instance against docs/contracts/<name>, read
// from the embedded forecrew.Files, like Check.
func CheckContract(name string, instance any) error {
	sch, err := fs.ReadFile(forecrew.Files, "docs/contracts/"+name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return Check(name, string(sch), instance)
}

// leaves flattens a validation error into its leaf messages.
func leaves(ve *jsonschema.ValidationError) string {
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, e.Error()) // "at '/path': message"
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	slices.Sort(out) // the validator's order is not stable
	return strings.Join(out, "; ")
}

// DecodeJSON parses data into the value Check takes.
func DecodeJSON(data []byte) (any, error) {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: parse json: %w", ErrInvalid, err)
	}
	return inst, nil
}

// DecodeYAML returns data converted to JSON and as the value Check takes.
// YAML scalars keep their type, so 1.20 is the number 1.2 here.
func DecodeYAML(data []byte) (json.RawMessage, any, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: parse yaml: %w", ErrInvalid, err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: convert to json: %w", ErrInvalid, err)
	}
	inst, err := DecodeJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, inst, nil
}

// Strict decodes raw (JSON) into out through its json tags, refusing
// unknown fields, wrapping ErrInvalid.
func Strict(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}
