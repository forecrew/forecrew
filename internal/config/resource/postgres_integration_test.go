//go:build integration

package resource_test

import (
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestPrivateCodePolicyOnPostgres: confirming a plan that puts a project's
// role on a connection records the authorization, the job's decision follows
// it, and revoking it stops the role's next job.
func TestPrivateCodePolicyOnPostgres(t *testing.T) {
	ctx := t.Context()
	pool := testkit.NewDatabase(t)
	check(t, postgres.Migrate(ctx, pool))
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(ctx)
	check(t, err)
	account := accounts[0]
	project := domain.Project{AccountID: account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	check(t, st.Projects().Create(ctx, &project))
	u := domain.User{Name: "Ana", Email: "ana@example.com"}
	check(t, st.Users().Create(ctx, &u))
	check(t, st.Memberships().Create(ctx, &domain.Membership{AccountID: account.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	testkit.Bind(t, st, account.ID, domain.SubjectUser, u.ID, domain.RoleScopeOrganization, domain.RoleOwner, uuid.Nil())
	owner := domain.UserActor(account.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})

	vault, err := secrets.New(st.Secrets(), make([]byte, secrets.KeySize))
	check(t, err)
	server := &config.Config{}
	engine := resource.NewEngine(resource.Config{Store: st, Server: server, Secrets: vault, Getenv: env})
	plan := func(changes ...mcp.ConfigWanted) domain.ConfigPlan {
		t.Helper()
		p, err := engine.Plan(ctx, owner, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: changes})
		check(t, err)
		return p
	}
	apply := func(p domain.ConfigPlan) {
		t.Helper()
		_, err := engine.Apply(ctx, owner, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: len(p.LowersProtection) > 0})
		check(t, err)
	}
	check(t, vault.Put(ctx, owner, account.ID, "models/cloud", []byte("k")))
	apply(plan(wanted(t, "model_connection", "cloud", cloudConn("store:models/cloud"))))

	svc := models.New(st, server, vault, nil)
	decide := func() models.Decision {
		t.Helper()
		d, err := svc.Decide(ctx, account.ID, models.Request{ProjectID: project.ID, Role: "developer"})
		check(t, err)
		return d
	}
	onCloud := func(roles ...string) map[string]any {
		a := assignment("cloud", roles...)
		a["tiers"] = map[string]any{"strong": map[string]any{"anthropic": "claude-sonnet-4-5"}}
		return a
	}
	apply(plan(wanted(t, "model_assignment", "organization", onCloud())))
	if d := decide(); d.Code != models.ReasonPrivateCode {
		t.Fatalf("before the authorization: %+v", d)
	}

	p := plan(wanted(t, "model_assignment", "project:app", onCloud("architect")))
	apply(p)
	rows, err := st.ModelAuthorizations().List(ctx, account.ID)
	if err != nil || len(rows) != 1 || rows[0].ProjectID != project.ID || rows[0].AuthorizedBy != owner.String() {
		t.Fatalf("authorizations = %+v, %v", rows, err)
	}
	if d := decide(); d.Waiting != "" || d.Connection != "cloud" {
		t.Fatalf("authorized: %+v", d)
	}
	audit, err := st.Audit().List(ctx, account.ID, store.AuditFilter{Action: "model_authorization.created"})
	if err != nil || len(audit) != 1 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}

	apply(plan(mcp.ConfigWanted{Kind: "model_authorization", Name: "cloud@app", Delete: true}))
	if d := decide(); d.Code != models.ReasonPrivateCode {
		t.Fatalf("after revoking: %+v", d)
	}
}
