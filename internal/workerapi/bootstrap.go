package workerapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
)

// bootstrapPath is the first call of the worker on a machine.
const bootstrapPath = "/worker/v1/bootstrap"

// BootstrapRetry is the retry_after_seconds of a worker told to wait.
const BootstrapRetry = 5 * time.Second

type bootstrapResponse struct {
	WorkerToken string   `json:"worker_token"`
	Name        string   `json:"name"`
	Labels      []string `json:"labels"`
	MaxJobs     int      `json:"max_jobs"`
}

// bootstrap trades a machine's one-time token for a worker token
// (worker-api.md, POST /worker/v1/bootstrap). Neither token is ever logged.
func (s *Server) bootstrap(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		writeError(rw, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if !decode(rw, r, &req, true) {
		return
	}
	g, err := fleet.Exchange(ctx, s.cfg.Store, mcp.HashToken(token), req.Version)
	switch {
	case errors.Is(err, fleet.ErrBootstrapRefused):
		writeError(rw, http.StatusUnauthorized, "unauthorized", "unknown or used bootstrap token")
	case err != nil:
		fail(ctx, rw, s.cfg.Logger, "bootstrap", err)
	case g.Wait:
		writeJSON(rw, http.StatusAccepted, map[string]int{"retry_after_seconds": int(BootstrapRetry / time.Second)})
	default:
		writeJSON(rw, http.StatusOK, bootstrapResponse{WorkerToken: g.WorkerToken, Name: g.Name, Labels: g.Labels, MaxJobs: g.MaxJobs})
	}
}
