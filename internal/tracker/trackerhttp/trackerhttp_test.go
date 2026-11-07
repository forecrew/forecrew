package trackerhttp

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/tracker"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRedirectsStayOnTheirServer(t *testing.T) {
	first, _ := http.NewRequest(http.MethodGet, "https://a.test/x", nil)
	for name, tt := range map[string]struct {
		to      string
		via     int
		refused bool
		failed  bool
	}{
		"same server": {to: "https://a.test/y", via: 1},
		"other host":  {to: "https://b.test/y", via: 1, refused: true},
		"other port":  {to: "https://a.test:8443/y", via: 1, refused: true},
		"clear":       {to: "http://a.test/y", via: 1, refused: true},
		"too many":    {to: "https://a.test/y", via: MaxRedirects, failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			next, _ := http.NewRequest(http.MethodGet, tt.to, nil)
			via := make([]*http.Request, tt.via)
			for i := range via {
				via[i] = first
			}
			err := CheckRedirect(next, via)
			switch {
			case tt.refused && !errors.Is(err, ErrRefused):
				t.Errorf("err = %v", err)
			case tt.failed && err == nil:
				t.Error("followed")
			case !tt.refused && !tt.failed && err != nil:
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestGuardedSendsProxiedRequestsToTheProxiedTransport(t *testing.T) {
	used := ""
	transport := func(name string) http.RoundTripper {
		return roundTripper(func(*http.Request) (*http.Response, error) { used = name; return nil, errors.New("stop") })
	}
	proxy, _ := url.Parse("http://proxy.internal:3128")
	o := Options{PrivateAddresses: true, Proxy: func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == "t.test" {
			return proxy, nil
		}
		return nil, nil
	}}
	g := Guarded{Options: o, Direct: transport("direct"), Proxied: transport("proxied")}
	for host, want := range map[string]string{"t.test": "proxied", "other.test": "direct"} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		_, _ = g.RoundTrip(req)
		if used != want {
			t.Errorf("%s went %s", host, used)
		}
	}
}

func TestGuardedRefusesPrivateAddressesUnlessAllowed(t *testing.T) {
	sent := false
	direct := roundTripper(func(*http.Request) (*http.Response, error) { sent = true; return nil, errors.New("stop") })
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "private.test" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		return nil, errors.New("no such host")
	}
	o := Options{Lookup: lookup, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	req, _ := http.NewRequest(http.MethodGet, "https://private.test/", nil)
	_, err := Guarded{Options: o, Direct: direct}.RoundTrip(req)
	if !errors.Is(err, ErrRefused) || sent {
		t.Errorf("err = %v, sent = %v", err, sent)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://nowhere.test/", nil)
	if _, err := (Guarded{Options: o, Direct: direct}).RoundTrip(req); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("an unknown host: %v", err)
	}
	o.PrivateAddresses = true
	req, _ = http.NewRequest(http.MethodGet, "https://private.test/", nil)
	_, _ = Guarded{Options: o, Direct: direct}.RoundTrip(req)
	if !sent {
		t.Error("a self-hosted server could not reach it")
	}
}

func TestNewClient(t *testing.T) {
	o := Options{CABundle: "not a certificate"}.WithDefaults()
	if _, err := NewClient(o); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("a bundle with no certificate: %v", err)
	}
	c, err := NewClient(Options{}.WithDefaults())
	if err != nil || c.Timeout != DefaultTimeout || c.CheckRedirect == nil {
		t.Errorf("client = %+v, %v", c, err)
	}
	if o := (Options{}).WithDefaults(); o.Proxy == nil || o.Lookup == nil {
		t.Error("no defaults")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		h       string
		limited bool
		want    time.Duration
	}{
		{"3", true, 3 * time.Second}, {"0", true, time.Second}, {"99999999999", true, 24 * time.Hour},
		{now.Add(time.Minute).Format(http.TimeFormat), true, time.Minute}, {"", true, 5 * time.Second}, {"soon", false, 0},
	} {
		if got := RetryAfter(tt.h, now, tt.limited); got != tt.want {
			t.Errorf("RetryAfter(%q) = %s, want %s", tt.h, got, tt.want)
		}
	}
}

func TestSleep(t *testing.T) {
	if err := Sleep(t.Context(), time.Millisecond); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// TestPrivateAddressesStillRefuseMetadata: allowing private addresses never
// opens link-local ones, where cloud metadata lives.
func TestPrivateAddressesStillRefuseMetadata(t *testing.T) {
	c, err := NewClient(Options{PrivateAddresses: true}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get("http://169.254.169.254/latest/meta-data/") //nolint:noctx // refused before any request
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
}

// TestPrivateAddressesStillRefuseAlibabaMetadata: the metadata address that
// is not link-local is refused too.
func TestPrivateAddressesStillRefuseAlibabaMetadata(t *testing.T) {
	c, err := NewClient(Options{PrivateAddresses: true}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get("http://100.100.100.200/latest/meta-data/") //nolint:noctx // refused before any request
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
}
