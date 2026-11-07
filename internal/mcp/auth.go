package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Caller is who a request is made by and what it was made with. It is the
// one place the credential of a request is recorded: Authenticate sets it for
// tokens and the session middleware of /api/v1 for sessions.
type Caller struct {
	// Actor holds the subject, its organization and its credential.
	Actor domain.Actor
	// Scopes bound a token or OAuth grant; nil for a session, which only has
	// its user's permissions.
	Scopes []domain.Scope
	// TokenName is the name of the token; empty for a session or OAuth grant.
	TokenName string
	// BreakGlass is an instance admin in an organization they are not an
	// active member of (docs/contracts/identity.md, Recovery).
	BreakGlass bool
}

type callerKey struct{}

// WithCaller returns ctx with the caller of the request.
func WithCaller(ctx context.Context, c Caller) context.Context {
	if c.BreakGlass {
		ctx = authz.WithBreakGlass(ctx)
	}
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFromContext returns the caller attached to the request context.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// ActorFromContext returns the actor of the caller attached to the request
// context.
func ActorFromContext(ctx context.Context) (domain.Actor, bool) {
	c, ok := CallerFromContext(ctx)
	return c.Actor, ok
}

// HashToken returns the SHA-256 under which a token is stored.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// errNoAccess means a token is unknown, revoked, expired or its owner may
// not use it any more.
var errNoAccess = errors.New("invalid, expired or revoked token")

// tokenCaller returns the caller of a token, after checking the token and its
// owner: a disabled user, a user who is not an active member and a disabled
// service account have no working tokens (docs/contracts/identity.md).
func tokenCaller(ctx context.Context, st store.Store, token string, now time.Time) (Caller, domain.MCPToken, error) {
	t, err := st.MCPTokens().GetByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !t.Usable(now)) {
		return Caller{}, t, errNoAccess
	}
	if err != nil {
		return Caller{}, t, err
	}
	c := Caller{Actor: TokenActor(t), Scopes: t.ScopesOrLegacy(), TokenName: t.Name}
	switch t.OwnerKind {
	case domain.SubjectUser:
		user, err := st.Users().Get(ctx, t.OwnerID)
		if err != nil {
			return c, t, err
		}
		m, err := st.Memberships().Get(ctx, t.AccountID, t.OwnerID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && m.Status != domain.MembershipActive) || !user.DisabledAt.IsZero() {
			return c, t, errNoAccess
		}
		if err != nil {
			return c, t, err
		}
		limit, err := store.PersonalTokenCap(ctx, st, t.AccountID, t.OwnerID)
		if err != nil {
			return c, t, err
		}
		if limit > 0 && !now.Before(t.CreatedAt.Add(limit)) {
			return c, t, errNoAccess
		}
	case domain.SubjectServiceAccount:
		sa, err := st.ServiceAccounts().Get(ctx, t.AccountID, t.OwnerID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !sa.DisabledAt.IsZero()) {
			return c, t, errNoAccess
		}
		if err != nil {
			return c, t, err
		}
	}
	return c, t, nil
}

// PrefixAccessToken starts an OAuth access token (docs/contracts/identity.md).
const PrefixAccessToken = "fca_"

// Paths of /mcp as an OAuth protected resource.
const (
	ResourcePath = "/mcp"
	MetadataPath = "/.well-known/oauth-protected-resource/mcp"
)

// challengeScopes is what a 401 tells a client to ask for: enough for the
// work of the MCP tools. admin is there for whoever asks for it on purpose.
const challengeScopes = "read write"

// Resource is /mcp as an OAuth protected resource. The zero Resource takes no
// OAuth grants.
type Resource struct {
	URL      string // the audience of access tokens
	Metadata string // the protected resource metadata (RFC 9728)
}

// NewResource returns the resource of the forecrewd at publicURL.
func NewResource(publicURL string) Resource {
	base := strings.TrimRight(publicURL, "/")
	return Resource{URL: base + ResourcePath, Metadata: base + MetadataPath}
}

// challenge is the WWW-Authenticate header of a 401: where a client finds
// out how to sign in. A failed token also gets invalid_token (RFC 6750).
func (res Resource) challenge(failed bool) string {
	if res.URL == "" {
		return "Bearer"
	}
	c := fmt.Sprintf("Bearer resource_metadata=%q, scope=%q", res.Metadata, challengeScopes)
	if failed {
		c += `, error="invalid_token"`
	}
	return c
}

