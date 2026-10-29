package workflow

import (
	"context"
	"sync"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

var _ SignalBus = (*Bus)(nil)

// SubscriberBuffer is the channel size of a subscription; when it is full,
// further signals are dropped for that subscriber.
const SubscriberBuffer = 64

// Bus is the SignalBus on a store's signals table. Subscribers are in
// process: the engine subscribes and wakes the runs waiting on the key.
type Bus struct {
	signals store.SignalRepo

	mu   sync.Mutex
	subs map[chan domain.Signal]struct{}
}

// NewBus returns a bus storing its signals in signals.
func NewBus(signals store.SignalRepo) *Bus {
	return &Bus{signals: signals, subs: map[chan domain.Signal]struct{}{}}
}

// Publish implements SignalBus.
func (b *Bus) Publish(ctx context.Context, s domain.Signal) (bool, error) {
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

// Lookup implements SignalBus.
func (b *Bus) Lookup(ctx context.Context, accountID uuid.UUID, key string) ([]domain.Signal, error) {
	return b.signals.ListByKey(ctx, accountID, key)
}

// Subscribe implements SignalBus.
func (b *Bus) Subscribe(ctx context.Context) <-chan domain.Signal {
	ch := make(chan domain.Signal, SubscriberBuffer)
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
