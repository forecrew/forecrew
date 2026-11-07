package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Client authentication methods at the token endpoint.
const (
	authNone        = "none"
	authSecretPost  = "client_secret_post"
	authSecretBasic = "client_secret_basic"
)

// Bounds of what a client may tell about itself.
const (
	maxRedirectURIs = 10
	maxURILength    = 2000
	maxNameLength   = 100
	maxDocumentSize = 16 << 10
	unnamedClient   = "Unnamed app"
)

// loopback reports whether host is this machine, which an http redirect URI
// may name (RFC 8252).
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validRedirectURI accepts https URIs and loopback http URIs without a
// fragment or credentials.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxURILength || u.Fragment != "" || u.User != nil || u.Hostname() == "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && loopback(u.Hostname()))
}

// redirectMatches compares a requested redirect URI with the registered ones:
// exactly, except that the port of a loopback URI is free, because a native
// app gets it from the system when it runs (RFC 8252, section 7.3).
func redirectMatches(registered []string, raw string) bool {
	if slices.Contains(registered, raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || !validRedirectURI(raw) || !loopback(u.Hostname()) {
		return false
	}
	return slices.ContainsFunc(registered, func(r string) bool {
		x, err := url.Parse(r)
		return err == nil && loopback(x.Hostname()) && x.Scheme == u.Scheme && x.Hostname() == u.Hostname() && x.Path == u.Path && x.RawQuery == u.RawQuery
	})
}

func checkRedirectURIs(uris []string) error {
	if len(uris) == 0 || len(uris) > maxRedirectURIs {
		return fmt.Errorf("between 1 and %d redirect URIs are needed", maxRedirectURIs)
	}
	for _, u := range uris {
		if !validRedirectURI(u) {
			return fmt.Errorf("redirect URI %q must be https or loopback, without a fragment", u)
		}
	}
	return nil
}

// cleanName keeps a client's name to one short printable line.
func cleanName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	if runes := []rune(name); len(runes) > maxNameLength {
		name = string(runes[:maxNameLength])
	}
	if name == "" {
		return unnamedClient
	}
	return name
}

// registration is the part of RFC 7591 client metadata Forecrew reads.
type registration struct {
	RedirectURIs []string `json:"redirect_uris"`
	ClientName   string   `json:"client_name"`
	AuthMethod   string   `json:"token_endpoint_auth_method"`
	GrantTypes   []string `json:"grant_types"`
	Responses    []string `json:"response_types"`
}

