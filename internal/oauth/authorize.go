package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"golang.org/x/net/idna"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// authRequest is an authorization request, from the query of the first visit
// and from the fields of the consent form.
type authRequest struct {
	ClientID, RedirectURI, ResponseType, State, Scope, Resource, Challenge, Method string
}

func requestOf(form url.Values) authRequest {
	return authRequest{
		ClientID: form.Get("client_id"), RedirectURI: form.Get("redirect_uri"), ResponseType: form.Get("response_type"), State: form.Get("state"),
		Scope: form.Get("scope"), Resource: form.Get("resource"), Challenge: form.Get("code_challenge"), Method: form.Get("code_challenge_method"),
	}
}

// check validates everything that is answered by redirecting to the client,
// and returns the scopes. The client and redirect URI are already known good.
func (s *Server) check(q authRequest) ([]domain.Scope, *protocolError) {
	if q.ResponseType != "code" {
		return nil, refuse(0, "unsupported_response_type", "only the authorization code flow is supported")
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Challenge)
	if q.Method != "S256" || err != nil || len(raw) != sha256.Size {
		return nil, refuse(0, "invalid_request", "PKCE with S256 is required")
	}
	if q.Resource != "" && !s.sameResource(q.Resource) {
		return nil, refuse(0, "invalid_target", "this server only issues tokens for "+s.resource.URL)
	}
	scopes, err := parseScopes(q.Scope)
	if err != nil {
		e, _ := errors.AsType[*protocolError](err)
		return nil, refuse(0, e.code, e.text)
	}
	return scopes, nil
}

// back sends the browser to the client with the answer of the request; iss
// names this server (RFC 9207).
func (s *Server) back(w http.ResponseWriter, r *http.Request, q authRequest, values url.Values) {
	u, err := url.Parse(q.RedirectURI)
	if err != nil {
		s.page(w, r, http.StatusBadRequest, "This app sent a bad request", "Its redirect address cannot be read.")
		return
	}
	query := u.Query()
	maps.Copy(query, values)
	query.Set("iss", s.issuer)
	if q.State != "" {
		query.Set("state", q.State)
	}
	u.RawQuery = query.Encode()
	auth.NoStore(w)
	http.Redirect(w, r, u.String(), http.StatusSeeOther) // #nosec G710 -- a redirect URI registered for this client
}

func (s *Server) backWithError(w http.ResponseWriter, r *http.Request, q authRequest, e *protocolError) {
	s.back(w, r, q, url.Values{"error": {e.code}, "error_description": {e.text}})
}

// signedIn returns the person of the session cookie; ok is false without one.
// A legacy token cookie is no sign-in here: a token's scopes are not the
// person's, so it must not consent to more than it may do.
func (s *Server) signedIn(r *http.Request) (c mcp.Caller, cookie string, ok bool, err error) {
	ck, err := r.Cookie(auth.CookieName)
	if err != nil || ck.Value == "" {
		return c, "", false, nil
	}
	c, err = s.cfg.Auth.Resolve(r.Context(), ck.Value)
	if errors.Is(err, auth.ErrNoSession) {
		return c, "", false, nil
	}
	return c, ck.Value, err == nil, err
}

// toLogin sends the browser through sign-in and back here. The session cookie
// is SameSite=Strict, so it is also how a person arriving from another site
// gets here with it: the sign-in page, once it sees a session, comes back with
// a same-site navigation.
func (s *Server) toLogin(w http.ResponseWriter, r *http.Request) {
	auth.NoStore(w)
	http.Redirect(w, r, "/login?"+url.Values{"redirect": {auth.AuthorizePath + "?" + r.URL.RawQuery}}.Encode(), http.StatusFound)
}

