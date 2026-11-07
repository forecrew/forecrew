package councils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// Limits of a fetch of a cited URL.
const (
	DefaultFetchSize    = 5 << 20 // bytes
	DefaultFetchTimeout = 15 * time.Second
	maxRedirects        = 3
)

// ErrRefused means forecrewd will not fetch a URL: it is or redirects to a
// private address, or it is not a plain http(s) URL.
var ErrRefused = errors.New("refused")

// blocked are the ranges that are not public beyond what netip.Addr's
// predicates cover.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
}

// Public reports whether a is an address on the public internet.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Fetcher fetches the URLs members cite. Every host must resolve to public
// addresses only, at the start and at each redirect, and the connection is
// checked again when it is made, so a name that changes its address in
// between reaches nothing private. The zero Fetcher is ready to use.
type Fetcher struct {
	// Transport replaces the guarded transport in tests.
	Transport http.RoundTripper
	// Lookup resolves a host; it defaults to the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// MaxSize and Timeout default to DefaultFetchSize and DefaultFetchTimeout.
	MaxSize int64
	Timeout time.Duration
}

// Page is a fetched URL.
type Page struct {
	Body        []byte
	ContentType string
}

// GuardDial refuses a connection to an address that is not public: the
// Control of a net.Dialer, so a name that changes its address after it was
// checked reaches nothing private.
func GuardDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: address %s: %w", ErrRefused, address, err)
	}
	if !Public(ap.Addr()) {
		return fmt.Errorf("%w: %s is not a public address", ErrRefused, ap.Addr())
	}
	return nil
}

func (f *Fetcher) transport() http.RoundTripper {
	if f.Transport != nil {
		return f.Transport
	}
	return PublicTransport()
}

// PublicTransport returns a transport that only connects to public
// addresses. No proxy: a proxy would make the connection, and the guard would
// see the proxy's address and not the target's.
func PublicTransport() http.RoundTripper {
	return &http.Transport{
		DialContext:       (&net.Dialer{Control: GuardDial}).DialContext,
		DisableKeepAlives: true,
	}
}

func (f *Fetcher) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if f.Lookup != nil {
		return f.Lookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// allow fails with ErrRefused unless u may be fetched; a host that does not
// resolve is another error.
func (f *Fetcher) allow(ctx context.Context, u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("%w: only plain http(s) URLs are fetched", ErrRefused)
	}
	return AllowHost(ctx, f.lookup, u.Hostname())
}

// AllowHost fails with ErrRefused unless every address of host, an IP or a
// name that lookup resolves, is public. A host that does not resolve is
// another error.
func AllowHost(ctx context.Context, lookup func(ctx context.Context, host string) ([]netip.Addr, error), host string) error {
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else if addrs, err = lookup(ctx, host); err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no address", host)
	}
	for _, a := range addrs {
		if !Public(a) {
			return fmt.Errorf("%w: %s is %s, not a public address", ErrRefused, host, a.Unmap())
		}
	}
	return nil
}

// Fetch gets rawURL within the limits. It fails with ErrRefused without
// connecting when the URL may not be fetched; any other error means it did
// not answer, answered with an error, or was too slow or too large.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Page, error) {
	timeout, size := f.Timeout, f.MaxSize
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	if size <= 0 {
		size = DefaultFetchSize
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := url.Parse(rawURL)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if err := f.allow(ctx, u); err != nil {
		return Page{}, err
	}
	client := &http.Client{Transport: f.transport(), CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("more than %d redirects", maxRedirects)
		}
		return f.allow(req.Context(), req.URL)
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	req.Header.Set("User-Agent", "forecrewd")
	resp, err := client.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return Page{}, err
	}
	if int64(len(body)) > size {
		return Page{}, fmt.Errorf("larger than %d bytes", size)
	}
	contentType := "application/octet-stream"
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
		contentType = mt
	}
	return Page{Body: body, ContentType: contentType}, nil
}
