package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// person returns the caller when it is a person with sessions, else it
// answers and returns false.
func (s *Server) person(w http.ResponseWriter, r *http.Request) (mcp.Caller, bool) {
	w.Header().Set("Cache-Control", "no-store")
	c, _ := mcp.CallerFromContext(r.Context())
	if _, ok := c.Actor.SubjectID(); !ok || c.Actor.Kind != domain.ActorUser || s.cfg.Auth == nil {
		s.fail(w, r, errors.New("only a person has sessions and sign-ins"), http.StatusBadRequest, "invalid_input")
		return c, false
	}
	return c, true
}

// sessions lists the active sessions of the calling user.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	c, ok := s.person(w, r)
	if !ok {
		return
	}
	out, err := s.cfg.Auth.Sessions(r.Context(), c)
	if err != nil {
		s.failTool(w, r, c.Actor, err)
		return
	}
	s.write(w, r, http.StatusOK, out)
}

// endSession ends one session of the calling user.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	c, ok := s.person(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("session"))
	if err == nil {
		err = s.cfg.Auth.EndSession(r.Context(), c, id)
	}
	switch {
	case errors.Is(err, auth.ErrNotFound) || id == uuid.Nil():
		s.fail(w, r, errors.New("no such session"), http.StatusNotFound, "not_found")
	case err != nil:
		s.fail(w, r, errors.New("not a session ID"), http.StatusBadRequest, "invalid_input")
	default:
		if c.Actor.Credential.Kind == domain.CredentialSession && c.Actor.Credential.ID == id {
			http.SetCookie(w, auth.Cookie("", -1))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// identities lists the sign-ins linked to the calling user.
func (s *Server) identities(w http.ResponseWriter, r *http.Request) {
	c, ok := s.person(w, r)
	if !ok {
		return
	}
	out, err := s.cfg.Auth.Identities(r.Context(), c)
	if err != nil {
		s.failTool(w, r, c.Actor, err)
		return
	}
	s.write(w, r, http.StatusOK, out)
}

// cliAuthorize lets a signed-in person allow the CLI on a loopback address.
func (s *Server) cliAuthorize(w http.ResponseWriter, r *http.Request) {
	c, ok := s.person(w, r)
	if !ok {
		return
	}
	var in auth.CLIAuthorizeInput
	if err := decode(r, &in); err != nil {
		s.badRequest(w, r, err)
		return
	}
	out, err := s.cfg.Auth.CLIAuthorize(r.Context(), c, in)
	switch {
	case errors.Is(err, auth.ErrCLIRequest):
		s.fail(w, r, err, http.StatusBadRequest, "invalid_input")
	case errors.Is(err, auth.ErrNotMember):
		s.fail(w, r, err, http.StatusForbidden, "forbidden")
	case err != nil:
		s.failTool(w, r, c.Actor, err)
	default:
		s.write(w, r, http.StatusOK, out)
	}
}

// CLITokenInput is the request of POST /api/v1/cli/token.
type CLITokenInput struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	// Name is what the token is called; it is made a valid name, and gets a
	// number when the person has a token of that name.
	Name string `json:"name"`
}

// CLIToken is the answer of POST /api/v1/cli/token.
type CLIToken struct {
	Secret  string         `json:"secret"`
	Token   mcp.TokenInfo  `json:"token"`
	Account domain.Account `json:"account"`
	User    domain.User    `json:"user"`
}

var nonNameRE = regexp.MustCompile(`[^a-z0-9._-]+`)

// cliTokenName makes a token name of what the CLI sent.
func cliTokenName(in string) string {
	n := strings.Trim(nonNameRE.ReplaceAllString(strings.ToLower(in), "-"), "-._")
	if n == "" {
		n = "cli"
	}
	return n[:min(len(n), 56)]
}

// cliToken redeems the code of a CLI login for a personal token. It is
// public: the code and its PKCE verifier are the credential.
func (s *Server) cliToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in CLITokenInput
	if err := decode(r, &in); err != nil {
		s.badRequest(w, r, err)
		return
	}
	if s.cfg.Auth == nil {
		s.fail(w, r, errors.New("sign-in is not set up"), http.StatusServiceUnavailable, "unavailable")
		return
	}
	userID, accountID, err := s.cfg.Auth.CLIExchange(in.Code, in.CodeVerifier)
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest, "invalid_input")
		return
	}
	by := domain.UserActor(accountID, userID, domain.Credential{})
	base := cliTokenName(in.Name)
	var out mcp.CreateTokenOutput
	// The token gets the highest scope the person's roles allow.
	for n := 1; n <= 20; n++ {
		name := base
		if n > 1 {
			name = base + "-" + strconv.Itoa(n)
		}
		for _, scope := range []domain.Scope{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead} {
			out, err = s.cfg.Tools.CreateToken(r.Context(), by, mcp.CreateTokenInput{Name: name, Scopes: []domain.Scope{scope}})
			if !errors.Is(err, authz.ErrDenied) {
				break
			}
		}
		if !errors.Is(err, mcp.ErrConflict) {
			break
		}
	}
	if err != nil {
		s.failTool(w, r, by, err)
		return
	}
	acc, err := s.cfg.Store.Accounts().Get(r.Context(), accountID)
	if err == nil {
		var u domain.User
		if u, err = s.cfg.Store.Users().Get(r.Context(), userID); err == nil {
			s.write(w, r, http.StatusOK, CLIToken{Secret: out.Secret, Token: out.Token, Account: acc, User: u})
			return
		}
	}
	s.failTool(w, r, by, err)
}
