package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestPermissionCatalog(t *testing.T) {
	all := Permissions()
	if !slices.IsSorted(all) || len(slices.Compact(slices.Clone(all))) != len(all) {
		t.Fatalf("Permissions() not sorted and unique: %v", all)
	}
	for _, p := range all {
		if !p.Valid() || p.Scope() == "" || !strings.Contains(string(p), ".") {
			t.Errorf("%s: valid %v, scope %q", p, p.Valid(), p.Scope())
		}
	}
	if Permission("goal.delete").Valid() || Permission("goal.delete").Scope() != "" {
		t.Error("unknown permission accepted")
	}
	if PermOrgManage.ProjectScoped() || !PermGateApproveMerge.ProjectScoped() {
		t.Error("project scoping wrong")
	}
}

func TestGatePermissionCoversEveryKind(t *testing.T) {
	seen := map[Permission]bool{}
	for _, k := range GateKinds() {
		p := GatePermission(k, false)
		if k == GateTicketIntake {
			if p != PermGoalCreate {
				t.Errorf("%s: %q, accepting a ticket creates a goal", k, p)
			}
			continue
		}
		if !p.Valid() || !strings.HasPrefix(string(p), "gate.approve.") || seen[p] {
			t.Errorf("%s: %q", k, p)
		}
		seen[p] = true
	}
	if GatePermission(GateKnowledgeProposal, true) != PermGateApproveKnowledgeProtected {
		t.Error("protected knowledge proposal")
	}
	if GatePermission(GateMergeApproval, true) != PermGateApproveMerge {
		t.Error("protected only matters for knowledge")
	}
	if GatePermission("other", false) != "" {
		t.Error("unknown kind")
	}
	// Every gate.approve.* permission belongs to a gate kind.
	seen[PermGateApproveKnowledgeProtected] = true
	for _, p := range Permissions() {
		if strings.HasPrefix(string(p), "gate.approve.") && !seen[p] {
			t.Errorf("%s has no gate kind", p)
		}
	}
}

func TestScopes(t *testing.T) {
	tests := []struct {
		scopes []Scope
		perm   Permission
		want   bool
	}{
		{[]Scope{ScopeRead}, PermGoalRead, true},
		{[]Scope{ScopeRead}, PermGoalCreate, false},
		{[]Scope{ScopeWrite}, PermGoalRead, true},
		{[]Scope{ScopeWrite}, PermGateApproveMerge, true},
		{[]Scope{ScopeWrite}, PermMemberManage, false},
		{[]Scope{ScopeRead, ScopeAdmin}, PermMemberManage, true},
		{nil, PermGoalRead, false},
		{[]Scope{"root"}, PermGoalRead, false},
		{[]Scope{ScopeAdmin}, "goal.delete", false},
	}
	for _, tt := range tests {
		if got := ScopesAllow(tt.scopes, tt.perm); got != tt.want {
			t.Errorf("ScopesAllow(%v, %s) = %v", tt.scopes, tt.perm, got)
		}
	}
	if Scope("x").Includes(ScopeRead) || ScopeRead.Includes("x") || !ScopeAdmin.Includes(ScopeWrite) || ScopeWrite.Includes(ScopeAdmin) {
		t.Error("Includes")
	}
}