// validated resolves the client and its redirect URI. Without both there is
// nowhere safe to send an answer, so a failure is a page here.
func (s *Server) validated(w http.ResponseWriter, r *http.Request, c domain.OAuthClient, q authRequest, err error) bool {
	switch {
	case errors.Is(err, errUnknownClient):
		s.page(w, r, http.StatusBadRequest, "This app is not registered", "Forecrew does not know this app, or could not read its description. Connect it again from the app.")
	case err != nil:
		s.cfg.Logger.ErrorContext(r.Context(), "find oauth client", "error", err)
		s.page(w, r, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
	case !redirectMatches(c.RedirectURIs, q.RedirectURI):
		s.page(w, r, http.StatusBadRequest, "This app sent a bad request", "The address it wants to return to is not one it registered, so Forecrew will not send you there.")
	default:
		return true
	}
	return false
}

// authorize is the first visit: it shows the consent screen.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	caller, cookie, ok, err := s.signedIn(r)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "resolve session", "error", err)
		s.page(w, r, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	if !ok {
		if ck, err := r.Cookie(auth.CookieName); err == nil && strings.HasPrefix(ck.Value, auth.PrefixToken) {
			s.page(w, r, http.StatusForbidden, "Sign in to connect an app", "You logged in with a token, which cannot authorize an app. Sign in through a connection instead.")
			return
		}
		s.toLogin(w, r)
		return
	}
	q := requestOf(r.URL.Query())
	c, err := s.resolveClient(r.Context(), q.ClientID, auth.RemoteIP(r))
	if !s.validated(w, r, c, q, err) {
		return
	}
	userID, _ := caller.Actor.SubjectID()
	known, err := s.cfg.Store.OAuth().UserAuthorized(r.Context(), c.ID, userID)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read oauth grants", "error", err)
		s.page(w, r, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	scopes, e := s.check(q)
	if e != nil {
		// Without a click, a bad request would send the browser to any
		// registered address: only an app the person connected before gets
		// its error back, the others get a page here.
		if !known {
			s.page(w, r, http.StatusBadRequest, "This app sent a bad request", "Forecrew did not send you back to it. What it asked for: "+e.text+".")
			return
		}
		s.backWithError(w, r, q, e)
		return
	}
	_, orgs, err := s.cfg.Auth.Profile(r.Context(), caller)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read profile", "error", err)
		s.page(w, r, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	if len(orgs) == 0 {
		s.page(w, r, http.StatusForbidden, "You do not have access yet", "This account is not an active member of an organization here, so there is nothing to connect the app to.")
		return
	}
	s.render(w, r, http.StatusOK, consentPage, consentView{
		Client: c.Name, ClientHost: shownHost(c.ClientID), RedirectHost: shownHost(q.RedirectURI), Loopback: isLoopback(q.RedirectURI), New: !known,
		Organizations: orgs, Scopes: describe(scopes), Request: q, CSRF: csrfToken(cookie),
	})
}

// consent is the answer of the form.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.page(w, r, http.StatusBadRequest, "This request cannot be read", "Start again from the app.")
		return
	}
	caller, cookie, ok, err := s.signedIn(r)
	if err != nil || !ok {
		s.page(w, r, http.StatusForbidden, "That session ended", "Start again from the app.")
		return
	}
	given := []byte(r.PostForm.Get("csrf"))
	if subtle.ConstantTimeCompare(given, []byte(csrfToken(cookie))) != 1 {
		s.page(w, r, http.StatusForbidden, "That form expired", "Start again from the app.")
		return
	}
	q := requestOf(r.PostForm)
	c, err := s.cfg.Store.OAuth().GetClient(r.Context(), q.ClientID)
	if errors.Is(err, store.ErrNotFound) {
		err = errUnknownClient
	}
	if !s.validated(w, r, c, q, err) {
		return
	}
	scopes, e := s.check(q)
	if e != nil {
		s.backWithError(w, r, q, e)
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		s.backWithError(w, r, q, refuse(0, "access_denied", "the person did not allow this app"))
		return
	}
	_, orgs, err := s.cfg.Auth.Profile(r.Context(), caller)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read profile", "error", err)
		s.page(w, r, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	i := slices.IndexFunc(orgs, func(o auth.Organization) bool { return o.Slug == r.PostForm.Get("organization") })
	if i < 0 {
		s.page(w, r, http.StatusForbidden, "You are not in that organization", "Pick one of your organizations. Start again from the app.")
		return
	}
	userID, _ := caller.Actor.SubjectID()
	code, err := s.issueCode(r, c, q, scopes, userID, orgs[i], caller.Actor.Credential)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "issue authorization code", "error", err)
		s.page(w, r, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	s.back(w, r, q, url.Values{"code": {code}})
}

// issueCode stores the code of a new family and audits the consent.
func (s *Server) issueCode(r *http.Request, c domain.OAuthClient, q authRequest, scopes []domain.Scope, userID uuid.UUID, org auth.Organization, cred domain.Credential) (string, error) {
	code, err := auth.NewToken(auth.PrefixCode)
	if err != nil {
		return "", err
	}
	g := domain.OAuthGrant{Kind: domain.OAuthCode, TokenHash: mcp.HashToken(code), FamilyID: domain.NewID(), ClientID: c.ID, UserID: userID, AccountID: org.ID,
		Scopes: scopes, Resource: s.resource.URL, RedirectURI: q.RedirectURI, CodeChallenge: q.Challenge, ExpiresAt: s.cfg.Now().Add(codeTTL)}
	data, err := json.Marshal(map[string]any{"client": c.ClientID, "name": c.Name, "scopes": scopes, "family_id": g.FamilyID.String()})
	if err != nil {
		return "", err
	}
	by := domain.UserActor(org.ID, userID, cred)
	err = s.cfg.Store.WithTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		if err := tx.OAuth().CreateGrant(ctx, &g); err != nil {
			return err
		}
		if err := tx.OAuth().TouchClient(ctx, c.ID); err != nil {
			return err
		}
		return tx.Audit().Append(ctx, &domain.AuditEntry{AccountID: org.ID, Actor: by.String(), Credential: by.Credential.String(),
			Action: "oauth.authorized", SubjectType: "oauth_client", SubjectID: c.ID, Data: data})
	})
	return code, err
}

