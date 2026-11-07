package auth

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"
	"uuid"

	"golang.org/x/oauth2"

	"github.com/forecrew/forecrew/internal/authz"
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
	Setup      string `json:"u,omitempty"` // setup link token, from the cookie POST /auth/setup set
	// Return is the page of this site to go back to after the sign-in.
	Return string `json:"r,omitempty"`
	// Test marks a test of a connection: the flow signs nobody in and only
	// shows what the provider says. Who tests it is kept on the server
	// (pendingTest), never in this cookie.
	Test string `json:"t,omitempty"`
}

// AuthorizePath is the OAuth authorization endpoint (internal/oauth). A
// sign-in on the way to it comes back there through Return, like to any
// other page of this site.
const AuthorizePath = "/oauth/authorize"

// Handler serves /auth/<connection>/login and /auth/<connection>/callback.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{connection}/login", s.login)
	mux.HandleFunc("GET /auth/{connection}/callback", s.callback)
	mux.HandleFunc("POST /auth/setup", s.setupOrganization)
	return mux
}

// connection finds a connection a sign-in may use: an enabled one, or with
// test any of an organization's own, which is tested before it is enabled.
func (s *Service) connection(ctx context.Context, id string, test bool) (domain.Connection, error) {
	cid, err := uuid.Parse(id)
	if err != nil {
		return domain.Connection{}, store.ErrNotFound
	}
	c, err := s.cfg.Store.Connections().Get(ctx, cid)
	if err == nil && !c.Enabled && (!test || c.AccountID == uuid.Nil()) {
		return c, store.ErrNotFound
	}
	return c, err
}

// authorizeTest returns the user who may test conn: a member of its
// organization with connection.manage, signed in with the session cookie.
func (s *Service) authorizeTest(ctx context.Context, r *http.Request, conn domain.Connection) (uuid.UUID, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || conn.AccountID == uuid.Nil() {
		return uuid.Nil(), refusal(reasonTestDenied)
	}
	caller, err := s.Resolve(ctx, c.Value)
	if err != nil {
		return uuid.Nil(), refusal(reasonTestDenied)
	}
	id, ok := caller.Actor.SubjectID()
	if !ok || caller.Actor.AccountID != conn.AccountID {
		return uuid.Nil(), refusal(reasonTestDenied)
	}
	if err := s.authz.Authorize(ctx, caller.Actor, domain.PermConnectionManage, authz.Organization(conn.AccountID)); err != nil {
		return uuid.Nil(), refusal(reasonTestDenied)
	}
	return id, nil
}

func flowCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: loginCookie, Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

// login starts a sign-in: it remembers the flow in the cookie and sends the
// browser to the provider.
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	test := r.URL.Query().Get("test") != ""
	var testBy uuid.UUID
	conn, err := s.connection(r.Context(), r.PathValue("connection"), test)
	if err != nil {
		s.refuse(w, r, refusal(reasonUnknownConnection), err)
		return
	}
	f := flow{Connection: conn.ID.String(), Org: r.URL.Query().Get("org"), Invite: r.URL.Query().Get("invite"), Return: sitePath(r.URL.Query().Get("redirect")), Verifier: oauth2.GenerateVerifier()}
	if c, err := r.Cookie(setupCookie); err == nil && !test {
		f.Setup = c.Value
	}
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	if test {
		tester, err := s.authorizeTest(r.Context(), r, conn)
		if err != nil {
			s.refuse(w, r, nil, err)
			return
		}
		f = flow{Connection: conn.ID.String(), Verifier: f.Verifier, Test: "1"}
		testBy = tester
	}
	p, err := s.provider(r.Context(), conn)
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	if f.State, err = NewToken(""); err == nil {
		f.Nonce, err = NewToken("")
	}
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	if test {
		s.rememberTest(f.State, testBy, conn.ID)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		s.refuse(w, r, refusal(reasonProvider), err)
		return
	}
	http.SetCookie(w, flowCookie(base64.RawURLEncoding.EncodeToString(raw), int(loginTTL.Seconds())))
	NoStore(w)
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
	// A test is what the server remembers, not what the cookie claims.
	tester, pending := s.takeTest(f.State, f.Connection)
	if f.Test != "" && !pending {
		s.refuse(w, r, refusal(reasonState), nil)
		return
	}
	conn, err := s.connection(r.Context(), f.Connection, f.Test != "")
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
	if f.Test != "" {
		s.testResult(w, r, conn, tester, claims)
		return
	}
	token, err := s.signIn(r.Context(), conn, claims, f, r.UserAgent(), RemoteIP(r))
	if err != nil {
		s.refuse(w, r, nil, err)
		return
	}
	http.SetCookie(w, s.SessionCookie(token))
	NoStore(w)
	// The cookie is not signed: check the path again.
	http.Redirect(w, r, cmp.Or(sitePath(f.Return), "/"), http.StatusFound)
}

