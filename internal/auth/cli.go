package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"regexp"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// The login of the forecrew command line: a person signed in in the browser
// allows the CLI, which listens on a loopback address, and gets back a
// one-time code. The CLI redeems the code with its PKCE verifier for a
// personal token. Codes live in memory for a few minutes: a restart only
// makes a login in progress start again.
const (
	cliCodeTTL = 5 * time.Minute
	// maxCLICodes bounds the codes waiting to be redeemed.
	maxCLICodes = 1000
	// PrefixCLICode starts a code.
	PrefixCLICode = "fcl_"
)

// ErrCLICode means a code is unknown, used, expired or its verifier is wrong.
var ErrCLICode = errors.New("the login code is not valid: start the login again")

// ErrCLIRequest means the request to allow the CLI is not valid.
var ErrCLIRequest = errors.New("invalid login request")

type cliCode struct {
	userID, accountID uuid.UUID
	challenge         string
	expires           time.Time
}

// CLIAuthorizeInput is what the CLI asks the browser to allow.
type CLIAuthorizeInput struct {
	// RedirectURI is a loopback address of the CLI: http://127.0.0.1:port/path.
	RedirectURI string `json:"redirect_uri"`
	State       string `json:"state"`
	// CodeChallenge is the PKCE S256 challenge of the CLI's verifier.
	CodeChallenge string `json:"code_challenge"`
	// Organization is the slug the token is for; the session's own when empty.
	Organization string `json:"organization,omitempty"`
}

// CLIAuthorizeOutput is where the browser goes next, and the code, for a
// person whose browser cannot reach the CLI to paste.
type CLIAuthorizeOutput struct {
	Redirect string `json:"redirect"`
	Code     string `json:"code"`
}

var challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

// loopback checks that u is an http address on the loopback interface.
func loopback(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.Port() == "" {
		return nil, false
	}
	ip := net.ParseIP(u.Hostname())
	return u, ip != nil && ip.IsLoopback()
}

// CLIAuthorize lets the session c allow the CLI at in.RedirectURI. The token
// it leads to is for the session's organization, or in.Organization, of which
// the user must be an active member.
func (s *Service) CLIAuthorize(ctx context.Context, c mcp.Caller, in CLIAuthorizeInput) (CLIAuthorizeOutput, error) {
	var out CLIAuthorizeOutput
	userID, ok := c.Actor.SubjectID()
	if !ok || c.Actor.Credential.Kind != domain.CredentialSession {
		return out, ErrCLIRequest
	}
	target, ok := loopback(in.RedirectURI)
	if !ok || len(in.State) > 256 || !challengeRE.MatchString(in.CodeChallenge) {
		return out, ErrCLIRequest
	}
	accountID := c.Actor.AccountID
	if in.Organization != "" {
		a, err := s.cfg.Store.Accounts().GetBySlug(ctx, in.Organization)
		if errors.Is(err, store.ErrNotFound) {
			return out, ErrNotMember
		}
		if err != nil {
			return out, err
		}
		if err := checkOrigin(ctx, s.cfg.Store, userID, c.Actor.Credential.ID, a.ID); err != nil {
			return out, err
		}
		accountID = a.ID
	}
	if accountID == uuid.Nil() {
		return out, ErrNotMember
	}
	m, err := s.cfg.Store.Memberships().Get(ctx, accountID, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.Status != domain.MembershipActive) {
		return out, ErrNotMember
	}
	if err != nil {
		return out, err
	}
	code, err := NewToken(PrefixCLICode)
	if err != nil {
		return out, err
	}
	now := s.cfg.Now()
	s.cliMu.Lock()
	defer s.cliMu.Unlock()
	for k, v := range s.cliCodes {
		if now.After(v.expires) {
			delete(s.cliCodes, k)
		}
	}
	if len(s.cliCodes) >= maxCLICodes {
		return out, errors.New("too many logins in progress: try again in a few minutes")
	}
	s.cliCodes[sha256.Sum256([]byte(code))] = cliCode{userID: userID, accountID: accountID, challenge: in.CodeChallenge, expires: now.Add(cliCodeTTL)}
	q := target.Query()
	q.Set("code", code)
	if in.State != "" {
		q.Set("state", in.State)
	}
	target.RawQuery = q.Encode()
	return CLIAuthorizeOutput{Redirect: target.String(), Code: code}, nil
}

// CLIExchange redeems a code: it is single use, whatever the outcome, and
// the verifier must match the challenge the code was made for. It returns
// who the token is for.
func (s *Service) CLIExchange(code, verifier string) (userID, accountID uuid.UUID, err error) {
	key := sha256.Sum256([]byte(code))
	s.cliMu.Lock()
	c, ok := s.cliCodes[key]
	delete(s.cliCodes, key)
	s.cliMu.Unlock()
	if !ok || s.cfg.Now().After(c.expires) {
		return uuid.Nil(), uuid.Nil(), ErrCLICode
	}
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.challenge)) != 1 {
		return uuid.Nil(), uuid.Nil(), ErrCLICode
	}
	return c.userID, c.accountID, nil
}