// registerClient is dynamic client registration (RFC 7591). The specification
// prefers client ID metadata documents; clients that only know this keep
// working.
func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now()
	if !s.register.allow(auth.RemoteIP(r), now) || !s.registerAll.allow("", now) {
		tooMany(w)
		return
	}
	var in registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes)).Decode(&in); err != nil {
		s.fail(w, r, refuse(http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON object"))
		return
	}
	if err := checkRedirectURIs(in.RedirectURIs); err != nil {
		s.fail(w, r, refuse(http.StatusBadRequest, "invalid_redirect_uri", err.Error()))
		return
	}
	if len(in.GrantTypes) > 0 && !slices.Contains(in.GrantTypes, "authorization_code") {
		s.fail(w, r, refuse(http.StatusBadRequest, "invalid_client_metadata", "grant_types must include authorization_code"))
		return
	}
	if len(in.Responses) > 0 && !slices.Contains(in.Responses, "code") {
		s.fail(w, r, refuse(http.StatusBadRequest, "invalid_client_metadata", "response_types must include code"))
		return
	}
	if in.AuthMethod == "" {
		in.AuthMethod = authNone
	}
	if !slices.Contains([]string{authNone, authSecretPost, authSecretBasic}, in.AuthMethod) {
		s.fail(w, r, refuse(http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method"))
		return
	}
	id, err := auth.NewToken("")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := domain.OAuthClient{ClientID: id, Name: cleanName(in.ClientName), RedirectURIs: in.RedirectURIs}
	out := map[string]any{
		"client_id": id, "client_name": c.Name, "redirect_uris": c.RedirectURIs, "token_endpoint_auth_method": in.AuthMethod,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	}
	if in.AuthMethod != authNone {
		secret, err := auth.NewToken("")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		c.SecretHash = mcp.HashToken(secret)
		out["client_secret"], out["client_secret_expires_at"] = secret, 0
	}
	if err := s.cfg.Store.OAuth().CreateClient(r.Context(), &c); err != nil {
		s.fail(w, r, err)
		return
	}
	out["client_id_issued_at"] = c.CreatedAt.Unix()
	writeJSON(w, http.StatusCreated, out)
}

// metadataURL reports whether a client_id has the form of a client ID
// metadata document URL.
func metadataURL(clientID string) bool {
	u, err := url.Parse(clientID)
	if err != nil || len(clientID) > maxURILength || u.Scheme != "https" || u.Host == "" || u.Fragment != "" || u.User != nil || u.Path == "" || u.Path == "/" {
		return false
	}
	return !slices.ContainsFunc(strings.Split(u.Path, "/"), func(seg string) bool { return seg == "." || seg == ".." })
}

// errUnknownClient is a client_id that is neither registered nor a document.
var errUnknownClient = errors.New("this app is not registered here")

// document is a client ID metadata document.
type document struct {
	registration
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// fetchDocument reads and checks a client ID metadata document. Nothing it
// says is cached: the sign-in of a person is rare, and a document that changed
// or went away must count at once.
func (s *Server) fetchDocument(ctx context.Context, clientID string) (document, error) {
	var d document
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return d, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.cfg.HTTP.Do(req) // #nosec G704 -- the public-only client of Config.HTTP
	if err != nil {
		return d, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return d, fmt.Errorf("status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxDocumentSize+1))
	if err != nil {
		return d, err
	}
	if len(body) > maxDocumentSize {
		return d, errors.New("document too large")
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return d, err
	}
	switch {
	case d.ClientID != clientID:
		return d, errors.New("client_id does not match the URL of the document")
	case d.ClientName == "":
		return d, errors.New("client_name is missing")
	case d.ClientSecret != "" || (d.AuthMethod != "" && d.AuthMethod != authNone):
		return d, errors.New("a document describes a public client")
	}
	return d, checkRedirectURIs(d.RedirectURIs)
}

// resolveClient finds the client of an authorization request: a registered
// one, or the one a metadata document describes, which is stored or updated so
// its grants have a row.
func (s *Server) resolveClient(ctx context.Context, clientID, address string) (domain.OAuthClient, error) {
	repo := s.cfg.Store.OAuth()
	if !metadataURL(clientID) {
		c, err := repo.GetClient(ctx, clientID)
		if errors.Is(err, store.ErrNotFound) {
			return c, errUnknownClient
		}
		return c, err
	}
	if !s.fetch.allow(address, s.cfg.Now()) {
		return domain.OAuthClient{}, errUnknownClient
	}
	d, err := s.fetchDocument(ctx, clientID)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "client metadata document refused", "client_id", clientID, "error", err)
		return domain.OAuthClient{}, errUnknownClient
	}
	c, err := repo.GetClient(ctx, clientID)
	if errors.Is(err, store.ErrNotFound) {
		c = domain.OAuthClient{ClientID: clientID, Name: cleanName(d.ClientName), RedirectURIs: d.RedirectURIs}
		if err := repo.CreateClient(ctx, &c); !errors.Is(err, store.ErrConflict) {
			return c, err
		}
		c, err = repo.GetClient(ctx, clientID) // created meanwhile
	}
	if err != nil {
		return c, err
	}
	c.Name, c.RedirectURIs = cleanName(d.ClientName), d.RedirectURIs
	return c, repo.UpdateClient(ctx, &c)
}
