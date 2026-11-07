package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/retry"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/testreport"
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
	// BootstrapWait is the wait between Bootstrap calls while forecrewd says
	// to wait; zero means bootstrapRetry.
	BootstrapWait time.Duration
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
	lease, _, err := c.PollFor(ctx, PollRequest{Labels: labels, FreeSlots: freeSlots, Version: version})
	return lease, err
}

// PollRequest is what a poll says of the worker.
type PollRequest struct {
	Labels    []string
	FreeSlots int
	Version   string
	// Agents are the agent CLIs the worker has, and why an install failed.
	Agents []domain.WorkerAgent
	// Install says the worker installs a missing agent when asked.
	Install bool
}

// PollFor asks for a job like Poll, and also answers the agent forecrewd
// asks the worker to install ("" for none) when no job came.
func (c *Client) PollFor(ctx context.Context, p PollRequest) (*Lease, string, error) {
	capabilities := []string{domain.OrganizationModelCapability, domain.ModelCheckCapability}
	if p.Install {
		capabilities = append(capabilities, domain.AgentInstallCapability)
	}
	req := struct {
		Labels       []string             `json:"labels"`
		FreeSlots    int                  `json:"free_slots"`
		Version      string               `json:"version"`
		Capabilities []string             `json:"capabilities"`
		Agents       []domain.WorkerAgent `json:"agents,omitempty"`
	}{p.Labels, p.FreeSlots, p.Version, capabilities, p.Agents}
	var lease Lease
	sent := time.Now()
	status, header, err := c.send(ctx, "/worker/v1/poll", req, &lease)
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusNoContent {
		return nil, header.Get(domain.AgentInstallHeader), nil
	}
	lease.SentAt = sent
	return &lease, "", nil
}

// bootstrapRetry is how long Bootstrap waits when told to wait, the
// retry_after_seconds forecrewd sends.
const bootstrapRetry = 5 * time.Second

// Grant is what the worker on a machine gets for its bootstrap token.
// WorkerToken is a secret.
type Grant struct {
	WorkerToken string   `json:"worker_token"`
	Name        string   `json:"name"`
	Labels      []string `json:"labels"`
	MaxJobs     int      `json:"max_jobs"`
}

// Bootstrap trades the client's bootstrap token for a worker token, waiting
// as long as forecrewd says to: the machine may have no organization yet. It
// returns ErrUnauthorized when the token is refused, and retries anything
// else: a machine has nothing better to do.
func (c *Client) Bootstrap(ctx context.Context, version string) (Grant, error) {
	req := struct {
		Version string `json:"version"`
	}{version}
	for failures := 1; ; {
		var g Grant
		status, err := c.once(ctx, "/worker/v1/bootstrap", req, &g)
		if err == nil && status != http.StatusAccepted {
			return g, nil
		}
		if err != nil && !retryable(err) {
			return Grant{}, err
		}
		wait := bootstrapRetry
		switch {
		case err != nil:
			wait = c.backoff(failures)
			failures++
		case c.BootstrapWait > 0:
			wait = c.BootstrapWait
		}
		if err := sleep(ctx, wait); err != nil {
			return Grant{}, err
		}
	}
}

// Heartbeat actions.
const (
	ActionContinue = "continue"
	ActionCancel   = "cancel"
)

// Beat is the answer to a heartbeat.
type Beat struct {
	Action string `json:"action"`
	// LeaseExpiresAt is the new end of the lease (zero when the server did
	// not say).
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// Attach are the files the agent marked as evidence (job MCP
	// evidence_attach) that the worker has not acknowledged yet.
	Attach []Attachment `json:"attach"`
}

// Attachment is a file of the work tree or of the job's scratch directory to
// upload as an artifact.
type Attachment struct {
	Path string `json:"path"` // relative to the repo root, or "evidence:<rel>" under the scratch directory
	Name string `json:"name"` // artifact name
}

// Heartbeat extends the lease of a job and returns the server's answer. A
// canceled job answers ActionCancel with a nil error; ErrLeaseLost means
// the same. received acknowledges the attachments the worker holds.
func (c *Client) Heartbeat(ctx context.Context, jobID uuid.UUID, received []Attachment) (Beat, error) {
	body := struct {
		AttachReceived []Attachment `json:"attach_received,omitempty"`
	}{received}
	var b Beat
	if err := c.retry(ctx, 3, jobPath(jobID, "heartbeat"), body, &b); err != nil {
		return Beat{}, err
	}
	return b, nil
}

// JobMCPURL is the job MCP gateway of the forecrewd this client talks to.
// Agents use it rather than the public URL in the job spec: a worker next
// to forecrewd may only reach it at its own server URL.
func (c *Client) JobMCPURL() string { return c.base + runtime.JobMCPPath }

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
	// TestRuns are reported by command jobs.
	TestRuns []TestRun `json:"test_runs,omitempty"`
}

// TestRun is the result of the suite a command job ran (worker-api.md,
// complete). Results holds one entry per test case; Counts always counts
// every case, also the ones Results had to leave out.
type TestRun struct {
	Suite           string               `json:"suite"`
	Status          domain.TestRunStatus `json:"status"`
	Counts          domain.TestCounts    `json:"counts"`
	DurationMS      int64                `json:"duration_ms"`
	Results         []testreport.Case    `json:"results"`
	ReportArtifacts []string             `json:"report_artifacts"`
	Versions        map[string]string    `json:"versions"`
	ExitCode        int                  `json:"exit_code"`
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
	return retry.Backoff(n, lo, hi)
}

func sleep(ctx context.Context, d time.Duration) error { return retry.Sleep(ctx, d) }

// once posts in as JSON to path and decodes a 200 answer into out (when
// non-nil). It returns the status code of a successful call.
func (c *Client) once(ctx context.Context, path string, in, out any) (int, error) {
	status, _, err := c.send(ctx, path, in, out)
	return status, err
}

// send is once with the headers of the answer.
func (c *Client) send(ctx context.Context, path string, in, out any) (int, http.Header, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post %s: %w", path, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("read %s answer: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return 0, nil, answerError(resp.StatusCode, data)
	}
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return 0, nil, fmt.Errorf("decode %s answer: %w", path, err)
		}
	}
	return resp.StatusCode, resp.Header, nil
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
