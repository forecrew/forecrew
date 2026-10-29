// Package workerapi serves the worker API (docs/contracts/worker-api.md):
// workers authenticate with their token, poll for jobs, heartbeat, send
// events and complete jobs. Repo tokens and runtime secrets are added to the
// job spec at lease time and never stored or logged.
package workerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Limits and timings of the API.
const (
	// MaxBodyBytes is the largest request body accepted.
	MaxBodyBytes = 1 << 20
	// MaxEvents is the largest events batch accepted.
	MaxEvents = 500
	// PollWait is how long a poll waits for a job before answering 204.
	PollWait = 30 * time.Second
	// PollRecheck is how often a waiting poll looks for a job without a
	// wake-up, which catches jobs whose retry backoff ended and jobs
	// created by another forecrewd.
	PollRecheck = 2 * time.Second
	// HeartbeatInterval is the heartbeat_interval_seconds returned by poll.
	HeartbeatInterval = 30 * time.Second
)

// TokenSource mints repo tokens; github.Tracker implements it.
type TokenSource interface {
	InstallationToken(ctx context.Context, repo string, access github.TokenAccess) (github.Token, error)
}

// Config holds the dependencies of a Server.
type Config struct {
	Store  store.Store
	Jobs   *jobs.Service
	Tokens TokenSource
	// Env returns the runtime secrets of a leased job; see RuntimeEnv.
	Env    func(j domain.Job) map[string]string
	Logger *slog.Logger
	// PollWait and PollRecheck default to the constants of the same name.
	PollWait    time.Duration
	PollRecheck time.Duration
}

// Server is the worker API.
type Server struct {
	cfg Config
}

// New returns the worker API.
func New(cfg Config) *Server {
	if cfg.PollWait == 0 {
		cfg.PollWait = PollWait
	}
	if cfg.PollRecheck == 0 {
		cfg.PollRecheck = PollRecheck
	}
	if cfg.Env == nil {
		cfg.Env = func(domain.Job) map[string]string { return nil }
	}
	return &Server{cfg: cfg}
}

// Handler returns the routes under /worker/v1/, behind worker
// authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worker/v1/poll", s.poll)
	mux.HandleFunc("POST /worker/v1/jobs/{job_id}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST /worker/v1/jobs/{job_id}/events", s.events)
	mux.HandleFunc("POST /worker/v1/jobs/{job_id}/complete", s.complete)
	return s.authenticate(mux)
}

type workerKey struct{}

// WorkerFromContext returns the worker authenticated for a request.
func WorkerFromContext(ctx context.Context) (domain.Worker, bool) {
	w, ok := ctx.Value(workerKey{}).(domain.Worker)
	return w, ok
}

// authenticate requires a known, unrevoked worker token and puts the worker
// (name, account) into the request context.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
			writeError(rw, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		w, err := s.cfg.Store.Workers().GetByTokenHash(r.Context(), mcp.HashToken(token))
		switch {
		case errors.Is(err, store.ErrNotFound) || (err == nil && w.Revoked()):
			writeError(rw, http.StatusUnauthorized, "unauthorized", "unknown or revoked worker token")
			return
		case err != nil:
			fail(r.Context(), rw, s.cfg.Logger, "look up worker", err)
			return
		}
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), workerKey{}, w)))
	})
}

// worker returns the authenticated worker and a logger with its IDs.
func (s *Server) worker(r *http.Request) (domain.Worker, *slog.Logger) {
	w, _ := WorkerFromContext(r.Context())
	return w, s.cfg.Logger.With("account_id", w.AccountID, "worker_id", w.ID, "worker", w.Name)
}

// decode reads a JSON body of at most MaxBodyBytes into v. An empty body
// leaves v alone when allowEmpty is set. It writes the error response and
// returns false on failure.
func decode(rw http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, MaxBodyBytes)).Decode(v)
	if err == nil || (allowEmpty && errors.Is(err, io.EOF)) {
		return true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeError(rw, http.StatusRequestEntityTooLarge, "too_large", "request body over 1 MiB")
		return false
	}
	writeError(rw, http.StatusBadRequest, "bad_request", "malformed JSON: "+err.Error())
	return false
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	// The status line is already sent; a failed write has no recovery.
	_ = json.NewEncoder(rw).Encode(v)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(rw http.ResponseWriter, status int, code, message string) {
	if status == http.StatusUnauthorized {
		rw.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(rw, status, map[string]apiError{"error": {Code: code, Message: message}})
}

// fail answers a store or service error with its status code.
func fail(ctx context.Context, rw http.ResponseWriter, logger *slog.Logger, what string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(rw, http.StatusNotFound, "not_found", "job not found")
	case errors.Is(err, store.ErrLeaseLost):
		writeError(rw, http.StatusConflict, "lease_lost", "job is not leased to this worker")
	default:
		logger.ErrorContext(ctx, what, "error", err)
		writeError(rw, http.StatusInternalServerError, "internal", "internal error")
	}
}
