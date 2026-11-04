package testkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/evidence"
)

var _ evidence.Store = (*Evidence)(nil)

// StoredObject is an object of the Evidence fake.
type StoredObject struct {
	Data        []byte
	ContentType string
	// Modified is when the object was stored (the real time unless a test
	// sets it with SetModified).
	Modified time.Time
}

// Evidence is an in-memory evidence.Store. Like S3, a Put whose reader
// yields other than size bytes, or fails, stores nothing.
type Evidence struct {
	mu      sync.Mutex
	objects map[string]StoredObject
	// PutErr, GetErr and DeleteErr, when set, make the call fail.
	PutErr, GetErr, DeleteErr, ListErr error
	// AfterPut, when set, runs after each successful Put, to let a test
	// change the world while an upload is in flight.
	AfterPut func()
}

// NewEvidence returns an empty store.
func NewEvidence() *Evidence { return &Evidence{objects: map[string]StoredObject{}} }

// Put implements evidence.Store.
func (e *Evidence) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	if e.PutErr != nil {
		return e.PutErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return fmt.Errorf("body is %d bytes, declared %d", len(data), size)
	}
	e.mu.Lock()
	e.objects[key] = StoredObject{Data: data, ContentType: contentType, Modified: time.Now().UTC()}
	e.mu.Unlock()
	if e.AfterPut != nil {
		e.AfterPut()
	}
	return nil
}

// Get implements evidence.Store.
func (e *Evidence) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if e.GetErr != nil {
		return nil, e.GetErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.objects[key]
	if !ok {
		return nil, evidence.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.Data)), nil
}

// Delete implements evidence.Store.
func (e *Evidence) Delete(_ context.Context, key string) error {
	if e.DeleteErr != nil {
		return e.DeleteErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.objects, key)
	return nil
}

// Object returns the object at key.
func (e *Evidence) Object(key string) (StoredObject, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.objects[key]
	return o, ok
}

// Keys returns the stored keys, sorted.
func (e *Evidence) Keys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Sorted(maps.Keys(e.objects))
}

// SetModified sets the last-modified time of the object at key.
func (e *Evidence) SetModified(key string, t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.objects[key]
	o.Modified = t
	e.objects[key] = o
}

// List implements evidence.Store.
func (e *Evidence) List(_ context.Context, prefix, after string, limit int) ([]evidence.Object, error) {
	if e.ListErr != nil {
		return nil, e.ListErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []evidence.Object{}
	for _, k := range slices.Sorted(maps.Keys(e.objects)) {
		if len(out) >= limit {
			break
		}
		if k > after && strings.HasPrefix(k, prefix) {
			o := e.objects[k]
			out = append(out, evidence.Object{Key: k, Size: int64(len(o.Data)), LastModified: o.Modified})
		}
	}
	return out, nil
}
