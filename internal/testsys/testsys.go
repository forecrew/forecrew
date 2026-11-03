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
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/schema"
)

// ErrInvalid is wrapped by every validation error.
var ErrInvalid = schema.ErrInvalid

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

// validate runs the checks of one YAML file. decode fills the typed value
// from data or its JSON form; an error means the file has the wrong shape.
// check returns the problems with the rules of the typed value. When the
// file decodes, check runs first, as its messages are clearer; the schema
// then catches whatever check does not cover.
func validate(schemaName, sch string, data []byte, decode func(raw json.RawMessage) error, check func() error) error {
	raw, inst, err := schema.DecodeYAML(data)
	if err != nil {
		return err
	}
	decErr := decode(raw)
	if decErr == nil {
		if err := check(); err != nil {
			return err
		}
	}
	if err := schema.Check(schemaName, sch, inst); err != nil {
		return err
	}
	return decErr
}

// ValidateVerification checks a verification report (the JSON stored in
// verification_reports.report) against verification.schema.json.
func ValidateVerification(report []byte) error {
	inst, err := schema.DecodeJSON(report)
	if err != nil {
		return err
	}
	return schema.Check("verification.schema.json", verificationSchema, inst)
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
