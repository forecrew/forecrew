package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

var (
	errBadGrant  = refuse(http.StatusBadRequest, "invalid_grant", "the code or refresh token is invalid, expired or revoked")
	errBadClient = refuse(http.StatusUnauthorized, "invalid_client", "client authentication failed")
)

// tokenAnswer is a successful answer of the token endpoint.
type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// tokens is the token endpoint.
func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	if !s.token.allow(auth.RemoteIP(r), s.cfg.Now()) {
		tooMany(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, refuse(http.StatusBadRequest, "invalid_request", "the body must be a form"))
		return
	}
	c, err := s.authenticate(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var out tokenAnswer
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		out, err = s.exchange(r.Context(), c, r.PostForm)
	case "refresh_token":
		out, err = s.refresh(r.Context(), c, r.PostForm)
	default:
		err = refuse(http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// authenticate finds the client of a token request: by its secret, for a
// confidential client, in the header or the form.
func (s *Server) authenticate(r *http.Request) (domain.OAuthClient, error) {
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if user, password, ok := r.BasicAuth(); ok {
		var err error
		if id, err = url.QueryUnescape(user); err != nil {
			return domain.OAuthClient{}, errBadClient
		}
		if secret, err = url.QueryUnescape(password); err != nil {
			return domain.OAuthClient{}, errBadClient
		}
	}
	c, err := s.cfg.Store.OAuth().GetClient(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return c, errBadClient
	}
	if err != nil {
		return c, err
	}
	if c.SecretHash != nil && subtle.ConstantTimeCompare(c.SecretHash, mcp.HashToken(secret)) != 1 {
		return c, errBadClient
	}
	return c, nil
}

// credential finds the code or refresh token of a request. A used one is a
// reuse: someone holds what they should not, so the family ends.
func (s *Server) credential(ctx context.Context, c domain.OAuthClient, kind domain.OAuthGrantKind, token string) (domain.OAuthGrant, error) {
	g, err := s.cfg.Store.OAuth().GetGrantByTokenHash(ctx, mcp.HashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && (g.Kind != kind || g.ClientID != c.ID)) {
		return g, errBadGrant
	}
	if err != nil {
		return g, err
	}
	if !g.UsedAt.IsZero() && g.RevokedAt.IsZero() {
		return g, s.reused(ctx, g)
	}
	if !g.Usable(s.cfg.Now()) {
		return g, errBadGrant
	}
	return g, s.stillMember(ctx, g)
}

// stillMember refuses a user who lost access, and ends their grants there.
func (s *Server) stillMember(ctx context.Context, g domain.OAuthGrant) error {
	ok, err := mcp.UserActive(ctx, s.cfg.Store, g.AccountID, g.UserID)
	if err != nil || ok {
		return err
	}
	if err := s.cfg.Store.OAuth().RevokeByUser(ctx, g.AccountID, g.UserID); err != nil {
		return err
	}
	return errBadGrant
}

// reused revokes the family of g and answers invalid_grant.
func (s *Server) reused(ctx context.Context, g domain.OAuthGrant) error {
	s.cfg.Logger.WarnContext(ctx, "oauth credential reused, family revoked", "account_id", g.AccountID, "family_id", g.FamilyID, "kind", g.Kind)
	data, err := json.Marshal(map[string]string{"family_id": g.FamilyID.String(), "kind": string(g.Kind)})
	if err != nil {
		return err
	}
	by := domain.SystemActor(g.AccountID, domain.ForecrewdName)
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.OAuth().RevokeFamily(ctx, g.FamilyID); err != nil {
			return err
		}
		return tx.Audit().Append(ctx, &domain.AuditEntry{AccountID: g.AccountID, Actor: by.String(), Action: "oauth.reuse_detected",
			SubjectType: "oauth_family", SubjectID: g.FamilyID, Data: data})
	})
	if err != nil {
		return err
	}
	return errBadGrant
}

