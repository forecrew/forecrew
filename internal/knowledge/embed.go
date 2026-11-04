package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// EmbeddingDimensions is the size of every embedding in the index
// (migration 00006): OpenAI's text-embedding-3-small. It was picked over
// Voyage because its 1536 numbers fit pgvector's index limit of 2000, it is
// cheap, and teams running Codex already hold an OpenAI key.
const EmbeddingDimensions = 1536

// Embedder turns texts into vectors for the semantic part of the ranking.
// Search works without one.
type Embedder interface {
	// Embed returns one vector of EmbeddingDimensions numbers per text.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Limits of the OpenAI embeddings API are in tokens (8192 per input, 300000
// per request, 2048 inputs). Tokens are estimated by bytes at one token per
// byte, the worst case of a byte-level tokenizer, so no text can go over:
// an input is cut to embedMaxBytes (under 8192 tokens) and a request holds at
// most embedRequestBytes (a third of the request limit) in at most
// embedBatch inputs.
const (
	embedBatch        = 64
	embedMaxBytes     = 8000
	embedRequestBytes = 100_000
)

// cutText returns s cut to max bytes at a character boundary.
func cutText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// embedRequests cuts the texts to the input limit and splits them into
// requests that respect the count and byte limits. Each request is a run of
// consecutive texts: ranges[i] says which.
func embedRequests(texts []string) (inputs []string, ranges [][2]int) {
	inputs = make([]string, len(texts))
	start, size := 0, 0
	for i, t := range texts {
		t = cutText(t, embedMaxBytes)
		if strings.TrimSpace(t) == "" {
			t = " " // the API refuses an empty input
		}
		inputs[i] = t
		if i > start && (i-start == embedBatch || size+len(t) > embedRequestBytes) {
			ranges = append(ranges, [2]int{start, i})
			start, size = i, 0
		}
		size += len(t)
	}
	if start < len(inputs) {
		ranges = append(ranges, [2]int{start, len(inputs)})
	}
	return inputs, ranges
}

// OpenAI is an Embedder over the OpenAI embeddings API.
type OpenAI struct {
	// Key is the API key; it is only sent to BaseURL, never logged.
	Key string
	// BaseURL defaults to https://api.openai.com/v1.
	BaseURL string
	// Model defaults to text-embedding-3-small.
	Model string
	// HTTP defaults to a client with a 30 s timeout.
	HTTP *http.Client
}

var _ Embedder = (*OpenAI)(nil)

// Embed implements Embedder.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	inputs, ranges := embedRequests(texts)
	var out [][]float32
	for _, r := range ranges {
		part, err := o.embed(ctx, inputs[r[0]:r[1]])
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (o *OpenAI) embed(ctx context.Context, texts []string) ([][]float32, error) {
	raw, status, err := o.post(ctx, texts)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	decodeErr := json.Unmarshal(raw, &answer)
	switch {
	case status != http.StatusOK:
		// Only the status: the answer may quote the text that was sent.
		return nil, fmt.Errorf("embed: the API answered status %d", status)
	case decodeErr != nil:
		return nil, fmt.Errorf("embed: decode answer: %w", decodeErr)
	}
	out := make([][]float32, len(texts))
	for _, d := range answer.Data {
		if d.Index < 0 || d.Index >= len(out) || len(d.Embedding) != EmbeddingDimensions {
			return nil, fmt.Errorf("embed: unexpected vector (index %d, %d numbers)", d.Index, len(d.Embedding))
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("embed: no vector for text %d", i)
		}
	}
	return out, nil
}

// post sends one embeddings request and returns the answer body and status.
func (o *OpenAI) post(ctx context.Context, texts []string) (body []byte, status int, err error) {
	in := texts // already cut and split by embedRequests
	model, base, client := o.Model, o.BaseURL, o.HTTP
	if model == "" {
		model = "text-embedding-3-small"
	}
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	payload, err := json.Marshal(map[string]any{"model": model, "input": in, "dimensions": EmbeddingDimensions, "encoding_format": "float"})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("embed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("embed: read answer: %w", err)
	}
	return body, resp.StatusCode, nil
}
