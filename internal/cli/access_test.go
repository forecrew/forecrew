package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestRolesCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListRolesFunc = func(context.Context, domain.Actor, any) ([]domain.Role, error) {
		return []domain.Role{
			{Name: "owner", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{domain.PermOrgRead}, BuiltIn: true},
			{Name: "qa", Scope: domain.RoleScopeProject, Custom: true},
		}, nil
	}
	out, _, code := e.run(t, "roles")
	for _, want := range []string{"owner  organization  built-in  1", "qa     project       custom    0"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("code %d, output lacks %q:\n%s", code, want, out)
		}
	}
}

func TestTeamsCommands(t *testing.T) {
	e := newRemoteEnv(t)
	team := domain.Team{ID: domain.NewID(), Name: "Core", Slug: "core", Source: domain.SourceManual}
	user := domain.NewID()
	e.svc.ListTeamsFunc = func(context.Context, domain.Actor, any) ([]domain.Team, error) { return []domain.Team{team}, nil }
	e.svc.CreateTeamFunc = func(context.Context, domain.Actor, mcp.CreateTeamInput) (domain.Team, error) { return team, nil }
	e.svc.UpdateTeamFunc = func(context.Context, domain.Actor, mcp.UpdateTeamInput) (domain.Team, error) { return team, nil }
	e.svc.DeleteTeamFunc = func(context.Context, domain.Actor, mcp.TeamRefInput) (domain.Team, error) { return team, nil }
	e.svc.ListTeamMembersFunc = func(context.Context, domain.Actor, mcp.TeamRefInput) ([]domain.TeamMember, error) {
		return []domain.TeamMember{{UserID: user, Source: domain.SourceManual, CreatedAt: time.Unix(0, 0)}}, nil
	}
	member := func(context.Context, domain.Actor, mcp.TeamMemberInput) (domain.TeamMember, error) {
		return domain.TeamMember{UserID: user}, nil
	}
	e.svc.AddTeamMemberFunc, e.svc.RemoveTeamMemberFunc = member, member
	for _, tt := range []struct {
		args []string
		want string
		tool string
		in   any
	}{
		{[]string{"teams"}, "core  Core  manual", mcp.ToolListTeams, nil},
		{[]string{"teams", "create", "Core", "--slug", "core"}, "team core created", mcp.ToolCreateTeam, mcp.CreateTeamInput{Name: "Core", Slug: "core"}},
		{[]string{"teams", "update", "core", "--name", "Base", "--slug", "base"}, "team core updated", mcp.ToolUpdateTeam, mcp.UpdateTeamInput{Team: "core", Name: "Base", Slug: "base"}},
		{[]string{"teams", "delete", "core"}, "team core deleted", mcp.ToolDeleteTeam, mcp.TeamRefInput{Team: "core"}},
		{[]string{"teams", "members", "core"}, user.String() + "  manual", mcp.ToolListTeamMembers, mcp.TeamRefInput{Team: "core"}},
		{[]string{"teams", "add-member", "core", user.String()}, "is in team core", mcp.ToolAddTeamMember, mcp.TeamMemberInput{Team: "core", User: user.String()}},
		{[]string{"teams", "remove-member", "core", user.String()}, "left team core", mcp.ToolRemoveTeamMember, mcp.TeamMemberInput{Team: "core", User: user.String()}},
	} {
		e.check(t, tt.args, tt.want, tt.tool, tt.in)
	}
}

// check runs a command and checks its output and the last tool it called.
func (e *remoteEnv) check(t *testing.T, args []string, want, tool string, in any) {
	t.Helper()
	out, errOut, code := e.run(t, args...)
	if code != 0 || !strings.Contains(out, want) {
		t.Errorf("%v: code %d, stderr %q, output lacks %q:\n%s", args, code, errOut, want, out)
	}
	calls := e.svc.Calls()
	if got := calls[len(calls)-1]; got.Tool != tool || fmt.Sprint(got.Input) != fmt.Sprint(in) {
		t.Errorf("%v: called %+v", args, got)
	}
}

