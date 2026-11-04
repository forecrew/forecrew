package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// Batch limits of the events endpoint: 500 events and 1 MiB per request.
const (
	maxBatchEvents = 500
	maxBatchBytes  = 512 << 10
	flushTimeout   = 30 * time.Second
)

// eventBuffer collects the events of one job and sends them in batches.
// Secrets are redacted before an event is stored.
type eventBuffer struct {
	jobID  uuid.UUID
	client *Client
	red    *runtime.Redactor
	now    func() time.Time
	log    *slog.Logger

	mu      sync.Mutex // guards pending
	pending []domain.JobEvent
	sendMu  sync.Mutex // keeps batches in order
}

// add stores an event, redacting its text and data.
func (b *eventBuffer) add(ev domain.JobEvent) {
	if ev.Time.IsZero() {
		ev.Time = b.now()
	}
	ev.Time = ev.Time.UTC()
	ev.Text = cleanText(b.red.String(ev.Text))
	if len(ev.Data) > 0 {
		data := json.RawMessage(b.red.String(string(ev.Data)))
		if json.Valid(data) {
			ev.Data = data
		} else {
			ev.Data = nil
		}
	}
	b.mu.Lock()
	b.pending = append(b.pending, ev)
	b.mu.Unlock()
}

// text stores an event of type typ with text.
func (b *eventBuffer) text(typ domain.JobEventType, text string) {
	b.add(domain.JobEvent{Type: typ, Text: text})
}

func (b *eventBuffer) take() []domain.JobEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pending
	b.pending = nil
	return out
}

// flush sends every pending event. A batch the server did not take after the
// client's retries is dropped, so a broken log never holds a job up;
// ErrLeaseLost and ErrUnauthorized are returned for the caller to stop.
func (b *eventBuffer) flush(ctx context.Context) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	events := b.take()
	for len(events) > 0 {
		n, size := 0, 0
		for n < len(events) && n < maxBatchEvents && (n == 0 || size < maxBatchBytes) {
			size += len(events[n].Text) + len(events[n].Data) + 128
			n++
		}
		if err := b.client.SendEvents(ctx, b.jobID, events[:n]); err != nil {
			if stopsJob(err) {
				return err
			}
			b.log.WarnContext(ctx, "drop job events", "error", err, "count", n)
		}
		events = events[n:]
	}
	return nil
}

// run flushes every interval until ctx is done. onStop is called with the
// error when the server says the job must stop.
func (b *eventBuffer) run(ctx context.Context, interval time.Duration, onStop func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A batch in flight when ctx ends must still arrive.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
			err := b.flush(flushCtx)
			cancel()
			if err != nil {
				onStop(err)
				return
			}
		}
	}
}

// cleanText removes what Postgres refuses in text: NUL bytes and invalid
// UTF-8.
func cleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
}
