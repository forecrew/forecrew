// Package fleetapi serves the fleet agent API
// (docs/contracts/fleet-agent-api.md): fleet agents authenticate with their
// provider token, or join with a code, then register, poll for the machines
// that should exist and report what happened. It never logs a body, a token,
// a code or a claim.
package fleetapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workerapi"
)

// Timings of the API.
const (
	// PollWait is how long a poll waits for a change.
	PollWait = 30 * time.Second
	// PollRecheck is how often a waiting poll looks again.
	PollRecheck = 2 * time.Second
	// JoinRetry is the retry_after_seconds of an agent told to wait.
	JoinRetry = 5 * time.Second
)

// Bounds on what an agent says about itself; a person reads it, and until a
// join is confirmed nobody vouches for the caller.
const (
	maxFact   = 255
	maxChecks = 64
	claimSize = 32
	// maxListed bounds the instances of a reconcile and the have of a poll:
	// each unknown instance costs a lookup and an audit row, and have is
	// searched for every requested machine. Far above what a host runs.
	maxListed = 1000
)

// Config holds the dependencies of a Server.
type Config struct {
	Store  store.Store
	Fleet  *fleet.Controller
	Logger *slog.Logger
	Now    func() time.Time
	// PollWait and PollRecheck default to the constants of the same name.
	PollWait    time.Duration
	PollRecheck time.Duration
}

// Server is the fleet agent API.
type Server struct {
	cfg   Config
	limit *limiter
}

// New returns the fleet agent API.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.PollWait == 0 {
		cfg.PollWait = PollWait
	}
	if cfg.PollRecheck == 0 {
		cfg.PollRecheck = PollRecheck
	}
	return &Server{cfg: cfg, limit: newLimiter(cfg.Now)}
}

// Handler returns the routes under /fleet/v1/. All but join need a provider
// token.
func (s *Server) Handler() http.Handler {
	outer := http.NewServeMux()
	outer.HandleFunc("POST /fleet/v1/join", s.join)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fleet/v1/register", s.register)
	mux.HandleFunc("POST /fleet/v1/poll", s.poll)
	mux.HandleFunc("POST /fleet/v1/machines/{machine_id}/state", s.state)
	mux.HandleFunc("POST /fleet/v1/reconcile", s.reconcile)
	outer.Handle("/", s.authenticate(mux))
	return outer
}

type providerKey struct{}

func providerOf(ctx context.Context) domain.CapacityProvider {
	p, _ := ctx.Value(providerKey{}).(domain.CapacityProvider)
	return p
}

func unauthorized(rw http.ResponseWriter) {
	workerapi.WriteError(rw, http.StatusUnauthorized, "unauthorized", "unknown or revoked provider token")
}

// authenticate finds the provider of the bearer token. The token identifies
// the provider and so its organization: nothing below takes either from the
// request.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
			unauthorized(rw)
			return
		}
		p, err := s.cfg.Store.CapacityProviders().GetByTokenHash(ctx, mcp.HashToken(token))
		switch {
		case errors.Is(err, store.ErrNotFound), err == nil && p.Status == domain.ProviderRevoked:
			unauthorized(rw)
		case err != nil:
			s.fail(ctx, rw, "find provider", err)
		default:
			next.ServeHTTP(rw, r.WithContext(context.WithValue(ctx, providerKey{}, p)))
		}
	})
}

// fail answers a server error. A client that went away gets nothing, and
// nothing is logged for it.
func (s *Server) fail(ctx context.Context, rw http.ResponseWriter, what string, err error) {
	if workerapi.ClientGone(ctx, err) {
		return
	}
	s.cfg.Logger.ErrorContext(ctx, what, "error", err)
	workerapi.WriteError(rw, http.StatusInternalServerError, "internal", "internal error")
}

func badRequest(rw http.ResponseWriter, message string) {
	workerapi.WriteError(rw, http.StatusBadRequest, "bad_request", message)
}

// plainText reports a short line with no control characters: what is fit to
// show a person and to store.
func plainText(s string) bool {
	return len(s) <= maxFact && strings.IndexFunc(s, unicode.IsControl) < 0
}

// checkFacts refuses capabilities that are invalid or more than facts.
func checkFacts(c domain.ProviderCapabilities) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if len(c.Checks) > maxChecks {
		return errors.New("too many checks")
	}
	values := []string{c.Host, c.CloudAccount, c.Region, c.AgentVersion}
	for _, k := range c.Checks {
		values = append(values, k.Name, k.Detail)
	}
	for _, v := range values {
		if !plainText(v) {
			return errors.New("capabilities hold short lines of text only")
		}
	}
	return nil
}

