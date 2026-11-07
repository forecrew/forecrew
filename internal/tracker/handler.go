package tracker

import (
	"errors"
	"io"
	"net/http"
	"uuid"

	"github.com/forecrew/forecrew/internal/store"
)

// WebhookPath is the pattern of the webhook of one connection.
const WebhookPath = "POST /webhooks/tracker/{connection}"

const maxWebhookBody = 25 << 20

// ServeHTTP serves WebhookPath: 404 for an unknown connection, 401 for a bad
// signature, 400 for an unreadable delivery, 503 when the queue is full and
// 202 otherwise. Events are only queued here; the ticket is read by Run.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("connection"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, err := s.cfg.Store.TrackerConnections().Find(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.cfg.Logger.Error("find tracker connection", "connection", id, "error", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	ad, err := s.cfg.Registry.Build(c)
	if err != nil {
		s.cfg.Logger.Error("build tracker adapter", "connection", id, "error", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	events, err := ad.ParseEvent(Request{Header: r.Header, Query: r.URL.Query(), Body: body})
	switch {
	case errors.Is(err, ErrBadSignature):
		s.cfg.Logger.Warn("tracker webhook rejected", "connection", id, "error", err)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	case err != nil:
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	for _, ev := range events {
		if !s.enqueueEvent(c, ev) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
