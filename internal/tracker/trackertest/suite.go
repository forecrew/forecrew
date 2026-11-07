package trackertest

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/tracker"
)

// Harness is what the suite needs to drive an adapter through its tracker,
// real or recorded.
type Harness struct {
	Adapter tracker.Adapter
	// NewTicket creates an open ticket with this title and body and returns
	// its key.
	NewTicket func(t *testing.T, title, body string) string
	// Touch changes a ticket on the tracker, so Changes reports it.
	Touch func(t *testing.T, key string)
	// Signed returns a delivery about a ticket that passes the signature
	// check; nil for an adapter without webhooks.
	Signed func(t *testing.T, key string) tracker.Request
	// Forged returns the same delivery with a signature made with the wrong
	// secret.
	Forged func(t *testing.T, key string) tracker.Request
}

// Run checks the contract of an adapter.
func Run(t *testing.T, h Harness) {
	t.Helper()
	caps := h.Adapter.Capabilities()
	t.Run("kind", h.kind)
	t.Run("get", h.get)
	t.Run("polling", h.polling)
	if caps.Webhooks {
		t.Run("webhooks", h.webhooks)
	}
	if caps.Comments {
		t.Run("comments", h.comments)
	}
	t.Run("states", h.states)
}

func (h Harness) kind(t *testing.T) {
	if h.Adapter.Kind() == "" {
		t.Error("no kind")
	}
	if err := h.Adapter.Verify(t.Context()); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func (h Harness) get(t *testing.T) {
	key := h.NewTicket(t, "Add export", "Export *everything*.")
	got, err := h.Adapter.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Add export" || got.Body != "Export *everything*." || got.Closed || got.URL == "" || got.State == "" {
		t.Errorf("Get = %+v", got)
	}
	if _, err := h.Adapter.Get(t.Context(), missingKey(key)); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get of a missing ticket: %v", err)
	}
}

func hasKey(events []tracker.Event, key string) bool {
	return slices.ContainsFunc(events, func(e tracker.Event) bool { return e.Key == key })
}

func (h Harness) polling(t *testing.T) {
	key := h.NewTicket(t, "Polled", "")
	events, cursor, err := h.Adapter.Changes(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || cursor == "" {
		t.Fatalf("the first poll read the backlog: %+v, cursor %q", events, cursor)
	}
	h.Touch(t, key)
	events, next, err := h.Adapter.Changes(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKey(events, key) || next == "" {
		t.Errorf("Changes = %+v, cursor %q", events, next)
	}
}

func (h Harness) webhooks(t *testing.T) {
	key := h.NewTicket(t, "Hooked", "")
	events, err := h.Adapter.ParseEvent(h.Signed(t, key))
	if err != nil {
		t.Fatal(err)
	}
	if !hasKey(events, key) {
		t.Errorf("ParseEvent = %+v", events)
	}
	if _, err := h.Adapter.ParseEvent(h.Forged(t, key)); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("forged delivery: %v", err)
	}
	if _, err := h.Adapter.ParseEvent(tracker.Request{Header: http.Header{}, Body: []byte("{}")}); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("unsigned delivery: %v", err)
	}
}

func (h Harness) comments(t *testing.T) {
	key := h.NewTicket(t, "Commented", "")
	id, err := h.Adapter.Comment(t.Context(), key, "hello")
	if err != nil || id == "" {
		t.Fatalf("Comment = %q, %v", id, err)
	}
	got, err := h.Adapter.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got.Comments, func(c tracker.Comment) bool { return c.ID == id && c.Body == "hello" }) {
		t.Errorf("comments = %+v", got.Comments)
	}
}

func (h Harness) states(t *testing.T) {
	states, err := h.Adapter.States(t.Context())
	if err != nil || len(states) == 0 {
		t.Fatalf("States = %+v, %v", states, err)
	}
	i := slices.IndexFunc(states, func(s tracker.State) bool { return s.Closed })
	if i < 0 {
		t.Fatalf("no closed state in %+v", states)
	}
	if !h.Adapter.Capabilities().Transitions {
		return
	}
	key := h.NewTicket(t, "Moved", "")
	if err := h.Adapter.Transition(t.Context(), key, states[i].Name); err != nil {
		t.Fatal(err)
	}
	got, err := h.Adapter.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != states[i].Name || !got.Closed {
		t.Errorf("state = %q, closed %v, want %q", got.State, got.Closed, states[i].Name)
	}
}

func missingKey(key string) string { return key + "404" }
