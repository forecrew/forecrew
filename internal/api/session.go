package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// CookieName is the cookie of a browser session.
const CookieName = auth.CookieName

// protect requires a credential: the bearer header, or the session cookie
// when there is no header. A cookie is sent by the browser on its own, so
// with it a state-changing request must be application/json, which a
// cross-site form cannot send, and come from our own origin.
func (s *Server) protect(next http.Handler) http.Handler {
	bearer := s.auth(next)
	tokenCookie := s.auth(s.tokenCookie(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if r.Header.Get("Authorization") != "" || err != nil || c.Value == "" {
			bearer.ServeHTTP(w, r)
			return
		}
		if !s.checkCookieRequest(w, r) {
			return
		}
		w = &clearOn401{ResponseWriter: w} // a dead session is dropped from the browser
		if s.cfg.TokenLogin && strings.HasPrefix(c.Value, auth.PrefixToken) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+c.Value)
			tokenCookie.ServeHTTP(w, r)
			return
		}
		s.session(next, c.Value).ServeHTTP(w, r)
	})
}

// tokenCookie ends a token login once its user's organization requires a
// connection, as it ends a session from another connection.
func (s *Server) tokenCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		by, _ := mcp.ActorFromContext(r.Context())
		err := auth.CheckTokenLogin(r.Context(), s.cfg.Store, by, false)
		switch {
		case errors.Is(err, auth.ErrSSORequired):
			s.fail(w, r, errNotAuthenticated, http.StatusUnauthorized, "unauthorized")
		case err != nil:
			s.failTool(w, r, by, err)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// session serves next as the caller of the session token.
func (s *Server) session(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Auth == nil {
			s.fail(w, r, errNotAuthenticated, http.StatusUnauthorized, "unauthorized")
			return
		}
		caller, err := s.cfg.Auth.Resolve(r.Context(), token)
		switch {
		case errors.Is(err, auth.ErrNoSession):
			s.fail(w, r, errNotAuthenticated, http.StatusUnauthorized, "unauthorized")
		case err != nil:
			s.cfg.Logger.ErrorContext(r.Context(), "resolve session", "error", err)
			s.fail(w, r, errors.New("internal error, try again later"), http.StatusInternalServerError, "internal")
		default:
			next.ServeHTTP(w, r.WithContext(context.WithValue(mcp.WithCaller(r.Context(), caller), browserKey{}, true)))
		}
	})
}

// needOrganization refuses a session that has not picked an organization;
// everything but /me and the session endpoints works in one.
func (s *Server) needOrganization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if by, ok := mcp.ActorFromContext(r.Context()); ok && by.AccountID == uuid.Nil() {
			s.fail(w, r, errors.New("pick an organization first"), http.StatusForbidden, "no_organization")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkCookieRequest applies the rules of a request that a browser may send
// on its own, and answers when it breaks them.
func (s *Server) checkCookieRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if !isJSON(r) {
		s.fail(w, r, errors.New("requests with a session cookie must be application/json"), http.StatusUnsupportedMediaType, "unsupported_media_type")
		return false
	}
	if !s.sameOrigin(r) {
		s.fail(w, r, errors.New("cross-origin request refused"), http.StatusForbidden, "bad_origin")
		return false
	}
	return true
}

// sameOrigin reports whether the Origin header, when a browser sent one,
// is the public URL or the host the request was sent to.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" {
		return false
	}
	if o.Host == r.Host {
		return true
	}
	public, err := url.Parse(s.cfg.PublicURL)
	return err == nil && o.Scheme == public.Scheme && o.Host == public.Host
}

// browserKey marks a request that came with a browser session.
type browserKey struct{}

// fromBrowser reports whether r came with a session of the web UI. A token
// never counts, in a header or in the cookie of token_login: this is how a
// plan a model wrote is confirmed by a person.
func fromBrowser(r *http.Request) bool {
	ok, _ := r.Context().Value(browserKey{}).(bool)
	return ok
}

// clearOn401 clears the session cookie when the answer is 401.
type clearOn401 struct{ http.ResponseWriter }

func (c *clearOn401) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		http.SetCookie(c.ResponseWriter, auth.Cookie("", -1))
	}
	c.ResponseWriter.WriteHeader(status)
}

// Session is the answer of POST /api/v1/session and GET /api/v1/me: who the
// caller is, its current organization and what it may do there, for the UI
// to hide what it cannot.
type Session struct {
	Token string `json:"token,omitempty"` // name of the token, when the caller uses one
	// User is the person; empty for a service account or an ownerless token.
	User *domain.User `json:"user,omitempty"`
	// Account is the current organization; empty until a session picks one.
	Account domain.Account `json:"account,omitzero"`
	// Organizations are the ones a session can switch to.
	Organizations []auth.Organization `json:"organizations,omitempty"`
	// Enterprise is true in the enterprise build: the UI offers its screens.
	Enterprise bool `json:"enterprise,omitempty"`
	authz.Effective
}

