// Package workerapi serves the worker API (docs/contracts/worker-api.md):
// workers authenticate with their token, poll for jobs, heartbeat, send
// events and complete jobs. Repo tokens and runtime secrets are added to the
// job spec at lease time and never stored or logged.
package workerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
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
	// Grant decides, for a leased job of its own organization, its
	// connection, key, endpoint and model, all from one decision; see
	// models.Service.Grant. Nil grants nothing.
	Grant func(ctx context.Context, j domain.Job) (models.Grant, error)
	// CheckGrant fills in a model_check job from its connection as it is
	// now: the address its key goes to, and the key; see
	// models.Service.CheckGrant. Nil leaves every check waiting.
	CheckGrant func(ctx context.Context, j domain.Job) (spec domain.ModelCheckSpec, env map[string]string, waiting string, err error)
	// Settle gives the queued jobs of an organization the runtime of their
	// connection before a poll leases, or domain.RuntimeNone while they have
	// none; see models.Service.Settle. Nil settles nothing.
	Settle func(ctx context.Context, accountID uuid.UUID) error
	// Pools is the pool policy that limits the credentials of leased jobs
	// (config.Config.Pools); nil means config.DefaultPools.
	Pools  map[string]config.Pool
	Logger *slog.Logger
	// Evidence stores artifacts; nil answers artifact uploads with 503.
	Evidence evidence.Store
	// MaxTestResultsSize is the largest body of a complete request, in
	// bytes (max_test_results_size); it defaults to
	// config.DefaultMaxTestResultsSize.
	MaxTestResultsSize int64
	// MaxArtifactSize is the largest artifact a worker may upload, in bytes
	// (evidence.max_artifact_size); it defaults to
	// config.DefaultMaxArtifactSize.
	MaxArtifactSize int64
	// JobMCPURL is the gateway's public URL (<public base>/job-mcp), sent as
	// job_mcp.url for information; workers use their own server URL.
	JobMCPURL string
	// Attachments are the files the gateway's evidence_attach marked;
	// heartbeat answers hand them to the worker until it acknowledges them.
	Attachments *jobmcp.Attachments
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
	if cfg.MaxTestResultsSize <= 0 {
		cfg.MaxTestResultsSize = config.DefaultMaxTestResultsSize
	}
	if cfg.MaxArtifactSize <= 0 {
		cfg.MaxArtifactSize = config.DefaultMaxArtifactSize
	}
	if cfg.Grant == nil {
		cfg.Grant = func(context.Context, domain.Job) (models.Grant, error) { return models.Grant{}, nil }
	}
	if cfg.Settle == nil {
		cfg.Settle = func(context.Context, uuid.UUID) error { return nil }
	}
	if cfg.CheckGrant == nil {
		cfg.CheckGrant = func(context.Context, domain.Job) (domain.ModelCheckSpec, map[string]string, string, error) {
			return domain.ModelCheckSpec{}, nil, "this server runs no model checks", nil
		}
	}
	if cfg.Pools == nil {
		cfg.Pools = config.DefaultPools()
	}
	return &Server{cfg: cfg}
}

// Handler returns the routes under /worker/v1/, behind worker
// authentication, and the bootstrap route, which has its own credential.
func (s *Server) Handler() http.Handler {
	outer := http.NewServeMux()
	outer.HandleFunc("POST "+bootstrapPath, s.bootstrap)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pollPath, s.poll)
	mux.HandleFunc("POST /worker/v1/jobs/{job_id}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST /worker/v1/jobs/{job_id}/events", s.events)
	mux.HandleFunc("PUT /worker/v1/jobs/{job_id}/artifacts/{name...}", s.uploadArtifact)
	mux.HandleFunc("POST /worker/v1/jobs/{job_id}/complete", s.complete)
	outer.Handle("/", s.authenticate(mux))
	return outer
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
		case r.URL.Path == pollPath && clientGone(r.Context(), err):
			// Only poll: elsewhere the context is also done on shutdown while
			// the worker is alive, and a 2xx would make it drop its call.
			rw.WriteHeader(http.StatusNoContent)
			return
		case err != nil:
			fail(r.Context(), rw, s.cfg.Logger, "look up worker", err)
			return
		}
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), workerKey{}, w)))
	})
}

// leasedJob returns the job id if w still holds its lease: the job is running
// and leased to w. Otherwise it wraps store.ErrLeaseLost (store.ErrNotFound
// when the job is not one of the worker's account).
func (s *Server) leasedJob(ctx context.Context, w domain.Worker, id uuid.UUID) (domain.Job, error) {
	j, err := s.cfg.Store.Jobs().Get(ctx, w.AccountID, id)
	if err == nil && (j.Status != domain.JobRunning || j.WorkerID != w.ID) {
		err = fmt.Errorf("job %s is %s: %w", j.Ref(), j.Status, store.ErrLeaseLost)
	}
	return j, err
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
	return decodeLimited(rw, r, v, allowEmpty, MaxBodyBytes)
}

// decodeLimited is decode with another body limit.
func decodeLimited(rw http.ResponseWriter, r *http.Request, v any, allowEmpty bool, limit int64) bool {
	err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, limit)).Decode(v)
	if err == nil || (allowEmpty && errors.Is(err, io.EOF)) {
		return true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeError(rw, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body over %d bytes", limit))
		return false
	}
	writeError(rw, http.StatusBadRequest, "bad_request", "malformed JSON: "+err.Error())
	return false
}

// Decode reads a JSON body of at most MaxBodyBytes into v, or answers the error
// itself and returns false. The fleet agent API has the errors of this one.
func Decode(rw http.ResponseWriter, r *http.Request, v any) bool { return decode(rw, r, v, false) }

// WriteJSON answers with v as JSON.
func WriteJSON(rw http.ResponseWriter, status int, v any) { writeJSON(rw, status, v) }

// WriteError answers with the API's error body.
func WriteError(rw http.ResponseWriter, status int, code, message string) {
	writeError(rw, status, code, message)
}

// ClientGone reports whether err is the request context ending, which is not
// a server error.
func ClientGone(ctx context.Context, err error) bool { return clientGone(ctx, err) }

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

// pollPath is the long-poll route, the only one answered 204 when its client
// is gone.
const pollPath = "/worker/v1/poll"

// clientGone reports whether err is the request context ending (the client
// went away or forecrewd is shutting down), which is not a server error. It
// includes the i/o timeout pgx reports when the deadline hits a socket write.
func clientGone(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded))
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
