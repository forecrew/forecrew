package linear

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/councils"
)

// Limits of a call to Linear.
const (
	defaultTimeout = 30 * time.Second
	maxResponse    = 16 << 20
	maxRedirects   = 3
	// maxRetryAfter is the longest wait for a rate limit within one call;
	// a longer one fails the call, and the outbox or the next poll tries
	// again.
	maxRetryAfter = time.Minute
	maxAttempts   = 3
	maxWait       = 24 * time.Hour
)

// Errors of the client.
var (
	// ErrRefused is the address guard's: the endpoint, or a redirect, leaves
	// the server it was given or reaches a private address.
	ErrRefused = councils.ErrRefused
	errLogin   = errors.New("linear refused the credentials")
	errDenied  = errors.New("linear denied access")
)

// apiError is an answer of Linear that is not a success. Its text comes from
// Linear and is clipped.
type apiError struct {
	Status  int
	Code    string // extensions.code of the first GraphQL error
	Message string
}

func (e *apiError) Error() string {
	switch {
	case e.Message == "":
		return fmt.Sprintf("linear answered %d", e.Status)
	case e.Status == http.StatusOK:
		return "linear: " + e.Message
	}
	return fmt.Sprintf("linear answered %d: %s", e.Status, e.Message)
}

// Unwrap lets callers test for login and access errors.
func (e *apiError) Unwrap() error {
	switch {
	case e.Status == http.StatusUnauthorized, e.Code == "AUTHENTICATION_ERROR":
		return errLogin
	case e.Status == http.StatusForbidden, e.Code == "FORBIDDEN":
		return errDenied
	}
	return nil
}

func (e *apiError) notFound() bool {
	return strings.Contains(strings.ToLower(e.Message), "not found")
}

// guarded checks the host of every request, redirects included, before it
// goes out, unless the server allows private addresses.
type guarded struct {
	d       Deps
	direct  http.RoundTripper
	proxied http.RoundTripper
}

func (g guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	if !g.d.PrivateAddresses {
		if err := councils.AllowHost(req.Context(), g.d.Lookup, req.URL.Hostname()); err != nil {
			return nil, err
		}
	}
	if p, err := g.d.Proxy(req); err == nil && p != nil {
		return g.proxied.RoundTrip(req)
	}
	return g.direct.RoundTrip(req)
}

// httpClient trusts the system's CAs (and Deps.RootCAs, in tests), never
// skips TLS verification, honors the server's proxy and follows a redirect
// only on the same scheme, host and port.
func (d Deps) httpClient() *http.Client {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	for _, c := range d.RootCAs {
		pool.AddCert(c)
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !d.PrivateAddresses {
		dialer.Control = councils.GuardDial
	}
	dial := dialer.DialContext
	if d.Dial != nil {
		dial = d.Dial
	}
	direct := &http.Transport{DialContext: dial, TLSClientConfig: tlsConf, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true, DisableKeepAlives: true}
	proxied := direct.Clone()
	proxied.Proxy = d.Proxy
	proxied.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	if d.Dial != nil {
		proxied.DialContext = d.Dial
	}
	return &http.Client{Transport: guarded{d: d, direct: direct, proxied: proxied}, Timeout: d.Timeout, CheckRedirect: checkRedirect}
}

// checkRedirect: the token goes with a redirect, and must never go in clear
// or to another server.
func checkRedirect(req *http.Request, via []*http.Request) error {
	first := via[0].URL
	switch {
	case len(via) >= maxRedirects:
		return fmt.Errorf("more than %d redirects", maxRedirects)
	case req.URL.Scheme != first.Scheme || req.URL.Host != first.Host:
		return fmt.Errorf("%w: a redirect from %s://%s to %s://%s leaves the server", ErrRefused, first.Scheme, first.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// client calls the GraphQL API of Linear.
type client struct {
	http     *http.Client
	d        Deps
	endpoint string
	// auth returns the Authorization header.
	auth func(ctx context.Context) (string, error)
}

// gql runs a query or mutation and decodes its data into out. A rate limit
// with a short Retry-After is waited out and the call tried again, but only
// for a read: a mutation is a POST that could then happen twice, so the
// outbox retries it.
func (c *client) gql(ctx context.Context, query string, vars map[string]any, out any, read bool) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	for attempt := 1; ; attempt++ {
		wait, err := c.once(ctx, body, out)
		if wait <= 0 || attempt >= maxAttempts || !read {
			return err
		}
		if wait > maxRetryAfter {
			return fmt.Errorf("%w; Linear asks to wait %s", err, wait)
		}
		if err := c.d.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *client) once(ctx context.Context, body []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	h, err := c.auth(ctx)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", h)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("POST %s: %w", req.URL.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, fmt.Errorf("read the answer of Linear: %w", err)
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &answer) // an answer that is no JSON is told by its status below
	var failure *apiError
	if len(answer.Errors) > 0 {
		failure = &apiError{Status: resp.StatusCode, Code: answer.Errors[0].Extensions.Code, Message: clip(answer.Errors[0].Message)}
	}
	if resp.StatusCode == http.StatusTooManyRequests || (failure != nil && failure.Code == "RATELIMITED") {
		if failure == nil {
			failure = &apiError{Status: resp.StatusCode}
		}
		failure.Message = "rate limited"
		return retryAfter(resp.Header.Get("Retry-After")), failure
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || failure != nil {
		if failure == nil {
			failure = &apiError{Status: resp.StatusCode}
		}
		return 0, failure // a GraphQL error is told even next to partial data
	}
	if out == nil || len(answer.Data) == 0 {
		return 0, nil
	}
	if err := json.Unmarshal(answer.Data, out); err != nil {
		return 0, fmt.Errorf("decode the answer of Linear: %w", err)
	}
	return 0, nil
}

// retryAfter reads Retry-After in seconds; Linear sends it with a 429. Without
// one a rate limit waits a little.
func retryAfter(h string) time.Duration {
	if s, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64); err == nil && s >= 0 {
		return max(time.Second, time.Duration(min(s, int64(maxWait/time.Second)))*time.Second)
	}
	return 5 * time.Second
}

func clip(s string) string {
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "")
	}
	return s
}
