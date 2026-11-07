//go:build enterprise

package ee

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/enterprise"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeDNS answers TXT lookups from a map; err, when set, fails every lookup.
type fakeDNS struct {
	mu      sync.Mutex
	records map[string][]string
	err     error
}

func (d *fakeDNS) set(name string, values ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records[name] = values
}

func (d *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	if v, ok := d.records[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// env is the service on a store, with an organization and an Owner.
type env struct {
	t     *testing.T
	st    store.Store
	svc   *Service
	dns   *fakeDNS
	clock *testkit.Clock
	gates *gates.Keeper
	org   domain.Account
	owner domain.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := testkit.NewClock(t0)
	return newEnvOn(t, testkit.NewStore(clock.Now), clock)
}

func newEnvOn(t *testing.T, st store.Store, clock *testkit.Clock) *env {
	t.Helper()
	ctx := t.Context()
	e := &env{t: t, st: st, clock: clock, dns: &fakeDNS{records: map[string][]string{}}}
	e.org = domain.Account{Name: "Acme"}
	must(t, st.Accounts().Create(ctx, &e.org))
	e.gates = gates.New(st, testkit.NewSignalBus(st), clock.Now)
	e.svc = New(enterprise.Deps{Store: st, Authz: authz.New(st, nil), Gates: e.gates, PublicURL: "https://forecrew.example/", Now: clock.Now}, e.dns)
	e.owner = e.person("owner@example.com", domain.RoleOwner)
	return e
}

// person adds a member with an organization role and returns their actor.
func (e *env) person(email, role string) domain.Actor {
	e.t.Helper()
	ctx := e.t.Context()
	u := domain.User{Name: email, Email: email, EmailVerified: true}
	must(e.t, e.st.Users().Create(ctx, &u))
	must(e.t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: e.org.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	testkit.Bind(e.t, e.st, e.org.ID, domain.SubjectUser, u.ID, domain.RoleScopeOrganization, role, uuid.Nil())
	return domain.UserActor(e.org.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
}

// verified adds a domain and verifies it through DNS.
func (e *env) verified(name string) mcp.DomainInfo {
	e.t.Helper()
	ctx := e.t.Context()
	d, err := e.svc.AddDomain(ctx, e.owner, mcp.DomainInput{Domain: name})
	must(e.t, err)
	e.dns.set(d.RecordName, "unrelated", d.RecordValue)
	d, err = e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: name})
	must(e.t, err)
	return d
}

func (e *env) connection(name string) domain.Connection {
	e.t.Helper()
	c := domain.Connection{AccountID: e.org.ID, Kind: domain.ConnectionOIDC, Name: name, Issuer: "https://idp.example", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_X", Enabled: true}
	must(e.t, e.st.Connections().Create(e.t.Context(), &c))
	return c
}

func (e *env) team(name string, source domain.Source) domain.Team {
	e.t.Helper()
	x := domain.Team{AccountID: e.org.ID, Name: name, Slug: domain.Slugify(name), Source: source}
	must(e.t, e.st.Teams().Create(e.t.Context(), &x))
	return x
}

func (e *env) audited(action string) bool {
	e.t.Helper()
	entries, err := e.st.Audit().List(e.t.Context(), e.org.ID, store.AuditFilter{})
	must(e.t, err)
	for _, a := range entries {
		if a.Action == action {
			return true
		}
	}
	return false
}

// scimClient calls the SCIM endpoint of the organization.
type scimClient struct {
	e      *env
	h      http.Handler
	secret string
	vars   map[string]string
}

func (e *env) scim() *scimClient {
	e.t.Helper()
	out, err := e.svc.CreateSCIMToken(e.t.Context(), e.owner, mcp.CreateSCIMTokenInput{Name: "idp"})
	must(e.t, err)
	return &scimClient{e: e, h: e.svc.SCIM(), secret: out.Secret, vars: map[string]string{}}
}

func (c *scimClient) expand(s string) string {
	for k, v := range c.vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}

// do sends a request and returns the status and the decoded body.
func (c *scimClient) do(method, path, body string) (int, map[string]any) {
	c.e.t.Helper()
	r := httptest.NewRequestWithContext(c.e.t.Context(), method, "/scim/v2/"+c.e.org.Slug+c.expand(path), strings.NewReader(c.expand(body)))
	r.Header.Set("Authorization", "Bearer "+c.secret)
	r.Header.Set("Content-Type", scimContentType)
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	var out map[string]any
	if w.Body.Len() > 0 {
		must(c.e.t, json.Unmarshal(w.Body.Bytes(), &out))
		if got := w.Header().Get("Content-Type"); got != scimContentType {
			c.e.t.Errorf("%s %s: content type %s", method, path, got)
		}
	}
	return w.Code, out
}

// lookup follows a dotted path through decoded JSON.
func lookup(v any, path string) any {
	for part := range strings.SplitSeq(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// step is one request of a recorded sequence, or a hook between requests.
type step struct {
	Name   string            `json:"name"`
	Hook   string            `json:"hook"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Body   json.RawMessage   `json:"body"`
	Status int               `json:"status"`
	Save   map[string]string `json:"save"`
	Expect map[string]any    `json:"expect"`
}

var unexpanded = regexp.MustCompile(`\{\{\w+\}\}`)

func same(c *scimClient, got, want any) bool {
	a, err := json.Marshal(got)
	if err != nil {
		return false
	}
	b, err := json.Marshal(want)
	if err != nil {
		return false
	}
	return string(a) == c.expand(string(b))
}

// replay runs a sequence; hooks are its calls that are not SCIM.
func (c *scimClient) replay(file string, hooks map[string]func(user uuid.UUID)) {
	t := c.e.t
	raw, err := os.ReadFile(file)
	must(t, err)
	var steps []step
	must(t, json.Unmarshal(raw, &steps))
	for _, s := range steps {
		if s.Hook != "" {
			hooks[s.Hook](uuid.MustParse(c.vars["user"]))
			continue
		}
		status, body := c.do(s.Method, s.Path, string(s.Body))
		if status != s.Status {
			t.Fatalf("%s: status %d, want %d: %v", s.Name, status, s.Status, body)
		}
		for k, from := range s.Save {
			c.vars[k], _ = lookup(body, from).(string)
		}
		for path, want := range s.Expect {
			if got := lookup(body, path); !same(c, got, want) {
				t.Errorf("%s: %s = %v, want %v", s.Name, path, got, want)
			}
		}
		if unexpanded.MatchString(c.expand(s.Path)) {
			t.Fatalf("%s: unknown variable in %s", s.Name, s.Path)
		}
	}
}

// accessHooks seed a session, a token and an OAuth grant for the user and
// check that all three ended. grant adds a grant; revoked says it is revoked.
func (e *env) accessHooks(grant func(user uuid.UUID), revoked func() bool) map[string]func(uuid.UUID) {
	ctx := e.t.Context()
	hash := func() []byte {
		token, err := auth.NewToken("")
		must(e.t, err)
		return mcp.HashToken(token)
	}
	return map[string]func(uuid.UUID){
		"grant_access": func(user uuid.UUID) {
			now := e.clock.Now()
			must(e.t, e.st.Sessions().Create(ctx, &domain.Session{TokenHash: hash(), UserID: user, AccountID: e.org.ID, IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(time.Hour)}))
			must(e.t, e.st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: e.org.ID, Name: "laptop-" + user.String(), TokenHash: hash(), OwnerKind: domain.SubjectUser, OwnerID: user, Scopes: []domain.Scope{domain.ScopeRead}}))
			grant(user)
		},
		"access_ended": func(user uuid.UUID) {
			sessions, err := e.st.Sessions().ListByUser(ctx, user)
			must(e.t, err)
			if len(sessions) != 1 || sessions[0].RevokedAt.IsZero() {
				e.t.Errorf("sessions = %+v", sessions)
			}
			tokens, err := e.st.MCPTokens().ListByOwner(ctx, e.org.ID, domain.SubjectUser, user)
			must(e.t, err)
			if len(tokens) != 1 || tokens[0].RevokedAt.IsZero() {
				e.t.Errorf("tokens = %+v", tokens)
			}
			if !revoked() {
				e.t.Error("the OAuth grant is not revoked")
			}
			m, err := e.st.Memberships().Get(ctx, e.org.ID, user)
			must(e.t, err)
			if m.Status != domain.MembershipSuspended {
				e.t.Errorf("membership = %+v", m)
			}
		},
	}
}

func TestRegisteredFeatures(t *testing.T) {
	clock := testkit.NewClock(t0)
	f := enterprise.Load(enterprise.Deps{Store: testkit.NewStore(clock.Now), Authz: authz.New(testkit.NewStore(clock.Now), nil)})
	if f.Tools == nil || f.SignIn == nil || f.SCIM == nil || f.Run == nil {
		t.Fatalf("features = %+v", f)
	}
}
