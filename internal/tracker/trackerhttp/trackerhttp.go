// Package trackerhttp is the HTTP client the tracker adapters that call a
// remote API share: TLS that is always verified, the server's proxy, the
// private-address guard, redirects that stay on their server, and the reading
// of Retry-After.
package trackerhttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/netguard"
	"github.com/forecrew/forecrew/internal/tracker"
)

// MaxRedirects is how many redirects a call follows, all on its own server.
const MaxRedirects = 3

// maxWait bounds what a Retry-After can say, so that no value overflows.
const maxWait = 24 * time.Hour

// DefaultTimeout bounds one request unless an adapter says otherwise.
const DefaultTimeout = 30 * time.Second

// ErrRefused means the address is private or loopback on a server that does
// not allow those, link-local or metadata anywhere, or a redirect leaves the
// server; it is netguard's.
var ErrRefused = netguard.ErrRefused

// Options say how a client reaches its server.
type Options struct {
	// PrivateAddresses lets the client reach private and loopback addresses:
	// a self-hosted forecrewd in the same network as its tracker
	// (trackers.private_addresses).
	PrivateAddresses bool
	// Proxy picks the proxy of a request; it defaults to the server's
	// HTTPS_PROXY and NO_PROXY.
	Proxy func(*http.Request) (*url.URL, error)
	// Lookup resolves a host; it defaults to the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial replaces the dialer, in tests.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Timeout bounds one request.
	Timeout time.Duration
	// CABundle is PEM certificates to trust besides the system's.
	CABundle string
	// RootCAs are certificates to trust besides the system's, in tests.
	RootCAs []*x509.Certificate
}

// WithDefaults fills in the proxy, the resolver and the timeout.
func (o Options) WithDefaults() Options {
	if o.Proxy == nil {
		o.Proxy = http.ProxyFromEnvironment
	}
	if o.Lookup == nil {
		o.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	return o
}

// Sleep waits for d, or until ctx is done: the wait of a rate limit.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Guarded checks the host of every request, redirects included, before it
// goes out. A request through a proxy goes to the proxied transport: the
// proxy makes the connection, so the dial guard would see the proxy, and only
// this check applies; the proxy enforces egress.
type Guarded struct {
	Options Options
	Direct  http.RoundTripper
	Proxied http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (g Guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	// With private addresses allowed, the dialer still refuses link-local
	// ones (netguard.Dial).
	if !g.Options.PrivateAddresses {
		if err := netguard.AllowHost(req.Context(), g.Options.Lookup, req.URL.Hostname()); err != nil {
			if errors.Is(err, ErrRefused) {
				return nil, fmt.Errorf("%w; this server does not reach private addresses (trackers.private_addresses)", err)
			}
			return nil, err
		}
	}
	if p, err := g.Options.Proxy(req); err == nil && p != nil {
		return g.Proxied.RoundTrip(req)
	}
	return g.Direct.RoundTrip(req)
}

// NewClient returns a client that trusts the system's CAs and the options',
// never skips TLS verification, honors the proxy and, unless private
// addresses are allowed, reaches only public ones. o must have its defaults.
func NewClient(o Options) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	for _, c := range o.RootCAs {
		pool.AddCert(c)
	}
	if o.CABundle != "" && !pool.AppendCertsFromPEM([]byte(o.CABundle)) {
		return nil, fmt.Errorf("ca_bundle holds no PEM certificate: %w", tracker.ErrInvalid)
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	// Link-local addresses, where cloud metadata lives, stay out of reach
	// even where private ones are allowed.
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: netguard.Dial(o.PrivateAddresses)}
	dial := dialer.DialContext
	if o.Dial != nil {
		dial = o.Dial
	}
	// No idle connections: an adapter is built for each operation, and its
	// transport would keep them open after it is gone.
	direct := &http.Transport{DialContext: dial, TLSClientConfig: tlsConf, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true, DisableKeepAlives: true}
	proxied := direct.Clone()
	proxied.Proxy = o.Proxy
	proxied.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	if o.Dial != nil {
		proxied.DialContext = o.Dial
	}
	return &http.Client{Transport: Guarded{Options: o, Direct: direct, Proxied: proxied}, Timeout: o.Timeout, CheckRedirect: CheckRedirect}, nil
}

// CheckRedirect follows a redirect only on the same scheme, host and port:
// the token goes with it, and must never go in clear or to another server.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	first := via[0].URL
	switch {
	case len(via) >= MaxRedirects:
		return fmt.Errorf("more than %d redirects", MaxRedirects)
	case req.URL.Scheme != first.Scheme || req.URL.Host != first.Host:
		return fmt.Errorf("%w: a redirect from %s://%s to %s://%s leaves the server", ErrRefused, first.Scheme, first.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// RetryAfter reads Retry-After, in seconds or as a date. A 429 without one
// waits a little; a 503 without one is an outage, not a limit.
func RetryAfter(h string, now time.Time, limited bool) time.Duration {
	if s, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64); err == nil && s >= 0 {
		return max(time.Second, time.Duration(min(s, int64(maxWait/time.Second)))*time.Second)
	}
	if t, err := http.ParseTime(h); err == nil {
		return min(maxWait, max(time.Second, t.Sub(now)))
	}
	if limited {
		return 5 * time.Second
	}
	return 0
}
