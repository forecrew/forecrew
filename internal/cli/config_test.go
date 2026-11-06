package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func samplePlan() mcp.ConfigPlanView {
	return mcp.ConfigPlanView{ID: domain.NewID(), Title: "Add", Status: domain.ConfigPlanPending, Applies: "now", Affects: "Everyone.", Cost: "Free.", Undo: "Can be reverted.",
		Changes: []domain.ConfigChange{{Line: "+ Team core"}}, Steps: []domain.ConfigStep{{Kind: domain.ConfigStepSecret, Title: "Client secret"}, {Kind: domain.ConfigStepSecret, Title: "Other", Filled: true}}}
}

func TestConfigReadCommands(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ConfigSchemaFunc = func(context.Context, domain.Actor, mcp.ConfigKindInput) ([]mcp.ConfigSchema, error) {
		return []mcp.ConfigSchema{{Kind: "team", Title: "Team", Scope: "organization", Schema: []byte(`{"type":"object"}`)}}, nil
	}
	e.svc.ConfigGetFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigGetInput) ([]mcp.ConfigResource, error) {
		return []mcp.ConfigResource{{Kind: in.Kind, Name: in.Name, Spec: []byte(`{"a":1}`), Origin: "file"}}, nil
	}
	e.svc.ConfigExportFunc = func(context.Context, domain.Actor, any) (mcp.ConfigExport, error) {
		return mcp.ConfigExport{YAML: "version: 1\n"}, nil
	}
	e.svc.ConfigPlansFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigPlansInput) ([]mcp.ConfigPlanView, error) {
		if !in.Inbox {
			t.Error("no inbox")
		}
		return []mcp.ConfigPlanView{samplePlan()}, nil
	}
	e.svc.ConfigGetPlanFunc = func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
		return samplePlan(), nil
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"config", "schema"}, "team\torganization\tTeam"},
		{[]string{"config", "schema", "team"}, `{"type":"object"}`},
		{[]string{"config", "get", "pool", "build"}, "pool/build (file)"},
		{[]string{"config", "export"}, "version: 1"},
		{[]string{"config", "plans", "--inbox"}, "Add"},
		{[]string{"config", "show", "p"}, "Step: secret_required: Client secret (empty)"},
		{[]string{"config", "show", "p"}, "Other (saved)"},
	} {
		out, errOut, code := e.run(t, tt.args...)
		if code != 0 || !strings.Contains(out, tt.want) {
			t.Errorf("%v: code %d, %q, stderr %q", tt.args, code, out, errOut)
		}
	}
}

func TestConfigPlanCommands(t *testing.T) {
	e := newRemoteEnv(t)
	var planned mcp.ConfigPlanInput
	e.svc.ConfigPlanFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigPlanInput) (mcp.ConfigPlanView, error) {
		planned = in
		return samplePlan(), nil
	}
	e.svc.ConfigImportFunc = func(context.Context, domain.Actor, mcp.ConfigImportInput) (mcp.ConfigPlanView, error) {
		return mcp.ConfigPlanView{}, nil
	}
	e.svc.ConfigRevertFunc = func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
		return samplePlan(), nil
	}
	e.svc.ConfigCancelFunc = func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
		return samplePlan(), nil
	}
	e.svc.ConfigRequestApprovalFunc = func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigConfirmation, error) {
		return mcp.ConfigConfirmation{Message: "In the Inbox."}, nil
	}
	file := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(file, []byte("- kind: team\n  name: core\n  spec: {display_name: Core}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run(t, "config", "plan", file, "--title", "Add")
	if code != 0 || !strings.Contains(out, "+ Team core") {
		t.Fatalf("plan: %d %q %q", code, out, errOut)
	}
	if planned.Source != domain.ConfigFromCLI || planned.Title != "Add" || planned.Changes[0].Name != "core" {
		t.Errorf("planned = %+v", planned)
	}
	if out, _, code := e.runIn(t, "version: 1\n", "config", "import", "-"); code != 0 || !strings.Contains(out, "Nothing to change") {
		t.Errorf("import: %d %q", code, out)
	}
	if out, _, code := e.run(t, "config", "revert", "p"); code != 0 || !strings.Contains(out, "Plan") {
		t.Errorf("revert: %d %q", code, out)
	}
	if out, _, code := e.run(t, "config", "cancel", "p"); code != 0 || !strings.Contains(out, "pending") {
		t.Errorf("cancel: %d %q", code, out)
	}
	if out, _, code := e.run(t, "config", "request-approval", "p"); code != 0 || !strings.Contains(out, "Inbox") {
		t.Errorf("request-approval: %d %q", code, out)
	}
	if _, errOut, code := e.run(t, "config", "plan", "/does/not/exist"); code == 0 {
		t.Errorf("a missing file: %q", errOut)
	}
}

func TestConfigApplyAsks(t *testing.T) {
	e := newRemoteEnv(t)
	plan := samplePlan()
	e.svc.ConfigGetPlanFunc = func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
		return plan, nil
	}
	var got []mcp.ConfigApplyInput
	e.svc.ConfigApplyFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
		got = append(got, in)
		return mcp.ConfigApplied{Plan: plan, ShowOnce: []mcp.ConfigShowOnce{{Label: "Invitation", Value: "fci_x"}}}, nil
	}
	if _, errOut, code := e.runIn(t, "no\n", "config", "apply", "p"); code == 0 || len(got) != 0 {
		t.Fatalf("applied without yes: %d %q", code, errOut)
	}
	out, _, code := e.runIn(t, "yes\n", "config", "apply", "p")
	if code != 0 || len(got) != 1 || !strings.Contains(out, "fci_x") {
		t.Fatalf("apply: %d %q %+v", code, out, got)
	}
	plan.PRURL = "https://x"
	if out, _, code = e.runIn(t, "yes\n", "config", "apply", "p"); code != 0 || !strings.Contains(out, "https://x") {
		t.Fatalf("a pull request: %d %q", code, out)
	}
	// A plan that lowers protection is not for a terminal.
	plan.LowersProtection, plan.ConfirmWord = []string{"Removes an Owner"}, "acme"
	calls := len(got)
	out, errOut, code := e.runIn(t, "acme\n", "config", "apply", "p")
	if code == 0 || len(got) != calls || !strings.Contains(errOut, "web UI") || !strings.Contains(out, "Lowers protection: Removes an Owner") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

