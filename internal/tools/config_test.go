package tools_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tools"
)

func configFixture(t *testing.T) (*fixture, domain.Actor) {
	t.Helper()
	f := newFixture(t)
	by, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	return f, by
}

func teamChange(name string) []mcp.ConfigWanted {
	return []mcp.ConfigWanted{{Kind: "team", Name: name, Spec: json.RawMessage(`{"display_name":"Core","members":[]}`)}}
}

func TestConfigTools(t *testing.T) {
	f, by := configFixture(t)
	ctx := t.Context()
	schemas, err := f.svc.ConfigSchema(ctx, by, mcp.ConfigKindInput{Kind: "team"})
	must(t, err)
	if len(schemas) != 1 {
		t.Fatalf("schemas = %+v", schemas)
	}
	p, err := f.svc.ConfigPlan(ctx, by, mcp.ConfigPlanInput{Title: "Add", Request: "add a team", Changes: teamChange("core")})
	must(t, err)
	if p.Source != domain.ConfigFromForm || !p.CanConfirm {
		t.Fatalf("plan = %+v", p)
	}
	got, err := f.svc.ConfigGetPlan(ctx, by, mcp.ConfigPlanRefInput{Plan: p.ID.String()})
	if err != nil || got.ID != p.ID {
		t.Fatalf("ConfigGetPlan = %+v, %v", got, err)
	}
	list, err := f.svc.ConfigPlans(ctx, by, mcp.ConfigPlansInput{})
	if err != nil || len(list) != 1 {
		t.Fatalf("ConfigPlans = %+v, %v", list, err)
	}
	applied, err := f.svc.ConfigApply(ctx, by, mcp.ConfigApplyInput{Plan: p.ID.String()})
	must(t, err)
	if applied.Plan.Status != domain.ConfigPlanApplied {
		t.Fatalf("applied = %+v", applied)
	}
	rs, err := f.svc.ConfigGet(ctx, by, mcp.ConfigGetInput{Kind: "team", Name: "core"})
	if err != nil || len(rs) != 1 {
		t.Fatalf("ConfigGet = %+v, %v", rs, err)
	}
	e, err := f.svc.ConfigExport(ctx, by)
	must(t, err)
	if !strings.Contains(e.YAML, "core") {
		t.Fatalf("export = %s", e.YAML)
	}
	same, err := f.svc.ConfigImport(ctx, by, mcp.ConfigImportInput{YAML: e.YAML})
	must(t, err)
	if len(same.Changes) != 0 || same.ID != uuid.Nil() {
		t.Fatalf("import = %+v", same)
	}
	r, err := f.svc.ConfigRevert(ctx, by, mcp.ConfigPlanRefInput{Plan: p.ID.String()})
	must(t, err)
	if r.Reverts != p.ID {
		t.Fatalf("revert = %+v", r)
	}
	c, err := f.svc.ConfigCancel(ctx, by, mcp.ConfigPlanRefInput{Plan: r.ID.String()})
	if err != nil || c.Status != domain.ConfigPlanCanceled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	for name, call := range map[string]func() error{
		"apply": func() error { _, err := f.svc.ConfigApply(ctx, by, mcp.ConfigApplyInput{Plan: "x"}); return err },
		"ask": func() error {
			_, err := f.svc.ConfigRequestApproval(ctx, by, mcp.ConfigPlanRefInput{Plan: "x"})
			return err
		},
		"revert": func() error { _, err := f.svc.ConfigRevert(ctx, by, mcp.ConfigPlanRefInput{Plan: "x"}); return err },
		"get":    func() error { _, err := f.svc.ConfigGetPlan(ctx, by, mcp.ConfigPlanRefInput{Plan: "x"}); return err },
		"cancel": func() error { _, err := f.svc.ConfigCancel(ctx, by, mcp.ConfigPlanRefInput{Plan: "x"}); return err },
		"secret": func() error { _, err := f.svc.ConfigPutSecret(ctx, by, mcp.ConfigSecretInput{Plan: "x"}); return err },
	} {
		if err := call(); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.ConfigPutSecret(ctx, by, mcp.ConfigSecretInput{Plan: p.ID.String(), Secret: "x", Value: "v"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Errorf("no secret store: %v", err)
	}
}

// TestConfigApplyOverMCPNeverApplies: the tool of a client only asks.
func TestConfigApplyOverMCPNeverApplies(t *testing.T) {
	f, by := configFixture(t)
	ctx := t.Context()
	svc := tools.New(tools.Config{Store: f.st, PublicURL: "https://forecrew.example.com/"})
	p, err := svc.ConfigPlan(ctx, by, mcp.ConfigPlanInput{Changes: teamChange("core"), Source: domain.ConfigFromMCP})
	must(t, err)
	c, err := svc.ConfigRequestApproval(ctx, by, mcp.ConfigPlanRefInput{Plan: p.ID.String()})
	must(t, err)
	if c.Status != "confirmation_required" || c.ConfirmURL != "https://forecrew.example.com/inbox?plan="+p.ID.String() || c.Plan.Status != domain.ConfigPlanAwaiting {
		t.Fatalf("confirmation = %+v", c)
	}
	if rs, _ := svc.ConfigGet(ctx, by, mcp.ConfigGetInput{Kind: "team", Name: "core"}); len(rs) != 0 {
		t.Fatalf("applied: %+v", rs)
	}
	if _, err := svc.ConfigApply(ctx, by, mcp.ConfigApplyInput{Plan: p.ID.String()}); !errors.Is(err, mcp.ErrConflict) {
		t.Fatalf("a token confirmed a plan of an MCP client: %v", err)
	}
	if _, err := svc.ConfigApply(ctx, by, mcp.ConfigApplyInput{Plan: p.ID.String(), Browser: true}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigToolsNeedConfigRead(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, _ := f.member(t, orgScope, "", uuid.Nil())
	for name, call := range map[string]func() error{
		"get":    func() error { _, err := f.svc.ConfigGet(ctx, by, mcp.ConfigGetInput{}); return err },
		"export": func() error { _, err := f.svc.ConfigExport(ctx, by); return err },
		"plans":  func() error { _, err := f.svc.ConfigPlans(ctx, by, mcp.ConfigPlansInput{}); return err },
		"plan": func() error {
			_, err := f.svc.ConfigGetPlan(ctx, by, mcp.ConfigPlanRefInput{Plan: domain.NewID().String()})
			return err
		},
		"send": func() error { _, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "x"}); return err },
		"read": func() error {
			_, err := f.svc.AssistantGet(ctx, by, mcp.AssistantRefInput{Conversation: "x"})
			return err
		},
		"list": func() error { _, err := f.svc.AssistantList(ctx, by, mcp.AssistantListInput{}); return err },
	} {
		if err := call(); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// finishTurn runs the assistant's job to its end with output.
func finishTurn(t *testing.T, f *fixture, jobID uuid.UUID, status domain.JobStatus, output string) {
	t.Helper()
	ctx := t.Context()
	j, err := f.st.Jobs().Get(ctx, f.account.ID, jobID)
	must(t, err)
	w := domain.Worker{AccountID: f.account.ID, Name: "control-" + jobID.String(), Pool: j.Pool, TokenHash: []byte(jobID.String())}
	must(t, f.st.Workers().Create(ctx, &w))
	_, err = f.st.Jobs().LeaseNext(ctx, f.account.ID, w.ID, j.RequiredLabels(), time.Minute)
	must(t, err)
	_, err = f.st.Jobs().Complete(ctx, f.account.ID, jobID, w.ID, store.JobResult{Status: status, Output: json.RawMessage(output)})
	must(t, err)
}

func TestAssistantConversation(t *testing.T) {
	f, by := configFixture(t)
	ctx := t.Context()
	v, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "Add a team called core", Page: "/settings/teams"})
	must(t, err)
	if !v.Thinking || len(v.Messages) != 1 || v.Owner != by.String() || v.ProjectID != f.project.ID {
		t.Fatalf("view = %+v", v)
	}
	reqs := f.creator.Requests()
	if len(reqs) != 1 || reqs[0].Role != tools.AssistantRole || !strings.Contains(reqs[0].Prompt, "Add a team called core") || !strings.Contains(reqs[0].Prompt, "/settings/teams") {
		t.Fatalf("requests = %+v", reqs)
	}
	ref := v.ID.String()
	if _, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Conversation: ref, Message: "more"}); !errors.Is(err, mcp.ErrConflict) {
		t.Fatalf("a second message while it thinks: %v", err)
	}
	job, err := f.st.Jobs().Get(ctx, f.account.ID, v.Messages[0].JobID)
	must(t, err)

	// The job plans as the person, and the plan is the conversation's.
	p, err := f.svc.ConfigPlanFor(ctx, job, jobmcp.ConfigPlanInput{Title: "Add core", Changes: teamChange("core"), BasedOn: "README.md"})
	must(t, err)
	if p.Source != domain.ConfigFromAssistant || p.CreatedBy != by.String() || p.Request != "Add a team called core" || p.ConversationID != v.ID ||
		p.Untrusted != "Based on text from README.md, which is not verified" {
		t.Fatalf("plan = %+v", p)
	}
	out, err := f.svc.ConfigRead(ctx, job, jobmcp.ConfigReadInput{Kind: "team", Schema: true, Waiting: true})
	must(t, err)
	if len(out.Schemas) != 1 || len(out.Waiting) != 1 || out.Waiting[0].Role != tools.AssistantRole {
		t.Fatalf("read = %+v", out)
	}

	finishTurn(t, f, job.ID, domain.JobSucceeded, `{"reply":"Here is the plan. Nothing changes until you confirm."}`)
	v, err = f.svc.AssistantGet(ctx, by, mcp.AssistantRefInput{Conversation: ref})
	must(t, err)
	if v.Thinking || len(v.Messages) != 2 || v.Messages[1].Author != domain.AssistantReply || len(v.Plans) != 1 || v.Plans[0].ID != p.ID {
		t.Fatalf("view = %+v", v)
	}
	// Reading again records nothing twice.
	v, err = f.svc.AssistantGet(ctx, by, mcp.AssistantRefInput{Conversation: ref})
	if err != nil || len(v.Messages) != 2 {
		t.Fatalf("view = %+v, %v", v, err)
	}

	// The next turn carries the conversation, as data.
	v, err = f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Conversation: ref, Message: "Thanks"})
	must(t, err)
	prompt := f.creator.Requests()[1].Prompt
	if !strings.Contains(prompt, "<untrusted-data") || !strings.Contains(prompt, "Here is the plan") {
		t.Fatalf("prompt = %s", prompt)
	}
	finishTurn(t, f, v.Messages[2].JobID, domain.JobFailed, "null")
	v, err = f.svc.AssistantGet(ctx, by, mcp.AssistantRefInput{Conversation: ref})
	must(t, err)
	if last := v.Messages[len(v.Messages)-1]; !strings.Contains(last.Body, "Nothing changed") {
		t.Fatalf("answer of a failed turn = %+v", last)
	}
	assertPersonal(t, f, by, ref)
}

