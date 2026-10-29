package github

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

const (
	maxRetries = 3
	// maxWait is the longest rate-limit wait honored; longer waits fail with
	// the rate-limit response so the caller can retry later.
	maxWait = 2 * time.Minute
)

// retryTransport retries requests that GitHub rate-limited, waiting as long
// as Retry-After or the rate-limit reset header ask.
type retryTransport struct {
	base  http.RoundTripper
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// RoundTrip implements http.RoundTripper.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil || attempt == maxRetries {
			return resp, err
		}
		wait, ok := t.rateLimitWait(resp)
		if !ok || wait > maxWait {
			return resp, nil
		}
		if req.Body != nil && req.GetBody == nil {
			return resp, nil // cannot replay the body
		}
		_ = resp.Body.Close()
		if err := t.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
}

// rateLimitWait reports whether resp is a rate-limit answer and how long to
// wait before retrying.
func (t *retryTransport) rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Unix(reset, 0).Sub(t.now()), 0) + time.Second, true
		}
	}
	return 0, false
}
