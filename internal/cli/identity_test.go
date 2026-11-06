package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func (e *adminEnv) audit(t *testing.T, action string) domain.AuditEntry {
	t.Helper()
	entries, err := e.db.st.Audit().List(t.Context(), e.account.ID, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range entries {
		if a.Action == action {
			return a
		}
	}
	t.Fatalf("no audit entry %s in %+v", action, entries)
	return domain.AuditEntry{}
}

func TestUserAddByEmail(t *testing.T) {
	e := newAdminEnv(t)
	e.vars["USER"] = "ops"
	ctx := t.Context()
	legacy := domain.MCPToken{AccountID: e.account.ID, Name: "old", TokenHash: mcp.HashToken("fcm_old")}
	if err := e.db.st.MCPTokens().Create(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run(t, "user", "add", "ana@example.com", "--org", e.account.Slug, "--role", "owner")
	if code != 0 || !strings.Contains(out, "owner") {
		t.Fatalf("code %d, %q, %q", code, out, errOut)
	}
	u, err := e.db.st.Users().GetByVerifiedEmail(ctx, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	eff, err := e.db.st.RoleBindings().Effective(ctx, e.account.ID, domain.SubjectUser, u.ID)
	if err != nil || len(eff) != 1 || eff[0].Role.Name != domain.RoleOwner {
		t.Fatalf("roles = %+v, %v", eff, err)
	}
	if a := e.audit(t, "user.added"); a.Actor != "user:ops" || a.SubjectID != u.ID {
		t.Errorf("audit = %+v", a)
	}
	// The ownerless tokens now have their first Owner.
	if tok, _ := e.db.st.MCPTokens().GetByTokenHash(ctx, legacy.TokenHash); tok.OwnerID != u.ID {
		t.Errorf("token = %+v", tok)
	}
	// Adding again reuses the user.
	if _, errOut, code := e.run(t, "user", "add", "ANA@example.com", "--org", e.account.Slug); code != 0 {
		t.Errorf("again: %d %q", code, errOut)
	}
	if users, _ := e.db.st.Users().ListByEmail(ctx, "ana@example.com"); len(users) != 1 {
		t.Errorf("users = %+v", users)
	}
}

func TestUserAddRefusals(t *testing.T) {
	e := newAdminEnv(t)
	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"no org":       {[]string{"user", "add", "a@b.c"}, "org"},
		"unknown org":  {[]string{"user", "add", "a@b.c", "--org", "nope"}, "organization nope"},
		"unknown role": {[]string{"user", "add", "a@b.c", "--org", e.account.Slug, "--role", "ghost"}, "role ghost"},
		"no github":    {[]string{"user", "add", "octocat", "--org", e.account.Slug}, "no GitHub connection"},
	} {
		if _, errOut, code := e.run(t, tt.args...); code != 1 || !strings.Contains(errOut, tt.want) {
			t.Errorf("%s: code %d, %q", name, code, errOut)
		}
	}
}

func TestUserAddByGitHubLogin(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	gh := testkit.NewFakeGitHub(t)
	gh.ID, gh.Login = 583231, "octocat"
	e.env.GitHubAPI, e.env.HTTP = gh.URL, gh.Client()
	conn := domain.Connection{Kind: domain.ConnectionGitHub, Name: "github", ClientID: "x", ClientSecretRef: "env:X", Enabled: true}
	if err := e.db.st.Connections().UpsertInstance(ctx, &conn); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run(t, "user", "add", "octocat", "--org", e.account.Slug, "--role", "admin"); code != 0 {
		t.Fatalf("code %d, %q", code, errOut)
	}
	id, err := e.db.st.Identities().GetBySubject(ctx, conn.ID, "583231")
	if err != nil || id.Login != "octocat" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if m, err := e.db.st.Memberships().Get(ctx, e.account.ID, id.UserID); err != nil || m.Source != domain.SourceManual {
		t.Errorf("membership = %+v, %v", m, err)
	}
	if _, errOut, code := e.run(t, "user", "add", "octocat", "--org", e.account.Slug); code != 0 {
		t.Errorf("again: %d %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "user", "add", "ghost", "--org", e.account.Slug); code != 1 || !strings.Contains(errOut, "status 404") {
		t.Errorf("unknown login: %d %q", code, errOut)
	}
	// The login names the user in the other commands.
	if _, errOut, code := e.run(t, "session", "revoke", "--user", "octocat"); code != 0 {
		t.Errorf("revoke by login: %d %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "session", "revoke", "--user", "ghost"); code != 1 || !strings.Contains(errOut, "GitHub login") {
		t.Errorf("unknown login: %d %q", code, errOut)
	}
}

func TestGitHubLookupErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":0}`)) }))
	defer srv.Close()
	if _, err := gitHubUserID(t.Context(), Env{GitHubAPI: srv.URL}, "x"); err == nil {
		t.Error("an answer without an ID was taken")
	}
	url := srv.URL
	srv.Close()
	if _, err := gitHubUserID(t.Context(), Env{GitHubAPI: url}, "x"); err == nil {
		t.Error("an unreachable API was taken")
	}
}

func TestOrgCreate(t *testing.T) {
	e := newAdminEnv(t)
	out, _, code := e.run(t, "org", "create", "Acme Labs")
	if code != 0 || !strings.Contains(out, "acme-labs") {
		t.Fatalf("code %d, %q", code, out)
	}
	a, err := e.db.st.Accounts().GetBySlug(t.Context(), "acme-labs")
	if err != nil || !strings.HasPrefix(a.CreatedBy, "user:") {
		t.Fatalf("account = %+v, %v", a, err)
	}
	if _, _, code := e.run(t, "org", "create", "Other", "--slug", "acme"); code != 0 {
		t.Error("given slug")
	}
	if _, errOut, code := e.run(t, "org", "create", "Again", "--slug", "acme"); code != 1 || !strings.Contains(errOut, "already exists") {
		t.Errorf("taken slug: %d %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "org", "create", "Bad", "--slug", "Not Valid"); code != 1 || !strings.Contains(errOut, "slug") {
		t.Errorf("bad slug: %d %q", code, errOut)
	}
}

func signedInUser(t *testing.T, e *adminEnv) (domain.User, domain.Session) {
	t.Helper()
	ctx := t.Context()
	u, err := e.db.st.Users().GetByVerifiedEmail(ctx, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	s := domain.Session{TokenHash: mcp.HashToken("fcs_x"), UserID: u.ID, AccountID: e.account.ID, IdleExpiresAt: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}
	if err := e.db.st.Sessions().Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	return u, s
}

func TestUserDisable(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	u, s := signedInUser(t, e)
	if out, errOut, code := e.run(t, "user", "disable", testOwner); code != 0 || !strings.Contains(out, "disabled") {
		t.Fatalf("code %d, %q %q", code, out, errOut)
	}
	got, err := e.db.st.Users().Get(ctx, u.ID)
	if err != nil || got.DisabledAt.IsZero() {
		t.Fatalf("user = %+v, %v", got, err)
	}
	if sess, _ := e.db.st.Sessions().GetByTokenHash(ctx, s.TokenHash); sess.RevokedAt.IsZero() {
		t.Error("session goes on")
	}
	e.audit(t, "user.disabled")
	if _, _, code := e.run(t, "user", "disable", u.ID.String()); code != 0 {
		t.Error("by ID")
	}
	if _, errOut, code := e.run(t, "user", "disable", "ghost@example.com"); code != 1 || !strings.Contains(errOut, "no single user") {
		t.Errorf("unknown: %d %q", code, errOut)
	}
}

func TestUserDisableLastOwner(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, code := e.run(t, "user", "add", "boss@example.com", "--org", e.account.Slug, "--role", "owner"); code != 0 {
		t.Fatal("add")
	}
	if _, errOut, code := e.run(t, "user", "disable", "boss@example.com"); code != 1 || !strings.Contains(errOut, "active owner") {
		t.Errorf("last owner: %d %q", code, errOut)
	}
}

func TestSessionRevoke(t *testing.T) {
	e := newAdminEnv(t)
	_, s := signedInUser(t, e)
	if _, _, code := e.run(t, "session", "revoke"); code != 1 {
		t.Error("--user is required")
	}
	if out, _, code := e.run(t, "session", "revoke", "--user", testOwner); code != 0 || !strings.Contains(out, "ended") {
		t.Fatalf("code %d, %q", code, out)
	}
	if sess, _ := e.db.st.Sessions().GetByTokenHash(t.Context(), s.TokenHash); sess.RevokedAt.IsZero() {
		t.Error("session goes on")
	}
	e.audit(t, "session.revoked")
}

func TestTokenCreateNeedsAnOwner(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	if _, errOut, code := e.run(t, "token", "create", "x"); code != 1 || !strings.Contains(errOut, "needs an owner") {
		t.Errorf("no owner: %d %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "token", "create", "x", "--user", "a@b.c", "--service-account", "ci"); code != 1 || !strings.Contains(errOut, "needs an owner") {
		t.Errorf("both: %d %q", code, errOut)
	}
	outsider := domain.User{Email: "out@example.com", EmailVerified: true}
	if err := e.db.st.Users().Create(ctx, &outsider); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run(t, "token", "create", "x", "--user", "out@example.com"); code != 1 || !strings.Contains(errOut, "not a member") {
		t.Errorf("outsider: %d %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "token", "create", "x", "--service-account", "ghost"); code != 1 || !strings.Contains(errOut, "ghost") {
		t.Errorf("unknown service account: %d %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "token", "create", "x", "--user", testOwner, "--scope", "root"); code != 1 || !strings.Contains(errOut, "scopes") {
		t.Errorf("bad scope: %d %q", code, errOut)
	}
	sa := domain.ServiceAccount{AccountID: e.account.ID, Name: "ci", CreatedBy: "user:x"}
	if err := e.db.st.ServiceAccounts().Create(ctx, &sa); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run(t, "token", "create", "deploy", "--service-account", "ci", "--scope", "read", "--ttl", "1h")
	if code != 0 {
		t.Fatalf("code %d, %q", code, errOut)
	}
	tok, err := e.db.st.MCPTokens().GetByTokenHash(ctx, mcp.HashToken(strings.TrimSpace(out)))
	if err != nil || tok.OwnerKind != domain.SubjectServiceAccount || tok.OwnerID != sa.ID || tok.Scopes[0] != domain.ScopeRead || tok.ExpiresAt.After(time.Now().Add(2*time.Hour)) {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	e.audit(t, "token.created")
}

func TestFindUserWithUnverifiedEmail(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	u := domain.User{Email: "same@example.com"}
	if err := e.db.st.Users().Create(ctx, &u); err != nil {
		t.Fatal(err)
	}
	if got, err := findUser(ctx, e.db.st, "same@example.com"); err != nil || got.ID != u.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
	other := domain.User{Email: "same@example.com"}
	if err := e.db.st.Users().Create(ctx, &other); err != nil {
		t.Fatal(err)
	}
	if _, err := findUser(ctx, e.db.st, "same@example.com"); err == nil {
		t.Error("two users, one taken")
	}
}
