package cli

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestMembersCommands(t *testing.T) {
	e := newRemoteEnv(t)
	marta := mcp.MemberInfo{UserID: domain.NewID(), Name: "Marta Rossi\x1b[2J", Email: "marta@acme.test", Status: domain.MembershipActive,
		Roles: []mcp.MemberRole{{Role: "member"}, {Role: "maintainer", Project: "app"}}, Teams: []string{"mobile"}}
	luca := mcp.MemberInfo{UserID: domain.NewID(), Name: "Luca", Status: domain.MembershipSuspended}
	twin := mcp.MemberInfo{UserID: domain.NewID(), Name: "Luca"}
	e.svc.ListMembersFunc = func(context.Context, domain.Actor, any) ([]mcp.MemberInfo, error) {
		return []mcp.MemberInfo{marta, luca, twin}, nil
	}
	e.svc.SetMemberSuspendedFunc = func(_ context.Context, _ domain.Actor, in mcp.SetMemberInput) (mcp.MemberInfo, error) {
		return marta, nil
	}
	e.svc.RemoveMemberFunc = func(context.Context, domain.Actor, mcp.MemberRefInput) (mcp.MemberInfo, error) { return marta, nil }
	e.svc.UpdateOrganizationFunc = func(_ context.Context, _ domain.Actor, in mcp.UpdateOrganizationInput) (domain.Account, error) {
		return domain.Account{Name: in.Name, Slug: in.Slug}, nil
	}

	out, _, code := e.run(t, "members")
	for _, want := range []string{"Marta Rossi [2J", "marta@acme.test", "member,maintainer@app", "mobile", "suspended"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("code %d, output lacks %q:\n%s", code, want, out)
		}
	}
	e.check(t, []string{"members", "suspend", "marta@acme.test"}, "is suspended", mcp.ToolSetMemberSuspended, mcp.SetMemberInput{User: marta.UserID.String(), Suspended: true})
	e.check(t, []string{"members", "resume", marta.UserID.String()}, "is active again", mcp.ToolSetMemberSuspended, mcp.SetMemberInput{User: marta.UserID.String()})
	e.check(t, []string{"members", "remove", "marta rossi\x1b[2j"}, "was removed", mcp.ToolRemoveMember, mcp.MemberRefInput{User: marta.UserID.String()})
	e.check(t, []string{"org", "update", "--name", "Acme", "--slug", "acme"}, "organization Acme updated (slug acme)", mcp.ToolUpdateOrganization, mcp.UpdateOrganizationInput{Name: "Acme", Slug: "acme"})

	for ref, want := range map[string]string{"ghost": `no member "ghost"`, "luca": `2 members are called "luca"`, uuid.New().String(): "no member"} {
		if _, errOut, code := e.run(t, "members", "remove", ref); code == 0 || !strings.Contains(errOut, want) {
			t.Errorf("%s: code %d, stderr %q, want %q", ref, code, errOut, want)
		}
	}
}

func TestAuditCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListAuditFunc = func(context.Context, domain.Actor, mcp.ListAuditInput) ([]mcp.AuditInfo, error) {
		return []mcp.AuditInfo{{Actor: "user:x", Action: "team.created", SubjectType: "team\x1b[2J", SubjectID: domain.NewID(), ActorName: "Ana"}}, nil
	}
	e.check(t, []string{"audit", "--actor", "user:x", "--action", "team.created", "--subject-type", "team", "--subject", "s", "--since", "a", "--until", "b", "--limit", "5"}, "Ana    team.created  team [2J",
		mcp.ToolListAudit, mcp.ListAuditInput{Actor: "user:x", Action: "team.created", SubjectType: "team", Subject: "s", Since: "a", Until: "b", Limit: 5})
}

func TestConnectionsCommands(t *testing.T) {
	e := newRemoteEnv(t)
	okta := mcp.ConnectionInfo{Name: "okta", Kind: domain.ConnectionOIDC, Issuer: "https://acme.okta.com", Enabled: true, Tested: true,
		RedirectURL: "https://f.example/auth/1/callback", TestURL: "https://f.example/auth/1/login?test=1"}
	gh := mcp.ConnectionInfo{Name: "github", Kind: domain.ConnectionGitHub, Instance: true}
	e.svc.ListConnectionsFunc = func(context.Context, domain.Actor, any) ([]mcp.ConnectionInfo, error) {
		return []mcp.ConnectionInfo{gh, okta}, nil
	}
	e.svc.CreateConnectionFunc = func(context.Context, domain.Actor, mcp.CreateConnectionInput) (mcp.ConnectionInfo, error) {
		return okta, nil
	}
	e.svc.SetConnectionFunc = func(_ context.Context, _ domain.Actor, in mcp.SetConnectionInput) (mcp.ConnectionInfo, error) {
		okta.Enabled = in.Enabled
		return okta, nil
	}
	out, _, code := e.run(t, "connections")
	for _, want := range []string{"github  github  -", "server file", "okta    oidc    https://acme.okta.com  on     yes     organization"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("code %d, output lacks %q:\n%s", code, want, out)
		}
	}
	e.check(t, []string{"connections", "add", "okta", "--issuer", "https://acme.okta.com", "--client-id", "c", "--secret-ref", "env:FORECREW_CONN_OKTA"},
		"login?test=1", mcp.ToolCreateConnection, mcp.CreateConnectionInput{Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://acme.okta.com", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_OKTA"})
	e.check(t, []string{"connections", "enable", "okta"}, "connection okta is on", mcp.ToolSetConnection, mcp.SetConnectionInput{Connection: "okta", Enabled: true})
	e.check(t, []string{"connections", "disable", "okta"}, "connection okta is off", mcp.ToolSetConnection, mcp.SetConnectionInput{Connection: "okta"})
}
