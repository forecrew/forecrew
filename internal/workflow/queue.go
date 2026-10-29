package workflow

import (
	"context"
	"sync"
	"uuid"
)

// runKey names a workflow run.
type runKey struct {
	account uuid.UUID
	run     uuid.UUID
}

// workQueue holds "advance run X" items. A run is queued at most once and
// handed to one worker at a time: pushing a run that is being advanced marks
// it to be advanced again when that worker is done, so concurrent items for
// one run never execute a step twice.
type workQueue struct {
	mu      sync.Mutex
	pending []runKey
	queued  map[runKey]bool
	active  map[runKey]bool
	again   map[runKey]bool
	wake    chan struct{}
}

func newWorkQueue() *workQueue {
	return &workQueue{
		queued: map[runKey]bool{}, active: map[runKey]bool{}, again: map[runKey]bool{},
		wake: make(chan struct{}, 1),
	}
}

// push queues k unless it is queued already.
func (q *workQueue) push(k runKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.active[k]:
		q.again[k] = true
	case !q.queued[k]:
		q.queued[k] = true
		q.pending = append(q.pending, k)
		q.notify()
	}
}

// notify wakes one waiting pop; q.mu is held.
func (q *workQueue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pop waits for the next item and marks it active. It returns false when ctx
// is done.
func (q *workQueue) pop(ctx context.Context) (runKey, bool) {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			k := q.pending[0]
			q.pending = q.pending[1:]
			delete(q.queued, k)
			q.active[k] = true
			if len(q.pending) > 0 {
				q.notify()
			}
			q.mu.Unlock()
			return k, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return runKey{}, false
		case <-q.wake:
		}
	}
}

// done ends the processing of k and queues it again if it was pushed
// meanwhile.
func (q *workQueue) done(k runKey) {
	q.mu.Lock()
	delete(q.active, k)
	again := q.again[k]
	delete(q.again, k)
	q.mu.Unlock()
	if again {
		q.push(k)
	}
}
