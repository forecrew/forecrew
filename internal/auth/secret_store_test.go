package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestClientSecretFromTheStore(t *testing.T) {
	ctx := context.Background()
	st := testkit.NewStore(nil)
	vault, err := secrets.New(st.Secrets(), make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	acc, other := domain.NewID(), domain.NewID()
	if err := vault.Put(ctx, domain.SystemActor(acc, "t"), acc, "conn-okta", []byte("value")); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Store: st, Secrets: vault})
	c := domain.Connection{AccountID: acc, Name: "okta", ClientSecretRef: "store:conn-okta"}
	if got, err := s.clientSecret(ctx, c); err != nil || got != "value" {
		t.Fatalf("clientSecret = %q, %v", got, err)
	}
	// A reference reaches its own organization's values only.
	c.AccountID = other
	if _, err := s.clientSecret(ctx, c); err == nil || strings.Contains(err.Error(), "value") {
		t.Fatalf("another organization: %v", err)
	}
	if _, err := New(Config{Store: st}).clientSecret(ctx, c); err == nil {
		t.Fatal("no store: no error")
	}
}
