package councils

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/forecrew/forecrew/internal/netguard"
)

// Limits of a fetch of a cited URL.
const (
	DefaultFetchSize    = 5 << 20 // bytes
	DefaultFetchTimeout = 15 * time.Second
	maxRedirects        = 3
)

// ErrRefused means forecrewd will not fetch a URL: it is or redirects to a
// private address, or it is not a plain http(s) URL.
var ErrRefused = netguard.ErrRefused

// Public reports whether a is an address on the public internet.
func Public(a netip.Addr) bool { return netguard.Public(a) }

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
func GuardDial(network, address string, c syscall.RawConn) error {
	return netguard.GuardDial(network, address, c)
}

func (f *Fetcher) transport() http.RoundTripper {
	if f.Transport != nil {
		return f.Transport
	}
	return PublicTransport()
}

// PublicTransport returns a transport that only connects to public
// addresses.
func PublicTransport() http.RoundTripper { return netguard.PublicTransport() }

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
	return netguard.AllowHost(ctx, lookup, host)
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
