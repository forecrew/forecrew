package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Errors of the worker API client.
var (
	// ErrUnauthorized means forecrewd rejected the worker token (401); it was
	// revoked or is wrong.
	ErrUnauthorized = errors.New("worker token rejected")
	// ErrLeaseLost means the job does not run under this worker's lease (409).
	ErrLeaseLost = errors.New("lease lost")
	// ErrJobNotFound means the job is not one of the worker's account (404).
	ErrJobNotFound = errors.New("job not found")
	// ErrTooLarge means forecrewd refused a request as too large (413); for
	// an artifact, it is over the evidence.max_artifact_size of the server.
	ErrTooLarge = errors.New("too large")
	// ErrEvidenceUnavailable means forecrewd has no evidence storage (503
	// unavailable): artifacts cannot be uploaded until it is configured.
	ErrEvidenceUnavailable = errors.New("evidence storage not configured")
)

// APIError is an unexpected error answer of forecrewd.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("forecrewd answered %d %s: %s", e.Status, e.Code, e.Message)
}

// Backoff limits for retrying 5xx answers and network errors, and for the
// poll loop while the server is unreachable.
const (
	DefaultBackoffMin = time.Second
	DefaultBackoffMax = 30 * time.Second
)

// requestTimeout is the HTTP client timeout. It must exceed the long-poll
// wait of 30 s.
const requestTimeout = 60 * time.Second

// Client calls the worker API (docs/contracts/worker-api.md).
type Client struct {
	base  string
	token string
	http  *http.Client
	// BackoffMin and BackoffMax bound the retry delays; zero means the
	// Default values.
	BackoffMin, BackoffMax time.Duration
}

// NewClient returns a client for the forecrewd at serverURL. httpClient may
// be nil.
func NewClient(serverURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{base: strings.TrimRight(serverURL, "/"), token: token, http: httpClient}
}

// Lease is a job handed out by Poll.
type Lease struct {
	Job               domain.JobSpec `json:"job"`
	LeaseExpiresAt    time.Time      `json:"lease_expires_at"`
	HeartbeatInterval int            `json:"heartbeat_interval_seconds"`
	// SentAt is when the poll that returned the lease was sent, on the
	// worker's clock.
	SentAt time.Time `json:"-"`
}

// Poll asks for a job. It returns nil, nil when none is available (204). It
// does not retry: the caller's poll loop backs off.
func (c *Client) Poll(ctx context.Context, labels []string, freeSlots int, version string) (*Lease, error) {
	req := struct {
		Labels    []string `json:"labels"`
		FreeSlots int      `json:"free_slots"`
		Version   string   `json:"version"`
	}{labels, freeSlots, version}
	var lease Lease
	sent := time.Now()
	status, err := c.once(ctx, "/worker/v1/poll", req, &lease)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	lease.SentAt = sent
	return &lease, nil
}

// Heartbeat actions.
const (
	ActionContinue = "continue"
	ActionCancel   = "cancel"
)

// Heartbeat extends the lease of a job and returns the server's action. A
// canceled job answers ActionCancel with a nil error; ErrLeaseLost means
// the same. expires is the new end of the lease (zero when the server did not
// say).
func (c *Client) Heartbeat(ctx context.Context, jobID uuid.UUID) (action string, expires time.Time, err error) {
	var resp struct {
		Action         string    `json:"action"`
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := c.retry(ctx, 3, jobPath(jobID, "heartbeat"), struct{}{}, &resp); err != nil {
		return "", time.Time{}, err
	}
	return resp.Action, resp.LeaseExpiresAt, nil
}

// SendEvents appends events to the job log.
func (c *Client) SendEvents(ctx context.Context, jobID uuid.UUID, events []domain.JobEvent) error {
	body := struct {
		Events []domain.JobEvent `json:"events"`
	}{events}
	return c.retry(ctx, 3, jobPath(jobID, "events"), body, nil)
}

// Completion is the result of a job reported by Complete.
type Completion struct {
	Status  string          `json:"status"` // succeeded, failed or retry
	Output  json.RawMessage `json:"output,omitempty"`
	HeadSHA string          `json:"head_sha,omitempty"`
	Error   string          `json:"error,omitempty"`
	Usage   domain.Usage    `json:"usage"`
	CostUSD float64         `json:"cost_usd"`
}

// Completion statuses.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusRetry     = "retry"
)

// Complete reports the end of a job and returns the job's resulting state.
func (c *Client) Complete(ctx context.Context, jobID uuid.UUID, comp Completion) (domain.JobStatus, error) {
	var resp struct {
		Status domain.JobStatus `json:"status"`
	}
	if err := c.retry(ctx, 6, jobPath(jobID, "complete"), comp, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}

func jobPath(id uuid.UUID, action string) string {
	return "/worker/v1/jobs/" + url.PathEscape(id.String()) + "/" + action
}

// retry posts like once, retrying 5xx answers and network errors up to
// attempts times with exponential backoff and jitter.
func (c *Client) retry(ctx context.Context, attempts int, path string, in, out any) error {
	return c.attempt(ctx, attempts, func() error {
		_, err := c.once(ctx, path, in, out)
		return err
	})
}

// attempt calls do up to attempts times, sleeping with backoff between the
// calls, until it succeeds or fails with an error that is not retryable.
func (c *Client) attempt(ctx context.Context, attempts int, do func() error) error {
	var err error
	for i := range attempts {
		if i > 0 {
			if serr := sleep(ctx, c.backoff(i)); serr != nil {
				return errors.Join(err, serr)
			}
		}
		if err = do(); err == nil || !retryable(err) {
			return err
		}
	}
	return err
}

// retryable reports whether err is a network error or a 5xx answer.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Status >= 500
	}
	return !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrLeaseLost) && !errors.Is(err, ErrJobNotFound) &&
		!errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrEvidenceUnavailable)
}

// backoff returns the delay before retry n (1, 2, ...): BackoffMin doubling
// up to BackoffMax, with the upper half jittered.
func (c *Client) backoff(n int) time.Duration {
	lo, hi := c.BackoffMin, c.BackoffMax
	if lo <= 0 {
		lo = DefaultBackoffMin
	}
	if hi <= 0 {
		hi = DefaultBackoffMax
	}
	d := lo
	for range n - 1 {
		if d >= hi {
			break
		}
		d *= 2
	}
	d = min(d, hi)
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not security
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// once posts in as JSON to path and decodes a 200 answer into out (when
// non-nil). It returns the status code of a successful call.
func (c *Client) once(ctx context.Context, path string, in, out any) (int, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post %s: %w", path, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read %s answer: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return 0, answerError(resp.StatusCode, data)
	}
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return 0, fmt.Errorf("decode %s answer: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// redactURLError drops the request from a *url.Error, keeping the cause.
func redactURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// answerError maps a non-2xx answer to a sentinel or an *APIError.
func answerError(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrLeaseLost, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrJobNotFound, msg)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %s", ErrTooLarge, msg)
	case http.StatusServiceUnavailable:
		if e.Error.Code == "unavailable" {
			return fmt.Errorf("%w: %s", ErrEvidenceUnavailable, msg)
		}
	}
	return &APIError{Status: status, Code: e.Error.Code, Message: msg}
}
