package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// InvalidOutputError is returned when the final output is missing, is not
// JSON, or does not match the output schema. Message is the validator
// message, suitable for a retry prompt. It matches ErrInvalidOutput.
type InvalidOutputError struct {
	Message string
}

// Error implements error.
func (e *InvalidOutputError) Error() string {
	return ErrInvalidOutput.Error() + ": " + e.Message
}

// Unwrap lets errors.Is(err, ErrInvalidOutput) match.
func (e *InvalidOutputError) Unwrap() error { return ErrInvalidOutput }

var fencedBlock = regexp.MustCompile("(?s)```[A-Za-z]*[ \\t]*\\n(.*?)```")

// ExtractOutput finds the structured output in the agent's final message and
// validates it against schema (an inline JSON Schema). The output is the last
// fenced block that holds valid JSON, or the whole text when that is JSON. An
// empty schema means free text and returns nil.
func ExtractOutput(text string, schema json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil, nil
	}
	sch, err := compileSchema(schema)
	if err != nil {
		return nil, err
	}
	raw := findJSON(text)
	if raw == nil {
		return nil, &InvalidOutputError{Message: "the final message has no JSON block; end it with one fenced ```json block"}
	}
	var doc any
	if doc, err = jsonschema.UnmarshalJSON(bytes.NewReader(raw)); err != nil {
		return nil, &InvalidOutputError{Message: "the JSON block is not valid JSON: " + err.Error()}
	}
	if err := sch.Validate(doc); err != nil {
		return nil, &InvalidOutputError{Message: err.Error()}
	}
	return raw, nil
}

func compileSchema(schema json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("parse output schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const url = "mem:///output.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("load output schema: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("compile output schema: %w", err)
	}
	return sch, nil
}

// findJSON returns the last fenced block that looks like JSON, falling back
// to the whole text. A block that is not valid JSON is returned as is when no
// better candidate exists, so the caller reports the parse error.
func findJSON(text string) json.RawMessage {
	var fallback json.RawMessage
	blocks := fencedBlock.FindAllStringSubmatch(text, -1)
	for _, block := range slices.Backward(blocks) {
		b := strings.TrimSpace(block[1])
		if json.Valid([]byte(b)) {
			return json.RawMessage(b)
		}
		if fallback == nil && strings.HasPrefix(b, "{") {
			fallback = json.RawMessage(b)
		}
	}
	if t := strings.TrimSpace(text); json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	return fallback
}
