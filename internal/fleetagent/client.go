// Package fleetagent is forecrew-fleet-agent: it runs where machines live,
// dials out to forecrewd and makes the local fleet.Provider hold the machines
// forecrewd wants (docs/contracts/fleet-agent-api.md). Credentials of the place
// stay in the place. Bootstrap tokens pass through memory only, and neither
// they, the provider token nor a join code are ever logged.
package fleetagent

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
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

// requestTimeout must exceed the long poll's 30 s by a margin
// (fleet-agent-api.md says at least 45 s).
const requestTimeout = 60 * time.Second

// maxResponse bounds what the agent reads of an answer.
const maxResponse = 16 << 20

// ErrRejected means forecrewd answered 401: the provider token is revoked or
// wrong, or the join code unknown, used or expired. Retrying cannot help.
var ErrRejected = errors.New("rejected by forecrewd")

// APIError is any other error answer of forecrewd.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("forecrewd answered %d %s: %s", e.Status, e.Code, e.Message)
}

// retryable reports an error worth trying again: a network error, a 5xx or
// being rate limited.
func retryable(err error) bool {
	if errors.Is(err, ErrRejected) {
		return false
	}
	var api *APIError
	return !errors.As(err, &api) || api.Status >= 500 || api.Status == http.StatusTooManyRequests
}

// Client calls the fleet agent API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the forecrewd at serverURL. httpClient may be
// nil.
func NewClient(serverURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{base: strings.TrimRight(serverURL, "/"), token: token, http: httpClient}
}

// post sends one request. It does not retry: the agent's loop does, and what it
// does next depends on what failed.
func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrRejected
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e) // a proxy's page has none of it
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode answer of %s: %w", path, err)
	}
	return nil
}

// JoinAnswer is forecrewd's answer to join. Token is a secret.
type JoinAnswer struct {
	Status     string `json:"status"`
	RetryAfter int    `json:"retry_after_seconds"`
	Token      string `json:"token"`
}

// Join is one call with a join code.
func (c *Client) Join(ctx context.Context, code, claim string, kind domain.ProviderKind, facts domain.ProviderCapabilities) (JoinAnswer, error) {
	in := struct {
		Code  string                      `json:"code"`
		Claim string                      `json:"claim"`
		Kind  domain.ProviderKind         `json:"kind"`
		Facts domain.ProviderCapabilities `json:"facts"`
	}{code, claim, kind, facts}
	var out JoinAnswer
	err := c.post(ctx, "/fleet/v1/join", in, &out)
	return out, err
}

// Registration is what forecrewd says about this provider.
type Registration struct {
	ID     uuid.UUID             `json:"id"`
	Name   string                `json:"name"`
	Kind   domain.ProviderKind   `json:"kind"`
	Status domain.ProviderStatus `json:"status"`
}

// Register records the capabilities and the contact.
func (c *Client) Register(ctx context.Context, capabilities domain.ProviderCapabilities) (Registration, error) {
	var out Registration
	err := c.post(ctx, "/fleet/v1/register", map[string]any{"capabilities": capabilities}, &out)
	return out, err
}

// PollAnswer is the machines that should exist here. Bootstrap tokens in it are
// secrets.
type PollAnswer struct {
	Generation string                `json:"generation"`
	Machines   []fleet.CreateRequest `json:"machines"`
}

// Poll long-polls the desired machines. have must never be nil, so that it
// reads as a list.
func (c *Client) Poll(ctx context.Context, generation string, have []uuid.UUID) (PollAnswer, error) {
	in := struct {
		Generation string      `json:"generation,omitempty"`
		Have       []uuid.UUID `json:"have"`
	}{generation, have}
	var out PollAnswer
	err := c.post(ctx, "/fleet/v1/poll", in, &out)
	return out, err
}

// StateReport is what happened to a machine.
type StateReport struct {
	State      domain.MachineState `json:"state"`
	ExternalID string              `json:"external_id,omitempty"`
	Error      string              `json:"error,omitempty"`
}

// State reports what happened to a machine.
func (c *Client) State(ctx context.Context, id uuid.UUID, r StateReport) error {
	return c.post(ctx, "/fleet/v1/machines/"+id.String()+"/state", r, nil)
}

// Reconcile lists what the provider holds and returns what to destroy.
func (c *Client) Reconcile(ctx context.Context, instances []fleet.Instance) ([]string, error) {
	if instances == nil {
		instances = []fleet.Instance{}
	}
	var out struct {
		Destroy []string `json:"destroy"`
	}
	err := c.post(ctx, "/fleet/v1/reconcile", map[string]any{"instances": instances}, &out)
	return out.Destroy, err
}
