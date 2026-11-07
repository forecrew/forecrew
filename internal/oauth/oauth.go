// Package oauth makes forecrewd the authorization server of its own /mcp
// (docs/mcp.md, docs/contracts/identity.md): protected resource and
// authorization server metadata, client registration, the consent screen and
// the token endpoint, as the MCP authorization specification describes. Every
// code, access token and refresh token is a row of oauth_grants; the bearer
// check itself is in internal/mcp, with the personal tokens.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// How long credentials last.
const (
	codeTTL    = 5 * time.Minute
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
)

// UnusedClientTTL is how long a client nobody uses stays registered.
const UnusedClientTTL = 30 * 24 * time.Hour

// MaxUnusedClients caps the registrations nobody has authorized yet: past it,
// a new one waits until the sweep or a consent makes room. Rate limits bound
// how fast they come, this bounds how many rows they leave.
const MaxUnusedClients = 1000

// SweepEvery is how often expired grants and unused clients are deleted.
const SweepEvery = time.Hour

// Abuse limits, in requests per minute. Behind a proxy that hides the peer,
// everybody counts as one address, so the per-address limits are generous.
const (
	registerPerAddress = 20
	registerPerServer  = 200
	tokenPerAddress    = 240
	fetchPerAddress    = 30
)

const (
	metadataPath     = "/.well-known/oauth-authorization-server"
	resourceRootPath = "/.well-known/oauth-protected-resource"
	tokenPath        = "/oauth/token" // #nosec G101 -- a path, not a credential
	registerPath     = "/oauth/register"
	maxFormBytes     = 16 << 10
	fetchTimeout     = 5 * time.Second
)

// Config holds the dependencies of a Server.
type Config struct {
	Store store.Store
	// Auth resolves the signed-in person behind the consent screen.
	Auth *auth.Service
	// BaseURL is the public URL of forecrewd: the issuer.
	BaseURL string
	Logger  *slog.Logger // defaults to discarding
	Now     func() time.Time
	// HTTP fetches client ID metadata documents; by default it reaches public
	// addresses only and follows no redirect. Tests replace it.
	HTTP *http.Client
	// MaxUnusedClients defaults to the constant; tests lower it.
	MaxUnusedClients int
}

// Server is the authorization server.
type Server struct {
	cfg      Config
	issuer   string
	resource mcp.Resource
	// Limits by address, and registerAll on the whole server: registrations
	// are rows, so they are bounded however many addresses send them.
	register, registerAll, token, fetch *limiter
}

// New returns the server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxUnusedClients == 0 {
		cfg.MaxUnusedClients = MaxUnusedClients
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{
			Timeout: fetchTimeout, Transport: councils.PublicTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Server{
		cfg: cfg, issuer: strings.TrimRight(cfg.BaseURL, "/"), resource: mcp.NewResource(cfg.BaseURL),
		register: newLimiter(registerPerAddress), registerAll: newLimiter(registerPerServer),
		token: newLimiter(tokenPerAddress), fetch: newLimiter(fetchPerAddress),
	}
}

// Paths returns what Handler serves, to mount it next to the web UI.
func Paths() []string {
	return []string{"/oauth/", metadataPath, resourceRootPath, mcp.MetadataPath}
}

// Handler serves the metadata and the OAuth endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	resource := sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource: s.resource.URL, AuthorizationServers: []string{s.issuer}, ScopesSupported: scopeNames(),
		BearerMethodsSupported: []string{"header"}, ResourceName: "Forecrew",
	})
	mux.Handle(mcp.MetadataPath, resource)
	mux.Handle(resourceRootPath, resource)
	mux.HandleFunc("GET "+metadataPath, s.metadata)
	mux.HandleFunc("GET "+auth.AuthorizePath, s.authorize)
	mux.HandleFunc("POST "+auth.AuthorizePath, s.consent)
	mux.HandleFunc("POST "+tokenPath, s.tokens)
	mux.HandleFunc("POST "+registerPath, s.registerClient)
	return mux
}

func scopeNames() []string {
	names := []string{}
	for _, sc := range domain.Scopes() {
		names = append(names, string(sc))
	}
	return names
}

// metadata is the authorization server metadata (RFC 8414).
func (s *Server) metadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + auth.AuthorizePath,
		"token_endpoint":                                 s.issuer + tokenPath,
		"registration_endpoint":                          s.issuer + registerPath,
		"scopes_supported":                               scopeNames(),
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{authNone, authSecretPost, authSecretBasic},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// RunSweep deletes expired grants and the clients nobody uses every interval
// until ctx ends.
func (s *Server) RunSweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.cfg.Logger.WarnContext(ctx, "sweep oauth grants and clients", "error", err)
			}
		}
	}
}

// Sweep runs one round of RunSweep.
func (s *Server) Sweep(ctx context.Context) error {
	now := s.cfg.Now()
	if _, err := s.cfg.Store.OAuth().DeleteExpiredGrants(ctx, now); err != nil {
		return err
	}
	_, err := s.cfg.Store.OAuth().DeleteUnusedClients(ctx, now.Add(-UnusedClientTTL))
	return err
}

// limiter counts requests by key in fixed windows of a minute. Counts live in
// memory: forecrewd is one replica, and a restart forgiving them does no harm.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Time
	counts map[string]int
}

func newLimiter(perMinute int) *limiter { return &limiter{limit: perMinute, counts: map[string]int{}} }

// allow counts a request and reports whether it is within the limit.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := now.Truncate(time.Minute); !w.Equal(l.window) {
		l.window, l.counts = w, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.limit
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The status line is already sent; a failed write has no recovery.
	_ = json.NewEncoder(w).Encode(body)
}

// protocolError is an error of RFC 6749 section 5.2, also the shape of RFC 7591.
type protocolError struct {
	status int
	code   string
	text   string
}

func (e *protocolError) Error() string { return e.code + ": " + e.text }

func refuse(status int, code, text string) *protocolError {
	return &protocolError{status: status, code: code, text: text}
}

// fail answers with err: what a client may see of a protocolError, and a
// generic server_error for the rest, with the cause in the log.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := errors.AsType[*protocolError](err)
	if !ok {
		s.cfg.Logger.ErrorContext(r.Context(), "oauth request failed", "path", r.URL.Path, "error", err)
		e = refuse(http.StatusInternalServerError, "server_error", "internal error, try again later")
	}
	if e.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Basic")
	}
	writeJSON(w, e.status, map[string]string{"error": e.code, "error_description": e.text})
}

// tooMany answers a request over its limit.
func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "temporarily_unavailable", "error_description": "too many requests, try again in a minute"})
}

// parseScopes reads a scope parameter: known scopes, lowest first. Empty asks
// for read. offline_access is not a scope here: every grant has a refresh token.
func parseScopes(value string) ([]domain.Scope, error) {
	asked := strings.Fields(value)
	asked = slices.DeleteFunc(asked, func(s string) bool { return s == "offline_access" })
	if len(asked) == 0 {
		return []domain.Scope{domain.ScopeRead}, nil
	}
	for _, a := range asked {
		if !slices.Contains(domain.Scopes(), domain.Scope(a)) {
			return nil, refuse(http.StatusBadRequest, "invalid_scope", "unknown scope "+a)
		}
	}
	return slices.DeleteFunc(domain.Scopes(), func(s domain.Scope) bool { return !slices.Contains(asked, string(s)) }), nil
}

// sameResource reports whether a resource parameter names /mcp, ignoring the
// case of the scheme and host, as the specification asks, and a final slash.
func (s *Server) sameResource(value string) bool {
	return strings.EqualFold(strings.TrimRight(value, "/"), s.resource.URL)
}
