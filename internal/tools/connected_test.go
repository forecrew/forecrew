package tools_test

import (
	"crypto/sha256"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// grantFor stores an access token of a new family of user and returns its
// actor.
func grantFor(t *testing.T, f *fixture, user uuid.UUID, name string, scopes ...domain.Scope) (domain.Actor, domain.OAuthGrant) {
	t.Helper()
	ctx := t.Context()
	c := domain.OAuthClient{ClientID: name, Name: name, RedirectURIs: []string{"https://app.example/cb"}}
	must(t, f.st.OAuth().CreateClient(ctx, &c))
	h := sha256.Sum256([]byte(name))
	g := domain.OAuthGrant{Kind: domain.OAuthAccess, TokenHash: h[:], FamilyID: domain.NewID(), ClientID: c.ID, UserID: user, AccountID: f.account.ID,
		Scopes: scopes, Resource: "https://forecrew.example/mcp", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, f.st.OAuth().CreateGrant(ctx, &g))
	return domain.UserActor(f.account.ID, user, domain.Credential{Kind: domain.CredentialOAuthGrant, ID: g.ID}), g
}

func TestConnectedApps(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, ana := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	_, bob := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	reader, readGrant := grantFor(t, f, ana, "reader", domain.ScopeRead)
	writer, writeGrant := grantFor(t, f, ana, "writer", domain.ScopeWrite)
	_, bobGrant := grantFor(t, f, bob, "bobs", domain.ScopeAdmin)

	apps, err := f.svc.ListConnectedApps(ctx, reader)
	must(t, err)
	if len(apps) != 2 || !slices.ContainsFunc(apps, func(a mcp.ConnectedApp) bool {
		return a.ID == readGrant.FamilyID && a.Current && a.Name == "reader" && a.Client == "reader" && a.Scopes[0] == domain.ScopeRead
	}) {
		t.Fatalf("apps = %+v", apps)
	}

	// A read grant ends itself, not its owner's others.
	_, err = f.svc.RevokeConnectedApp(ctx, reader, mcp.ConnectedAppInput{App: writeGrant.FamilyID.String()})
	wantErr(t, err, authz.ErrDenied)
	// Nobody reaches another person's app.
	_, err = f.svc.RevokeConnectedApp(ctx, writer, mcp.ConnectedAppInput{App: bobGrant.FamilyID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.RevokeConnectedApp(ctx, writer, mcp.ConnectedAppInput{App: "nope"})
	wantErr(t, err, mcp.ErrInvalidInput)

	out, err := f.svc.RevokeConnectedApp(ctx, reader, mcp.ConnectedAppInput{App: readGrant.FamilyID.String()})
	must(t, err)
	if out.Name != "reader" {
		t.Errorf("revoked = %+v", out)
	}
	if got, _ := f.auditOf(ctx, t); !slices.Contains(got, "oauth.revoked") {
		t.Errorf("audit = %v", got)
	}
	session := domain.UserActor(f.account.ID, ana, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
	if _, err := f.svc.RevokeConnectedApp(ctx, session, mcp.ConnectedAppInput{App: writeGrant.FamilyID.String()}); err != nil {
		t.Fatal(err)
	}
	if apps, err := f.svc.ListConnectedApps(ctx, session); err != nil || len(apps) != 0 {
		t.Fatalf("apps = %+v, %v", apps, err)
	}

	sa := domain.ServiceAccountActor(f.account.ID, domain.NewID(), domain.NewID())
	if _, err := f.svc.ListConnectedApps(ctx, sa); err == nil {
		t.Error("a service account listed connected apps")
	}
}

func TestConnectedAppCreatesNoToken(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, ana := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	by, _ := grantFor(t, f, ana, "app", domain.ScopeAdmin)
	_, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "longer", Scopes: []domain.Scope{domain.ScopeRead}})
	wantErr(t, err, authz.ErrDenied)
	if got, _ := f.auditOf(ctx, t); !slices.Contains(got, "access.denied") {
		t.Errorf("audit = %v", got)
	}
}
