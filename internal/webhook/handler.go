package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/githubissues"
	"github.com/forecrew/forecrew/internal/tracker/githubproject"
)

// maxBody is GitHub's limit for webhook payloads.
const maxBody = 25 << 20

// queueSize is how many accepted events wait for Run.
const queueSize = 256

// delivery is an accepted webhook delivery waiting for Run.
type delivery struct {
	id  string
	ev  github.Event
	raw tracker.Request
}

// Handler serves POST /webhooks/github. It verifies and queues events and
// answers at once; Run deduplicates and processes the queue.
type Handler struct {
	secret []byte
	e      *engine
	queue  chan delivery
}

// NewHandler returns a handler that verifies deliveries with secret.
func NewHandler(secret []byte, d Deps) *Handler {
	return &Handler{secret: secret, e: newEngine(d), queue: make(chan delivery, queueSize)}
}

// ServeHTTP implements http.Handler. It answers 401 for a bad signature, 400
// for an unreadable delivery, 202 for the events it queued or does not use,
// and 503 when the queue is full. A delivery is remembered only when Run
// processes it, so GitHub's redelivery after a 503 is accepted.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if err := github.VerifySignature(h.secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		h.e.Log.Warn("webhook rejected", "error", err)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	id, kind := r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event")
	if id == "" {
		http.Error(w, "missing delivery id", http.StatusBadRequest)
		return
	}
	ev, err := github.ParseEvent(kind, body)
	switch {
	case errors.Is(err, github.ErrUnsupportedEvent):
		w.WriteHeader(http.StatusAccepted)
		return
	case err != nil:
		h.e.Log.Warn("webhook payload rejected", "delivery", id, "event", kind, "error", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	select {
	case h.queue <- delivery{id: id, ev: ev, raw: tracker.Request{Header: r.Header, Body: body}}:
		w.WriteHeader(http.StatusAccepted)
	default:
		h.e.Log.Warn("webhook queue full", "delivery", id, "event", kind)
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}
}

// Run processes queued events until ctx is done. An event that fails is
// logged and left to the poller.
func (h *Handler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-h.queue:
			h.process(ctx, d)
		}
	}
}

// process skips a delivery seen before and otherwise handles its event. If
// the delivery cannot be recorded the event is dropped for the poller.
func (h *Handler) process(ctx context.Context, d delivery) {
	isNew, err := h.e.Store.WebhookDeliveries().Record(ctx, domain.WebhookDelivery{DeliveryID: d.id, Event: d.ev.Kind, ReceivedAt: h.e.Now()})
	switch {
	case err != nil:
		h.e.Log.Error("record webhook delivery", "delivery", d.id, "error", err)
		return
	case !isNew:
		h.e.Log.Warn("webhook redelivered", "delivery", d.id, "event", d.ev.Kind)
		return
	}
	ev := d.ev
	if err := h.e.handle(ctx, ev); err != nil {
		h.e.Log.Error("process webhook event", "delivery", d.id, "event", ev.Kind, "action", ev.Action, "repo", ev.Repo, "number", ev.Number, "error", err)
	}
	if kind := ticketKind(ev.Kind); h.e.Tickets != nil && kind != "" {
		if err := h.e.Tickets.Deliver(ctx, kind, d.raw); err != nil {
			h.e.Log.Error("process ticket event", "delivery", d.id, "event", ev.Kind, "repo", ev.Repo, "number", ev.Number, "error", err)
		}
	}
}

// ticketKind is the kind of tracker connection an event of this kind is for,
// or empty when none is.
func ticketKind(event string) string {
	switch event {
	case "issues", "issue_comment":
		return githubissues.Kind
	case "projects_v2_item":
		return githubproject.Kind
	}
	return ""
}
