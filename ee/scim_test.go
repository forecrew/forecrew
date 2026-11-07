//go:build enterprise

package ee

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func fakeGrants(t *testing.T, e *env) map[string]func(uuid.UUID) {
	t.Helper()
	fake, ok := e.st.(*testkit.Store)
	if !ok {
		t.Fatal("not the fake store")
	}
	var id uuid.UUID
	return e.accessHooks(func(user uuid.UUID) {
		id = fake.AddOAuthGrant(domain.OAuthGrant{AccountID: e.org.ID, UserID: user, Kind: domain.OAuthAccess}).ID
	}, func() bool {
		g, err := fake.OAuthGrant(e.org.ID, id)
		must(t, err)
		return !g.RevokedAt.IsZero()
	})
}

func TestRecordedSequences(t *testing.T) {
	for _, file := range []string{"testdata/scim/okta.json", "testdata/scim/entra.json"} {
		t.Run(file, func(t *testing.T) {
			e := newEnv(t)
			e.verified("acme.example")
			e.scim().replay(file, fakeGrants(t, e))
			if !e.audited("scim.user_created") || !e.audited("scim.user_deactivated") || !e.audited("scim.group_created") {
				t.Error("the changes are not audited")
			}
		})
	}
}

func TestSCIMAuthentication(t *testing.T) {
	e := newEnv(t)
	c := e.scim()
	other := domain.Account{Name: "Other"}
	must(t, e.st.Accounts().Create(t.Context(), &other))
	call := func(slug, header string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/scim/v2/"+slug+"/Users", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		c.h.ServeHTTP(w, r)
		return w
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"no header":            call(e.org.Slug, ""),
		"unknown token":        call(e.org.Slug, "Bearer fcscim_nope"),
		"another organization": call(other.Slug, "Bearer "+c.secret),
		"unknown organization": call("nowhere", "Bearer "+c.secret),
		"wrong scheme":         call(e.org.Slug, "Basic "+c.secret),
	} {
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s = %d", name, w.Code)
		}
	}
	if w := call(e.org.Slug, "bearer "+c.secret); w.Code != http.StatusOK {
		t.Errorf("good token = %d %s", w.Code, w.Body)
	}
	list, err := e.svc.ListSCIMTokens(t.Context(), e.owner)
	must(t, err)
	if len(list) != 1 || list[0].LastUsedAt.IsZero() {
		t.Errorf("tokens = %+v", list)
	}
	if _, err := e.svc.RevokeSCIMToken(t.Context(), e.owner, mcp.SCIMTokenRefInput{Token: "idp"}); err != nil {
		t.Fatal(err)
	}
	if w := call(e.org.Slug, "Bearer "+c.secret); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked = %d", w.Code)
	}
}

func TestSCIMDiscovery(t *testing.T) {
	e := newEnv(t)
	c := e.scim()
	for path, want := range map[string]map[string]any{
		"/ServiceProviderConfig": {"patch.supported": true, "bulk.supported": false, "filter.maxResults": float64(200)},
		"/ResourceTypes":         {"totalResults": float64(2), "Resources.0.id": "User"},
		"/ResourceTypes/Group":   {"endpoint": "/Groups"},
		"/Schemas":               {"totalResults": float64(2)},
		"/Schemas/urn:ietf:params:scim:schemas:core:2.0:User": {"name": "User"},
	} {
		status, body := c.do(http.MethodGet, path, "")
		if status != http.StatusOK {
			t.Errorf("%s = %d", path, status)
		}
		for k, v := range want {
			if got := lookup(body, k); got != v {
				t.Errorf("%s: %s = %v", path, k, got)
			}
		}
	}
	if status, _ := c.do(http.MethodGet, "/Schemas/nope", ""); status != http.StatusNotFound {
		t.Errorf("unknown schema = %d", status)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/scim/v2/", nil)
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("no such endpoint = %d", w.Code)
	}
}

const newUser = `{"userName":"%s","active":true,"name":{"formatted":"Ann Smith"}}`

func userBody(email string) string { return strings.Replace(newUser, "%s", email, 1) }