func TestBuiltInRoles(t *testing.T) {
	roles := BuiltInRoles()
	if len(roles) != 9 {
		t.Fatalf("%d built-in roles", len(roles))
	}
	for _, r := range roles {
		if err := r.Validate(); err != nil {
			t.Error(err)
		}
		if !slices.IsSorted(r.Permissions) {
			t.Errorf("%s permissions not sorted", r.Name)
		}
	}
	get := func(scope RoleScope, name string) Role {
		r, ok := BuiltInRole(scope, name)
		if !ok {
			t.Fatalf("no built-in %s role %s", scope, name)
		}
		return r
	}
	org, proj := RoleScopeOrganization, RoleScopeProject
	tests := []struct {
		role    Role
		allowed []Permission
		refused []Permission
	}{
		{get(org, RoleOwner), Permissions(), nil},
		{get(org, RoleAdmin), []Permission{PermMemberManage, PermConnectionManage, PermGateApproveMerge, PermProjectCreate}, []Permission{PermOrgManage, PermBillingManage}},
		{get(org, RoleBilling), []Permission{PermBillingManage, PermOrgRead}, []Permission{PermGoalRead, PermMemberManage}},
		{get(org, RoleMember), []Permission{PermGoalRead, PermGoalCreate, PermKnowledgePropose}, []Permission{PermGateApproveSpec, PermJobDispatch, PermProjectCreate, PermAuditRead}},
		{get(org, RoleViewer), []Permission{PermOrgRead, PermGoalRead, PermJobRead, PermGateRead}, []Permission{PermGoalCreate, PermAuditRead}},
		{get(proj, RoleProjectAdmin), []Permission{PermProjectManage, PermGateApproveMerge}, []Permission{PermMemberManage, PermOrgRead}},
		{get(proj, RoleMaintainer), []Permission{PermGateApproveMerge, PermGateApproveKnowledgeProtected, PermProposalDecide}, []Permission{PermProjectManage}},
		{get(proj, RoleContributor), []Permission{PermGateApproveSpec, PermGoalCreate, PermJobDispatch}, []Permission{PermGateApproveMerge, PermGateApproveProfile, PermGateApproveKnowledgeProtected, PermProjectManage}},
		{get(proj, RoleViewer), []Permission{PermProjectRead, PermGateRead}, []Permission{PermGoalCreate, PermGateApproveSpec}},
	}
	for _, tt := range tests {
		for _, p := range tt.allowed {
			if !tt.role.Has(p) {
				t.Errorf("%s %s lacks %s", tt.role.Scope, tt.role.Name, p)
			}
		}
		for _, p := range tt.refused {
			if tt.role.Has(p) {
				t.Errorf("%s %s has %s", tt.role.Scope, tt.role.Name, p)
			}
		}
	}
	if _, ok := BuiltInRole(proj, RoleOwner); ok {
		t.Error("owner is not a project role")
	}
}

// TestViewerRolesOnlyRead: both Viewer roles hold read-class permissions only.
func TestViewerRolesOnlyRead(t *testing.T) {
	for _, scope := range RoleScopes() {
		r, ok := BuiltInRole(scope, "viewer")
		if !ok {
			t.Fatalf("no %s viewer", scope)
		}
		for _, p := range r.Permissions {
			if p.Scope() != ScopeRead {
				t.Errorf("%s viewer holds %s, of class %s", scope, p, p.Scope())
			}
		}
	}
}

func TestRoleValidate(t *testing.T) {
	acc := NewID()
	custom := Role{AccountID: acc, Name: "release", Scope: RoleScopeProject, Permissions: []Permission{PermGateApproveMerge}, Custom: true}
	if err := custom.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(r *Role){
		"bad name":            func(r *Role) { r.Name = "Release Managers" },
		"bad scope":           func(r *Role) { r.Scope = "global" },
		"no permissions":      func(r *Role) { r.Permissions = nil },
		"unknown permission":  func(r *Role) { r.Permissions = []Permission{"goal.delete"} },
		"twice":               func(r *Role) { r.Permissions = []Permission{PermGoalRead, PermGoalRead} },
		"org-only on project": func(r *Role) { r.Permissions = []Permission{PermMemberManage} },
		"custom and built in": func(r *Role) { r.BuiltIn = true },
		"custom without org":  func(r *Role) { r.AccountID = uuid.Nil() },
		"built in with org":   func(r *Role) { r.Custom, r.BuiltIn = false, true },
	}
	for name, mutate := range tests {
		r := custom
		r.Permissions = slices.Clone(custom.Permissions)
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	org := custom
	org.Scope, org.Permissions = RoleScopeOrganization, []Permission{PermMemberManage}
	if err := org.Validate(); err != nil {
		t.Errorf("organization role: %v", err)
	}
}

func TestRoleBindingValidateFor(t *testing.T) {
	acc, project := NewID(), NewID()
	owner, _ := BuiltInRole(RoleScopeOrganization, RoleOwner)
	owner.ID = NewID()
	maint, _ := BuiltInRole(RoleScopeProject, RoleMaintainer)
	maint.ID = NewID()
	foreign := Role{ID: NewID(), AccountID: NewID(), Name: "x", Scope: RoleScopeProject, Custom: true}
	user := RoleBinding{AccountID: acc, SubjectKind: SubjectUser, SubjectID: NewID()}
	with := func(b RoleBinding, f func(*RoleBinding)) RoleBinding { f(&b); return b }
	tests := []struct {
		name string
		b    RoleBinding
		role Role
		ok   bool
	}{
		{"owner user", with(user, func(b *RoleBinding) { b.RoleID = owner.ID }), owner, true},
		{"owner team", with(user, func(b *RoleBinding) { b.RoleID, b.SubjectKind = owner.ID, SubjectTeam }), owner, false},
		{"owner on project", with(user, func(b *RoleBinding) { b.RoleID, b.ProjectID = owner.ID, project }), owner, false},
		{"maintainer on project", with(user, func(b *RoleBinding) { b.RoleID, b.ProjectID, b.SubjectKind = maint.ID, project, SubjectServiceAccount }), maint, true},
		{"maintainer on organization", with(user, func(b *RoleBinding) { b.RoleID, b.SubjectKind = maint.ID, SubjectTeam }), maint, true},
		{"other role ID", with(user, func(b *RoleBinding) { b.RoleID = NewID() }), maint, false},
		{"foreign custom role", with(user, func(b *RoleBinding) { b.RoleID = foreign.ID }), foreign, false},
		{"bad subject kind", with(user, func(b *RoleBinding) { b.RoleID, b.SubjectKind = maint.ID, "group" }), maint, false},
		{"no subject", with(user, func(b *RoleBinding) { b.RoleID, b.SubjectID = maint.ID, uuid.Nil() }), maint, false},
	}
	for _, tt := range tests {
		err := tt.b.ValidateFor(tt.role)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrInvalidRecord)) {
			t.Errorf("%s: err = %v", tt.name, err)
		}
	}
}

