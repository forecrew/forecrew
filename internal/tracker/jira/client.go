package jira

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Limits of a call to Jira.
const (
	defaultTimeout = 30 * time.Second
	maxResponse    = 16 << 20
	maxRedirects   = 3
	// maxRetryAfter is the longest wait for a rate limit within one call;
	// a longer one fails the call, and the outbox or the next poll tries
	// again.
	maxRetryAfter = time.Minute
	maxAttempts   = 3
)

// Errors of the client.
var (
	// ErrRefused means the address is private or loopback on a server that
	// does not allow those, or redirects to one.
	ErrRefused = errors.New("jira: address refused")
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

// guardDial refuses a connection to an address that is not public.
func guardDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: address %s: %w", ErrRefused, address, err)
	}
	if !councils.Public(ap.Addr()) {
		return fmt.Errorf("%w: %s is not a public address", ErrRefused, ap.Addr())
	}
	return nil
}

// guarded checks the host of every request, redirects included, before it
// goes out. A request through a proxy goes to the proxied transport: the
// proxy makes the connection, so the dial guard would see the proxy.
type guarded struct {
	d       Deps
	direct  http.RoundTripper
	proxied http.RoundTripper
}

func (g guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	if !g.d.PrivateAddresses {
		if err := g.d.allow(req.Context(), req.URL.Hostname()); err != nil {
			return nil, err
		}
	}
	if p, err := g.d.Proxy(req); err == nil && p != nil {
		return g.proxied.RoundTrip(req)
	}
	return g.direct.RoundTrip(req)
}

// allow fails with ErrRefused unless every address of host is public.
func (d Deps) allow(ctx context.Context, host string) error {
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else if addrs, err = d.Lookup(ctx, host); err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no address", host)
	}
	for _, a := range addrs {
		if !councils.Public(a) {
			return fmt.Errorf("%w: %s is %s, a private address, and this server does not reach private addresses (trackers.private_addresses)", ErrRefused, host, a.Unmap())
		}
	}
	return nil
}

// httpClient returns a client that trusts the system's CAs and caBundle,
// never skips TLS verification, honors the server's proxy and, unless the
// server allows private addresses, reaches only public ones.
func (d Deps) httpClient(caBundle string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caBundle != "" && !pool.AppendCertsFromPEM([]byte(caBundle)) {
		return nil, fmt.Errorf("ca_bundle holds no PEM certificate: %w", tracker.ErrInvalid)
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !d.PrivateAddresses {
		dialer.Control = guardDial
	}
	dial := dialer.DialContext
	if d.Dial != nil {
		dial = d.Dial
	}
	direct := &http.Transport{DialContext: dial, TLSClientConfig: tlsConf, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true, MaxIdleConns: 4}
	proxied := direct.Clone()
	proxied.Proxy = d.Proxy
	proxied.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	if d.Dial != nil {
		proxied.DialContext = d.Dial
	}
	return &http.Client{
		Transport: guarded{d: d, direct: direct, proxied: proxied},
		Timeout:   d.Timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			return nil
		},
	}, nil
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
// Retry-After is waited out and tried again.
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
		if wait <= 0 || attempt >= maxAttempts {
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

// retryAfter reads Retry-After, in seconds or as a date. A 429 without one
// waits a little; a 503 without one is an outage, not a limit.
func retryAfter(h string, now time.Time, limited bool) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s >= 0 {
		return max(time.Second, time.Duration(s)*time.Second)
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(time.Second, t.Sub(now))
	}
	if limited {
		return 5 * time.Second
	}
	return 0
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
