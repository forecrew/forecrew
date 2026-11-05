package runtime

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnknownAdapter means no adapter is registered under the name.
var ErrUnknownAdapter = errors.New("runtime: unknown adapter")

// Registry looks adapters up by name (the `adapter` field of the runtimes
// config).
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns a Registry holding adapters, keyed by Name. A later
// adapter replaces an earlier one with the same name.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, a := range adapters {
		r.adapters[a.Name()] = a
	}
	return r
}

// DefaultRegistry returns a Registry with the claude-code, codex and opencode
// adapters, each looking for its CLI in PATH. env and now are the Config fields of the
// same name.
func DefaultRegistry(env []string, now func() time.Time) *Registry {
	cfg := Config{Env: env, Now: now}
	return NewRegistry(NewClaudeCode(cfg), NewCodex(cfg), NewOpencode(cfg))
}

// Get returns the adapter registered as name, or ErrUnknownAdapter.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("adapter %q: %w", name, ErrUnknownAdapter)
	}
	return a, nil
}