// csrfToken ties the consent form to the session: it is derived from the
// session's secret, so another site cannot compute it, and reveals nothing of it.
func csrfToken(cookie string) string {
	h := sha256.Sum256([]byte("oauth-consent:" + cookie))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// shownHost is the host of raw as the consent screen shows it: in its ASCII
// (punycode) form, so a look-alike in another script does not read as the
// host it imitates. A host that has no such form is shown quoted.
func shownHost(raw string) string {
	h := hostOf(raw)
	if h == "" || net.ParseIP(h) != nil {
		return h
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return strconv.QuoteToASCII(h)
	}
	return ascii
}

func isLoopback(raw string) bool { return loopback(hostOf(raw)) }

// scopeText is what each scope lets an app do, within the person's roles.
var scopeText = map[domain.Scope]string{
	domain.ScopeRead:  "Read projects, goals, jobs, gates and knowledge",
	domain.ScopeWrite: "Create goals, run and cancel jobs, propose knowledge and resolve gates",
	domain.ScopeAdmin: "Manage members, teams, roles, workers and project settings",
}

func describe(scopes []domain.Scope) []string {
	out := []string{}
	for _, sc := range scopes {
		out = append(out, scopeText[sc])
	}
	return out
}

// consentView is what the consent screen shows. Client names are what the
// client says: the template escapes them, and the page says what is verified.
type consentView struct {
	Client, ClientHost, RedirectHost string
	Loopback, New                    bool
	Organizations                    []auth.Organization
	Scopes                           []string
	Request                          authRequest
	CSRF                             string
}

const pageHead = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">`

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Connect an app to Forecrew</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">
<h1>Connect {{.Client}} to Forecrew?</h1>
{{if .New}}<p role="alert"><strong>You have not connected this app before.</strong> The name is what the app calls itself. Continue only if you started this from the app yourself and the address below is the one you expect.</p>{{end}}
<p>After you allow it, you go back to <strong>{{.RedirectHost}}</strong>.{{if .Loopback}} That is an app on this computer, and Forecrew cannot check which one.{{end}}{{if and .ClientHost (ne .ClientHost .RedirectHost)}} The app describes itself at <strong>{{.ClientHost}}</strong>.{{end}}</p>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="client_id" value="{{.Request.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Request.RedirectURI}}">
<input type="hidden" name="response_type" value="{{.Request.ResponseType}}">
<input type="hidden" name="state" value="{{.Request.State}}">
<input type="hidden" name="scope" value="{{.Request.Scope}}">
<input type="hidden" name="resource" value="{{.Request.Resource}}">
<input type="hidden" name="code_challenge" value="{{.Request.Challenge}}">
<input type="hidden" name="code_challenge_method" value="{{.Request.Method}}">
{{if eq (len .Organizations) 1}}{{with index .Organizations 0}}<p>Organization: <strong>{{.Name}}</strong></p><input type="hidden" name="organization" value="{{.Slug}}">{{end}}
{{else}}<p><label>Organization: <select name="organization">{{range .Organizations}}<option value="{{.Slug}}">{{.Name}}</option>{{end}}</select></label></p>{{end}}
<p>It may, as you and never beyond your roles there:</p>
<ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul>
<p>You can disconnect it at any time under Tokens.</p>
<p><button type="submit" name="decision" value="allow">Allow</button> <button type="submit" name="decision" value="deny">Cancel</button></p>
</form></body></html>
`))

var messagePage = template.Must(template.New("message").Parse(pageHead + `<h1>{{.Title}}</h1><p>{{.Text}}</p></body></html>
`))

// render writes a page that must not be framed: a framed consent screen could
// be clicked through by another site.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, t *template.Template, view any) {
	auth.NoStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	if err := t.Execute(w, view); err != nil {
		s.cfg.Logger.WarnContext(r.Context(), "render oauth page", "error", err)
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, title, text string) {
	s.render(w, r, status, messagePage, struct{ Title, Text string }{title, text})
}