// assertPersonal: a conversation is its owner's alone.
func assertPersonal(t *testing.T, f *fixture, by domain.Actor, ref string) {
	t.Helper()
	ctx := t.Context()
	other, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	if _, err := f.svc.AssistantGet(ctx, other, mcp.AssistantRefInput{Conversation: ref}); !errors.Is(err, mcp.ErrNotFound) {
		t.Fatalf("another person's conversation: %v", err)
	}
	if rows, err := f.svc.AssistantList(ctx, other, mcp.AssistantListInput{}); err != nil || len(rows) != 0 {
		t.Fatalf("another person's list = %+v, %v", rows, err)
	}
	if rows, err := f.svc.AssistantList(ctx, by, mcp.AssistantListInput{}); err != nil || len(rows) != 1 {
		t.Fatalf("list = %+v, %v", rows, err)
	}
}

func TestSecretsNeverEnterAConversation(t *testing.T) {
	f, by := configFixture(t)
	ctx := t.Context()
	token := "ghp_abcdefghijklmnopqrstuvwxyz123456"
	_, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "my token is " + token})
	if !errors.Is(err, mcp.ErrInvalidInput) || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	if rows, _ := f.svc.AssistantList(ctx, by, mcp.AssistantListInput{}); len(rows) != 0 {
		t.Fatalf("stored: %+v", rows)
	}
	if len(f.creator.Requests()) != 0 {
		t.Fatal("a job got the secret")
	}
	// A reply that holds one is not kept either.
	v, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "hello"})
	must(t, err)
	finishTurn(t, f, v.Messages[0].JobID, domain.JobSucceeded, `{"reply":"use `+token+`"}`)
	v, err = f.svc.AssistantGet(ctx, by, mcp.AssistantRefInput{Conversation: v.ID.String()})
	must(t, err)
	if strings.Contains(v.Messages[1].Body, token) {
		t.Fatalf("reply = %s", v.Messages[1].Body)
	}
}

