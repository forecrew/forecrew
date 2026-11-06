package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/testkit"
)

var key = bytes.Repeat([]byte{7}, secrets.KeySize)

func TestPutGetHasDelete(t *testing.T) {
	ctx := context.Background()
	st := testkit.NewStore(time.Now)
	s, err := secrets.New(st.Secrets(), key)
	if err != nil {
		t.Fatal(err)
	}
	acc, by := domain.NewID(), domain.SystemActor(domain.NewID(), "t")
	if ok, err := s.Has(ctx, acc, "conn-okta"); ok || err != nil {
		t.Fatalf("Has = %v, %v", ok, err)
	}
	if _, err := s.Get(ctx, acc, "conn-okta"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if err := s.Put(ctx, by, acc, "conn-okta", []byte("hunter2")); err != nil {
		t.Fatal(err)
	}
	row, err := st.Secrets().Get(ctx, acc, "conn-okta")
	if err != nil || bytes.Contains(row.Ciphertext, []byte("hunter2")) {
		t.Fatalf("stored in the clear: %v", err)
	}
	got, err := secrets.Resolve(ctx, s, acc, "store:conn-okta")
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	if ok, _ := s.Has(ctx, acc, "conn-okta"); !ok {
		t.Fatal("Has = false")
	}
	if err := s.Put(ctx, by, acc, "conn-okta", []byte("next")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, acc, "conn-okta"); string(got) != "next" {
		t.Fatalf("Get = %q", got)
	}
	if err := s.Delete(ctx, acc, "conn-okta"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, acc, "conn-okta"); err != nil {
		t.Fatalf("a second Delete: %v", err)
	}
}

func TestRowsBoundToScopeAndKey(t *testing.T) {
	ctx := context.Background()
	st := testkit.NewStore(time.Now)
	s, _ := secrets.New(st.Secrets(), key)
	a, b := domain.NewID(), domain.NewID()
	by := domain.SystemActor(a, "t")
	if err := s.Put(ctx, by, a, "x", []byte("v")); err != nil {
		t.Fatal(err)
	}
	row, _ := st.Secrets().Get(ctx, a, "x")
	moved := row
	moved.ID, moved.AccountID = domain.NewID(), b // keeps its key
	if err := st.Secrets().Put(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, b, "x"); !errors.Is(err, secrets.ErrCorrupt) {
		t.Fatalf("a row moved to another organization: %v", err)
	}
	other, _ := secrets.New(st.Secrets(), bytes.Repeat([]byte{8}, secrets.KeySize))
	if _, err := other.Get(ctx, a, "x"); !errors.Is(err, secrets.ErrCorrupt) {
		t.Fatalf("another key: %v", err)
	}
}

func TestRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := secrets.New(testkit.NewStore(time.Now).Secrets(), key)
	acc, by := domain.NewID(), domain.SystemActor(domain.NewID(), "t")
	for name, value := range map[string][]byte{"Bad Name": []byte("v"), "a/../b": []byte("v"), "empty": nil, "big": make([]byte, secrets.MaxValueSize+1)} {
		if err := s.Put(ctx, by, acc, name, value); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("Put(%q) = %v", name, err)
		}
	}
	if _, err := secrets.Resolve(ctx, s, acc, "env:X"); !errors.Is(err, secrets.ErrInvalid) {
		t.Errorf("Resolve = %v", err)
	}
	if _, err := secrets.New(nil, []byte("short")); !errors.Is(err, secrets.ErrInvalid) {
		t.Errorf("New = %v", err)
	}
}

func TestParseKey(t *testing.T) {
	got, err := secrets.ParseKey(base64.StdEncoding.EncodeToString(key))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("ParseKey = %x, %v", got, err)
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		_, err := secrets.ParseKey(bad)
		if !errors.Is(err, secrets.ErrInvalid) || (bad != "" && strings.Contains(err.Error(), bad)) {
			t.Errorf("ParseKey(%q) = %v", bad, err)
		}
	}
}

func TestPromote(t *testing.T) {
	ctx := context.Background()
	st := testkit.NewStore(time.Now)
	s, _ := secrets.New(st.Secrets(), key)
	acc, by := domain.NewID(), domain.SystemActor(domain.NewID(), "t")
	if err := s.Put(ctx, by, acc, "staged/p/x", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, st.Secrets(), by, acc, "staged/p/x", "x"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, acc, "x"); err != nil || string(got) != "v" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if ok, _ := s.Has(ctx, acc, "staged/p/x"); ok {
		t.Error("the staged value is still there")
	}
	// A live value is never replaced.
	if err := s.Put(ctx, by, acc, "staged/q/x", []byte("other")); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, st.Secrets(), by, acc, "staged/q/x", "x"); !errors.Is(err, secrets.ErrExists) {
		t.Fatalf("Promote over a live value: %v", err)
	}
	if got, _ := s.Get(ctx, acc, "x"); string(got) != "v" {
		t.Errorf("Get = %q", got)
	}
	if err := s.Promote(ctx, st.Secrets(), by, acc, "staged/none", "y"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Promote of nothing: %v", err)
	}
}

func TestRotate(t *testing.T) {
	ctx := context.Background()
	st := testkit.NewStore(time.Now)
	next := bytes.Repeat([]byte{9}, secrets.KeySize)
	acc, by := domain.NewID(), domain.SystemActor(domain.NewID(), "t")
	old, _ := secrets.New(st.Secrets(), key)
	for _, name := range []string{"a", "b"} {
		if err := old.Put(ctx, by, acc, name, []byte("v-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	// Without the previous key the values do not read, and Rotate says so.
	alone, _ := secrets.New(st.Secrets(), next)
	if _, err := alone.Get(ctx, acc, "a"); !errors.Is(err, secrets.ErrCorrupt) {
		t.Fatalf("Get under an unknown key: %v", err)
	}
	if _, err := alone.Rotate(ctx); !errors.Is(err, secrets.ErrCorrupt) {
		t.Fatalf("Rotate = %v", err)
	}
	// With it they read at once, and new values take the new key.
	both, err := secrets.New(st.Secrets(), next, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := both.Get(ctx, acc, "a"); err != nil || string(got) != "v-a" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if n, err := both.Rotate(ctx); err != nil || n != 2 {
		t.Fatalf("Rotate = %d, %v", n, err)
	}
	if n, err := both.Rotate(ctx); err != nil || n != 0 {
		t.Fatalf("a second Rotate = %d, %v", n, err)
	}
	// The previous key can go now.
	if got, err := alone.Get(ctx, acc, "b"); err != nil || string(got) != "v-b" {
		t.Fatalf("Get after the rotation = %q, %v", got, err)
	}
	row, _ := st.Secrets().Get(ctx, acc, "a")
	if row.KeyID != secrets.KeyID(next) || strings.Contains(row.KeyID, string(next)) {
		t.Errorf("key id = %q", row.KeyID)
	}
	if _, err := secrets.New(st.Secrets(), next, []byte("short")); !errors.Is(err, secrets.ErrInvalid) {
		t.Errorf("a bad previous key: %v", err)
	}
}
