package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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

// Limits of one OpenAI embeddings request: texts per call, and the runes
// of a text (the model reads 8191 tokens; longer text is cut).
const (
	embedBatch   = 64
	embedMaxRune = 20000
)

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
	var out [][]float32
	for start := 0; start < len(texts); start += embedBatch {
		part, err := o.embed(ctx, texts[start:min(start+embedBatch, len(texts))])
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
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &answer)
	switch {
	case status != http.StatusOK:
		return nil, fmt.Errorf("embed: status %d: %s", status, answer.Error.Message)
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
	in := make([]string, len(texts))
	for i, t := range texts {
		if r := []rune(t); len(r) > embedMaxRune {
			t = string(r[:embedMaxRune])
		}
		if t == "" {
			t = " " // the API refuses an empty input
		}
		in[i] = t
	}
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