func TestAssistantRefusals(t *testing.T) {
	f, by := configFixture(t)
	ctx := t.Context()
	for name, in := range map[string]mcp.AssistantSendInput{
		"empty":    {Message: "  "},
		"too long": {Message: strings.Repeat("x", tools.MaxAssistantMessage+1)},
		"bad ref":  {Message: "x", Conversation: "nope"},
	} {
		if _, err := f.svc.AssistantSend(ctx, by, in); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "x", Project: "ghost"}); !errors.Is(err, mcp.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	// No project the caller may read: the job could not run for them.
	billing, _ := f.member(t, orgScope, domain.RoleBilling, uuid.Nil())
	if _, err := f.svc.AssistantSend(ctx, billing, mcp.AssistantSendInput{Message: "x"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Errorf("no project: %v", err)
	}
	if _, err := f.svc.AssistantSend(ctx, billing, mcp.AssistantSendInput{Message: "x", Project: "app"}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a project out of reach: %v", err)
	}
	f.creator.Err = jobs.ErrInvalid
	if _, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "x"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Errorf("role not configured: %v", err)
	}
	if _, err := tools.New(tools.Config{Store: f.st}).AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "x"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Errorf("no creator: %v", err)
	}
}

// TestUntrustedComesFromTheServer: what the turn read marks the plan, whatever
// the model says.
func TestUntrustedComesFromTheServer(t *testing.T) {
	f, by := configFixture(t)
	ctx := t.Context()
	v, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "add a team"})
	must(t, err)
	job, err := f.st.Jobs().Get(ctx, f.account.ID, v.Messages[0].JobID)
	must(t, err)
	call := func(tool, status string) {
		t.Helper()
		raw, err := json.Marshal(map[string]string{"tool": tool, "status": status})
		must(t, err)
		must(t, f.st.Audit().Append(ctx, &domain.AuditEntry{AccountID: f.account.ID, Actor: "worker:w", Action: jobmcp.AuditCall, SubjectType: "job", SubjectID: job.ID, Data: raw}))
	}
	p, err := f.svc.ConfigPlanFor(ctx, job, jobmcp.ConfigPlanInput{Title: "t", Changes: teamChange("none")})
	must(t, err)
	if p.Untrusted != "" {
		t.Errorf("nothing read: %q", p.Untrusted)
	}
	call("report_progress", "ok")
	call("kb_search", "error") // a call that failed returned no text
	p, err = f.svc.ConfigPlanFor(ctx, job, jobmcp.ConfigPlanInput{Title: "t", Changes: teamChange("failed")})
	must(t, err)
	if p.Untrusted != "" {
		t.Errorf("a failed call: %q", p.Untrusted)
	}
	call("config_read", "ok")
	call("kb_search", "ok")
	call("config_read", "ok")
	p, err = f.svc.ConfigPlanFor(ctx, job, jobmcp.ConfigPlanInput{Title: "t", Changes: teamChange("read")})
	must(t, err)
	if want := "Based on text from the configuration, the project's knowledge, which is not verified"; p.Untrusted != want {
		t.Errorf("untrusted = %q", p.Untrusted)
	}
}

