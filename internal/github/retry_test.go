package github_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

func TestRetryAfter(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.handle("GET /repos/acme/slow/installation", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"id": 1}`)
	})
	ok, err := e.client.IsInstalled(t.Context(), "acme/slow")
	noErr(t, err)
	if !ok || calls.Load() != 3 {
		t.Fatalf("ok=%v calls=%d", ok, calls.Load())
	}
	if len(e.sleeps) != 2 || e.sleeps[0] != 7*time.Second || e.sleeps[1] != 7*time.Second {
		t.Fatalf("sleeps = %v", e.sleeps)
	}
}

func TestRetryPrimaryRateLimit(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	reset := t0.Add(30 * time.Second).Unix()
	e.handle("GET /repos/acme/limited/installation", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"id": 1}`)
	})
	ok, err := e.client.IsInstalled(t.Context(), "acme/limited")
	noErr(t, err)
	if !ok || len(e.sleeps) != 1 || e.sleeps[0] != 31*time.Second {
		t.Fatalf("ok=%v sleeps=%v", ok, e.sleeps)
	}
}

func TestRetryGivesUp(t *testing.T) {
	tests := []struct {
		name   string
		header func(http.Header)
		status int
		calls  int32
		sleeps int
	}{
		{"retries exhausted", func(h http.Header) { h.Set("Retry-After", "1") }, http.StatusTooManyRequests, 4, 3},
		{"wait too long", func(h http.Header) { h.Set("Retry-After", "3600") }, http.StatusTooManyRequests, 1, 0},
		{"reset in the past", func(h http.Header) {
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset", "1")
		}, http.StatusForbidden, 4, 3},
		{"plain forbidden", func(http.Header) {}, http.StatusForbidden, 1, 0},
		{"unparsable retry after", func(h http.Header) { h.Set("Retry-After", "soon") }, http.StatusForbidden, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			var calls atomic.Int32
			e.handle("GET /repos/acme/limited/installation", func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				tt.header(w.Header())
				w.WriteHeader(tt.status)
				_, _ = w.Write(fixture(t, "api", "error_rate_limit.json"))
			})
			_, err := e.client.IsInstalled(t.Context(), "acme/limited")
			if err == nil {
				t.Fatal("want error")
			}
			if tt.status == http.StatusTooManyRequests {
				wantErr(t, err, github.ErrRateLimited)
			}
			if calls.Load() != tt.calls || len(e.sleeps) != tt.sleeps {
				t.Fatalf("calls=%d sleeps=%v", calls.Load(), e.sleeps)
			}
		})
	}
}

func TestRetryReplaysBody(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.handle("GET /repos/acme/replay/installation", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id": 5}`) })
	e.handle("POST /app/installations/5/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		if body["repositories"] == nil {
			t.Errorf("retry lost the body: %v", body)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token": "ghs_x", "expires_at": %q}`, t0.Add(time.Hour).Format(time.RFC3339))
	})
	tok, err := e.client.InstallationToken(t.Context(), "acme/replay", github.AccessRead)
	noErr(t, err)
	if tok.Value != "ghs_x" || calls.Load() != 2 {
		t.Fatalf("tok=%+v calls=%d", tok, calls.Load())
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	e.handle("GET /repos/acme/limited/installation", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := e.client.IsInstalled(ctx, "acme/limited")
	if err == nil {
		t.Fatal("want error")
	}
}