func TestBindingsCommands(t *testing.T) {
	e := newRemoteEnv(t)
	b := mcp.RoleBindingInfo{ID: domain.NewID(), SubjectKind: domain.SubjectTeam, SubjectID: domain.NewID(), Role: "viewer", Project: "app"}
	org := b
	org.Project = ""
	e.svc.ListRoleBindingsFunc = func(context.Context, domain.Actor, mcp.ListRoleBindingsInput) ([]mcp.RoleBindingInfo, error) {
		return []mcp.RoleBindingInfo{b, org}, nil
	}
	e.svc.CreateRoleBindingFunc = func(context.Context, domain.Actor, mcp.CreateRoleBindingInput) (mcp.RoleBindingInfo, error) {
		return b, nil
	}
	e.svc.DeleteRoleBindingFunc = func(context.Context, domain.Actor, mcp.RoleBindingRefInput) (mcp.RoleBindingInfo, error) {
		return b, nil
	}
	e.check(t, []string{"bindings", "--project", "app", "--subject-kind", "team", "--subject", "core"}, "viewer  app", mcp.ToolListRoleBindings,
		mcp.ListRoleBindingsInput{Project: "app", SubjectKind: domain.SubjectTeam, Subject: "core"})
	e.check(t, []string{"bindings"}, "organization", mcp.ToolListRoleBindings, mcp.ListRoleBindingsInput{})
	e.check(t, []string{"bindings", "add", "team", "core", "viewer", "--project", "app"}, "viewer on app", mcp.ToolCreateRoleBinding,
		mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "viewer", Project: "app"})
	e.check(t, []string{"bindings", "remove", b.ID.String()}, "removed", mcp.ToolDeleteRoleBinding, mcp.RoleBindingRefInput{Binding: b.ID.String()})
}

func TestServiceAccountsCommands(t *testing.T) {
	e := newRemoteEnv(t)
	a := domain.ServiceAccount{ID: domain.NewID(), Name: "ci", Description: "CI"}
	off := domain.ServiceAccount{ID: domain.NewID(), Name: "old", DisabledAt: time.Now()}
	e.svc.ListServiceAccountsFunc = func(context.Context, domain.Actor, any) ([]domain.ServiceAccount, error) {
		return []domain.ServiceAccount{a, off}, nil
	}
	e.svc.CreateServiceAccountFunc = func(context.Context, domain.Actor, mcp.CreateServiceAccountInput) (domain.ServiceAccount, error) {
		return a, nil
	}
	var updated mcp.UpdateServiceAccountInput
	e.svc.UpdateServiceAccountFunc = func(_ context.Context, _ domain.Actor, in mcp.UpdateServiceAccountInput) (domain.ServiceAccount, error) {
		updated = in
		return a, nil
	}
	e.svc.SetServiceAccountDisabledFunc = func(context.Context, domain.Actor, mcp.SetServiceAccountInput) (domain.ServiceAccount, error) {
		return a, nil
	}
	out, _, _ := e.run(t, "service-accounts")
	for _, want := range []string{"ci    enabled", "old   disabled", "CI"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	e.check(t, []string{"service-accounts", "create", "ci", "--description", "CI"}, "service account ci created", mcp.ToolCreateServiceAccount, mcp.CreateServiceAccountInput{Name: "ci", Description: "CI"})
	if _, errOut, code := e.run(t, "service-accounts", "update", "ci", "--name", "build", "--description", ""); code != 0 {
		t.Fatalf("update: %d %s", code, errOut)
	}
	if updated.Name != "build" || updated.Description == nil || *updated.Description != "" {
		t.Errorf("update = %+v", updated)
	}
	if _, _, code := e.run(t, "service-accounts", "update", "ci", "--name", "x"); code != 0 || updated.Description != nil {
		t.Errorf("update without a description = %+v", updated)
	}
	e.check(t, []string{"service-accounts", "disable", "ci"}, "disabled", mcp.ToolSetServiceAccount, mcp.SetServiceAccountInput{ServiceAccount: "ci", Disabled: true})
	e.check(t, []string{"service-accounts", "enable", "ci"}, "enabled", mcp.ToolSetServiceAccount, mcp.SetServiceAccountInput{ServiceAccount: "ci"})
}

func TestCouncilsListWithoutAProject(t *testing.T) {
	e := newRemoteEnv(t)
	e.check(t, []string{"councils", "list"}, "COUNCIL", mcp.ToolListCouncils, mcp.ListCouncilsInput{})
	e.check(t, []string{"councils", "list", "app", "--status", "running"}, "COUNCIL", mcp.ToolListCouncils, mcp.ListCouncilsInput{Project: "app", Status: domain.CouncilRunning})
}

func TestAccessCommandsPassOnErrors(t *testing.T) {
	e := newRemoteEnv(t)
	boom := fmt.Errorf("team.manage is needed: %w", mcp.ErrInvalidInput)
	e.svc.CreateTeamFunc = func(context.Context, domain.Actor, mcp.CreateTeamInput) (domain.Team, error) {
		return domain.Team{}, boom
	}
	if _, errOut, code := e.run(t, "teams", "create", "x"); code == 0 || !strings.Contains(errOut, "team.manage") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}
