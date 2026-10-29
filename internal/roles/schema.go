package roles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInvalidSchema means a schema file cannot be bundled or is not a valid
// JSON Schema.
var ErrInvalidSchema = errors.New("invalid schema")

// ErrInvalidOutput means a job output does not match its role's schema.
var ErrInvalidOutput = errors.New("output does not match schema")

// maxRefDepth bounds $ref nesting, so a reference cycle fails instead of
// recursing forever.
const maxRefDepth = 32

// BundleSchema reads the JSON Schema at name in fsys and returns it with every
// $ref replaced by the schema it points to, as job specs carry schemas
// inline. References are relative file paths with an optional JSON pointer
// fragment ("common.json#/$defs/text", "#/$defs/x"); keywords next to a $ref
// override those of its target. $defs, and $id outside the root, are dropped.
// The result is checked against the JSON Schema 2020-12 meta-schema and
// encoded with sorted keys, so it is deterministic.
func BundleSchema(fsys fs.FS, name string) (json.RawMessage, error) {
	b := bundler{fsys: fsys, docs: map[string]any{}}
	root, err := b.load(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSchema, err)
	}
	out, err := b.resolve(name, root, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidSchema, name, err)
	}
	if m, ok := out.(map[string]any); ok {
		delete(m, "$defs")
		delete(m, "$id")
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidSchema, name, err)
	}
	if _, err := compile(data); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidSchema, name, err)
	}
	return data, nil
}

// ValidateOutput checks a job output against a bundled schema.
func ValidateOutput(schema json.RawMessage, output []byte) error {
	s, err := compile(schema)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSchema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(output))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOutput, err)
	}
	if err := s.Validate(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOutput, err)
	}
	return nil
}

func compile(schema []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, err
	}
	const loc = "mem:///output.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(loc, doc); err != nil {
		return nil, err
	}
	return c.Compile(loc)
}

type bundler struct {
	fsys fs.FS
	docs map[string]any // decoded files by path
}

func (b *bundler) load(name string) (any, error) {
	if doc, ok := b.docs[name]; ok {
		return doc, nil
	}
	data, err := fs.ReadFile(b.fsys, name)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	b.docs[name] = doc
	return doc, nil
}

// resolve returns a copy of v, a value from the file doc, with every $ref
// inlined.
func (b *bundler) resolve(doc string, v any, depth int) (any, error) {
	switch v := v.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			r, err := b.resolve(doc, item, depth)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			return b.inline(doc, ref, v, depth)
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			r, err := b.resolve(doc, item, depth)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	}
	return v, nil
}

// inline resolves the $ref of node, found in the file doc, and merges the
// node's other keywords over the target.
func (b *bundler) inline(doc, ref string, node map[string]any, depth int) (any, error) {
	if depth >= maxRefDepth {
		return nil, fmt.Errorf("$ref %q: nested more than %d levels, is there a cycle?", ref, maxRefDepth)
	}
	file, pointer, _ := strings.Cut(ref, "#")
	target := doc
	if file != "" {
		if strings.Contains(file, ":") {
			return nil, fmt.Errorf("$ref %q: only relative file references are supported", ref)
		}
		target = path.Join(path.Dir(doc), file)
		if !fs.ValidPath(target) {
			return nil, fmt.Errorf("$ref %q: %q is outside the schema files", ref, target)
		}
	}
	root, err := b.load(target)
	if err != nil {
		return nil, fmt.Errorf("$ref %q: %w", ref, err)
	}
	sub, err := lookup(root, pointer)
	if err != nil {
		return nil, fmt.Errorf("$ref %q: %w", ref, err)
	}
	resolved, err := b.resolve(target, sub, depth+1)
	if err != nil {
		return nil, err
	}
	m, ok := resolved.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q: target is not a schema object", ref)
	}
	delete(m, "$id")
	delete(m, "$schema")
	delete(m, "$defs")
	for k, item := range node {
		if k == "$ref" {
			continue
		}
		r, err := b.resolve(doc, item, depth)
		if err != nil {
			return nil, err
		}
		m[k] = r
	}
	return m, nil
}

// lookup follows a JSON pointer (RFC 6901) such as "/$defs/text" in doc.
func lookup(doc any, pointer string) (any, error) {
	if pointer == "" {
		return doc, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("fragment %q is not a JSON pointer", pointer)
	}
	cur := doc
	for tok := range strings.SplitSeq(pointer[1:], "/") {
		tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[tok]
			if !ok {
				return nil, fmt.Errorf("pointer %q: no %q", pointer, tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("pointer %q: no index %q", pointer, tok)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("pointer %q: %q is not in an object or array", pointer, tok)
		}
	}
	return cur, nil
}
