package workflow

import (
	"reflect"
	"testing"
)

func testScope() *scope {
	return &scope{
		inputs: map[string]any{"repo": "acme/app", "n": 3},
		steps: map[string]any{
			"spec": map[string]any{
				"output":   map[string]any{"criteria": []any{"fast", "safe"}, "count": float64(2), "ok": true, "tasks": []any{map[string]any{"title": "a"}}},
				"head_sha": "abc",
			},
		},
		vars: map[string]any{"item": map[string]any{"title": "Add X"}, "index": 1},
	}
}

func TestResolve(t *testing.T) {
	s := testScope()
	tests := []struct {
		in   any
		want any
	}{
		{"plain", "plain"},
		{"${inputs.repo}", "acme/app"},
		{"${ inputs.n }", 3},
		{"${steps.spec.output.criteria}", []any{"fast", "safe"}},
		{"${steps.spec.output.criteria.1}", "safe"},
		{"${steps.spec.output.criteria.2}", nil},
		{"${steps.spec.output.criteria.x}", nil},
		{"${steps.spec.output.count.x}", nil},
		{"${steps.spec.output.tasks.0.title}", "a"},
		{"${steps.missing.output}", nil},
		{"${item.title}", "Add X"},
		{"repo ${inputs.repo} #${inputs.n} sha=${steps.spec.head_sha}", "repo acme/app #3 sha=abc"},
		{"n=${steps.spec.output.count} ok=${steps.spec.output.ok} none=${steps.x.output}", "n=2 ok=true none="},
		{"c=${steps.spec.output.criteria}", `c=["fast","safe"]`},
		{map[string]any{"a": []any{"${index}", 4}}, map[string]any{"a": []any{1, 4}}},
		{7, 7},
	}
	for _, tt := range tests {
		if got := s.resolve(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("resolve(%v) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestChildScope(t *testing.T) {
	s := testScope()
	c := s.child()
	c.steps["new"] = 1
	c.vars["item"] = "x"
	if s.steps["new"] != nil || s.vars["item"] == "x" {
		t.Fatal("child changed its parent")
	}
}

func TestFindRefs(t *testing.T) {
	refs, err := findRefs("a ${x.y} b ${ z }")
	if err != nil || !reflect.DeepEqual(refs, []string{"x.y", "z"}) {
		t.Fatalf("findRefs = %v, %v", refs, err)
	}
	if _, err := findRefs("${x"); err == nil {
		t.Fatal("unterminated ref accepted")
	}
	for s, want := range map[string]bool{"${a}": true, "${a}${b}": false, "x${a}": false, "${a": false} {
		if _, ok := singleRef(s); ok != want {
			t.Errorf("singleRef(%q) = %v", s, ok)
		}
	}
}

func TestCond(t *testing.T) {
	s := testScope()
	tests := []struct {
		until string
		want  bool
	}{
		{"steps.spec.head_sha == abc", true},
		{"steps.spec.head_sha == 'abc'", true},
		{`${steps.spec.head_sha} != "abc"`, false},
		{"steps.spec.output.count == 2", true},
		{"steps.spec.output.ok", true},
		{"steps.spec.output.count", true},
		{"steps.spec.output.criteria", true},
		{"inputs.n", true},
		{"index", true},
		{"steps.missing.output", false},
		{"steps.spec.output.nope == ", true},
	}
	for _, tt := range tests {
		c, err := parseCond(tt.until)
		if err != nil {
			t.Fatalf("parseCond(%q): %v", tt.until, err)
		}
		if got := c.holds(s); got != tt.want {
			t.Errorf("%q = %v, want %v", tt.until, got, tt.want)
		}
	}
	falsy := &scope{vars: map[string]any{"b": false, "s": "", "f": float64(0), "i": 0}}
	for _, ref := range []string{"b", "s", "f", "i"} {
		if (cond{ref: ref}).holds(falsy) {
			t.Errorf("%s holds", ref)
		}
	}
	for _, bad := range []string{"", "  ", "== x", "a b", "${a"} {
		if _, err := parseCond(bad); err == nil {
			t.Errorf("parseCond(%q) accepted", bad)
		}
	}
}
