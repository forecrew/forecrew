// Package auth signs people in (docs/contracts/identity.md): forecrewd is an
// OpenID Connect relying party of the connections, GitHub included through
// the GitHub App's user authorization. A sign-in resolves to a user, the user
// gets a server-side session, and every request with the session cookie is
// resolved here to the mcp.Caller the rest of forecrewd works with.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/netguard"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
)

// Token prefixes (docs/contracts/identity.md).
const (
	PrefixToken      = "fcm_"
	PrefixSession    = "fcs_"
	PrefixInvitation = "fci_"
	PrefixCode       = "fco_"
	PrefixRefresh    = "fcr_"
	PrefixSCIM       = "fcscim_"
)

// CookieName is the cookie of a browser session.
const CookieName = "forecrew_session"

// loginCookie holds the state of a sign-in on its way through the provider.
const loginCookie = "forecrew_login"

// loginTTL is how long a sign-in may take at the provider.
const loginTTL = 10 * time.Minute

// providerTimeout bounds every call to a provider.
const providerTimeout = 15 * time.Second

const secretDir = "/run/secrets"

// ErrNoSession means the cookie names no usable session.
var ErrNoSession = errors.New("not signed in")

// ErrNotMember means the organization of a switch is not one the user is an
// active member of.
var ErrNotMember = errors.New("not an active member of that organization")

// ErrSSORequired means the organization takes the user only through the
// connection it enforces.
var ErrSSORequired = errors.New("this organization requires its single sign-on")

// Policy is the enterprise side of a sign-in (ee/). Its methods run inside
// the sign-in's transaction, so a refused sign-in keeps nothing.
type Policy interface {
	// Provisions says whether conn may bring in someone nobody invited.
	Provisions(conn domain.Connection) bool
	// Admit runs once the user is known: just-in-time membership and group
	// mapping. An error stops the sign-in.
	Admit(ctx context.Context, tx store.Store, conn domain.Connection, c Claims, user domain.User) error
}

// noPolicy is the open-core build: invitations only, no groups.
type noPolicy struct{}

func (noPolicy) Provisions(domain.Connection) bool { return false }

func (noPolicy) Admit(context.Context, store.Store, domain.Connection, Claims, domain.User) error {
	return nil
}

// Config holds the dependencies of a Service.
type Config struct {
	Store store.Store
	Auth  config.Auth
	// BaseURL is the public URL of forecrewd, without a trailing slash.
	BaseURL string
	Logger  *slog.Logger // defaults to discarding
	Now     func() time.Time
	// HTTP calls the providers of instance connections and OrgHTTP those of
	// organizations, whose issuer the organization chooses: by default it
	// reaches public addresses only. Tests replace both.
	HTTP, OrgHTTP *http.Client
	// GitHubURL and GitHubAPIURL default to github.com; tests replace them.
	GitHubURL, GitHubAPIURL string
	// Getenv and ReadFile read connection secrets; they default to the
	// process environment and the file system.
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	// Secrets resolves store: references; nil refuses them.
	Secrets secrets.Store
	// Policy is registered by the enterprise build; nil is open core.
	Policy Policy
}

// Service signs people in and resolves sessions.
type Service struct {
	cfg   Config
	authz *authz.Engine
	mu    sync.Mutex
	oidc  map[string]*oidc.Provider // by issuer: discovery and JWKS are cached

	testMu sync.Mutex
	tests  map[[32]byte]pendingTest // tests of connections waiting for the provider, by SHA-256 of state

	cliMu    sync.Mutex
	cliCodes map[[32]byte]cliCode // codes of CLI logins by SHA-256
}

// New returns the service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: providerTimeout}
	}
	if cfg.OrgHTTP == nil {
		cfg.OrgHTTP = &http.Client{Timeout: providerTimeout, Transport: netguard.PublicTransport()}
	}
	if cfg.GitHubURL == "" {
		cfg.GitHubURL = "https://github.com"
	}
	if cfg.GitHubAPIURL == "" {
		cfg.GitHubAPIURL = "https://api.github.com"
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.ReadFile == nil {
		cfg.ReadFile = os.ReadFile
	}
	if cfg.Auth.SessionIdle == 0 {
		cfg.Auth.SessionIdle = config.DefaultSessionIdle
	}
	if cfg.Auth.SessionAbsolute == 0 {
		cfg.Auth.SessionAbsolute = config.DefaultSessionAbsolute
	}
	if cfg.Policy == nil {
		cfg.Policy = noPolicy{}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Service{cfg: cfg, authz: authz.New(cfg.Store, cfg.Logger), oidc: map[string]*oidc.Provider{}, cliCodes: map[[32]byte]cliCode{}, tests: map[[32]byte]pendingTest{}}
}

// NewToken returns a random token: 32 bytes, base64url without padding.
func NewToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// clientSecret reads the secret of a connection; it stays in memory only. A
// store: reference reads in the connection's own organization.
func (s *Service) clientSecret(ctx context.Context, c domain.Connection) (string, error) {
	ref := c.ClientSecretRef
	if _, ok := domain.StoreSecretName(ref); ok {
		if s.cfg.Secrets == nil {
			return "", errors.New("the secret store is not configured")
		}
		b, err := secrets.Resolve(ctx, s.cfg.Secrets, c.AccountID, ref)
		if err != nil {
			return "", fmt.Errorf("read the secret of %s: %w", c.Name, err)
		}
		return string(b), nil
	}
	if name, ok := strings.CutPrefix(ref, domain.SecretRefEnv); ok {
		v := s.cfg.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("%s is not set", name)
		}
		return v, nil
	}
	if name, ok := strings.CutPrefix(ref, domain.SecretRefFile); ok {
		b, err := s.cfg.ReadFile(path.Join(secretDir, name))
		if err != nil {
			return "", fmt.Errorf("read secret %s: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("unknown secret reference form")
}

// audit records what by did in the organization of by.
func audit(ctx context.Context, st store.Store, by domain.Actor, action, subject string, id uuid.UUID, data []byte) error {
	return st.Audit().Append(ctx, &domain.AuditEntry{AccountID: by.AccountID, Actor: by.String(), Credential: by.Credential.String(),
		Action: action, SubjectType: subject, SubjectID: id, Data: data})
}