// validClaim reports 32 bytes in base64url.
func validClaim(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == claimSize
}

func remoteAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type joinRequest struct {
	Code  string                      `json:"code"`
	Claim string                      `json:"claim"`
	Kind  domain.ProviderKind         `json:"kind"`
	Facts domain.ProviderCapabilities `json:"facts"`
}

type joinResponse struct {
	Status     string `json:"status"`
	RetryAfter int    `json:"retry_after_seconds,omitempty"`
	Token      string `json:"token,omitempty"`
}

func rateLimited(rw http.ResponseWriter) {
	rw.Header().Set("Retry-After", "1")
	workerapi.WriteError(rw, http.StatusTooManyRequests, "rate_limited", "too many join calls")
}

// join is the one route without a token. Both limits come before any lookup;
// a call whose code turns out to exist gets its place in the global limit back,
// so only guesses use it up.
func (s *Server) join(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.limit.allowAddress(remoteAddress(r)) {
		rateLimited(rw)
		return
	}
	var req joinRequest
	if !workerapi.Decode(rw, r, &req) {
		return
	}
	if req.Code == "" || !validClaim(req.Claim) || !slices.Contains(domain.ProviderKinds(), req.Kind) {
		badRequest(rw, "code, claim and kind are required")
		return
	}
	if err := checkFacts(req.Facts); err != nil {
		badRequest(rw, err.Error())
		return
	}
	if !s.limit.takeGuess() {
		rateLimited(rw)
		return
	}
	token, err := fleet.Join(ctx, s.cfg.Store, s.cfg.Now(), fleet.JoinRequest{Code: req.Code, Claim: req.Claim, Kind: req.Kind, Facts: req.Facts})
	if !errors.Is(err, fleet.ErrJoinRefused) {
		s.limit.refundGuess()
	}
	switch {
	case errors.Is(err, fleet.ErrJoinRefused):
		workerapi.WriteError(rw, http.StatusUnauthorized, "unauthorized", "unknown, used or expired join code")
	case errors.Is(err, fleet.ErrJoinClaimed):
		workerapi.WriteError(rw, http.StatusConflict, "conflict", "join code claimed by another agent")
	case err != nil:
		s.fail(ctx, rw, "join", err)
	case token == "":
		workerapi.WriteJSON(rw, http.StatusOK, joinResponse{Status: "waiting", RetryAfter: int(JoinRetry / time.Second)})
	default:
		workerapi.WriteJSON(rw, http.StatusOK, joinResponse{Status: "confirmed", Token: token})
	}
}

type registerResponse struct {
	ID          uuid.UUID             `json:"id"`
	Name        string                `json:"name"`
	Kind        domain.ProviderKind   `json:"kind"`
	Status      domain.ProviderStatus `json:"status"`
	Limits      domain.ProviderLimits `json:"limits"`
	Tags        map[string]string     `json:"tags"`
	PollTimeout int                   `json:"poll_timeout_seconds"`
}

func (s *Server) register(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := providerOf(ctx)
	var req struct {
		Capabilities domain.ProviderCapabilities `json:"capabilities"`
	}
	if !workerapi.Decode(rw, r, &req) {
		return
	}
	if err := checkFacts(req.Capabilities); err != nil {
		badRequest(rw, err.Error())
		return
	}
	if err := s.cfg.Store.CapacityProviders().Seen(ctx, p.ID, &req.Capabilities); err != nil {
		s.fail(ctx, rw, "record contact", err)
		return
	}
	workerapi.WriteJSON(rw, http.StatusOK, registerResponse{
		ID: p.ID, Name: p.Name, Kind: p.Kind, Status: p.Status, Limits: p.Limits, Tags: p.Tags, PollTimeout: int(s.cfg.PollWait / time.Second),
	})
}

type pollResponse struct {
	Generation string                `json:"generation"`
	Machines   []fleet.CreateRequest `json:"machines"`
}

