package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// findRefs returns the references of every ${...} in s.
func findRefs(s string) ([]string, error) {
	var refs []string
	for {
		_, rest, ok := strings.Cut(s, "${")
		if !ok {
			return refs, nil
		}
		ref, after, ok := strings.Cut(rest, "}")
		if !ok {
			return nil, fmt.Errorf("unterminated ${ in %q", s)
		}
		refs = append(refs, strings.TrimSpace(ref))
		s = after
	}
}

// singleRef returns the reference when s is exactly one ${...}.
func singleRef(s string) (string, bool) {
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") || strings.Count(s, "${") != 1 {
		return "", false
	}
	return strings.TrimSpace(s[2 : len(s)-1]), true
}

// scope holds the values references resolve against: the run inputs, the
// values of the steps that ran (step ID -> {"output": ..., "job_id": ...})
// and the variables item, index and iteration.
type scope struct {
	inputs map[string]any
	steps  map[string]any
	vars   map[string]any
}

// child returns a copy of s whose changes do not reach s.
func (s *scope) child() *scope {
	return &scope{inputs: s.inputs, steps: maps.Clone(s.steps), vars: maps.Clone(s.vars)}
}

// lookup returns the value of a reference, or nil when it is not set.
func (s *scope) lookup(ref string) any {
	parts := strings.Split(ref, ".")
	var v any
	switch parts[0] {
	case "inputs":
		v = s.inputs
	case "steps":
		v = s.steps
	default:
		v = s.vars[parts[0]]
	}
	for _, p := range parts[1:] {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// resolve replaces the references in v, a with value.
func (s *scope) resolve(v any) any {
	switch x := v.(type) {
	case string:
		return s.interpolate(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = s.resolve(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = s.resolve(e)
		}
		return out
	}
	return v
}

// interpolate resolves the references in str. Playbooks are validated, so
// every ${ is terminated.
func (s *scope) interpolate(str string) any {
	if ref, ok := singleRef(str); ok {
		return s.lookup(ref)
	}
	var b strings.Builder
	for {
		before, rest, ok := strings.Cut(str, "${")
		b.WriteString(before)
		if !ok {
			return b.String()
		}
		ref, after, _ := strings.Cut(rest, "}")
		b.WriteString(text(s.lookup(strings.TrimSpace(ref))))
		str = after
	}
}

// text formats a value for interpolation: nil is empty, lists and objects
// are JSON.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case map[string]any, []any:
		b, _ := json.Marshal(x) // decoded JSON and YAML values always marshal
		return string(b)
	}
	return fmt.Sprint(v)
}

// cond is a loop's until condition: <ref>, <ref> == <value> or
// <ref> != <value>.
type cond struct {
	ref string
	op  string // "", "==" or "!="
	val string
}

func parseCond(s string) (cond, error) {
	if strings.TrimSpace(s) == "" {
		return cond{}, errors.New("until is required")
	}
	c := cond{ref: s}
	for _, op := range []string{"==", "!="} {
		if left, right, ok := strings.Cut(s, op); ok {
			c = cond{ref: left, op: op, val: strings.Trim(strings.TrimSpace(right), `"'`)}
			break
		}
	}
	c.ref = strings.TrimSpace(c.ref)
	if ref, ok := singleRef(c.ref); ok {
		c.ref = ref
	}
	if c.ref == "" || strings.ContainsAny(c.ref, "${} ") {
		return cond{}, fmt.Errorf("until %q must be <ref>, <ref> == <value> or <ref> != <value>", s)
	}
	return c, nil
}

// holds evaluates the condition: a bare reference holds when its value is
// set and not false, empty or zero.
func (c cond) holds(s *scope) bool {
	v := s.lookup(c.ref)
	switch c.op {
	case "==":
		return text(v) == c.val
	case "!=":
		return text(v) != c.val
	}
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return true
}
