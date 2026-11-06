package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"uuid"

	"golang.org/x/oauth2"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// flow is a sign-in on its way through the provider. It lives in a
// short-lived cookie of the browser that started it.
type flow struct {
	Connection string `json:"c"`
	State      string `json:"s"`
	Nonce      string `json:"n"`
	Verifier   string `json:"v"`
	Org        string `json:"o,omitempty"` // slug the sign-in page was for
	Invite     string `json:"i,omitempty"` // invitation token of the link
}

// Handler serves /auth/<connection>/login and /auth/<connection>/callback.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{connection}/login", s.login)
	mux.HandleFunc("GET /auth/{connection}/callback", s.callback)
	return mux
}

func (s *Service) connection(ctx context.Context, id string) (domain.Connection, error) {
	cid, err := uuid.Parse(id)
	if err != nil {
		return domain.Connection{}, store.ErrNotFound
	}
	c, err := s.cfg.Store.Connections().Get(ctx, cid)
	if err == nil && !c.Enabled {
		return c, store.ErrNotFound
	}
	return c, err
}

func flowCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: loginCookie, Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

// login starts a sign-in: it remembers the flow in the cookie and sends the
// browser to the provider.
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	conn, err := s.connection(r.Context(), r.PathValue("connection"))
	if err != nil {
		s.refuse(w, r, refusal(reasonUnknownConnection), err)
		return
	}
	p, err := s.provider(r.Context(), conn)
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	f := flow{Connection: conn.ID.String(), Org: r.URL.Query().Get("org"), Invite: r.URL.Query().Get("invite"), Verifier: oauth2.GenerateVerifier()}
	if f.State, err = NewToken(""); err == nil {
		f.Nonce, err = NewToken("")
	}
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	raw, err := json.Marshal(f)
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	http.SetCookie(w, flowCookie(base64.RawURLEncoding.EncodeToString(raw), int(loginTTL.Seconds())))
	noStore(w)
	http.Redirect(w, r, p.authURL(f.State, f.Nonce, f.Verifier), http.StatusFound) // #nosec G710 -- the connection's own endpoint
}

// readFlow returns the flow of the cookie when it is for this connection and
// carries the state the provider sent back.
func readFlow(r *http.Request, connection string) (flow, bool) {
	var f flow
	c, err := r.Cookie(loginCookie)
	if err != nil {
		return f, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || json.Unmarshal(raw, &f) != nil {
		return f, false
	}
	state := r.URL.Query().Get("state")
	return f, f.Connection == connection && state != "" && subtle.ConstantTimeCompare([]byte(state), []byte(f.State)) == 1
}

// callback finishes a sign-in. The flow cookie is single use.
func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, flowCookie("", -1))
	f, ok := readFlow(r, r.PathValue("connection"))
	if !ok {
		s.refuse(w, r, refusal(reasonState), nil)
		return
	}
	conn, err := s.connection(r.Context(), f.Connection)
	if err != nil {
		s.refuse(w, r, refusal(reasonUnknownConnection), err)
		return
	}
	if r.URL.Query().Get("error") != "" {
		s.refuse(w, r, refusal(reasonDenied), nil)
		return
	}
	p, err := s.provider(r.Context(), conn)
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	claims, err := p.exchange(r.Context(), r.URL.Query().Get("code"), f.Nonce, f.Verifier)
	if err == nil && claims.Subject == "" {
		err = errors.New("the provider named no subject")
	}
	if err != nil {
		s.refuse(w, r, refusal(reasonRejected), err)
		return
	}
	token, err := s.signIn(r.Context(), conn, claims, f, r.UserAgent(), remoteIP(r))
	if err != nil {
		s.refuse(w, r, nil, err)
		return
	}
	http.SetCookie(w, s.SessionCookie(token))
	noStore(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// SessionCookie is the cookie of a session token, for as long as a session
// can last.
func (s *Service) SessionCookie(token string) *http.Cookie {
	return Cookie(token, int(s.cfg.Auth.SessionAbsolute.Seconds()))
}

// Cookie is the session cookie; maxAge -1 clears it.
func Cookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: token, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil {
		return ""
	}
	return host
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // the URLs carry codes and invitation tokens
}

// Why a sign-in stops. The reason is logged; the text is the page the person
// sees.
const (
	reasonUnknownConnection = "unknown_connection"
	reasonState             = "bad_state"
	reasonDenied            = "denied_at_provider"
	reasonProvider          = "provider_unavailable"
	reasonRejected          = "rejected_response"
	reasonNoMembership      = "no_membership"
	reasonInvitation        = "invitation_invalid"
	reasonInvitationOther   = "invitation_for_someone_else"
	reasonDisabled          = "user_disabled"
	reasonSuspended         = "membership_suspended"
)

var reasons = map[string]struct {
	status int
	title  string
	text   string
}{
	reasonUnknownConnection: {http.StatusNotFound, "This sign-in is not available", "The sign-in connection does not exist or is turned off."},
	reasonState:             {http.StatusBadRequest, "That sign-in expired", "Start again from the sign-in page, in the same browser."},
	reasonDenied:            {http.StatusForbidden, "Sign-in was canceled", "The identity provider did not sign you in. Start again from the sign-in page."},
	reasonProvider:          {http.StatusBadGateway, "The identity provider is not available", "Try again in a moment. If it keeps failing, ask your administrator to check the connection."},
	reasonRejected:          {http.StatusForbidden, "Sign-in failed", "The identity provider's answer could not be accepted. Start again from the sign-in page."},
	reasonNoMembership: {http.StatusForbidden, "You do not have access yet",
		"You signed in, but this account is not a member of an organization here. Ask an owner of your organization for an invitation, and open its link."},
	reasonInvitation:      {http.StatusForbidden, "This invitation cannot be used", "It expired, was revoked or was accepted already. Ask for a new one."},
	reasonInvitationOther: {http.StatusForbidden, "This invitation is for someone else", "Sign in with the account the invitation was sent to, with a verified email address."},
	reasonDisabled:        {http.StatusForbidden, "This account is disabled", "Ask your administrator."},
	reasonSuspended:       {http.StatusForbidden, "Your membership is suspended", "Ask an owner of your organization."},
}

// refused is a sign-in stopped for a reason.
type refused struct{ reason string }

func (r *refused) Error() string { return "sign-in refused: " + r.reason }

func refusal(reason string) *refused { return &refused{reason: reason} }

// refuse answers with the page of the reason. cause is logged; it never holds
// a code, token or secret (see exchangeError).
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, why *refused, cause error) {
	if why == nil {
		var ok bool
		if why, ok = errors.AsType[*refused](cause); ok {
			cause = nil
		} else {
			why = refusal(reasonProvider)
		}
	}
	log := s.cfg.Logger.With("connection_id", r.PathValue("connection"), "reason", why.reason)
	if cause != nil {
		log = log.With("error", cause)
	}
	log.WarnContext(r.Context(), "sign-in refused")
	page := reasons[why.reason]
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(page.status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%[1]s</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem"><h1>%[1]s</h1><p>%[2]s</p><p><a href="/login">Back to sign-in</a></p></body></html>
`, html.EscapeString(page.title), html.EscapeString(page.text))
}