func TestConfigSecretReadsStdin(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.ConfigSecretInput
	e.svc.ConfigPutSecretFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigSecretInput) (mcp.ConfigPlanView, error) {
		got = in
		return mcp.ConfigPlanView{}, nil
	}
	if out, errOut, code := e.runIn(t, "hunter2\n", "config", "secret", "p", "conn-okta"); code != 0 || strings.Contains(out, "hunter2") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if got != (mcp.ConfigSecretInput{Plan: "p", Secret: "conn-okta", Value: "hunter2"}) {
		t.Errorf("input = %+v", got)
	}
}

func TestAssistantCommands(t *testing.T) {
	e := newRemoteEnv(t)
	id := domain.NewID()
	var sent mcp.AssistantSendInput
	e.svc.AssistantSendFunc = func(_ context.Context, _ domain.Actor, in mcp.AssistantSendInput) (mcp.AssistantView, error) {
		sent = in
		return mcp.AssistantView{ID: id}, nil
	}
	e.svc.AssistantGetFunc = func(context.Context, domain.Actor, mcp.AssistantRefInput) (mcp.AssistantView, error) {
		return mcp.AssistantView{Messages: []domain.AssistantMessage{{Author: domain.AssistantReply, Body: "Hello"}}, Plans: []mcp.ConfigPlanView{samplePlan()}, Thinking: true}, nil
	}
	e.svc.AssistantListFunc = func(context.Context, domain.Actor, mcp.AssistantListInput) ([]domain.AssistantConversation, error) {
		return []domain.AssistantConversation{{ID: id, Title: "Add a machine"}}, nil
	}
	out, errOut, code := e.run(t, "assistant", "add a machine", "--project", "app")
	if code != 0 || !strings.Contains(out, id.String()) || sent.Project != "app" {
		t.Fatalf("%d %q %q %+v", code, out, errOut, sent)
	}
	if out, _, code := e.run(t, "assistant", "show", id.String()); code != 0 || !strings.Contains(out, "assistant: Hello") || !strings.Contains(out, "Thinking") || !strings.Contains(out, "+ Team core") {
		t.Errorf("show: %d %q", code, out)
	}
	if out, _, code := e.run(t, "assistant", "list"); code != 0 || !strings.Contains(out, "Add a machine") {
		t.Errorf("list: %d %q", code, out)
	}
	e.svc.AssistantSendFunc = func(context.Context, domain.Actor, mcp.AssistantSendInput) (mcp.AssistantView, error) {
		return mcp.AssistantView{}, errors.Join(mcp.ErrInvalidInput)
	}
	if _, _, code := e.run(t, "assistant", "x"); code == 0 {
		t.Error("an error did not fail the command")
	}
}
