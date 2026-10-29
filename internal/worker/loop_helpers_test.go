package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

func (a *fakeAPI) enqueue(specs ...domain.JobSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = append(a.queue, specs...)
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs the worker in the background and returns the channel of its
// result.
func (e *testEnv) start(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.w.Run(ctx) }()
	return done
}

// waitRun waits for Run to return.
func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

// gate is an adapter body that counts the jobs running at once and blocks
// each until released.
type gate struct {
	mu      sync.Mutex
	running int
	peak    int
	started int
	release chan struct{}
}

func newGate() *gate { return &gate{release: make(chan struct{}, 16)} }

func (g *gate) adapter() funcAdapter {
	return funcAdapter{fn: func(ctx context.Context, _ runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		g.mu.Lock()
		g.running++
		g.started++
		g.peak = max(g.peak, g.running)
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.running--
			g.mu.Unlock()
		}()
		select {
		case <-g.release:
			return runtime.Result{}, nil
		case <-ctx.Done():
			return runtime.Result{}, ctx.Err()
		}
	}}
}

func (g *gate) counts() (running, peak, started int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running, g.peak, g.started
}
