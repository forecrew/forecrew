package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	// Scopes bound a token; nil for a session, which only has its user's
	// permissions.
	Scopes []domain.Scope
	// TokenName is the name of the token; empty for a session.
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

// Authenticate returns middleware that requires "Authorization: Bearer
// <token>" with a usable personal, service account or ownerless token. It
// looks the token up by hash, checks its owner, records its use, and attaches
// the Caller to the request context; see CallerFromContext. Failures answer
// 401 in the API error format. It serves the MCP endpoint and /api/v1. now
// defaults to the real time.
func Authenticate(st store.Store, now func() time.Time, logger *slog.Logger) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			token = strings.TrimSpace(token)
			if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
				writeAuthError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			c, t, err := tokenCaller(r.Context(), st, token, now())
			switch {
			case errors.Is(err, errNoAccess):
				writeAuthError(w, http.StatusUnauthorized, "unauthorized", errNoAccess.Error())
				return
			case err != nil:
				logger.ErrorContext(r.Context(), "look up token", "error", err)
				writeAuthError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			if err := st.MCPTokens().Touch(r.Context(), t.AccountID, t.ID); err != nil {
				logger.WarnContext(r.Context(), "touch token", "account_id", t.AccountID, "error", err)
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

func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent; a failed write has no recovery.
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