// exchange trades a code for tokens. The code is single use, a failed attempt
// included: whoever sends a wrong verifier does not get to try another.
func (s *Server) exchange(ctx context.Context, c domain.OAuthClient, form url.Values) (tokenAnswer, error) {
	g, err := s.credential(ctx, c, domain.OAuthCode, form.Get("code"))
	if err != nil {
		return tokenAnswer{}, err
	}
	redirect := form.Get("redirect_uri")
	if !verifierMatches(form.Get("code_verifier"), g.CodeChallenge) || (redirect != "" && redirect != g.RedirectURI) {
		if err := s.cfg.Store.OAuth().UseGrant(ctx, g.ID); err != nil && !errors.Is(err, store.ErrConflict) {
			return tokenAnswer{}, err
		}
		return tokenAnswer{}, errBadGrant
	}
	if err := s.checkResource(form, g); err != nil {
		return tokenAnswer{}, err
	}
	return s.redeem(ctx, g, g.Scopes)
}

// verifierMatches checks PKCE S256, the only method there is.
func verifierMatches(verifier, challenge string) bool {
	if verifier == "" {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(h[:])), []byte(challenge)) == 1
}

func (s *Server) checkResource(form url.Values, g domain.OAuthGrant) error {
	if v := form.Get("resource"); v != "" && !s.sameResource(v) {
		return refuse(http.StatusBadRequest, "invalid_target", "this grant is for "+g.Resource)
	}
	return nil
}

// refresh rotates a refresh token. Scopes may narrow, never widen.
func (s *Server) refresh(ctx context.Context, c domain.OAuthClient, form url.Values) (tokenAnswer, error) {
	g, err := s.credential(ctx, c, domain.OAuthRefresh, form.Get("refresh_token"))
	if err != nil {
		return tokenAnswer{}, err
	}
	if err := s.checkResource(form, g); err != nil {
		return tokenAnswer{}, err
	}
	scopes := g.Scopes
	if form.Get("scope") != "" {
		if scopes, err = parseScopes(form.Get("scope")); err != nil {
			return tokenAnswer{}, err
		}
		for _, sc := range scopes {
			if !slices.ContainsFunc(g.Scopes, func(have domain.Scope) bool { return have.Includes(sc) }) {
				return tokenAnswer{}, refuse(http.StatusBadRequest, "invalid_scope", "a refresh cannot add scopes")
			}
		}
	}
	return s.redeem(ctx, g, scopes)
}

// redeem uses the code or refresh token and issues the next access and
// refresh tokens of its family, all or nothing, so that a failure here does
// not look like a reuse on the retry.
func (s *Server) redeem(ctx context.Context, g domain.OAuthGrant, scopes []domain.Scope) (tokenAnswer, error) {
	access, err := auth.NewToken(mcp.PrefixAccessToken)
	if err != nil {
		return tokenAnswer{}, err
	}
	refresh, err := auth.NewToken(auth.PrefixRefresh)
	if err != nil {
		return tokenAnswer{}, err
	}
	now := s.cfg.Now()
	next := func(kind domain.OAuthGrantKind, token string) *domain.OAuthGrant {
		return &domain.OAuthGrant{Kind: kind, TokenHash: mcp.HashToken(token), FamilyID: g.FamilyID, ClientID: g.ClientID, UserID: g.UserID,
			AccountID: g.AccountID, Scopes: scopes, Resource: g.Resource}
	}
	a, f := next(domain.OAuthAccess, access), next(domain.OAuthRefresh, refresh)
	a.ExpiresAt, f.ExpiresAt = now.Add(accessTTL), now.Add(refreshTTL)
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.OAuth().UseGrant(ctx, g.ID); err != nil {
			return err
		}
		if err := tx.OAuth().CreateGrant(ctx, a); err != nil {
			return err
		}
		if err := tx.OAuth().CreateGrant(ctx, f); err != nil {
			return err
		}
		return tx.OAuth().TouchClient(ctx, g.ClientID)
	})
	if errors.Is(err, store.ErrConflict) { // two requests with one credential: the loser is the reuse
		return tokenAnswer{}, s.reused(ctx, g)
	}
	if err != nil {
		return tokenAnswer{}, err
	}
	names := make([]string, len(scopes))
	for i, sc := range scopes {
		names[i] = string(sc)
	}
	return tokenAnswer{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(accessTTL.Seconds()), RefreshToken: refresh, Scope: strings.Join(names, " ")}, nil
}