// poll answers at once when the list changed or a machine is to be delivered,
// otherwise when the wait is over. The provider is read again on every look:
// one revoked or paused meanwhile gets no demand from a poll that was waiting.
func (s *Server) poll(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := providerOf(ctx).ID
	var req struct {
		Generation string      `json:"generation"`
		Have       []uuid.UUID `json:"have"`
	}
	if !workerapi.Decode(rw, r, &req) {
		return
	}
	if len(req.Have) > maxListed {
		badRequest(rw, fmt.Sprintf("have lists more than %d machines", maxListed))
		return
	}
	if err := s.cfg.Store.CapacityProviders().Seen(ctx, id, nil); err != nil {
		s.fail(ctx, rw, "record contact", err)
		return
	}
	wait := time.NewTimer(s.cfg.PollWait)
	defer wait.Stop()
	recheck := time.NewTicker(s.cfg.PollRecheck)
	defer recheck.Stop()
	for over := false; ; {
		p, err := s.cfg.Store.CapacityProviders().Get(ctx, id)
		if err != nil {
			s.fail(ctx, rw, "find provider", err)
			return
		}
		if p.Status == domain.ProviderRevoked {
			unauthorized(rw)
			return
		}
		d, err := s.cfg.Fleet.Desired(ctx, p, req.Have)
		if err != nil {
			s.fail(ctx, rw, "read desired machines", err)
			return
		}
		if over || d.Generation != req.Generation || d.Undelivered() {
			s.answerPoll(rw, r, p, d)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wait.C:
			over = true
		case <-recheck.C:
		}
	}
}

// answerPoll mints tokens only for a client that is still there: one that left
// would take the token of the machine from the connection it has now.
func (s *Server) answerPoll(rw http.ResponseWriter, r *http.Request, p domain.CapacityProvider, d fleet.Desired) {
	ctx := r.Context()
	if ctx.Err() != nil {
		return
	}
	if err := s.cfg.Fleet.Deliver(ctx, p, &d); err != nil {
		s.fail(ctx, rw, "deliver machines", err)
		return
	}
	workerapi.WriteJSON(rw, http.StatusOK, pollResponse{Generation: d.Generation, Machines: d.Machines})
}

func (s *Server) state(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(r.PathValue("machine_id"))
	if err != nil {
		badRequest(rw, "machine_id is not a UUID")
		return
	}
	var req struct {
		State      domain.MachineState `json:"state"`
		ExternalID string              `json:"external_id"`
		Error      string              `json:"error"`
	}
	if !workerapi.Decode(rw, r, &req) {
		return
	}
	if !plainText(req.ExternalID) {
		badRequest(rw, "external_id is not a short line of text")
		return
	}
	err = s.cfg.Fleet.ReportState(ctx, providerOf(ctx), id, req.State, req.ExternalID, req.Error)
	switch {
	case err == nil:
		workerapi.WriteJSON(rw, http.StatusOK, struct{}{})
	case errors.Is(err, fleet.ErrBadReport):
		badRequest(rw, err.Error())
	case errors.Is(err, store.ErrNotFound):
		workerapi.WriteError(rw, http.StatusNotFound, "not_found", "machine not found")
	case errors.Is(err, store.ErrConflict), errors.Is(err, domain.ErrInvalidTransition):
		workerapi.WriteError(rw, http.StatusConflict, "conflict", "the machine cannot make that change")
	default:
		s.fail(ctx, rw, "report machine state", err)
	}
}

// reconcile always answers what to destroy: that list is right whatever else
// went wrong, and a machine that changed under the look is seen by the next
// reconcile.
func (s *Server) reconcile(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := providerOf(ctx)
	var req struct {
		Instances []fleet.Instance `json:"instances"`
	}
	if !workerapi.Decode(rw, r, &req) {
		return
	}
	if len(req.Instances) > maxListed {
		badRequest(rw, fmt.Sprintf("instances lists more than %d", maxListed))
		return
	}
	for _, i := range req.Instances {
		if i.ExternalID == "" || !plainText(i.ExternalID) {
			badRequest(rw, "every instance needs an external_id, a short line of text")
			return
		}
	}
	if err := s.cfg.Store.CapacityProviders().Seen(ctx, p.ID, nil); err != nil {
		s.fail(ctx, rw, "record contact", err)
		return
	}
	destroy, err := s.cfg.Fleet.ReconcileAgent(ctx, p, req.Instances)
	if err != nil && !workerapi.ClientGone(ctx, err) {
		s.cfg.Logger.WarnContext(ctx, "reconcile provider", "provider", p.Name, "error", err)
	}
	workerapi.WriteJSON(rw, http.StatusOK, map[string][]string{"destroy": destroy})
}
