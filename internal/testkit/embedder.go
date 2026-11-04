package testkit

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"unicode"
)

// Embedder is a knowledge.Embedder that hashes words into a unit vector of
// 1536 numbers, so texts that share words are close. Err, when set, fails
// every call.
type Embedder struct {
	Err error

	mu    sync.Mutex
	calls int
}

// Embed implements knowledge.Embedder.
func (e *Embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.Err != nil {
		return nil, e.Err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = EmbedText(t)
	}
	return out, nil
}

// Calls is how many times Embed was called.
func (e *Embedder) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// EmbedText is the vector the fake gives a text.
func EmbedText(s string) []float32 {
	v := make([]float32, 1536)
	for w := range strings.FieldsFuncSeq(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%1536]++
	}
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}
