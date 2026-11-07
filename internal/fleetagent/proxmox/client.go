package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/fleet"
)

// maxResponse bounds what the API may send back.
const maxResponse = 8 << 20

// tokenRE is a Proxmox API token: user@realm!name=secret.
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+![A-Za-z0-9._-]+=[0-9A-Fa-f-]{8,}$`)

// APIError is an answer of the API that is not a success. Its message is text
// from the hypervisor: it is cleaned and shortened, never trusted.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("proxmox: %d", e.Status)
	}
	return fmt.Sprintf("proxmox: %d: %s", e.Status, e.Message)
}

// Client calls the Proxmox API as one API token.
type Client struct {
	base   *url.URL
	secret string // the whole PVEAPIToken value
	http   *http.Client
}

// NewClient returns a client of the API at apiURL. The connection is verified
// against caFile when given (the cluster's own CA, for a self-signed
// certificate) and the system's roots otherwise; there is no way to turn the
// verification off. serverName, when set, is the name the certificate must
// have, for reaching the API by an address.
func NewClient(apiURL, token, caFile, serverName string) (*Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("api_url %q is not an address", apiURL)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("api_url must be https: the API token would travel in the clear")
	}
	if !tokenRE.MatchString(token) {
		return nil, errors.New("the API token must look like user@realm!name=secret")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile) // #nosec G304 -- the operator's file
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ca_file %s holds no certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &Client{
		base: u, secret: "PVEAPIToken=" + token,
		http: &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// do calls the API. GET and DELETE send params in the query, POST and PUT as a
// form. out, when not nil, gets the answer's data.
func (c *Client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	u := *c.base
	u.Path = "/api2/json" + path
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		u.RawQuery = params.Encode()
	} else {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.scrub(fmt.Errorf("proxmox %s %s: %w", method, path, err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return c.scrub(fmt.Errorf("proxmox %s %s: %w", method, path, err))
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("proxmox %s %s: the answer is too large", method, path)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if msg == "" {
			msg = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Message: c.clean(msg)}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("proxmox %s %s: not an answer of the API", method, path)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("proxmox %s %s: unexpected answer: %w", method, path, err)
	}
	return nil
}

// clean makes text of the hypervisor safe to show: no secret, one short line.
func (c *Client) clean(s string) string {
	token := strings.TrimPrefix(c.secret, "PVEAPIToken=")
	s = strings.ReplaceAll(s, token, "[redacted]")
	if _, secret, ok := strings.Cut(token, "="); ok {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	s = strings.Join(strings.Fields(fleet.CleanError(s)), " ")
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "")
	}
	return s
}

func (c *Client) scrub(err error) error { return errors.New(c.clean(err.Error())) }

// notFound says that err is the API telling that a VM is not there.
func notFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusInternalServerError && strings.Contains(e.Message, "does not exist")
}

// task waits for the task upid to end, and fails if it ended in error.
func (c *Client) task(ctx context.Context, node, upid string, every time.Duration) error {
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/tasks/"+upid+"/status", nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox task failed: %s", c.clean(st.ExitStatus))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// start runs an API call that returns a task, and waits for it.
func (c *Client) start(ctx context.Context, method, path, node string, params url.Values, every time.Duration) error {
	var upid string
	if err := c.do(ctx, method, path, params, &upid); err != nil {
		return err
	}
	return c.task(ctx, node, upid, every)
}
