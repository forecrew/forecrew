package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// Session cookie of the web UI.
const (
	// CookieName is the cookie that holds the token of a browser session.
	CookieName = "forecrew_session"
	// SessionMaxAge is how long a browser session lasts.
	SessionMaxAge = 30 * 24 * time.Hour
	// cookiePath keeps the cookie away from /mcp and the worker API.
	cookiePath = "/api/"
)

// protect requires a valid token: the bearer header, or the session cookie
// when there is no header. A cookie is sent by the browser on its own, so
// with it a state-changing request must be application/json, which a
// cross-site form cannot send.
func (s *Server) protect(next http.Handler) http.Handler {
	authed := s.auth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
				if r.Method != http.MethodGet && r.Method != http.MethodHead && !isJSON(r) {
					s.fail(w, r, errors.New("requests with a session cookie must be application/json"), http.StatusUnsupportedMediaType, "unsupported_media_type")
					return
				}
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer "+c.Value)
				w = &clearOn401{ResponseWriter: w} // a dead token is dropped from the browser
			}
		}
		authed.ServeHTTP(w, r)
	})
}

// clearOn401 clears the session cookie when the answer is 401.
type clearOn401 struct{ http.ResponseWriter }

func (c *clearOn401) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		http.SetCookie(c.ResponseWriter, sessionCookie("", -1))
	}
	c.ResponseWriter.WriteHeader(status)
}

// Session is the answer of POST /api/v1/session and GET /api/v1/me: who the
// caller is, its current organization and what it may do there, for the UI
// to hide what it cannot.
type Session struct {
	Token   string         `json:"token"` // name of the token
	Account domain.Account `json:"account"`
	// User is the person behind the call; nil for a service account and an
	// ownerless token.
	User *domain.User `json:"user,omitempty"`
	authz.Effective
}

// login checks the token with the same middleware as every other call and
// stores it in the session cookie.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// A cross-site form can send text/plain that parses as JSON, and would
	// log the victim into the attacker's account.
	if !isJSON(r) {
		s.fail(w, r, errors.New("login must be application/json"), http.StatusUnsupportedMediaType, "unsupported_media_type")
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
		http.SetCookie(w, sessionCookie(body.Token, int(SessionMaxAge/time.Second)))
		s.me(w, r)
	})).ServeHTTP(w, r)
}

// logout clears the session cookie. It needs no token: clearing is harmless.
func (s *Server) logout(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, sessionCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

func sessionCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: token, Path: cookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

// me answers with the caller, its organization and its effective permissions.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	by, ok := mcp.ActorFromContext(r.Context())
	if !ok {
		s.fail(w, r, errors.New("not authenticated"), http.StatusUnauthorized, "unauthorized")
		return
	}
	acc, err := s.cfg.Accounts.Get(r.Context(), by.AccountID)
	if err != nil {
		s.failTool(w, r, by, err)
		return
	}
	eff, err := s.cfg.Authz.EffectiveOf(r.Context(), by)
	if err != nil {
		s.failTool(w, r, by, err)
		return
	}
	session := Session{Token: by.Name, Account: acc, Effective: eff}
	if id, ok := by.SubjectID(); ok && by.Kind == domain.ActorUser {
		u, err := s.cfg.Directory.User(r.Context(), id)
		if err != nil {
			s.failTool(w, r, by, err)
			return
		}
		session.User = &u
	}
	s.write(w, r, http.StatusOK, session)
}
