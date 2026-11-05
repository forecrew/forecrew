package runtime

import (
	"errors"
	"testing"
)

func TestRegistry(t *testing.T) {
	r := DefaultRegistry(nil, nil)
	for _, name := range []string{"claude-code", "codex", "opencode"} {
		a, err := r.Get(name)
		if err != nil || a.Name() != name {
			t.Errorf("Get(%q) = %v, %v", name, a, err)
		}
	}
	if _, err := r.Get("gemini"); !errors.Is(err, ErrUnknownAdapter) {
		t.Errorf("Get(gemini) err = %v", err)
	}

	s := NewScripted("script.json", Config{})
	r = NewRegistry(s, NewScripted("other.json", Config{}))
	if got, _ := r.Get("scripted"); got == s {
		t.Error("later adapter should replace the earlier one")
	}
}