// grantCaller returns the caller of an OAuth access token, after checking
// that it was issued for this resource, is usable, and that its user still
// has access. A user who lost it loses their grants there, whichever path made
// that change (docs/contracts/identity.md).
func grantCaller(ctx context.Context, st store.Store, token string, res Resource, now time.Time) (Caller, error) {
	g, err := st.OAuth().GetGrantByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && (g.Kind != domain.OAuthAccess || g.Resource != res.URL || !g.Usable(now))) {
		return Caller{}, errNoAccess
	}
	if err != nil {
		return Caller{}, err
	}
	ok, err := UserActive(ctx, st, g.AccountID, g.UserID)
	if err != nil {
		return Caller{}, err
	}
	if !ok {
		if err := st.OAuth().RevokeByUser(ctx, g.AccountID, g.UserID); err != nil {
			return Caller{}, err
		}
		return Caller{}, errNoAccess
	}
	cred := domain.Credential{Kind: domain.CredentialOAuthGrant, ID: g.ID}
	return Caller{Actor: domain.UserActor(g.AccountID, g.UserID, cred), Scopes: g.Scopes}, nil
}

// UserActive reports whether the user is enabled and an active member of the
// organization.
func UserActive(ctx context.Context, st store.Store, accountID, userID uuid.UUID) (bool, error) {
	user, err := st.Users().Get(ctx, userID)
	if err != nil {
		return false, err
	}
	m, err := st.Memberships().Get(ctx, accountID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.Status == domain.MembershipActive && user.DisabledAt.IsZero(), nil
}

// Authenticate returns middleware that requires "Authorization: Bearer
// <token>" with a usable personal, service account or ownerless token. It
// looks the token up by hash, checks its owner, records its use, and attaches
// the Caller to the request context; see CallerFromContext. Failures answer
// 401 in the API error format. It serves /api/v1, which takes no OAuth access
// token: those are for /mcp alone. now defaults to the real time.
func Authenticate(st store.Store, now func() time.Time, logger *slog.Logger) func(http.Handler) http.Handler {
	return AuthenticateResource(st, now, logger, Resource{})
}

// bearerCaller returns the caller of a bearer token, and records the use of a
// personal one.
func bearerCaller(ctx context.Context, st store.Store, token string, res Resource, now time.Time, logger *slog.Logger) (Caller, error) {
	if res.URL != "" && strings.HasPrefix(token, PrefixAccessToken) {
		return grantCaller(ctx, st, token, res, now)
	}
	c, t, err := tokenCaller(ctx, st, token, now)
	if err != nil {
		return c, err
	}
	if err := st.MCPTokens().Touch(ctx, t.AccountID, t.ID); err != nil {
		logger.WarnContext(ctx, "touch token", "account_id", t.AccountID, "error", err)
	}
	return c, nil
}

// AuthenticateResource is Authenticate for res: it also takes OAuth access
// tokens for it, and a 401 says how to get one. It serves /mcp.
func AuthenticateResource(st store.Store, now func() time.Time, logger *slog.Logger, res Resource) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			token = strings.TrimSpace(token)
			if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
				writeAuthError(w, http.StatusUnauthorized, res.challenge(false), "unauthorized", "missing bearer token")
				return
			}
			c, err := bearerCaller(r.Context(), st, token, res, now(), logger)
			switch {
			case errors.Is(err, errNoAccess):
				writeAuthError(w, http.StatusUnauthorized, res.challenge(true), "unauthorized", errNoAccess.Error())
				return
			case err != nil:
				logger.ErrorContext(r.Context(), "look up token", "error", err)
				writeAuthError(w, http.StatusInternalServerError, "", "internal", "internal error")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), c)))
		})
	}
}

// TokenActor is who acts with t: its owner, through the token, or mcp:<name>
// for a token without an owner.
func TokenActor(t domain.MCPToken) domain.Actor {
	switch t.OwnerKind {
	case domain.SubjectUser:
		return domain.UserActor(t.AccountID, t.OwnerID, domain.Credential{Kind: domain.CredentialToken, ID: t.ID})
	case domain.SubjectServiceAccount:
		return domain.ServiceAccountActor(t.AccountID, t.OwnerID, t.ID)
	}
	return domain.Actor{AccountID: t.AccountID, Kind: domain.ActorMCP, Name: t.Name, Credential: domain.Credential{Kind: domain.CredentialToken, ID: t.ID}}
}

func writeAuthError(w http.ResponseWriter, status int, challenge, code, message string) {
	if challenge != "" {
		w.Header().Set("WWW-Authenticate", challenge)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent; a failed write has no recovery.
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