// A SCIM token reaches only what it provisioned: a member who came another
// way is a conflict at creation and does not exist anywhere else.
func TestSCIMLeavesOtherMembersAlone(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.verified("acme.example")
	c := e.scim()
	manual := e.person("manual@acme.example", domain.RoleMember)
	id, _ := manual.SubjectID()
	invited := domain.User{Name: "I", Email: "invited@acme.example", EmailVerified: true}
	must(t, e.st.Users().Create(ctx, &invited))
	must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: e.org.ID, UserID: invited.ID, Status: domain.MembershipInvited, Source: domain.SourceInvitation}))
	for _, email := range []string{"manual@acme.example", "invited@acme.example", "MANUAL@acme.example"} {
		if status, body := c.do(http.MethodPost, "/Users", userBody(email)); status != http.StatusConflict || body["scimType"] != "uniqueness" {
			t.Errorf("%s = %d %v", email, status, body)
		}
	}
	for _, tt := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPut, userBody("manual@acme.example")},
		{http.MethodPatch, `{"Operations":[{"op":"replace","path":"active","value":false}]}`},
		{http.MethodDelete, ""},
	} {
		if status, _ := c.do(tt.method, "/Users/"+id.String(), tt.body); status != http.StatusNotFound {
			t.Errorf("%s = %d", tt.method, status)
		}
	}
	if m, err := e.st.Memberships().Get(ctx, e.org.ID, id); err != nil || m.Status != domain.MembershipActive {
		t.Errorf("membership = %+v, %v", m, err)
	}
	if _, body := c.do(http.MethodGet, "/Users", ""); body["totalResults"] != float64(0) {
		t.Errorf("list = %v", body)
	}
	team := e.team("Manual", domain.SourceManual)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ := c.do(method, "/Groups/"+team.ID.String(), ""); status != http.StatusNotFound {
			t.Errorf("%s manual team = %d", method, status)
		}
	}
	// It cannot add such a member to one of its groups either.
	if status, body := c.do(http.MethodPost, "/Groups", `{"displayName":"G","members":[{"value":"`+id.String()+`"}]}`); status != http.StatusBadRequest || body["scimType"] != "invalidValue" {
		t.Errorf("group with a manual member = %d %v", status, body)
	}
}

func TestSCIMNeedsAVerifiedDomain(t *testing.T) {
	e := newEnv(t)
	c := e.scim()
	if _, err := e.svc.AddDomain(t.Context(), e.owner, mcp.DomainInput{Domain: "pending.example"}); err != nil {
		t.Fatal(err)
	}
	other := domain.Account{Name: "Other"}
	must(t, e.st.Accounts().Create(t.Context(), &other))
	d := domain.VerifiedDomain{AccountID: other.ID, Domain: "other.example", VerificationToken: "t"}
	must(t, e.st.VerifiedDomains().Create(t.Context(), &d))
	must(t, e.st.VerifiedDomains().SetVerified(t.Context(), other.ID, d.ID, true))
	for _, name := range []string{"a@pending.example", "a@other.example", "a@gmail.example", "not-an-email"} {
		if status, body := c.do(http.MethodPost, "/Users", userBody(name)); status != http.StatusBadRequest || body["scimType"] != "invalidValue" {
			t.Errorf("%s = %d %v", name, status, body)
		}
	}
	e.verified("acme.example")
	status, body := c.do(http.MethodPost, "/Users", userBody("a@acme.example"))
	if status != http.StatusCreated {
		t.Fatalf("create = %d %v", status, body)
	}
	id, _ := body["id"].(string)
	if status, _ := c.do(http.MethodPatch, "/Users/"+id, `{"Operations":[{"op":"replace","path":"userName","value":"a@gmail.example"}]}`); status != http.StatusBadRequest {
		t.Errorf("rename out of the domain = %d", status)
	}
}

// Someone in the organization's domain who is not a member yet joins, with
// the user they have.
func TestSCIMLinksAnExistingUser(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.verified("acme.example")
	c := e.scim()
	u := domain.User{Name: "Ann", Email: "ann@acme.example", EmailVerified: true}
	must(t, e.st.Users().Create(ctx, &u))
	status, body := c.do(http.MethodPost, "/Users", userBody("ann@acme.example"))
	if status != http.StatusCreated || body["id"] != u.ID.String() {
		t.Fatalf("create = %d %v", status, body)
	}
	m, err := e.st.Memberships().Get(ctx, e.org.ID, u.ID)
	must(t, err)
	if m.Source != domain.SourceSCIM || m.Status != domain.MembershipActive {
		t.Errorf("membership = %+v", m)
	}
	// The new user of the next one is verified, so a sign-in links to it.
	status, body = c.do(http.MethodPost, "/Users", userBody("bea@acme.example"))
	if status != http.StatusCreated {
		t.Fatalf("create = %d %v", status, body)
	}
	got, err := e.st.Users().GetByVerifiedEmail(ctx, "bea@acme.example")
	must(t, err)
	if got.Name != "Ann Smith" {
		t.Errorf("user = %+v", got)
	}
}