// sitePath returns target when it is a path on this site, else nothing: a
// sign-in must never send the person to another site.
func sitePath(target string) string {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || len(target) > 2048 {
		return ""
	}
	// Browsers drop tabs and newlines from addresses, and read a backslash as
	// a slash: "/\t/evil.example" would go to another site.
	for _, r := range target {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return ""
		}
	}
	return target
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

// RemoteIP is the address of the peer of r; empty when it is not an IP.
func RemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil {
		return ""
	}
	return host
}

// NoStore keeps a page that carries codes or tokens out of caches and
// referrers.
func NoStore(w http.ResponseWriter) {
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
	reasonSetupLink         = "setup_link_invalid"
	reasonDisabled          = "user_disabled"
	reasonSuspended         = "membership_suspended"
	reasonTestDenied        = "test_denied"
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
	reasonSetupLink:       {http.StatusForbidden, "This setup link cannot be used", "It expired, was used already, is for another organization, or the organization has an owner already: a setup link only makes the first owner. Create a new one on the server with forecrew setup-link, or invite people."},
	reasonDisabled:        {http.StatusForbidden, "This account is disabled", "Ask your administrator."},
	reasonSuspended:       {http.StatusForbidden, "Your membership is suspended", "Ask an owner of your organization."},
	reasonTestDenied:      {http.StatusForbidden, "You cannot test this sign-in", "Sign in as an administrator of the organization that owns it, then start the test again from its settings."},
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
	NoStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(page.status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%[1]s</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem"><h1>%[1]s</h1><p>%[2]s</p><p><a href="/login">Back to sign-in</a></p></body></html>
`, html.EscapeString(page.title), html.EscapeString(page.text))
}

// testResult ends a test sign-in: it records that the connection works and
// shows what the provider said, and signs nobody in. The page is plain
// escaped text.
func (s *Service) testResult(w http.ResponseWriter, r *http.Request, conn domain.Connection, tester uuid.UUID, c Claims) {
	by := domain.UserActor(conn.AccountID, tester, domain.Credential{})
	// The tester may have lost the right since the test began.
	if err := s.authz.Authorize(r.Context(), by, domain.PermConnectionManage, authz.Organization(conn.AccountID)); err != nil {
		s.refuse(w, r, refusal(reasonTestDenied), err)
		return
	}
	if err := audit(r.Context(), s.cfg.Store, by, "connection.tested", "connection", conn.ID, nil); err != nil {
		s.refuse(w, r, nil, err)
		return
	}
	NoStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sign-in works</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem"><h1>Sign-in works</h1>
<p>%s answered. Nobody was signed in. You can close this window and turn the sign-in on.</p>
<dl><dt>Name</dt><dd>%s</dd><dt>Email</dt><dd>%s</dd><dt>Email verified</dt><dd>%t</dd><dt>Login</dt><dd>%s</dd></dl></body></html>
`, html.EscapeString(conn.Name), html.EscapeString(c.Name), html.EscapeString(c.Email), c.EmailVerified, html.EscapeString(c.Login))
}

// pendingTest is a test of a connection waiting for the provider: who started
// it and for which connection, found by the SHA-256 of its state.
type pendingTest struct {
	tester, connection uuid.UUID
	expires            time.Time
}

// maxPendingTests bounds the tests waiting at once.
const maxPendingTests = 1000

// rememberTest keeps a test until its callback or loginTTL.
func (s *Service) rememberTest(state string, tester, connection uuid.UUID) {
	now := s.cfg.Now()
	s.testMu.Lock()
	defer s.testMu.Unlock()
	for k, v := range s.tests {
		if now.After(v.expires) {
			delete(s.tests, k)
		}
	}
	if len(s.tests) < maxPendingTests {
		s.tests[sha256.Sum256([]byte(state))] = pendingTest{tester: tester, connection: connection, expires: now.Add(loginTTL)}
	}
}

// takeTest returns the test of state, once: it is single use whatever the
// outcome. It must be for the connection of the callback.
func (s *Service) takeTest(state, connection string) (uuid.UUID, bool) {
	key := sha256.Sum256([]byte(state))
	s.testMu.Lock()
	t, ok := s.tests[key]
	delete(s.tests, key)
	s.testMu.Unlock()
	if !ok || s.cfg.Now().After(t.expires) || t.connection.String() != connection {
		return uuid.Nil(), false
	}
	return t.tester, true
}