func TestSecretRef(t *testing.T) {
	tests := map[string]bool{
		"env:FORECREW_GOOGLE_CLIENT_SECRET": true,
		"env:_X":                            true,
		"secret:forecrew/okta-client":       true,
		"env:lower":                         false,
		"env:":                              false,
		"env:1ABC":                          false,
		"secret:":                           false,
		"secret:../etc/passwd":              false,
		"secret:a/../b":                     false,
		"secret:Upper":                      false,
		"GOCSPX-1a2b3c4d5e6f7g8h9i0j":       false, // a secret value
		"vault:path":                        false,
		"":                                  false,
		"env:" + strings.Repeat("A", 129):   false,
	}
	for ref, want := range tests {
		if got := ValidSecretRef(ref); got != want {
			t.Errorf("ValidSecretRef(%q) = %v", ref, got)
		}
	}
}

func TestOrganizationSecretRef(t *testing.T) {
	tests := map[string]bool{
		"env:FORECREW_CONN_OKTA":    true,
		"secret:conn-okta":          true,
		"secret:conn-acme/okta":     true,
		"env:FORECREW_CONN_":        false,
		"secret:conn-":              false,
		"env:FORECREW_DATABASE_URL": false, // an instance secret
		"env:FORECREW_CONNX":        false,
		"env:PATH":                  false,
		"secret:forecrew-google":    false,
		"secret:connection":         false,
		"secret:conn-a/../../b":     false,
		"store:okta":                true,
		"store:../okta":             false,
		"env:FORECREW_CONN_lower":   false,
		"env:FORECREW_CONN_" + strings.Repeat("A", 114): true,
		"env:FORECREW_CONN_" + strings.Repeat("A", 115): false,
		"secret:conn-" + strings.Repeat("a", 123):       true,
		"secret:conn-" + strings.Repeat("a", 124):       false,
	}
	for ref, want := range tests {
		if got := ValidOrganizationSecretRef(ref); got != want {
			t.Errorf("ValidOrganizationSecretRef(%q) = %v", ref, got)
		}
	}
}

func TestConnectionValidate(t *testing.T) {
	acc := NewID()
	ok := []Connection{
		{AccountID: acc, Kind: ConnectionOIDC, Name: "okta", Issuer: "https://acme.okta.com", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_OKTA", JIT: JIT{Enabled: true, DefaultRoles: []uuid.UUID{NewID()}}},
		{Kind: ConnectionGitHub, Name: "github", ClientID: "Iv1.x", ClientSecretRef: "env:FORECREW_GITHUB_CLIENT_SECRET"},
		{Kind: ConnectionGoogle, Name: "google", ClientID: "x.apps.googleusercontent.com", ClientSecretRef: "secret:google"},
		{Kind: ConnectionGoogle, Name: "google", Issuer: GoogleIssuer, ClientID: "x", ClientSecretRef: "secret:google"},
		{AccountID: acc, Kind: ConnectionMicrosoft, Name: "entra", Issuer: "https://login.microsoftonline.com/tenant/v2.0", ClientID: "x", ClientSecretRef: "secret:conn-entra"},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", c.Name, err)
		}
	}
	base := ok[0]
	bad := map[string]func(c *Connection){
		"secret value":          func(c *Connection) { c.ClientSecretRef = "s3cr3t-value" },
		"empty secret":          func(c *Connection) { c.ClientSecretRef = "" },
		"instance env secret":   func(c *Connection) { c.ClientSecretRef = "env:FORECREW_DATABASE_URL" },
		"instance file secret":  func(c *Connection) { c.ClientSecretRef = "secret:forecrew-google" },
		"kind":                  func(c *Connection) { c.Kind = "saml" },
		"name":                  func(c *Connection) { c.Name = "Okta SSO" },
		"client id":             func(c *Connection) { c.ClientID = " " },
		"http issuer":           func(c *Connection) { c.Issuer = "http://acme.okta.com" },
		"no issuer":             func(c *Connection) { c.Issuer = "" },
		"github issuer":         func(c *Connection) { c.Kind = ConnectionGitHub },
		"google issuer":         func(c *Connection) { c.Kind = ConnectionGoogle },
		"jit on instance":       func(c *Connection) { c.AccountID = uuid.Nil() },
		"jit roles on instance": func(c *Connection) { c.AccountID, c.JIT.Enabled = uuid.Nil(), false },
	}
	for name, mutate := range bad {
		c := base
		mutate(&c)
		err := c.Validate()
		if !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: err = %v", name, err)
		}
		if name == "secret value" && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("the error repeats the secret: %v", err)
		}
	}
}