func TestSCIMFiltersAndPages(t *testing.T) {
	e := newEnv(t)
	e.verified("acme.example")
	c := e.scim()
	for _, name := range []string{"a", "b", "c"} {
		if status, body := c.do(http.MethodPost, "/Users", `{"userName":"`+name+`@acme.example","externalId":"ext-`+name+`"}`); status != http.StatusCreated {
			t.Fatalf("create = %d %v", status, body)
		}
	}
	_, body := c.do(http.MethodGet, "/Users?startIndex=2&count=1", "")
	if body["totalResults"] != float64(3) || body["itemsPerPage"] != float64(1) || body["startIndex"] != float64(2) || lookup(body, "Resources.0.userName") != "b@acme.example" {
		t.Errorf("page = %v", body)
	}
	if _, body := c.do(http.MethodGet, "/Users?startIndex=9", ""); body["itemsPerPage"] != float64(0) || body["totalResults"] != float64(3) {
		t.Errorf("past the end = %v", body)
	}
	if _, body := c.do(http.MethodGet, "/Users?startIndex=0&count=-1", ""); body["startIndex"] != float64(1) || body["itemsPerPage"] != float64(0) {
		t.Errorf("lenient paging = %v", body)
	}
	if _, body := c.do(http.MethodGet, `/Users?filter=externalId%20eq%20%22ext-c%22`, ""); body["totalResults"] != float64(1) {
		t.Errorf("by externalId = %v", body)
	}
	for _, filter := range []string{`title%20eq%20%22x%22`, `userName%20co%20%22x%22`, `garbage`} {
		if status, body := c.do(http.MethodGet, "/Users?filter="+filter, ""); status != http.StatusBadRequest || body["scimType"] != "invalidFilter" {
			t.Errorf("%s = %d %v", filter, status, body)
		}
	}
	if status, _ := c.do(http.MethodPost, "/Users", `{"userName":"d@acme.example","externalId":"ext-a"}`); status != http.StatusConflict {
		t.Errorf("taken externalId = %d", status)
	}
}

func TestSCIMUserPatchErrors(t *testing.T) {
	e := newEnv(t)
	e.verified("acme.example")
	c := e.scim()
	_, body := c.do(http.MethodPost, "/Users", `{"userName":"a@acme.example","externalId":"x"}`)
	id, _ := body["id"].(string)
	c.do(http.MethodPost, "/Users", `{"userName":"b@acme.example","externalId":"y"}`)
	for _, tt := range []struct {
		body   string
		status int
	}{
		{`{not json`, 400},
		{`{"Operations":[]}`, 400},
		{`{"Operations":[{"op":"move","path":"active","value":true}]}`, 400},
		{`{"Operations":[{"op":"replace","path":"active","value":"maybe"}]}`, 400},
		{`{"Operations":[{"op":"remove","path":"active"}]}`, 400},
		{`{"Operations":[{"op":"replace","value":"x"}]}`, 400},
		{`{"Operations":[{"op":"replace","path":"userName","value":7}]}`, 400},
		{`{"Operations":[{"op":"replace","path":"name","value":7}]}`, 400},
		{`{"Operations":[{"op":"replace","path":"externalId","value":"y"}]}`, 409},
		{`{"Operations":[{"op":"replace","path":"userName","value":"b@acme.example"}]}`, 409},
		{`{"Operations":[{"op":"replace","path":"title","value":"ignored"}]}`, 200},
		{`{"Operations":[{"op":"remove","path":"externalId"}]}`, 200},
		{`{"Operations":[{"op":"replace","path":"name","value":{"givenName":"A","familyName":"B"}}]}`, 200},
		{`{"Operations":[{"op":"replace","path":"urn:ietf:params:scim:schemas:core:2.0:User:displayName","value":"C D"}]}`, 200},
	} {
		if status, got := c.do(http.MethodPatch, "/Users/"+id, tt.body); status != tt.status {
			t.Errorf("%s = %d %v", tt.body, status, got)
		}
	}
	status, got := c.do(http.MethodGet, "/Users/"+id, "")
	if status != http.StatusOK || got["displayName"] != "C D" || got["externalId"] != nil {
		t.Errorf("user = %v", got)
	}
	if status, _ := c.do(http.MethodPut, "/Users/"+id, `{"userName":""}`); status != http.StatusBadRequest {
		t.Errorf("PUT without userName = %d", status)
	}
	if status, _ := c.do(http.MethodPut, "/Users/not-a-uuid", `{"userName":"a@acme.example"}`); status != http.StatusNotFound {
		t.Errorf("PUT unknown = %d", status)
	}
	big := `{"userName":"` + strings.Repeat("x", scimMaxBody) + `"}`
	if status, _ := c.do(http.MethodPost, "/Users", big); status != http.StatusRequestEntityTooLarge {
		t.Errorf("large body = %d", status)
	}
}

