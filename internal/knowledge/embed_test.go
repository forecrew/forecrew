package knowledge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func vec(first float32) []float32 {
	v := make([]float32, EmbeddingDimensions)
	v[0] = first
	return v
}

// embedServer answers like the OpenAI API and records the request bodies.
func embedServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" || r.URL.Path != "/v1/embeddings" {
			http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*bodies = append(*bodies, body)
		in := body["input"].([]any)
		if len(in) == 1 && in[0] == "boom" {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		var data []map[string]any
		for i := range slices.Backward(in) { // out of order: the index decides
			data = append(data, map[string]any{"index": i, "embedding": vec(float32(i + 1))})
		}
		if len(in) == 1 && in[0] == "short" {
			data = []map[string]any{{"index": 0, "embedding": []float32{1}}}
		}
		if len(in) == 1 && in[0] == "missing" {
			data = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIEmbed(t *testing.T) {
	var bodies []map[string]any
	srv := embedServer(t, &bodies)
	o := &OpenAI{Key: "k", BaseURL: srv.URL + "/v1"}

	texts := make([]string, embedBatch+1)
	for i := range texts {
		texts[i] = "text"
	}
	texts[0] = ""
	texts[1] = strings.Repeat("x", embedMaxBytes+10)
	got, err := o.Embed(t.Context(), texts)
	if err != nil || len(got) != len(texts) || got[0][0] != 1 || got[embedBatch-1][0] != float32(embedBatch) {
		t.Fatalf("Embed = %d vectors, %v", len(got), err)
	}
	if len(bodies) != 2 || bodies[0]["model"] != "text-embedding-3-small" || bodies[0]["dimensions"] != float64(EmbeddingDimensions) {
		t.Errorf("requests = %v", bodies)
	}
	first := bodies[0]["input"].([]any)
	if first[0] != " " || len(first[1].(string)) != embedMaxBytes {
		t.Error("empty text was not replaced or long text not cut")
	}

	checkEmbedFailures(t, srv.URL+"/v1")
}

func checkEmbedFailures(t *testing.T, base string) {
	t.Helper()
	o := &OpenAI{Key: "k", BaseURL: base}
	for in, want := range map[string]string{"boom": "status 503", "short": "unexpected vector", "missing": "no vector"} {
		if _, err := o.Embed(t.Context(), []string{in}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Embed(%q) = %v, want %q", in, err, want)
		}
	}
	if _, err := (&OpenAI{Key: "wrong", BaseURL: base}).Embed(t.Context(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := (&OpenAI{Key: "k", BaseURL: "http://127.0.0.1:1"}).Embed(t.Context(), []string{"a"}); err == nil {
		t.Error("unreachable API accepted")
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer garbage.Close()
	if _, err := (&OpenAI{Key: "k", BaseURL: garbage.URL}).Embed(t.Context(), []string{"a"}); err == nil {
		t.Error("garbage answer accepted")
	}
}

// An error never repeats what the API answered: it may quote the text sent.
func TestOpenAIErrorHasOnlyTheStatus(t *testing.T) {
	var bodies []map[string]any
	srv := embedServer(t, &bodies)
	_, err := (&OpenAI{Key: "k", BaseURL: srv.URL + "/v1"}).Embed(t.Context(), []string{"boom"})
	if err == nil || strings.Contains(err.Error(), "overloaded") || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v", err)
	}
}

// Inputs stay under the token limit whatever they hold, requests under the
// request limit, and a cut never splits a character.
func TestOpenAIRequestLimits(t *testing.T) {
	var bodies []map[string]any
	srv := embedServer(t, &bodies)
	o := &OpenAI{Key: "k", BaseURL: srv.URL + "/v1"}

	texts := make([]string, 30)
	for i := range texts {
		texts[i] = strings.Repeat("é", embedMaxBytes) // two bytes each, so the cut must not split one
	}
	if _, err := o.Embed(t.Context(), texts); err != nil {
		t.Fatal(err)
	}
	total, inputs := 0, 0
	for _, b := range bodies {
		size := 0
		for _, in := range b["input"].([]any) {
			str := in.(string)
			if len(str) > embedMaxBytes || !utf8.ValidString(str) {
				t.Fatalf("input of %d bytes, valid %v", len(str), utf8.ValidString(str))
			}
			size += len(str)
			inputs++
		}
		if size > embedRequestBytes || len(b["input"].([]any)) > embedBatch {
			t.Errorf("request of %d bytes and %d inputs", size, len(b["input"].([]any)))
		}
		total += size
	}
	if inputs != len(texts) || len(bodies) < 2 {
		t.Errorf("%d inputs in %d requests", inputs, len(bodies))
	}
}
