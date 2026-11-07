// Package trackertest holds the conformance suite every tracker adapter must
// pass, and a fake tracker built only from the public interface of package
// tracker.
package trackertest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Kind is the kind of the fake tracker.
const Kind = "fake"

// SignatureHeader carries the fake's webhook signature.
const SignatureHeader = "X-Fake-Signature"

// Fake is an in-memory tracker. It is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	secret  []byte
	tickets map[string]*tracker.Ticket
	changed []string // keys, in order of change
	now     func() time.Time
	next    int
	// Down makes every write fail, as an outage does.
	Down bool
	// Writes counts the comments and transitions that reached the tracker.
	Writes int
	// Caps is what the fake says it can do.
	Caps tracker.Capabilities
}

// NewFake returns a fake tracker that can do everything.
func NewFake(secret string) *Fake {
	return &Fake{
		secret: []byte(secret), tickets: map[string]*tracker.Ticket{}, now: func() time.Time { return time.Now().UTC() },
		Caps: tracker.Capabilities{Webhooks: true, Assignment: true, Labels: true, Columns: true, Transitions: true, Comments: true},
	}
}

// Factory returns a factory for connections of kind Kind.
func (f *Fake) Factory() tracker.Factory {
	return func(domain.TrackerConnection) (tracker.Adapter, error) { return f, nil }
}

// Put stores a ticket as it is on the tracker now and marks it changed.
func (f *Fake) Put(t tracker.Ticket) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.State == "" {
		t.State = "open"
	}
	t.Updated = f.now()
	f.tickets[t.Ref.Key] = &t
	f.changed = append(f.changed, t.Ref.Key)
}

// Edit changes a ticket and marks it changed.
func (f *Fake) Edit(key string, fn func(*tracker.Ticket)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.tickets[key])
	f.tickets[key].Updated = f.now()
	f.changed = append(f.changed, key)
}

// Ticket returns a copy of a ticket.
func (f *Fake) Ticket(key string) tracker.Ticket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.tickets[key]
}

// Sign returns the signature of a delivery.
func (f *Fake) Sign(body []byte) string {
	mac := hmac.New(sha256.New, f.secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Payload is a signed delivery of the fake.
func (f *Fake) Payload(id, key string) (body []byte, signature string) {
	body, _ = json.Marshal(map[string]string{"id": id, "key": key})
	return body, f.Sign(body)
}

// Kind implements tracker.Adapter.
func (f *Fake) Kind() string { return Kind }

// Capabilities implements tracker.Adapter.
func (f *Fake) Capabilities() tracker.Capabilities { return f.Caps }

// Verify implements tracker.Adapter.
func (f *Fake) Verify(context.Context) error { return nil }

// ParseEvent implements tracker.Adapter.
func (f *Fake) ParseEvent(r tracker.Request) ([]tracker.Event, error) {
	got, err := hex.DecodeString(r.Header.Get(SignatureHeader))
	want, _ := hex.DecodeString(f.Sign(r.Body))
	if err != nil || !hmac.Equal(got, want) {
		return nil, fmt.Errorf("fake delivery: %w", tracker.ErrBadSignature)
	}
	var p struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Self bool   `json:"self"`
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return nil, fmt.Errorf("fake delivery: %w", err)
	}
	return []tracker.Event{{ID: p.ID, Key: p.Key, Self: p.Self}}, nil
}

// Changes implements tracker.Adapter. The cursor is a position in the list of
// changes.
func (f *Fake) Changes(_ context.Context, cursor string) ([]tracker.Event, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor == "" {
		return nil, strconv.Itoa(len(f.changed)), nil
	}
	from, err := strconv.Atoi(cursor)
	if err != nil {
		return nil, "", fmt.Errorf("cursor %q: %w", cursor, err)
	}
	var out []tracker.Event
	for _, key := range f.changed[from:] {
		if !slices.ContainsFunc(out, func(e tracker.Event) bool { return e.Key == key }) {
			out = append(out, tracker.Event{Key: key})
		}
	}
	return out, strconv.Itoa(len(f.changed)), nil
}

// Get implements tracker.Adapter.
func (f *Fake) Get(_ context.Context, key string) (tracker.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[key]
	if !ok {
		return tracker.Ticket{}, fmt.Errorf("%s: %w", key, tracker.ErrNotFound)
	}
	return *t, nil
}

var errDown = errors.New("fake tracker is down")

// Comment implements tracker.Adapter. A comment changes the ticket, so
// Forecrew sees its own writes as it would on a real tracker.
func (f *Fake) Comment(_ context.Context, key, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[key]
	switch {
	case f.Down:
		return "", errDown
	case !ok:
		return "", fmt.Errorf("%s: %w", key, tracker.ErrNotFound)
	}
	f.next++
	id := "c" + strconv.Itoa(f.next)
	t.Comments = append(t.Comments, tracker.Comment{ID: id, Author: tracker.User{ID: "forecrew", Name: "forecrew"}, Body: body, At: f.now()})
	f.Writes++
	f.changed = append(f.changed, key)
	return id, nil
}

// Transition implements tracker.Adapter.
func (f *Fake) Transition(_ context.Context, key, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[key]
	switch {
	case f.Down:
		return errDown
	case !ok:
		return fmt.Errorf("%s: %w", key, tracker.ErrNotFound)
	}
	t.State, t.Closed = state, state == "closed"
	f.Writes++
	f.changed = append(f.changed, key)
	return nil
}

// States implements tracker.Adapter.
func (f *Fake) States(context.Context) ([]tracker.State, error) {
	return []tracker.State{{Name: "open"}, {Name: "doing"}, {Name: "review"}, {Name: "ready"}, {Name: "closed", Closed: true}}, nil
}

// PersonComment adds a comment by a person.
func (f *Fake) PersonComment(key string, by tracker.User, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := "c" + strconv.Itoa(f.next)
	t := f.tickets[key]
	t.Comments = append(t.Comments, tracker.Comment{ID: id, Author: by, Body: body, At: f.now()})
	f.changed = append(f.changed, key)
	return id
}