// The last Owner stays, whichever way they came in.
func TestSCIMKeepsTheLastOwner(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.verified("acme.example")
	c := e.scim()
	_, body := c.do(http.MethodPost, "/Users", userBody("boss@acme.example"))
	id, _ := body["id"].(string)
	user := uuid.MustParse(id)
	testkit.Bind(t, e.st, e.org.ID, domain.SubjectUser, user, domain.RoleScopeOrganization, domain.RoleOwner, uuid.Nil())
	first, _ := e.owner.SubjectID()
	bindings, err := e.st.RoleBindings().List(ctx, e.org.ID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser, SubjectID: first})
	must(t, err)
	must(t, e.st.RoleBindings().Delete(ctx, e.org.ID, bindings[0].ID))
	if status, got := c.do(http.MethodPatch, "/Users/"+id, `{"Operations":[{"op":"replace","path":"active","value":false}]}`); status != http.StatusConflict || got["scimType"] != "mutability" {
		t.Fatalf("deactivate = %d %v", status, got)
	}
	if m, err := e.st.Memberships().Get(ctx, e.org.ID, user); err != nil || m.Status != domain.MembershipActive {
		t.Errorf("membership = %+v, %v", m, err)
	}
}

func TestSCIMGroupKeepsManualMembers(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.verified("acme.example")
	c := e.scim()
	_, a := c.do(http.MethodPost, "/Users", userBody("a@acme.example"))
	_, b := c.do(http.MethodPost, "/Users", userBody("b@acme.example"))
	aID, bID := a["id"].(string), b["id"].(string)
	status, g := c.do(http.MethodPost, "/Groups", `{"displayName":"Core","externalId":"g1","members":[{"value":"`+aID+`"}]}`)
	if status != http.StatusCreated {
		t.Fatalf("group = %d %v", status, g)
	}
	gid := uuid.MustParse(g["id"].(string))
	manual := e.person("m@example.com", domain.RoleMember)
	mid, _ := manual.SubjectID()
	must(t, e.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: gid, UserID: mid, AccountID: e.org.ID, Source: domain.SourceManual}))
	must(t, e.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: gid, UserID: uuid.MustParse(bID), AccountID: e.org.ID, Source: domain.SourceManual}))

	// A manual member does not show, and a replace does not take them out.
	if status, got := c.do(http.MethodPut, "/Groups/"+gid.String(), `{"displayName":"Core","externalId":"g1","members":[]}`); status != http.StatusOK || len(got["members"].([]any)) != 0 {
		t.Fatalf("replace = %d %v", status, got)
	}
	members, err := e.st.Teams().Members(ctx, e.org.ID, gid)
	must(t, err)
	if len(members) != 2 {
		t.Fatalf("members = %+v", members)
	}
	// Removing someone SCIM did not add is a no-op.
	if status, _ := c.do(http.MethodPatch, "/Groups/"+gid.String(), `{"Operations":[{"op":"remove","path":"members[value eq \"`+bID+`\"]"}]}`); status != http.StatusNoContent {
		t.Errorf("remove = %d", status)
	}
	if members, _ = e.st.Teams().Members(ctx, e.org.ID, gid); len(members) != 2 {
		t.Fatalf("members = %+v", members)
	}
	// Conflicts and errors of groups.
	for _, tt := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/Groups", `{"displayName":"CORE"}`, 409},
		{http.MethodPost, "/Groups", `{"displayName":"Other","externalId":"g1"}`, 409},
		{http.MethodPost, "/Groups", `{"displayName":" "}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"remove","path":"displayName"}]}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"add","path":"members[value eq \"x\"]"}]}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"add","path":"members","value":7}]}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"replace","value":3}]}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"replace","path":"displayName","value":7}]}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"replace","path":"externalId","value":7}]}`, 400},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"replace","path":"members","value":{"value":"` + aID + `"}}]}`, 204},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"remove","path":"members"}]}`, 204},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"add","value":{"members":[{"value":"` + aID + `"}]}}]}`, 204},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"remove","path":"members","value":[{"value":"nope"}]}]}`, 204},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"remove","path":"members[value eq \"nope\"]"}]}`, 204},
		{http.MethodPatch, "/Groups/" + gid.String(), `{"Operations":[{"op":"remove","path":"externalId"}]}`, 204},
		{http.MethodGet, "/Groups/not-a-uuid", "", 404},
	} {
		if status, got := c.do(tt.method, tt.path, tt.body); status != tt.status {
			t.Errorf("%s %s %s = %d %v", tt.method, tt.path, tt.body, status, got)
		}
	}
	if _, got := c.do(http.MethodGet, "/Groups?filter=externalId%20eq%20%22g1%22", ""); got["totalResults"] != float64(0) {
		t.Errorf("by externalId = %v", got)
	}
	// A slug a manual team has takes the next one.
	e.team("Taken", domain.SourceManual)
	_, got := c.do(http.MethodPost, "/Groups", `{"displayName":"taken"}`)
	team, err := e.st.Teams().Get(ctx, e.org.ID, uuid.MustParse(got["id"].(string)))
	must(t, err)
	if team.Slug != "taken-2" || team.Source != domain.SourceSCIM {
		t.Errorf("team = %+v", team)
	}
}

func TestSCIMTokenNoEscalation(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	admin := e.person("admin@example.com", domain.RoleAdmin)
	if _, err := e.svc.CreateSCIMToken(ctx, admin, mcp.CreateSCIMTokenInput{Name: "fine"}); err != nil {
		t.Fatalf("nothing provisioned yet: %v", err)
	}
	team := e.team("Billing", domain.SourceSCIM)
	testkit.Bind(t, e.st, e.org.ID, domain.SubjectTeam, team.ID, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	if _, err := e.svc.CreateSCIMToken(ctx, admin, mcp.CreateSCIMTokenInput{Name: "team"}); !isDenied(err) {
		t.Errorf("a team above the Admin: %v", err)
	}
	must(t, e.st.Teams().Delete(ctx, e.org.ID, team.ID))
	u := domain.User{Name: "B"}
	must(t, e.st.Users().Create(ctx, &u))
	must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: e.org.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceSCIM, ExternalID: "x"}))
	testkit.Bind(t, e.st, e.org.ID, domain.SubjectUser, u.ID, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	if _, err := e.svc.CreateSCIMToken(ctx, admin, mcp.CreateSCIMTokenInput{Name: "member"}); !isDenied(err) {
		t.Errorf("a member above the Admin: %v", err)
	}
	if out, err := e.svc.CreateSCIMToken(ctx, e.owner, mcp.CreateSCIMTokenInput{Name: "owner"}); err != nil || !strings.HasPrefix(out.Secret, "fcscim_") || out.URL != "https://forecrew.example/scim/v2/"+e.org.Slug {
		t.Errorf("owner: %+v, %v", out, err)
	}
	if _, err := e.svc.CreateSCIMToken(ctx, e.owner, mcp.CreateSCIMTokenInput{Name: "owner"}); !isConflict(err) {
		t.Errorf("a name taken: %v", err)
	}
	if _, err := e.svc.CreateSCIMToken(ctx, e.owner, mcp.CreateSCIMTokenInput{Name: "Not Valid"}); !isInvalid(err) {
		t.Errorf("a bad name: %v", err)
	}
	viewer := e.person("v@example.com", domain.RoleViewer)
	if _, err := e.svc.ListSCIMTokens(ctx, viewer); !isDenied(err) {
		t.Errorf("list: %v", err)
	}
	if _, err := e.svc.RevokeSCIMToken(ctx, viewer, mcp.SCIMTokenRefInput{Token: "owner"}); !isDenied(err) {
		t.Errorf("revoke: %v", err)
	}
	if _, err := e.svc.RevokeSCIMToken(ctx, e.owner, mcp.SCIMTokenRefInput{Token: "nope"}); !isNotFound(err) {
		t.Errorf("revoke unknown: %v", err)
	}
}
