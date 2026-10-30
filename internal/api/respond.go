package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// write answers with v as JSON.
func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "encode response", "path", r.URL.Path, "error", err)
		s.fail(w, r, errors.New("internal error"), http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		s.cfg.Logger.DebugContext(r.Context(), "write response", "path", r.URL.Path, "error", err)
	}
}

// fail answers with the API error format.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, status int, code string) {
	s.write(w, r, status, map[string]any{"error": map[string]string{"code": code, "message": err.Error()}})
}

// badRequest answers a request that cannot be decoded.
func (s *Server) badRequest(w http.ResponseWriter, r *http.Request, err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		s.fail(w, r, errors.New("request body too large"), http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	s.fail(w, r, err, http.StatusBadRequest, "bad_request")
}

// failTool answers an error of a tool: known conditions with their message,
// anything else as a generic 500 with the cause logged.
func (s *Server) failTool(w http.ResponseWriter, r *http.Request, by domain.Actor, err error) {
	code, ok := mcp.ErrorCode(err)
	if !ok {
		s.cfg.Logger.ErrorContext(r.Context(), "api call failed", "method", r.Method, "path", r.URL.Path, "account_id", by.AccountID, "actor", by.String(), "error", err)
		s.fail(w, r, errors.New("internal error, try again later"), http.StatusInternalServerError, "internal")
		return
	}
	status := map[string]int{"not_found": http.StatusNotFound, "invalid_input": http.StatusBadRequest, "conflict": http.StatusConflict}[code]
	s.fail(w, r, err, status, code)
}
