package testkit

import (
	"context"
	"sync"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var _ workflow.SignalBus = (*SignalBus)(nil)

// subscriberBuffer is the channel size of a subscription; when it is full,
// further signals are dropped for that subscriber.
const subscriberBuffer = 64

// SignalBus is a workflow.SignalBus that stores signals in a store.
type SignalBus struct {
	signals store.SignalRepo

	mu   sync.Mutex
	subs map[chan domain.Signal]struct{}
}

// NewSignalBus returns a bus storing its signals in st.
func NewSignalBus(st store.Store) *SignalBus {
	return &SignalBus{signals: st.Signals(), subs: map[chan domain.Signal]struct{}{}}
}

// Publish implements workflow.SignalBus.
func (b *SignalBus) Publish(ctx context.Context, s domain.Signal) (bool, error) {
	inserted, err := b.signals.Insert(ctx, &s)
	if err != nil || !inserted {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- s:
		default:
		}
	}
	return true, nil
}

// Lookup implements workflow.SignalBus.
func (b *SignalBus) Lookup(ctx context.Context, accountID uuid.UUID, key string) ([]domain.Signal, error) {
	return b.signals.ListByKey(ctx, accountID, key)
}

// Subscribe implements workflow.SignalBus.
func (b *SignalBus) Subscribe(ctx context.Context) <-chan domain.Signal {
	ch := make(chan domain.Signal, subscriberBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		close(ch)
		b.mu.Unlock()
	}()
	return ch
}
