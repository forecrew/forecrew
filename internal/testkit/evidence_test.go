package testkit

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/evidence"
)

func TestEvidence(t *testing.T) {
	e := NewEvidence()
	ctx := t.Context()
	must(t, e.Put(ctx, "a", strings.NewReader("hello"), 5, "text/plain"))
	must(t, e.Put(ctx, "b", strings.NewReader("x"), -1, "text/plain"))
	if err := e.Put(ctx, "c", strings.NewReader("abc"), 5, ""); err == nil {
		t.Fatal("a body shorter than the size must fail")
	}
	if keys := e.Keys(); len(keys) != 2 || keys[0] != "a" {
		t.Fatalf("keys = %v", keys)
	}
	o, ok := e.Object("a")
	if !ok || string(o.Data) != "hello" || o.ContentType != "text/plain" {
		t.Fatalf("object = %+v", o)
	}
	rc, err := e.Get(ctx, "a")
	must(t, err)
	if data, _ := io.ReadAll(rc); string(data) != "hello" {
		t.Fatalf("get = %q", data)
	}
	_, err = e.Get(ctx, "missing")
	wantErr(t, err, evidence.ErrNotFound)
	must(t, e.Delete(ctx, "a"))
	must(t, e.Delete(ctx, "a"))
	if _, ok := e.Object("a"); ok {
		t.Fatal("deleted object is still there")
	}

	boom := errors.New("boom")
	e.PutErr, e.GetErr, e.DeleteErr = boom, boom, boom
	if e.Put(ctx, "d", strings.NewReader(""), 0, "") != boom || e.Delete(ctx, "b") != boom {
		t.Fatal("injected errors not returned")
	}
	if _, err := e.Get(ctx, "b"); err != boom {
		t.Fatal("injected get error not returned")
	}
}

func TestEvidenceAfterPut(t *testing.T) {
	e := NewEvidence()
	var seen []string
	e.AfterPut = func() { seen = e.Keys() } // must not deadlock
	must(t, e.Put(t.Context(), "k", strings.NewReader("x"), 1, ""))
	if len(seen) != 1 || seen[0] != "k" {
		t.Fatalf("hook saw %v", seen)
	}
}

func TestEvidenceList(t *testing.T) {
	e := NewEvidence()
	ctx := t.Context()
	for _, k := range []string{"jobs/b", "jobs/a", "jobs/c", "x/a"} {
		must(t, e.Put(ctx, k, strings.NewReader("12"), 2, ""))
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.SetModified("jobs/a", at)
	got, err := e.List(ctx, "jobs/", "", 2)
	must(t, err)
	if len(got) != 2 || got[0].Key != "jobs/a" || got[0].Size != 2 || !got[0].LastModified.Equal(at) || got[1].Key != "jobs/b" {
		t.Fatalf("first page = %+v", got)
	}
	got, err = e.List(ctx, "jobs/", "jobs/b", 5)
	if err != nil || len(got) != 1 || got[0].Key != "jobs/c" {
		t.Fatalf("second page = %+v, %v", got, err)
	}
	e.ListErr = errors.New("boom")
	if _, err := e.List(ctx, "", "", 1); err == nil {
		t.Fatal("injected list error not returned")
	}
}
