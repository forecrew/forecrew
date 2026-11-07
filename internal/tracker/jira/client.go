package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/trackerhttp"
)

// Limits of a call to Jira.
const (
	defaultTimeout = 30 * time.Second
	maxResponse    = 16 << 20
	// maxRetryAfter is the longest wait for a rate limit within one call;
	// a longer one fails the call, and the outbox or the next poll tries
	// again.
	maxRetryAfter = time.Minute
	maxAttempts   = 3
)

// Errors of the client.
var (
	// ErrRefused means the address is private or loopback on a server that
	// does not allow those, or redirects to one; it is the councils guard's.
	ErrRefused = trackerhttp.ErrRefused
	errLogin   = errors.New("jira refused the credentials")
	errDenied  = errors.New("jira denied access")
)

// apiError is an answer of Jira that is not a success. Its text comes from
// Jira and is clipped.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("jira answered %d", e.Status)
	}
	return fmt.Sprintf("jira answered %d: %s", e.Status, e.Message)
}

// Unwrap lets callers test for not found, login and access errors.
func (e *apiError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return tracker.ErrNotFound
	case http.StatusUnauthorized:
		return errLogin
	case http.StatusForbidden:
		return errDenied
	}
	return nil
}

// options are the settings of the shared HTTP client, with the connection's
// private CA.
func (d Deps) options(caBundle string) trackerhttp.Options {
	return trackerhttp.Options{PrivateAddresses: d.PrivateAddresses, Proxy: d.Proxy, Lookup: d.Lookup, Dial: d.Dial, Timeout: d.Timeout, CABundle: caBundle}
}

// guarded is the shared guarded transport of these Deps.
type guarded struct {
	d       Deps
	direct  http.RoundTripper
	proxied http.RoundTripper
}

func (g guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	return trackerhttp.Guarded{Options: g.d.options(""), Direct: g.direct, Proxied: g.proxied}.RoundTrip(req)
}

// httpClient returns the shared client: system CAs and caBundle, never an
// unverified certificate, the server's proxy, and public addresses only unless
// the server allows private ones.
func (d Deps) httpClient(caBundle string) (*http.Client, error) {
	return trackerhttp.NewClient(d.options(caBundle))
}

// client calls the REST API of one Jira.
type client struct {
	http *http.Client
	d    Deps
	// auth returns the Authorization header; nil calls anonymously.
	auth func(ctx context.Context) (string, error)
}

// call is one request: path is joined to base, which keeps its context path.
type call struct {
	method string
	base   *url.URL
	path   string
	query  url.Values
	in     any
	anon   bool
}

// do runs a call and decodes the answer into out. A rate limit with a short
// Retry-After is waited out and the call tried again, unless it is a POST,
// which could then happen twice: the outbox retries those.
func (c *client) do(ctx context.Context, r call, out any) error {
	var body []byte
	if r.in != nil {
		var err error
		if body, err = json.Marshal(r.in); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	u := r.base.JoinPath(r.path)
	u.RawQuery = r.query.Encode()
	for attempt := 1; ; attempt++ {
		wait, err := c.once(ctx, r, u, body, out)
		if wait <= 0 || attempt >= maxAttempts || r.method == http.MethodPost {
			return err
		}
		if wait > maxRetryAfter {
			return fmt.Errorf("%w; Jira asks to wait %s", err, wait)
		}
		if err := c.d.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// once does one attempt; wait is the Retry-After of a rate limit.
func (c *client) once(ctx context.Context, r call, u *url.URL, body []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != nil && !r.anon {
		h, err := c.auth(ctx)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", h)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", r.method, u.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, fmt.Errorf("read the answer of %s: %w", u.Redacted(), err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return retryAfter(resp.Header.Get("Retry-After"), c.d.Now(), resp.StatusCode == http.StatusTooManyRequests), &apiError{Status: resp.StatusCode, Message: "rate limited"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, &apiError{Status: resp.StatusCode, Message: message(raw)}
	}
	if out == nil || len(raw) == 0 {
		return 0, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return 0, fmt.Errorf("decode the answer of %s: %w", u.Path, err)
	}
	return 0, nil
}

func retryAfter(h string, now time.Time, limited bool) time.Duration {
	return trackerhttp.RetryAfter(h, now, limited)
}

// message is Jira's own explanation of an error, clipped.
func message(raw []byte) string {
	var e struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		Message       string            `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	parts := e.ErrorMessages
	for _, k := range slices.Sorted(maps.Keys(e.Errors)) {
		parts = append(parts, k+": "+e.Errors[k])
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	s := strings.Join(parts, "; ")
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "")
	}
	return s
}
