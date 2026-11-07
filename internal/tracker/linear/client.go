package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/tracker/trackerhttp"
)

// Limits of a call to Linear.
const (
	maxResponse = 16 << 20
	// maxRetryAfter is the longest wait for a rate limit within one call;
	// a longer one fails the call, and the outbox or the next poll tries
	// again.
	maxRetryAfter = time.Minute
	maxAttempts   = 3
)

// Errors of the client.
var (
	// ErrRefused is the address guard's: the endpoint, or a redirect, leaves
	// the server it was given or reaches a private address.
	ErrRefused = trackerhttp.ErrRefused
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

// httpClient is the shared client of the trackers: verified TLS, the server's
// proxy, the address guard, and redirects that stay on their server.
func (d Deps) httpClient() (*http.Client, error) {
	return trackerhttp.NewClient(trackerhttp.Options{PrivateAddresses: d.PrivateAddresses, Proxy: d.Proxy, Lookup: d.Lookup, Dial: d.Dial, Timeout: d.Timeout, RootCAs: d.RootCAs})
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
		return trackerhttp.RetryAfter(resp.Header.Get("Retry-After"), c.d.Now(), true), failure
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

func clip(s string) string {
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "")
	}
	return s
}