// login is the legacy token login: it checks the token with the same
// middleware as every other call and stores it in the session cookie. It only
// works with auth.token_login.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.cfg.TokenLogin {
		s.fail(w, r, errors.New("token login is off: sign in through a connection"), http.StatusForbidden, "token_login_disabled")
		return
	}
	// A cross-site form can send text/plain that parses as JSON, and would
	// log the victim into the attacker's account.
	if !isJSON(r) {
		s.fail(w, r, errors.New("login must be application/json"), http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	if !s.sameOrigin(r) {
		s.fail(w, r, errors.New("cross-origin request refused"), http.StatusForbidden, "bad_origin")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		s.badRequest(w, r, err)
		return
	}
	if body.Token == "" {
		s.fail(w, r, errors.New("token is required"), http.StatusBadRequest, "bad_request")
		return
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+body.Token)
	s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		by, _ := mcp.ActorFromContext(r.Context())
		if err := auth.CheckTokenLogin(r.Context(), s.cfg.Store, by, true); errors.Is(err, auth.ErrSSORequired) {
			s.fail(w, r, err, http.StatusForbidden, "sso_required")
			return
		} else if err != nil {
			s.failTool(w, r, by, err)
			return
		}
		http.SetCookie(w, auth.Cookie(body.Token, int(s.cfg.SessionMaxAge.Seconds())))
		s.me(w, r)
	})).ServeHTTP(w, r)
}

// logout ends the session of the cookie and clears it. It needs no
// credential: signing out is harmless, and a dead cookie must still go.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		if !s.checkCookieRequest(w, r) {
			return
		}
		if s.cfg.Auth != nil {
			if err := s.cfg.Auth.SignOut(r.Context(), c.Value); err != nil {
				s.cfg.Logger.ErrorContext(r.Context(), "sign out", "error", err)
				s.fail(w, r, errors.New("internal error, try again later"), http.StatusInternalServerError, "internal")
				return
			}
		}
	}
	http.SetCookie(w, auth.Cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// signOutEverywhere ends every session of the calling user.
func (s *Server) signOutEverywhere(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	by, _ := mcp.ActorFromContext(r.Context())
	id, ok := by.SubjectID()
	if !ok || by.Kind != domain.ActorUser || s.cfg.Auth == nil {
		s.fail(w, r, errors.New("only a person has sessions"), http.StatusBadRequest, "invalid_input")
		return
	}
	// A token, of any scope, does not end the person's sessions.
	if by.Credential.Kind != domain.CredentialSession {
		s.fail(w, r, errors.New("sessions are ended from a browser session, not with a token"), http.StatusForbidden, "forbidden")
		return
	}
	if err := s.cfg.Auth.SignOutEverywhere(r.Context(), id); err != nil {
		s.failTool(w, r, by, err)
		return
	}
	http.SetCookie(w, auth.Cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// switchOrganization moves the session to another organization.
func (s *Server) switchOrganization(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	caller, _ := mcp.CallerFromContext(r.Context())
	if s.cfg.Auth == nil || caller.Actor.Credential.Kind != domain.CredentialSession {
		s.fail(w, r, errors.New("a token belongs to one organization"), http.StatusBadRequest, "invalid_input")
		return
	}
	var body struct {
		Organization string `json:"organization"` // slug
	}
	if err := decode(r, &body); err != nil {
		s.badRequest(w, r, err)
		return
	}
	acc, err := s.cfg.Auth.Switch(r.Context(), caller, body.Organization)
	if errors.Is(err, auth.ErrNotMember) || errors.Is(err, auth.ErrSSORequired) {
		s.fail(w, r, err, http.StatusForbidden, "forbidden")
		return
	}
	if err != nil {
		s.failTool(w, r, caller.Actor, err)
		return
	}
	caller.Actor.AccountID = acc.ID
	s.me(w, r.WithContext(mcp.WithCaller(r.Context(), caller)))
}

// profile returns the user and the organizations they can switch to, which
// only sessions do.
func (s *Server) profile(ctx context.Context, c mcp.Caller, id uuid.UUID) (domain.User, []auth.Organization, error) {
	if s.cfg.Auth != nil {
		return s.cfg.Auth.Profile(ctx, c)
	}
	u, err := s.cfg.Store.Users().Get(ctx, id)
	return u, nil, err
}

// discover tells a sign-in page which connections to show. It is public:
// connection names and kinds are what the page displays anyway.
func (s *Server) signInOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in auth.DiscoverInput
	if err := decode(r, &in); err != nil {
		s.badRequest(w, r, err)
		return
	}
	out := auth.Discovery{Connections: []auth.ConnectionInfo{}, TokenLogin: s.cfg.TokenLogin}
	if s.cfg.Auth != nil {
		var err error
		if out, err = s.cfg.Auth.Discover(r.Context(), in); err != nil {
			s.failTool(w, r, domain.Actor{}, err)
			return
		}
	}
	s.write(w, r, http.StatusOK, out)
}

// me answers with who the caller is: the person, the organizations they can
// switch to, the current one and what they may do there.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	caller, ok := mcp.CallerFromContext(r.Context())
	if !ok {
		s.fail(w, r, errors.New("not authenticated"), http.StatusUnauthorized, "unauthorized")
		return
	}
	by := caller.Actor
	out := Session{Token: caller.TokenName, Enterprise: s.cfg.Enterprise}
	if by.AccountID != uuid.Nil() {
		acc, err := s.cfg.Store.Accounts().Get(r.Context(), by.AccountID)
		if err != nil {
			s.failTool(w, r, by, err)
			return
		}
		out.Account = acc
	}
	if by.AccountID == uuid.Nil() {
		out.Effective = authz.Effective{Roles: []authz.HeldRole{}, Permissions: []domain.Permission{}, Projects: map[string][]domain.Permission{}}
	} else {
		eff, err := s.cfg.Authz.EffectiveOf(r.Context(), by)
		if err != nil {
			s.failTool(w, r, by, err)
			return
		}
		out.Effective = eff
	}
	if id, ok := by.SubjectID(); ok && by.Kind == domain.ActorUser {
		user, orgs, err := s.profile(r.Context(), caller, id)
		if err != nil {
			s.failTool(w, r, by, err)
			return
		}
		out.User, out.Organizations = &user, orgs
	}
	s.write(w, r, http.StatusOK, out)
}
