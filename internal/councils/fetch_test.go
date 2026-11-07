package councils

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/netguard"
)

// fakeWeb answers from a map of URL to response, with no network.
type fakeWeb map[string]*http.Response

func (w fakeWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, ok := w[r.URL.String()]
	if !ok {
		return nil, errors.New("no route")
	}
	resp.Request = r
	return resp, nil
}

func page(status int, body string, header ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func lookupFake(_ context.Context, host string) ([]netip.Addr, error) {
	switch host {
	case "public.example":
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	case "mixed.example":
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")}, nil
	case "intranet.example":
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}
	return nil, errors.New("no such host")
}

func TestFetch(t *testing.T) {
	web := func() fakeWeb {
		return fakeWeb{
			"https://public.example/doc":      page(200, "hello", "Content-Type", "text/html; charset=utf-8"),
			"https://public.example/plain":    page(200, "x"),
			"https://public.example/gone":     page(404, "no"),
			"https://public.example/big":      page(200, strings.Repeat("x", 11)),
			"https://public.example/moved":    page(302, "", "Location", "https://public.example/doc"),
			"https://public.example/to-meta":  page(302, "", "Location", "http://169.254.169.254/latest/meta-data"),
			"https://public.example/to-inner": page(302, "", "Location", "https://intranet.example/"),
			"https://public.example/loop":     page(302, "", "Location", "https://public.example/loop2"),
			"https://public.example/loop2":    page(302, "", "Location", "https://public.example/loop"),
		}
	}
	tests := []struct {
		name, url  string
		body, kind string
		refused    bool
		fails      string
	}{
		{name: "ok", url: "https://public.example/doc", body: "hello", kind: "text/html"},
		{name: "no content type", url: "https://public.example/plain", body: "x", kind: "application/octet-stream"},
		{name: "redirect", url: "https://public.example/moved", body: "hello", kind: "text/html"},
		{name: "error status", url: "https://public.example/gone", fails: "status 404"},
		{name: "too large", url: "https://public.example/big", fails: "larger than 10 bytes"},
		{name: "redirect loop", url: "https://public.example/loop", fails: "redirects"},
		{name: "unknown host", url: "https://nowhere.example/", fails: "no such host"},
		{name: "private literal", url: "http://10.0.0.1/", refused: true},
		{name: "loopback literal", url: "http://[::1]:8080/", refused: true},
		{name: "mapped loopback", url: "http://[::ffff:127.0.0.1]/", refused: true},
		{name: "private name", url: "https://intranet.example/", refused: true},
		{name: "one private address", url: "https://mixed.example/", refused: true},
		{name: "redirect to metadata", url: "https://public.example/to-meta", refused: true},
		{name: "redirect to a private name", url: "https://public.example/to-inner", refused: true},
		{name: "not http", url: "file:///etc/passwd", refused: true},
		{name: "credentials", url: "https://me:secret@public.example/doc", refused: true},
		{name: "malformed", url: "http://[::1", refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &Fetcher{Transport: web(), Lookup: lookupFake, MaxSize: 10}
			p, err := f.Fetch(t.Context(), tt.url)
			switch {
			case tt.refused:
				if !errors.Is(err, netguard.ErrRefused) {
					t.Fatalf("err = %v, want netguard.ErrRefused", err)
				}
			case tt.fails != "":
				if err == nil || errors.Is(err, netguard.ErrRefused) || !strings.Contains(err.Error(), tt.fails) {
					t.Fatalf("err = %v, want %q", err, tt.fails)
				}
			case err != nil || string(p.Body) != tt.body || p.ContentType != tt.kind:
				t.Fatalf("Fetch = %q %q, %v", p.Body, p.ContentType, err)
			}
		})
	}
}

// TestFetchGuardsTheConnection: a name that resolves to a public address at
// first and to a private one when connecting (DNS rebinding) is refused by
// the guarded transport, here for a real server on loopback.
func TestFetchGuardsTheConnection(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(srv.Close)
	f := &Fetcher{Lookup: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}}
	host := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := strings.Cut(host, ":")
	_, err := f.Fetch(t.Context(), "http://localhost:"+port+"/")
	if !errors.Is(err, netguard.ErrRefused) || hit {
		t.Fatalf("err = %v, hit = %v", err, hit)
	}
}

func TestFetchTimeout(t *testing.T) {
	f := &Fetcher{Lookup: lookupFake, Timeout: 10 * time.Millisecond, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	if _, err := f.Fetch(t.Context(), "https://public.example/slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