// TestAssistantActsAsItsPerson: a Viewer gets answers without plans, and a
// revoked credential ends what the job may do.
func TestAssistantActsAsItsPerson(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	viewer, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	v, err := f.svc.AssistantSend(ctx, viewer, mcp.AssistantSendInput{Message: "add a team"})
	must(t, err)
	job, err := f.st.Jobs().Get(ctx, f.account.ID, v.Messages[0].JobID)
	must(t, err)
	if _, err := f.svc.ConfigRead(ctx, job, jobmcp.ConfigReadInput{Kind: "team"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ConfigPlanFor(ctx, job, jobmcp.ConfigPlanInput{Title: "t", Changes: teamChange("core")})
	var te *jobmcp.ToolError
	if !errors.As(err, &te) || te.Code != jobmcp.CodeForbidden {
		t.Fatalf("a Viewer's plan: %v", err)
	}
	// A section the person cannot read is not read for them.
	if _, err := f.svc.ConfigRead(ctx, job, jobmcp.ConfigReadInput{Kind: "connection"}); !errors.As(err, &te) || te.Code != jobmcp.CodeForbidden {
		t.Fatalf("a section out of reach: %v", err)
	}
	// A job that is no turn gets nothing.
	other := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Role: "assistant", Runtime: "claude-code", Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, f.st.Jobs().Create(ctx, &other))
	if _, err := f.svc.ConfigRead(ctx, other, jobmcp.ConfigReadInput{}); !errors.As(err, &te) || te.Code != jobmcp.CodeForbidden {
		t.Fatalf("no conversation: %v", err)
	}
}
