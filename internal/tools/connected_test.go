package tools_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

var oauthResource = mcp.NewResource("https://forecrew.example")

func accessToken(name string) string { return "fca_" + name }

// grantFor stores the access token of name, of a new family of user, and
// returns its actor.
func grantFor(t *testing.T, f *fixture, user uuid.UUID, name string, scopes ...domain.Scope) (domain.Actor, domain.OAuthGrant) {
	t.Helper()
	ctx := t.Context()
	c := domain.OAuthClient{ClientID: name, Name: name, RedirectURIs: []string{"https://app.example/cb"}}
	must(t, f.st.OAuth().CreateClient(ctx, &c))
	g := domain.OAuthGrant{Kind: domain.OAuthAccess, TokenHash: mcp.HashToken(accessToken(name)), FamilyID: domain.NewID(), ClientID: c.ID, UserID: user, AccountID: f.account.ID,
		Scopes: scopes, Resource: oauthResource.URL, ExpiresAt: time.Now().Add(time.Hour)}
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

type grantBearer string

func (b grantBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// grantSession connects to url with the access token of name.
func grantSession(t *testing.T, url, name string) *sdk.ClientSession {
	t.Helper()
	tr := &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: grantBearer(accessToken(name))}}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "app", Version: "1"}, nil).Connect(t.Context(), tr, nil)
	must(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolError(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
	must(t, err)
	if !res.IsError {
		return ""
	}
	return res.Content[0].(*sdk.TextContent).Text
}

// TestGrantsOnMCP runs the real tools behind /mcp: scopes bound the roles and
// the roles bound the scopes, in one organization.
func TestGrantsOnMCP(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	srv := httptest.NewServer(mcp.NewHandler(f.svc, f.st, authz.New(f.st, nil), "test", oauthResource, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	_, viewer := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	_, maintainer := f.member(t, projectScope, domain.RoleMaintainer, f.project.ID)
	grantFor(t, f, viewer, "viewer-write", domain.ScopeWrite)
	grantFor(t, f, maintainer, "maintainer-read", domain.ScopeRead)
	grantFor(t, f, maintainer, "maintainer-write", domain.ScopeWrite)
	gate := f.gateOn(t, domain.GateMergeApproval, f.project.ID)
	approve := map[string]any{"gate": gate.Ref()}

	t.Run("a Viewer with write scope cannot resolve a gate", func(t *testing.T) {
		if msg := toolError(t, grantSession(t, srv.URL, "viewer-write"), mcp.ToolApprove, approve); !strings.Contains(msg, string(domain.PermGateApproveMerge)) {
			t.Fatalf("error = %q", msg)
		}
	})
	t.Run("a Maintainer needs write scope", func(t *testing.T) {
		if msg := toolError(t, grantSession(t, srv.URL, "maintainer-read"), mcp.ToolApprove, approve); !strings.Contains(msg, string(domain.PermGateApproveMerge)) {
			t.Fatalf("error = %q", msg)
		}
	})
	t.Run("the grant of organization A does not reach organization B", func(t *testing.T) {
		other := domain.Account{Name: "other"}
		must(t, f.st.Accounts().Create(ctx, &other))
		theirs := domain.Project{AccountID: other.ID, Name: "theirs", Repo: "other/theirs", DefaultBranch: "main", Status: domain.ProjectOnboarded}
		must(t, f.st.Projects().Create(ctx, &theirs))
		g, err := f.gates.Open(ctx, gates.OpenRequest{AccountID: other.ID, ProjectID: theirs.ID, Kind: domain.GateMergeApproval, Title: "theirs", SubjectRepo: "other/theirs", SubjectNumber: 1})
		must(t, err)
		// The person is in both, an Owner there: the grant still is for A.
		if err := f.st.Memberships().Create(ctx, &domain.Membership{AccountID: other.ID, UserID: maintainer, Status: domain.MembershipActive, Source: domain.SourceManual}); err != nil {
			t.Fatal(err)
		}
		testkit.Bind(t, f.st, other.ID, domain.SubjectUser, maintainer, orgScope, domain.RoleOwner, uuid.Nil())
		cs := grantSession(t, srv.URL, "maintainer-write")
		if msg := toolError(t, cs, mcp.ToolGetProject, map[string]any{"project": "theirs"}); msg == "" {
			t.Error("read a project of another organization")
		}
		if msg := toolError(t, cs, mcp.ToolApprove, map[string]any{"gate": g.ID.String()}); msg == "" {
			t.Error("resolved a gate of another organization")
		}
	})
	t.Run("roles and scope together resolve it", func(t *testing.T) {
		if msg := toolError(t, grantSession(t, srv.URL, "maintainer-write"), mcp.ToolApprove, approve); msg != "" {
			t.Fatalf("error = %q", msg)
		}
		g, err := f.st.Gates().Get(ctx, f.account.ID, gate.ID)
		must(t, err)
		if g.Status != domain.GateApproved {
			t.Errorf("gate = %+v", g)
		}
	})
	t.Run("the connected app can end itself but mints no token", func(t *testing.T) {
		cs := grantSession(t, srv.URL, "maintainer-read")
		if msg := toolError(t, cs, mcp.ToolCreateToken, map[string]any{"name": "longer", "scopes": []string{"read"}}); !strings.Contains(msg, "connected app") {
			t.Fatalf("error = %q", msg)
		}
		if msg := toolError(t, cs, mcp.ToolListConnectedApps, map[string]any{}); msg != "" {
			t.Fatalf("error = %q", msg)
		}
	})
}

// TestGrantsStayOffREST is why what only /api/v1 serves, and what needs a
// session, never sees a grant, whatever the actor it would act as.
func TestGrantsStayOffREST(t *testing.T) {
	f := newFixture(t)
	_, owner := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	grantFor(t, f, owner, "owner-admin", domain.ScopeAdmin)
	srv := httptest.NewServer(api.New(api.Config{Tools: f.svc, Store: f.st, Authz: authz.New(f.st, nil), Logger: slog.New(slog.DiscardHandler), TokenLogin: true}).Handler())
	t.Cleanup(srv.Close)
	for _, route := range [][2]string{{http.MethodGet, "/api/v1/me"}, {http.MethodGet, "/api/v1/projects"}, {http.MethodPost, "/api/v1/tokens"}, {http.MethodGet, "/api/v1/connected-apps"}} {
		for name, set := range map[string]func(*http.Request){
			"header": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+accessToken("owner-admin")) },
			"cookie": func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: api.CookieName, Value: accessToken("owner-admin")})
			},
		} {
			req, err := http.NewRequestWithContext(t.Context(), route[0], srv.URL+route[1], strings.NewReader("{}"))
			must(t, err)
			req.Header.Set("Content-Type", "application/json")
			set(req)
			res, err := http.DefaultClient.Do(req)
			must(t, err)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s by %s = %d", route[0], route[1], name, res.StatusCode)
			}
		}
	}
	login, err := http.Post(srv.URL+"/api/v1/session", "application/json", strings.NewReader(`{"token":"`+accessToken("owner-admin")+`"}`)) //nolint:noctx // a test
	must(t, err)
	_ = login.Body.Close()
	if login.StatusCode != http.StatusUnauthorized {
		t.Errorf("token login with a grant = %d", login.StatusCode)
	}
}
