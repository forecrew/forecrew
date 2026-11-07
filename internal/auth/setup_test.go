package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// setupLink stores a link for the organization and returns its token.
func (f *fixture) setupLink(ttl time.Duration) string {
	f.t.Helper()
	token, _, err := auth.CreateSetupLink(f.t.Context(), f.st, f.org.ID, "user:ops", ttl, f.clock.Now())
	must(f.t, err)
	return token
}

// postSetup is what the setup page does with the token of the link.
func (f *fixture) postSetup(token string) (status int, body string, cookies []*http.Cookie) {
	f.t.Helper()
	resp, err := f.browser.Post(f.web.URL+"/auth/setup", "application/json", strings.NewReader(`{"token":"`+token+`"}`))
	must(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	must(f.t, err)
	return resp.StatusCode, string(b), resp.Cookies()
}

func (f *fixture) hasCookie(name string) bool {
	u, err := url.Parse(f.web.URL + "/auth/")
	must(f.t, err)
	for _, c := range f.browser.Jar.Cookies(u) {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestSetupLinkMakesTheFirstOwner(t *testing.T) {
	f := newFixture(t)
	orphan := domain.MCPToken{AccountID: f.org.ID, Name: "old", TokenHash: mcp.HashToken("fcm_old")}
	must(t, f.st.MCPTokens().Create(t.Context(), &orphan))
	token := f.setupLink(time.Hour)

	status, body, cookies := f.postSetup(token)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"organization":"Acme"}` {
		t.Fatalf("setup = %d %s", status, body)
	}
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	if c := cookies[0]; c.Name != "fc_setup" || c.Path != "/auth/" || c.MaxAge != 900 || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v", c)
	}
	// Looking at the link does not spend it.
	if l, err := f.st.SetupLinks().GetByTokenHash(t.Context(), mcp.HashToken(token)); err != nil || !l.UsedAt.IsZero() {
		t.Fatalf("link = %+v, %v", l, err)
	}

	f.claims("sub-1", "ana@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	if f.hasCookie("fc_setup") {
		t.Error("the setup cookie is still in the browser")
	}
	users, err := f.st.Users().ListByEmail(t.Context(), "ana@example.com")
	must(t, err)
	if len(users) != 1 || !users[0].EmailVerified {
		t.Fatalf("users = %+v", users)
	}
	checkOwner(t, f, users[0])
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != f.org.ID {
		t.Errorf("organization = %s", c.Actor.AccountID)
	}
	link, err := f.st.SetupLinks().GetByTokenHash(t.Context(), mcp.HashToken(token))
	must(t, err)
	if link.UsedBy != users[0].ID || link.UsedAt.IsZero() {
		t.Errorf("link = %+v", link)
	}
	if tok, _ := f.st.MCPTokens().GetByTokenHash(t.Context(), orphan.TokenHash); tok.OwnerID != users[0].ID {
		t.Errorf("the waiting token was not handed to the first owner: %+v", tok)
	}
	checkUsedAudit(t, f, link, users[0], token)
}

func checkOwner(t *testing.T, f *fixture, u domain.User) {
	t.Helper()
	eff, err := f.st.RoleBindings().Effective(t.Context(), f.org.ID, domain.SubjectUser, u.ID)
	must(t, err)
	if len(eff) != 1 || eff[0].Role.Name != domain.RoleOwner {
		t.Errorf("roles = %+v", eff)
	}
}

func checkUsedAudit(t *testing.T, f *fixture, link store.SetupLink, u domain.User, token string) {
	t.Helper()
	entries, err := f.st.Audit().List(t.Context(), f.org.ID, store.AuditFilter{})
	must(t, err)
	var used bool
	for _, e := range entries {
		if e.Action == "setup_link.used" {
			used = e.SubjectID == link.ID && e.Actor == "user:"+u.ID.String()
		}
		if strings.Contains(string(e.Data), token) {
			t.Errorf("audit %s holds the token", e.Action)
		}
	}
	if !used {
		t.Errorf("no setup_link.used by the new owner in %+v", entries)
	}
}

func TestSetupLinkWorksOnce(t *testing.T) {
	f := newFixture(t)
	token := f.setupLink(time.Hour)
	// Two browsers open the link before either signs in.
	if status, _, _ := f.postSetup(token); status != http.StatusOK {
		t.Fatalf("first setup = %d", status)
	}
	first := f.browser
	f.newBrowser()
	if status, _, _ := f.postSetup(token); status != http.StatusOK {
		t.Fatalf("second setup = %d", status)
	}
	second := f.browser

	f.browser = first
	f.claims("sub-1", "ana@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatalf("first sign-in = %d %s", status, body)
	}
	f.browser = second
	f.claims("sub-2", "eve@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || !strings.Contains(body, "This setup link cannot be used") {
		t.Errorf("second sign-in = %d %s", status, body)
	}
	if users, err := f.st.Users().ListByEmail(t.Context(), "eve@example.com"); err != nil || len(users) != 0 {
		t.Errorf("the refused sign-in left a user: %+v, %v", users, err)
	}
	// Nobody can start over with a link that was used.
	f.newBrowser()
	if status, _, _ := f.postSetup(token); status != http.StatusNotFound {
		t.Errorf("setup of a used link = %d", status)
	}
}

// ownersOf counts the Owner bindings of the organization.
func ownersOf(t *testing.T, f *fixture) int {
	t.Helper()
	list, err := f.st.RoleBindings().List(t.Context(), f.org.ID, store.RoleBindingFilter{})
	must(t, err)
	var n int
	for _, b := range list {
		if r, err := f.st.Roles().Get(t.Context(), f.org.ID, b.RoleID); err == nil && r.Name == domain.RoleOwner {
			n++
		}
	}
	return n
}

// A second link, used after the first, adds nobody: a link only makes the
// first Owner.
func TestASecondSetupLinkMakesNoSecondOwner(t *testing.T) {
	f := newFixture(t)
	first, second := f.setupLink(time.Hour), f.setupLink(time.Hour)
	if status, _, _ := f.postSetup(first); status != http.StatusOK {
		t.Fatal("setup failed")
	}
	one := f.browser
	f.newBrowser()
	if status, _, _ := f.postSetup(second); status != http.StatusOK {
		t.Fatal("setup of the second link failed")
	}
	two := f.browser

	f.browser = one
	f.claims("sub-1", "ana@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatalf("first sign-in = %d %s", status, body)
	}
	f.browser = two
	f.claims("sub-2", "eve@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || !strings.Contains(body, "This setup link cannot be used") {
		t.Errorf("second link = %d %s", status, body)
	}
	if users, err := f.st.Users().ListByEmail(t.Context(), "eve@example.com"); err != nil || len(users) != 0 {
		t.Errorf("the refused sign-in left a user: %+v, %v", users, err)
	}
	if l, err := f.st.SetupLinks().GetByTokenHash(t.Context(), mcp.HashToken(second)); err != nil || !l.UsedAt.IsZero() {
		t.Errorf("the second link was spent: %+v, %v", l, err)
	}
	if n := ownersOf(t, f); n != 1 {
		t.Errorf("owners = %d", n)
	}
	// And a new one of it cannot even start.
	f.newBrowser()
	if status, _, _ := f.postSetup(second); status != http.StatusNotFound {
		t.Errorf("setup of a link of an organization with an owner = %d", status)
	}
}

// A link of an organization that has members gives nothing, and a member
// who signs in with the cookie of one is not promoted.
func TestASetupLinkDoesNotPromoteAMember(t *testing.T) {
	f := newFixture(t)
	token := f.setupLink(time.Hour)
	if status, _, _ := f.postSetup(token); status != http.StatusOK {
		t.Fatal("setup failed")
	}
	// The organization gets a member (forecrew user add, an invitation) before the sign-in.
	f.member("bea@example.com")
	f.claims("sub-bea", "bea@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || !strings.Contains(body, "This setup link cannot be used") {
		t.Fatalf("a member with a link = %d %s", status, body)
	}
	if n := ownersOf(t, f); n != 0 {
		t.Errorf("owners = %d: the member was promoted", n)
	}
	if l, err := f.st.SetupLinks().GetByTokenHash(t.Context(), mcp.HashToken(token)); err != nil || !l.UsedAt.IsZero() {
		t.Errorf("the link was spent: %+v, %v", l, err)
	}
	// Without the link's cookie, which the sign-in dropped, the member signs in as before.
	if f.hasCookie("fc_setup") {
		t.Error("the setup cookie is still in the browser")
	}
	if status, body := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Errorf("sign-in of the member = %d %s", status, body)
	}
	if n := ownersOf(t, f); n != 0 {
		t.Errorf("owners = %d: the member was promoted", n)
	}

	// A link made for an organization that has members is not even accepted.
	f.newBrowser()
	late := f.setupLink(time.Hour)
	if status, _, cookies := f.postSetup(late); status != http.StatusNotFound || len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Errorf("setup of a link of an organization with members = %d, cookies %v", status, cookies)
	}
}

// Two links used at the same moment make one Owner.
func TestTwoSetupLinksRacingMakeOneOwner(t *testing.T) {
	f := newFixture(t)
	var browsers []*http.Client
	for range 2 {
		f.newBrowser()
		if status, _, _ := f.postSetup(f.setupLink(time.Hour)); status != http.StatusOK {
			t.Fatal("setup failed")
		}
		browsers = append(browsers, f.browser)
	}
	f.claims("sub-1", "ana@example.com", true)
	statuses := make(chan int, len(browsers))
	for _, b := range browsers {
		go func() {
			resp, err := b.Get(f.web.URL + "/auth/" + f.inst.ID.String() + "/login")
			if err != nil {
				statuses <- 0
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	var ok, refused int
	for range browsers {
		switch s := <-statuses; s {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
			refused++
		default:
			t.Errorf("status %d", s)
		}
	}
	if ok != 1 || refused != 1 || ownersOf(t, f) != 1 {
		t.Errorf("ok %d, refused %d, owners %d", ok, refused, ownersOf(t, f))
	}
}

// Only a script of this site sends JSON: a form of another site cannot plant the cookie.
func TestSetupRefusesWhatIsNotJSON(t *testing.T) {
	f := newFixture(t)
	token := f.setupLink(time.Hour)
	for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data; boundary=x", ""} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.web.URL+"/auth/setup", strings.NewReader(`{"token":"`+token+`"}`))
		must(t, err)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := f.browser.Do(req)
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType || len(resp.Cookies()) != 0 {
			t.Errorf("%q: status %d, cookies %v", ct, resp.StatusCode, resp.Cookies())
		}
	}
	if f.hasCookie("fc_setup") {
		t.Error("a cookie was planted")
	}
	if status, _, _ := f.postSetup(token); status != http.StatusOK {
		t.Errorf("JSON = %d", status)
	}
}

func TestSetupLinkExpires(t *testing.T) {
	f := newFixture(t)
	token := f.setupLink(30 * time.Minute)
	if status, _, _ := f.postSetup(token); status != http.StatusOK {
		t.Fatalf("setup = %d", status)
	}
	f.clock.Advance(31 * time.Minute)
	f.claims("sub-1", "ana@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || !strings.Contains(body, "This setup link cannot be used") {
		t.Errorf("sign-in after expiry = %d %s", status, body)
	}
	f.newBrowser()
	if status, _, _ := f.postSetup(token); status != http.StatusNotFound {
		t.Errorf("setup after expiry = %d", status)
	}
}

func TestSetupLinkGivesNothingAway(t *testing.T) {
	f := newFixture(t)
	used := f.setupLink(time.Hour)
	if status, _, _ := f.postSetup(used); status != http.StatusOK {
		t.Fatal("setup failed")
	}
	f.claims("sub-1", "ana@example.com", true)
	f.signIn(f.inst, "")
	f.newBrowser()

	var want string
	for _, body := range []string{`{"token":"fcu_nope"}`, `{"token":""}`, `{}`, `not json`, `{"token":"` + strings.Repeat("x", 5000) + `"}`, `{"token":"` + used + `"}`} {
		resp, err := f.browser.Post(f.web.URL+"/auth/setup", "application/json", strings.NewReader(body))
		must(t, err)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		// The only cookie is the one that drops a setup cookie the browser may hold.
		if c := resp.Cookies(); resp.StatusCode != http.StatusNotFound || len(c) != 1 || c[0].Name != "fc_setup" || c[0].Value != "" || c[0].MaxAge >= 0 {
			t.Errorf("%.20s: status %d, cookies %v", body, resp.StatusCode, c)
		}
		if want == "" {
			want = string(b)
		} else if string(b) != want {
			t.Errorf("%.20s: the answer differs: %s", body, b)
		}
	}
	if !strings.Contains(want, "forecrew setup-link") {
		t.Errorf("answer = %s", want)
	}
	// Without a link nobody becomes anything.
	f.claims("sub-9", "stranger@example.com", true)
	if status, _ := f.signIn(f.inst, ""); status != http.StatusForbidden {
		t.Errorf("stranger = %d", status)
	}
}

func TestSetupLinkOpensOnlyItsOrganization(t *testing.T) {
	f := newFixture(t)
	conn := f.orgConnection() // connection of Acme
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(t.Context(), &other))
	token, _, err := auth.CreateSetupLink(t.Context(), f.st, other.ID, "user:ops", time.Hour, f.clock.Now())
	must(t, err)
	if status, _, _ := f.postSetup(token); status != http.StatusOK {
		t.Fatal("setup failed")
	}
	f.verifyDomain(f.org.ID, "example.com")
	f.claims("sub-1", "ana@example.com", true)
	if status, body := f.signIn(conn, ""); status != http.StatusForbidden || !strings.Contains(body, "This setup link cannot be used") {
		t.Errorf("sign-in = %d %s", status, body)
	}
	if l, err := f.st.SetupLinks().GetByTokenHash(t.Context(), mcp.HashToken(token)); err != nil || !l.UsedAt.IsZero() {
		t.Errorf("the link was spent: %+v, %v", l, err)
	}
}

func TestSetupLinkTTLRange(t *testing.T) {
	f := newFixture(t)
	for _, ttl := range []time.Duration{0, time.Second, auth.MaxSetupLinkTTL + time.Second} {
		if _, _, err := auth.CreateSetupLink(t.Context(), f.st, f.org.ID, "user:ops", ttl, f.clock.Now()); err == nil {
			t.Errorf("ttl %s was accepted", ttl)
		}
	}
	if got := auth.SetupURL("https://forecrew.example.com/", "fcu_x"); got != "https://forecrew.example.com/setup#fcu_x" {
		t.Errorf("url = %s", got)
	}
}

func TestSetupLinkIsNeverLogged(t *testing.T) {
	f := newFixture(t)
	tokens := []string{f.setupLink(time.Hour), f.setupLink(time.Hour), f.setupLink(time.Minute)}
	// A refused sign-in, a good one, and a bad request: all log.
	f.postSetup(tokens[0])
	one := f.browser
	f.newBrowser()
	f.postSetup(tokens[1])
	two := f.browser
	f.claims("sub-1", "ana@example.com", true)
	f.browser = one
	f.signIn(f.inst, "")
	f.browser = two
	f.signIn(f.inst, "") // refused: the organization has its Owner
	f.postSetup("fcu_nope")
	if !strings.Contains(f.logs.String(), "sign-in refused") {
		t.Fatalf("nothing logged: %s", f.logs)
	}
	for _, token := range append(tokens, "fcu_nope") {
		if strings.Contains(f.logs.String(), token) {
			t.Errorf("the log holds a setup token:\n%s", f.logs)
		}
	}
}
