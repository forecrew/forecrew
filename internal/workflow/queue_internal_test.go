package workflow

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"
)

func TestWorkQueue(t *testing.T) {
	q := newWorkQueue()
	a, b := runKey{run: uuid.NewV7()}, runKey{run: uuid.NewV7()}
	q.push(a)
	q.push(a)
	q.push(b)
	ctx := t.Context()
	if k, ok := q.pop(ctx); !ok || k != a {
		t.Fatalf("pop = %v, %v", k, ok)
	}
	q.push(a) // a is active: advanced again after done
	if k, _ := q.pop(ctx); k != b {
		t.Fatalf("pop = %v, want b", k)
	}
	q.done(b)
	q.done(a)
	if k, _ := q.pop(ctx); k != a {
		t.Fatalf("pop = %v, want a again", k)
	}
	q.done(a)
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, ok := q.pop(short); ok {
		t.Fatal("pop on an empty queue returned an item")
	}
}

func TestWorkQueueExclusive(t *testing.T) {
	q := newWorkQueue()
	keys := []runKey{{run: uuid.NewV7()}, {run: uuid.NewV7()}}
	var active [2]atomic.Int32
	var processed atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				k, ok := q.pop(ctx)
				if !ok {
					return
				}
				i := 0
				if k == keys[1] {
					i = 1
				}
				if active[i].Add(1) != 1 {
					t.Error("run advanced by two workers at once")
				}
				time.Sleep(50 * time.Microsecond)
				active[i].Add(-1)
				processed.Add(1)
				q.done(k)
			}
		})
	}
	for i := range 2000 {
		q.push(keys[i%2])
	}
	for processed.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
}
