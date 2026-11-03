// Package testsys validates the files that say where and how jobs run:
// environments (.forecrew/environments/<name>.yaml), the test matrix
// (.forecrew/matrix.yaml), toolboxes (packs/toolboxes/<name>.yaml) and
// verification reports. Each is checked against its JSON Schema in
// docs/contracts, then against the rules a schema cannot express.
//
// It is a package of its own rather than part of internal/analyzer, which
// detects facts in a repo; these files are written by agents, approved by
// humans and read by forecrewd and the workers at run time.
package testsys

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

// ErrInvalid is wrapped by every validation error.
var ErrInvalid = errors.New("invalid file")

// The schemas are copies of docs/contracts/*.schema.json (a test keeps
// them equal), since go:embed cannot reach outside the package.
var (
	//go:embed environment.schema.json
	environmentSchema string
	//go:embed matrix.schema.json
	matrixSchema string
	//go:embed toolbox.schema.json
	toolboxSchema string
	//go:embed verification.schema.json
	verificationSchema string
)

const schemaBase = "https://github.com/forecrew/forecrew/docs/contracts/"

// checkSchema validates instance, decoded JSON or YAML, against schema.
// Problems are joined with "; ", each as "at '<location>': <message>".
func checkSchema(name, schema string, instance any) error {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaBase+name, doc); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	s, err := c.Compile(schemaBase + name)
	if err != nil {
		return fmt.Errorf("compile %s: %w", name, err)
	}
	err = s.Validate(instance)
	if ve, ok := errors.AsType[*jsonschema.ValidationError](err); ok {
		return fmt.Errorf("%w: %s", ErrInvalid, schemaErrors(ve))
	}
	return err
}

// schemaErrors flattens a validation error into its leaf messages.
func schemaErrors(ve *jsonschema.ValidationError) string {
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

// decodeYAML returns data converted to JSON and as the value the schema
// validator takes. YAML scalars keep their type, so 1.20 is the number 1.2
// here; the matrix decodes its values from the YAML itself to keep them as
// written.
func decodeYAML(data []byte) (json.RawMessage, any, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: parse yaml: %w", ErrInvalid, err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: convert to json: %w", ErrInvalid, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: parse json: %w", ErrInvalid, err)
	}
	return raw, inst, nil
}

// validate runs the checks of one YAML file. decode fills the typed value
// from data or its JSON form; an error means the file has the wrong shape.
// check returns the problems with the rules of the typed value. When the
// file decodes, check runs first, as its messages are clearer; the schema
// then catches whatever check does not cover.
func validate(schemaName, schema string, data []byte, decode func(raw json.RawMessage) error, check func() error) error {
	raw, inst, err := decodeYAML(data)
	if err != nil {
		return err
	}
	decErr := decode(raw)
	if decErr == nil {
		if err := check(); err != nil {
			return err
		}
	}
	if err := checkSchema(schemaName, schema, inst); err != nil {
		return err
	}
	return decErr
}

// decodeJSON decodes raw into out through its json tags, refusing unknown
// fields.
func decodeJSON(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// ValidateVerification checks a verification report (the JSON stored in
// verification_reports.report) against verification.schema.json.
func ValidateVerification(report []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(report))
	if err != nil {
		return fmt.Errorf("%w: parse json: %w", ErrInvalid, err)
	}
	return checkSchema("verification.schema.json", verificationSchema, inst)
}

// decodeStrict decodes YAML into out, refusing unknown keys.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}
