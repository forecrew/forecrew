package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

type actorKey struct{}

// ActorFromContext returns the actor that Authenticate attached to the
// request context.
func ActorFromContext(ctx context.Context) (domain.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(domain.Actor)
	return a, ok
}

// HashToken returns the SHA-256 under which a token is stored.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Authenticate returns middleware that requires "Authorization: Bearer
// <token>" with a known, unrevoked MCP/API token. It looks the token up by
// hash, records its use, and attaches an ActorMCP actor (token name, account
// ID) to the request context; see ActorFromContext. Failures answer 401 in the
// API error format. It serves the MCP endpoint and /api/v1.
func Authenticate(tokens store.MCPTokenRepo, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			token = strings.TrimSpace(token)
			if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
				writeAuthError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			t, err := tokens.GetByTokenHash(r.Context(), HashToken(token))
			switch {
			case errors.Is(err, store.ErrNotFound) || (err == nil && !t.RevokedAt.IsZero()):
				writeAuthError(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked token")
				return
			case err != nil:
				logger.ErrorContext(r.Context(), "look up token", "error", err)
				writeAuthError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			if err := tokens.Touch(r.Context(), t.AccountID, t.ID); err != nil {
				logger.WarnContext(r.Context(), "touch token", "account_id", t.AccountID, "error", err)
			}
			actor := domain.Actor{AccountID: t.AccountID, Kind: domain.ActorMCP, Name: t.Name}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
		})
	}
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
