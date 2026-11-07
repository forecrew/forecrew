package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestEnforcedConnectionsAndTokenCap(t *testing.T) {
	ctx := t.Context()
	st := testkit.NewStore(time.Now)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	acme, other := domain.Account{Name: "Acme"}, domain.Account{Name: "Other"}
	must(st.Accounts().Create(ctx, &acme))
	must(st.Accounts().Create(ctx, &other))
	conn := domain.Connection{AccountID: acme.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://idp.example", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_X", Enabled: true}
	must(st.Connections().Create(ctx, &conn))
	d := domain.VerifiedDomain{AccountID: acme.ID, Domain: "acme.example", VerificationToken: "t"}
	must(st.VerifiedDomains().Create(ctx, &d))
	must(st.VerifiedDomains().SetVerified(ctx, acme.ID, d.ID, true))
	must(st.VerifiedDomains().SetEnforcement(ctx, acme.ID, d.ID, true, conn.ID))
	must(st.Accounts().SetMaxTokenTTL(ctx, acme.ID, time.Hour))

	inDomain := domain.User{Name: "A", Email: "Ann@ACME.example"}
	byIdentity := domain.User{Name: "B", Email: "b@home.example"}
	outside := domain.User{Name: "C", Email: "c@home.example"}
	noEmail := domain.User{Name: "D"}
	for _, u := range []*domain.User{&inDomain, &byIdentity, &outside, &noEmail} {
		must(st.Users().Create(ctx, u))
	}
	must(st.Identities().Create(ctx, &domain.Identity{UserID: byIdentity.ID, ConnectionID: conn.ID, Subject: "s", Email: "b@acme.example"}))

	for _, tt := range []struct {
		name    string
		account uuid.UUID
		user    uuid.UUID
		want    []uuid.UUID
		limit   time.Duration
	}{
		{"primary email", acme.ID, inDomain.ID, []uuid.UUID{conn.ID}, time.Hour},
		{"email of an identity", acme.ID, byIdentity.ID, []uuid.UUID{conn.ID}, time.Hour},
		{"another domain", acme.ID, outside.ID, nil, 0},
		{"no email", acme.ID, noEmail.ID, nil, 0},
		{"another organization", other.ID, inDomain.ID, nil, 0},
	} {
		got, err := store.EnforcedConnections(ctx, st, tt.account, tt.user)
		must(err)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: enforced = %v", tt.name, got)
		}
		limit, err := store.PersonalTokenCap(ctx, st, tt.account, tt.user)
		must(err)
		if limit != tt.limit {
			t.Errorf("%s: cap = %s", tt.name, limit)
		}
	}
	if _, err := store.EnforcedConnections(ctx, st, acme.ID, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}