func TestSlugCandidate(t *testing.T) {
	long := strings.Repeat("a", 63)
	dashed := strings.Repeat("a", 60) + "-bc"
	tests := []struct {
		base string
		n    int
		want string
	}{
		{"acme", 1, "acme"},
		{"acme", 2, "acme-2"},
		{"acme", 12, "acme-12"},
		{long, 1, long},
		{long, 2, strings.Repeat("a", 61) + "-2"},
		{long, 100, strings.Repeat("a", 59) + "-100"},
		{dashed, 2, strings.Repeat("a", 60) + "-2"},
	}
	for _, tt := range tests {
		got := SlugCandidate(tt.base, tt.n)
		if got != tt.want || !ValidSlug(got) {
			t.Errorf("SlugCandidate(%q, %d) = %q, want %q", tt.base, tt.n, got, tt.want)
		}
	}
}

func TestSlugs(t *testing.T) {
	for s, want := range map[string]bool{"acme": true, "a": true, "acme-labs-2": true, "-acme": false, "acme-": false, "Acme": false, "acme_labs": false, "": false, strings.Repeat("a", 64): false, strings.Repeat("a", 63): true} {
		if ValidSlug(s) != want {
			t.Errorf("ValidSlug(%q) != %v", s, want)
		}
	}
	for name, want := range map[string]string{"Acme Labs": "acme-labs", "  ¡Hola, Mundo!  ": "hola-mundo", "default": "default", "***": "org", strings.Repeat("ab-", 30): strings.TrimRight(strings.Repeat("ab-", 21), "-")} {
		got := Slugify(name)
		if got != want || !ValidSlug(got) {
			t.Errorf("Slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInvitation(t *testing.T) {
	inv := Invitation{AccountID: NewID(), Email: "ana@example.com", TokenHash: make([]byte, 32), ExpiresAt: now.Add(time.Hour), Grants: []Grant{{RoleID: NewID()}}}
	if err := inv.Validate(); err != nil {
		t.Fatal(err)
	}
	if !inv.Pending(now) || inv.Pending(now.Add(2*time.Hour)) {
		t.Error("Pending by expiry")
	}
	accepted := inv
	accepted.AcceptedAt = now
	revoked := inv
	revoked.RevokedAt = now
	if accepted.Pending(now) || revoked.Pending(now) {
		t.Error("Pending after acceptance or revocation")
	}
	bad := map[string]func(i *Invitation){
		"both":      func(i *Invitation) { i.GitHubLogin = "ana" },
		"neither":   func(i *Invitation) { i.Email = "" },
		"email":     func(i *Invitation) { i.Email = "ana" },
		"hash":      func(i *Invitation) { i.TokenHash = []byte("token") },
		"no expiry": func(i *Invitation) { i.ExpiresAt = time.Time{} },
		"role":      func(i *Invitation) { i.Grants = []Grant{{}} },
	}
	for name, mutate := range bad {
		i := inv
		mutate(&i)
		if err := i.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSessionAndTokenUse(t *testing.T) {
	s := Session{IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour)}
	if !s.Active(now) || s.Active(now.Add(time.Hour)) {
		t.Error("idle expiry")
	}
	s.IdleExpiresAt = now.Add(3 * time.Hour)
	if s.Active(now.Add(2 * time.Hour)) {
		t.Error("absolute expiry")
	}
	s.RevokedAt = now
	if s.Active(now) {
		t.Error("revoked session active")
	}

	tok := MCPToken{Name: "laptop", OwnerKind: SubjectUser, OwnerID: NewID(), Scopes: []Scope{ScopeRead}, TokenHash: make([]byte, 32), ExpiresAt: now.Add(time.Hour)}
	if err := tok.Validate(); err != nil {
		t.Fatal(err)
	}
	if !tok.Usable(now) || tok.Usable(now.Add(time.Hour)) {
		t.Error("token expiry")
	}
	legacy := MCPToken{Name: "old", Scopes: []Scope{ScopeAdmin}, TokenHash: make([]byte, 32)}
	if err := legacy.Validate(); err != nil || !legacy.Usable(now.Add(1000*time.Hour)) {
		t.Errorf("legacy token: %v", err)
	}
	legacy.RevokedAt = now
	if legacy.Usable(now) {
		t.Error("revoked token usable")
	}
	bad := map[string]func(t *MCPToken){
		"owner kind only": func(t *MCPToken) { t.OwnerID = uuid.Nil() },
		"owner id only":   func(t *MCPToken) { t.OwnerKind = "" },
		"team owner":      func(t *MCPToken) { t.OwnerKind = SubjectTeam },
		"no scopes":       func(t *MCPToken) { t.Scopes = nil },
		"bad scope":       func(t *MCPToken) { t.Scopes = []Scope{"root"} },
		"hash":            func(t *MCPToken) { t.TokenHash = nil },
	}
	for name, mutate := range bad {
		c := tok
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestActors(t *testing.T) {
	acc, user, sa, tok := NewID(), NewID(), NewID(), NewID()
	u := UserActor(acc, user, Credential{Kind: CredentialSession, ID: tok})
	if u.String() != "user:"+user.String() || u.Credential.String() != "session:"+tok.String() {
		t.Fatalf("user actor %s, credential %s", u, u.Credential)
	}
	if id, ok := u.SubjectID(); !ok || id != user {
		t.Error("user SubjectID")
	}
	s := ServiceAccountActor(acc, sa, tok)
	if s.String() != "service_account:"+sa.String() || s.Credential.String() != "token:"+tok.String() {
		t.Fatalf("service account actor %s, credential %s", s, s.Credential)
	}
	if (Credential{}).String() != "" {
		t.Error("zero credential")
	}
	if _, ok := SystemActor(acc, ForecrewdName).SubjectID(); ok {
		t.Error("system actor has a subject")
	}
	if _, ok := (Actor{Kind: ActorUser, Name: "gianmaria"}).SubjectID(); ok {
		t.Error("legacy user actor has a subject")
	}
	for _, in := range []string{u.String(), s.String(), "mcp:laptop", "user:gianmaria", "github:octocat", "worker:box", "system:forecrewd"} {
		a, err := ParseActor(in)
		if err != nil || a.String() != in {
			t.Errorf("ParseActor(%q) = %v, %v", in, a, err)
		}
	}
	for _, in := range []string{"", "mcp", "mcp:", "robot:x", "service_account:ci"} {
		if _, err := ParseActor(in); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("ParseActor(%q) err = %v", in, err)
		}
	}
	if len(ActorKinds()) != 6 || len(CredentialKinds()) != 3 || len(SubjectKinds()) != 3 || len(OAuthGrantKinds()) != 3 ||
		len(MembershipStatuses()) != 3 || len(MembershipSources()) != 4 || len(TeamSources()) != 3 || len(ConnectionKinds()) != 4 {
		t.Error("enum lists changed")
	}
}

func TestScopesOrLegacy(t *testing.T) {
	if got := (MCPToken{}).ScopesOrLegacy(); !slices.Equal(got, []Scope{ScopeAdmin}) {
		t.Errorf("legacy: %v", got)
	}
	if got := (MCPToken{OwnerKind: SubjectUser}).ScopesOrLegacy(); got != nil {
		t.Errorf("owned without scopes: %v", got)
	}
	if got := (MCPToken{Scopes: []Scope{ScopeRead}}).ScopesOrLegacy(); !slices.Equal(got, []Scope{ScopeRead}) {
		t.Errorf("with scopes: %v", got)
	}
}

func TestMaxTokenTTLJSON(t *testing.T) {
	plain, err := json.Marshal(Account{Name: "a"})
	if err != nil || strings.Contains(string(plain), "max_token_ttl") {
		t.Fatalf("no cap: %s, %v", plain, err)
	}
	capped, err := json.Marshal(Account{Name: "a", MaxTokenTTL: Duration(DefaultSSOTokenTTL)})
	if err != nil || !strings.Contains(string(capped), `"max_token_ttl":"168h0m0s"`) {
		t.Fatalf("cap: %s, %v", capped, err)
	}
}
