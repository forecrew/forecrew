package testkit

import (
	"context"
	"slices"
	"sync"

	"github.com/forecrew/forecrew/internal/analyzer"
)

var _ analyzer.Analyzer = (*Analyzer)(nil)

// Analyzer is an analyzer.Analyzer that returns Facts and Err.
type Analyzer struct {
	Facts analyzer.Facts
	Err   error

	mu   sync.Mutex
	dirs []string
}

// Analyze implements analyzer.Analyzer.
func (a *Analyzer) Analyze(_ context.Context, dir string) (analyzer.Facts, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dirs = append(a.dirs, dir)
	return a.Facts, a.Err
}

// Dirs returns the directories Analyze was called with.
func (a *Analyzer) Dirs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.dirs)
}
