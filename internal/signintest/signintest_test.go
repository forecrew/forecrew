package signintest_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/signintest"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestTestVouchesForWhatItWorkedFor(t *testing.T) {
	ctx := t.Context()
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "Acme", Slug: "acme"}
	if err := st.Accounts().Create(ctx, &acc); err != nil {
		t.Fatal(err)
	}
	by := domain.SystemActor(acc.ID, domain.ForecrewdName)
	c := domain.Connection{AccountID: acc.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://a.example.com", ClientID: "c1", ClientSecretRef: "env:FORECREW_CONN_OKTA"}
	if err := st.Connections().Create(ctx, &c); err != nil {
		t.Fatal(err)
	}
	valid := func(c domain.Connection) bool {
		t.Helper()
		ok, err := signintest.Valid(ctx, st, c)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if valid(c) {
		t.Fatal("never tested, valid")
	}
	if err := signintest.Record(ctx, st, by, c); err != nil {
		t.Fatal(err)
	}
	if !valid(c) {
		t.Fatal("tested, not valid")
	}
	for name, change := range map[string]func(*domain.Connection){
		"issuer":     func(c *domain.Connection) { c.Issuer = "https://b.example.com" },
		"client id":  func(c *domain.Connection) { c.ClientID = "c2" },
		"secret ref": func(c *domain.Connection) { c.ClientSecretRef = "env:FORECREW_CONN_OTHER" },
	} {
		other := c
		change(&other)
		if valid(other) {
			t.Errorf("%s: still valid", name)
		}
	}
	// What a test does not vouch for does not clear it.
	other := c
	other.GroupClaim, other.Enabled = "groups", true
	if !valid(other) {
		t.Error("group claim or enabled cleared the test")
	}
	// Cleared, and not back with the old values; another connection is alone.
	if err := signintest.Clear(ctx, st, by, c); err != nil {
		t.Fatal(err)
	}
	if valid(c) {
		t.Fatal("cleared, valid")
	}
	if err := signintest.Clear(ctx, st, by, c); err != nil { // nothing to clear
		t.Fatal(err)
	}
	if err := signintest.Record(ctx, st, by, c); err != nil {
		t.Fatal(err)
	}
	if !valid(c) {
		t.Fatal("tested again, not valid")
	}
	stranger := c
	stranger.ID = uuid.New()
	if valid(stranger) {
		t.Fatal("another connection is tested")
	}
	// A test from before fingerprints vouches for nothing.
	old := domain.Connection{AccountID: acc.ID, Kind: domain.ConnectionOIDC, Name: "old", Issuer: "https://a.example.com", ClientID: "c1", ClientSecretRef: "env:FORECREW_CONN_OLD"}
	if err := st.Connections().Create(ctx, &old); err != nil {
		t.Fatal(err)
	}
	if err := st.Audit().Append(ctx, &domain.AuditEntry{AccountID: acc.ID, Actor: by.String(), Action: signintest.ActionTested, SubjectType: "connection", SubjectID: old.ID}); err != nil {
		t.Fatal(err)
	}
	if valid(old) {
		t.Fatal("a test without a fingerprint is valid")
	}
}

// A test of values that are gone is refused, and does not come back with them.
func TestTestOfGoneValuesIsRefused(t *testing.T) {
	ctx := t.Context()
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "Acme", Slug: "acme"}
	if err := st.Accounts().Create(ctx, &acc); err != nil {
		t.Fatal(err)
	}
	by := domain.SystemActor(acc.ID, domain.ForecrewdName)
	c := domain.Connection{AccountID: acc.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://a.example.com", ClientID: "c1", ClientSecretRef: "env:FORECREW_CONN_OKTA"}
	if err := st.Connections().Create(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if err := signintest.Record(ctx, st, by, c); err != nil {
		t.Fatal(err)
	}
	valid := func() bool {
		ok, err := signintest.Valid(ctx, st, c)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	moved := c
	moved.Issuer = "https://b.example.com"
	if err := st.Connections().Update(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if err := signintest.Record(ctx, st, by, c); !errors.Is(err, store.ErrConflict) || !errors.Is(err, signintest.ErrChanged) {
		t.Fatalf("record of old values: %v", err)
	}
	if err := signintest.Clear(ctx, st, by, c); err != nil {
		t.Fatal(err)
	}
	if err := st.Connections().Update(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if valid() {
		t.Fatal("cleared test is back with the old values")
	}
	if err := signintest.Record(ctx, st, by, c); err != nil {
		t.Fatal(err)
	}
	if !valid() {
		t.Fatal("tested again, not valid")
	}
}
