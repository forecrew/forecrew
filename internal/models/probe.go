package models

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/netguard"
)

// Limits of a key check.
const (
	ProbeTimeout   = 20 * time.Second
	maxProbeBody   = 2 << 20 // bytes
	maxProbeModels = 1000
	maxModelName   = 200
)

// anthropicVersion is the API version the Anthropic list is asked with.
const anthropicVersion = "2023-06-01"

// modelName is the form of a model's name the key check keeps: what the
// provider says is untrusted text, so anything else is dropped.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

// ProbeRequest is one key check.
type ProbeRequest struct {
	URL    string
	Auth   string
	Format string
	Key    string
	// Local lets the check reach localhost and private addresses: the
	// worker's operator allows it (local_model_endpoints).
	Local bool
}

// Probe lists the models at r.URL with the key and says what it found. It
// never follows a redirect, so the key goes to r.URL only, and it never
// connects to a link-local or metadata address, nor to a private one unless
// r.Local. client is nil in production.
func Probe(ctx context.Context, r ProbeRequest, client *http.Client) domain.ModelCheckResult {
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return domain.ModelCheckResult{Status: domain.ModelCheckWrongAddress, Detail: "the address is not a plain http(s) URL"}
	}
	if client == nil {
		client = &http.Client{Transport: netguard.Transport(r.Local), Timeout: ProbeTimeout}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return domain.ModelCheckResult{Status: domain.ModelCheckWrongAddress, Detail: "the address is not a plain http(s) URL"}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "forecrew-worker")
	switch r.Auth {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+r.Key)
	case AuthXAPIKey:
		req.Header.Set("x-api-key", r.Key)
		req.Header.Set("anthropic-version", anthropicVersion)
	case AuthAPIKey:
		req.Header.Set("api-key", r.Key)
	}
	resp, err := c.Do(req)
	switch {
	case errors.Is(err, netguard.ErrRefused):
		return domain.ModelCheckResult{Status: domain.ModelCheckUnreachable,
			Detail: "the address is not public, and this worker reaches a model on a private address only with local_model_endpoints in worker.yaml"}
	case err != nil:
		return domain.ModelCheckResult{Status: domain.ModelCheckUnreachable, Detail: "no answer from the address: check it, and that this worker can reach it"}
	}
	defer func() { _ = resp.Body.Close() }()
	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return domain.ModelCheckResult{Status: domain.ModelCheckRefused, Detail: fmt.Sprintf("the provider refused the key (HTTP %d)", code)}
	case code == http.StatusTooManyRequests || code >= 500:
		return domain.ModelCheckResult{Status: domain.ModelCheckUnreachable, Detail: fmt.Sprintf("the provider is not answering now (HTTP %d): check again later", code)}
	case code < 200 || code > 299:
		return domain.ModelCheckResult{Status: domain.ModelCheckWrongAddress, Detail: fmt.Sprintf("the address answered HTTP %d, not a list of models", code)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
	if err != nil || len(body) > maxProbeBody {
		return domain.ModelCheckResult{Status: domain.ModelCheckWrongAddress, Detail: "the answer was not a list of models"}
	}
	names, ok := modelList(body)
	if !ok {
		return domain.ModelCheckResult{Status: domain.ModelCheckWrongAddress, Detail: "the address answered, but not with a list of models"}
	}
	return domain.ModelCheckResult{Status: domain.ModelCheckReachable, Models: names}
}

// modelList reads {data: [{id}]} (OpenAI, Anthropic and most others) or
// {models: [{name}]} (Ollama's own list), keeping only plain names.
func modelList(body []byte) ([]string, bool) {
	var list struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
		Models *[]struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false
	}
	var raw []string
	switch {
	case list.Data != nil:
		for _, m := range *list.Data {
			raw = append(raw, m.ID)
		}
	case list.Models != nil:
		for _, m := range *list.Models {
			raw = append(raw, cmp.Or(m.Name, m.Model))
		}
	default:
		return nil, false
	}
	out := []string{}
	seen := map[string]bool{}
	for _, name := range raw {
		if len(name) > maxModelName || !modelName.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) == maxProbeModels {
			break
		}
	}
	return out, true
}
