package github

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v93/github"
)

const (
	defaultBaseURL = "https://api.github.com/"
	// refreshMargin is how long before expiry a cached token is replaced.
	refreshMargin = 5 * time.Minute
	// jwtLifetime is below GitHub's 10 minute maximum.
	jwtLifetime = 9 * time.Minute
)

// Config configures a Client.
type Config struct {
	AppID int64
	// PrivateKey is the GitHub App private key, PEM.
	PrivateKey []byte
	// BaseURL is the API root; empty means https://api.github.com/.
	BaseURL string
	// HTTPClient is used for all calls; nil means a client with a 30s timeout.
	HTTPClient *http.Client
	// Now returns the current time; nil means time.Now().UTC().
	Now func() time.Time
	// Sleep waits for d or until ctx ends; nil means a real timer. Rate-limit
	// retries wait through it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client implements Tracker over a GitHub App. Safe for concurrent use.
type Client struct {
	appID   int64
	key     *rsa.PrivateKey
	base    *url.URL
	http    *http.Client
	now     func() time.Time
	retrier *retryTransport

	mu            sync.Mutex
	installations map[string]int64 // lowercase repo -> installation ID
	own           map[string]Token // lowercase repo -> the Client's own token
}

// New returns a Client. It fails when the private key is unusable.
func New(cfg Config) (*Client, error) {
	if cfg.AppID <= 0 {
		return nil, fmt.Errorf("new github client: %w: app id must be positive", ErrInvalid)
	}
	key, err := parseKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("new github client: %w", err)
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = defaultBaseURL
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("new github client: parse base url: %w", err)
	}
	c := &Client{
		appID: cfg.AppID, key: key, base: base, now: cfg.Now,
		installations: map[string]int64{}, own: map[string]Token{},
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	rt := hc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.retrier = &retryTransport{base: rt, now: c.now, sleep: cfg.Sleep}
	if c.retrier.sleep == nil {
		c.retrier.sleep = sleep
	}
	copied := *hc
	copied.Transport = c.retrier
	c.http = &copied
	return c, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// api returns a go-github client authenticated with token.
func (c *Client) api(token string) (*gh.Client, error) {
	base := c.base.String()
	g, err := gh.NewClient(gh.WithHTTPClient(c.http), gh.WithAuthToken(token), gh.WithURLs(&base, nil))
	if err != nil {
		return nil, fmt.Errorf("new api client: %w", err)
	}
	return g, nil
}

// appAPI returns a client authenticated as the App itself (JWT).
func (c *Client) appAPI() (*gh.Client, error) {
	jwt, err := signJWT(c.key, c.appID, c.now())
	if err != nil {
		return nil, err
	}
	return c.api(jwt)
}

// repoClient returns a client with a cached installation token for repo.
func (c *Client) repoClient(ctx context.Context, repo string) (cl *gh.Client, owner, name string, err error) {
	owner, name, err = splitRepo(repo)
	if err != nil {
		return nil, "", "", err
	}
	tok, err := c.token(ctx, repo, "")
	if err != nil {
		return nil, "", "", err
	}
	cl, err = c.api(tok.Value)
	return cl, owner, name, err
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repo %q: %w: want owner/name", repo, ErrInvalid)
	}
	return owner, name, nil
}

// IsInstalled implements Tracker.
func (c *Client) IsInstalled(ctx context.Context, repo string) (bool, error) {
	_, err := c.installationID(ctx, repo)
	if errors.Is(err, ErrNotInstalled) {
		return false, nil
	}
	return err == nil, err
}

// InstallationToken implements Tracker. The token is narrowed to repo and to
// contents read or write access. It is minted on every call and never cached,
// so a worker always gets a token with its full lifetime.
func (c *Client) InstallationToken(ctx context.Context, repo string, access TokenAccess) (Token, error) {
	if access != AccessRead && access != AccessWrite {
		return Token{}, fmt.Errorf("installation token: %w: unknown access %q", ErrInvalid, access)
	}
	return c.token(ctx, repo, access)
}

func (c *Client) installationID(ctx context.Context, repo string) (int64, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	key := strings.ToLower(repo)
	c.mu.Lock()
	id, ok := c.installations[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	app, err := c.appAPI()
	if err != nil {
		return 0, err
	}
	inst, _, err := app.Apps.GetRepositoryInstallation(ctx, owner, name)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return 0, fmt.Errorf("installation of %s: %w", repo, ErrNotInstalled)
		}
		return 0, wrap("find installation of "+repo, err)
	}
	c.mu.Lock()
	c.installations[key] = inst.GetID()
	c.mu.Unlock()
	return inst.GetID(), nil
}

// token mints an installation token for repo. An empty access asks for all
// permissions of the installation: that is the Client's own token, cached
// until refreshMargin before it expires. Job tokens (read or write) are
// always minted fresh.
func (c *Client) token(ctx context.Context, repo string, access TokenAccess) (Token, error) {
	_, name, err := splitRepo(repo)
	if err != nil {
		return Token{}, err
	}
	key := strings.ToLower(repo)
	own := access == ""
	if own {
		c.mu.Lock()
		tok, ok := c.own[key]
		c.mu.Unlock()
		if ok && c.now().Before(tok.ExpiresAt.Add(-refreshMargin)) {
			return tok, nil
		}
	}
	id, err := c.installationID(ctx, repo)
	if err != nil {
		return Token{}, err
	}
	app, err := c.appAPI()
	if err != nil {
		return Token{}, err
	}
	opts := &gh.InstallationTokenOptions{Repositories: []string{name}}
	if access != "" {
		opts.Permissions = &gh.InstallationPermissions{Contents: new(string(access))}
	}
	it, _, err := app.Apps.CreateInstallationToken(ctx, id, opts)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			// The installation was removed; forget its ID.
			c.mu.Lock()
			delete(c.installations, key)
			c.mu.Unlock()
			return Token{}, fmt.Errorf("token for %s: %w", repo, ErrNotInstalled)
		}
		return Token{}, wrap("create installation token for "+repo, err)
	}
	tok := Token{Value: it.GetToken(), ExpiresAt: it.GetExpiresAt().UTC()}
	if own {
		c.mu.Lock()
		c.own[key] = tok
		c.mu.Unlock()
	}
	return tok, nil
}

// statusOf returns the HTTP status of a go-github error, or 0.
func statusOf(err error) int {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		return er.Response.StatusCode
	}
	return 0
}

// wrap turns a go-github error into one wrapping a package sentinel.
func wrap(op string, err error) error {
	var rl *gh.RateLimitError
	var ab *gh.AbuseRateLimitError
	switch {
	case errors.As(err, &rl), errors.As(err, &ab):
		return fmt.Errorf("%s: %w: %w", op, ErrRateLimited, err)
	}
	switch statusOf(err) {
	case http.StatusNotFound:
		return fmt.Errorf("%s: %w: %w", op, ErrNotFound, err)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("%s: %w: %w", op, ErrInvalid, err)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w: %w", op, ErrRateLimited, err)
	case http.StatusForbidden:
		return fmt.Errorf("%s: %w: %w", op, ErrForbidden, err)
	case http.StatusConflict:
		return fmt.Errorf("%s: %w: %w", op, ErrConflict, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
