package testkit

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

var _ runtime.Adapter = (*Adapter)(nil)

// Adapter is a scripted runtime.Adapter: Run emits Events and returns
// Result and Err. Set the fields before use.
type Adapter struct {
	AdapterName string // returned by Name
	Version     string // returned by Detect; empty means not installed
	Events      []domain.JobEvent
	Result      runtime.Result
	Err         error

	mu    sync.Mutex
	specs []runtime.RunSpec
}

// Name implements runtime.Adapter.
func (a *Adapter) Name() string { return a.AdapterName }

// Detect implements runtime.Adapter.
func (a *Adapter) Detect(context.Context) (string, error) {
	if a.Version == "" {
		return "", fmt.Errorf("%s: %w", a.AdapterName, runtime.ErrNotInstalled)
	}
	return a.Version, nil
}

// Run implements runtime.Adapter. It stops early when ctx is done.
func (a *Adapter) Run(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
	a.mu.Lock()
	a.specs = append(a.specs, spec)
	a.mu.Unlock()
	for _, e := range a.Events {
		if err := ctx.Err(); err != nil {
			return runtime.Result{}, err
		}
		emit(e)
	}
	return a.Result, a.Err
}

// Specs returns the specs Run was called with.
func (a *Adapter) Specs() []runtime.RunSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.specs)
}
