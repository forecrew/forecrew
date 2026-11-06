package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"uuid"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/forecrew/forecrew/internal/domain"
)

// maxProviderBody bounds what is read from a provider's API.
const maxProviderBody = 1 << 20

// Claims is what a provider says about who signed in. Only these are kept;
// provider tokens never leave exchange.
type Claims struct {
	Subject       string // OIDC sub, or the GitHub user ID
	Login         string // GitHub login, or the provider's preferred username
	Email         string
	EmailVerified bool
	Name          string
}

// provider is the sign-in protocol of a connection.
type provider interface {
	// authURL is where the browser goes to sign in.
	authURL(state, nonce, verifier string) string
	// exchange redeems the code and validates what comes back.
	exchange(ctx context.Context, code, nonce, verifier string) (Claims, error)
}

// redirectURL is the callback of a connection.
func (s *Service) redirectURL(id uuid.UUID) string {
	return s.cfg.BaseURL + "/auth/" + id.String() + "/callback"
}

// client is the HTTP client for the provider of c.
func (s *Service) client(c domain.Connection) *http.Client {
	if c.AccountID != uuid.Nil() {
		return s.cfg.OrgHTTP
	}
	return s.cfg.HTTP
}

// provider returns the protocol of c.
func (s *Service) provider(ctx context.Context, c domain.Connection) (provider, error) {
	secret, err := s.clientSecret(c.ClientSecretRef)
	if err != nil {
		return nil, err
	}
	conf := oauth2.Config{ClientID: c.ClientID, ClientSecret: secret, RedirectURL: s.redirectURL(c.ID)}
	if c.Kind == domain.ConnectionGitHub {
		conf.Endpoint = oauth2.Endpoint{AuthURL: s.cfg.GitHubURL + "/login/oauth/authorize", TokenURL: s.cfg.GitHubURL + "/login/oauth/access_token"}
		return gitHubProvider{conf: conf, client: s.client(c), api: s.cfg.GitHubAPIURL}, nil
	}
	issuer := c.Issuer
	if c.Kind == domain.ConnectionGoogle {
		issuer = domain.GoogleIssuer
	}
	p, err := s.discover(ctx, c, issuer)
	if err != nil {
		return nil, err
	}
	conf.Endpoint = p.Endpoint()
	conf.Scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	return oidcProvider{conf: conf, client: s.client(c), verifier: p.Verifier(&oidc.Config{ClientID: c.ClientID, Now: s.cfg.Now}), microsoft: c.Kind == domain.ConnectionMicrosoft}, nil
}

// discover returns the cached discovery of issuer. The provider keeps its
// context to refresh its keys, so it must outlive the request.
func (s *Service) discover(ctx context.Context, c domain.Connection, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.oidc[issuer]; ok {
		return p, nil
	}
	p, err := oidc.NewProvider(oidc.ClientContext(context.WithoutCancel(ctx), s.client(c)), issuer)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", issuer, err)
	}
	s.oidc[issuer] = p
	return p, nil
}

type oidcProvider struct {
	conf     oauth2.Config
	client   *http.Client
	verifier *oidc.IDTokenVerifier
	// microsoft goes by xms_edov: Entra's email claim is not verified by
	// default and a tenant admin can set it to anything.
	microsoft bool
}

func (p oidcProvider) authURL(state, nonce, verifier string) string {
	return p.conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

// boolClaim is a boolean that some providers send as a string.
type boolClaim bool

// UnmarshalJSON implements json.Unmarshaler.
func (b *boolClaim) UnmarshalJSON(data []byte) error {
	v := strings.Trim(string(data), `"`)
	*b = boolClaim(strings.EqualFold(v, "true") || v == "1")
	return nil
}

func (p oidcProvider) exchange(ctx context.Context, code, nonce, verifier string) (Claims, error) {
	ctx = oidc.ClientContext(ctx, p.client)
	tok, err := p.conf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Claims{}, exchangeError(err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return Claims{}, errors.New("the token response has no id_token")
	}
	id, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("verify id token: %w", err)
	}
	if id.Nonce != nonce {
		return Claims{}, errors.New("the id token has the wrong nonce")
	}
	var c struct {
		Email             string    `json:"email"`
		EmailVerified     boolClaim `json:"email_verified"`
		Name              string    `json:"name"`
		PreferredUsername string    `json:"preferred_username"`
		DomainOwnerProof  boolClaim `json:"xms_edov"`
	}
	if err := id.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("read id token claims: %w", err)
	}
	verified := bool(c.EmailVerified)
	if p.microsoft {
		verified = bool(c.DomainOwnerProof)
	}
	return Claims{Subject: id.Subject, Login: c.PreferredUsername, Email: c.Email, EmailVerified: verified, Name: c.Name}, nil
}

// exchangeError drops what a provider answered: only its status is logged.
func exchangeError(err error) error {
	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		return fmt.Errorf("token endpoint answered %d %s", re.Response.StatusCode, re.ErrorCode)
	}
	return errors.New("token endpoint could not be reached")
}

type gitHubProvider struct {
	conf   oauth2.Config
	client *http.Client
	api    string
}

// GitHub has no ID token, so no nonce: state and PKCE bind the code.
func (p gitHubProvider) authURL(state, _, verifier string) string {
	return p.conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (p gitHubProvider) exchange(ctx context.Context, code, _, verifier string) (Claims, error) {
	tok, err := p.conf.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.client), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Claims{}, exchangeError(err)
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := p.get(ctx, tok.AccessToken, "/user", &user); err != nil {
		return Claims{}, err
	}
	if user.ID <= 0 {
		return Claims{}, errors.New("github answered without a user ID")
	}
	c := Claims{Subject: strconv.FormatInt(user.ID, 10), Login: user.Login, Name: user.Name}
	// Without the app's "Email addresses" permission this fails; the person
	// can still sign in by identity or GitHub login.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := p.get(ctx, tok.AccessToken, "/user/emails", &emails); err == nil {
		for _, e := range emails {
			if e.Primary {
				c.Email, c.EmailVerified = e.Email, e.Verified
			}
		}
	}
	return c, nil
}

// ErrNoGitHubUser means GitHub knows no such login.
var ErrNoGitHubUser = errors.New("no such GitHub user")

// GitHubUserID asks GitHub for the stable ID of a login, which is what an
// identity and an invitation hold: a login can be renamed and then taken by
// someone else.
func (s *Service) GitHubUserID(ctx context.Context, login string) (int64, error) {
	return LookupGitHubUser(ctx, s.cfg.HTTP, s.cfg.GitHubAPIURL, login)
}

// LookupGitHubUser is GitHubUserID for callers without a Service.
func LookupGitHubUser(ctx context.Context, client *http.Client, api, login string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return 0, fmt.Errorf("look up %s: %w", login, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("look up %s on GitHub: %w", login, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("look up %s on GitHub: status 404: %w", login, ErrNoGitHubUser)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("look up %s on GitHub: status %d", login, resp.StatusCode)
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderBody)).Decode(&user); err != nil || user.ID <= 0 {
		return 0, fmt.Errorf("look up %s on GitHub: unexpected answer", login)
	}
	return user.ID, nil
}

func (p gitHubProvider) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.api+path, nil)
	if err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("github %s could not be reached", path)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %s answered %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderBody)).Decode(out); err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}
	return nil
}
