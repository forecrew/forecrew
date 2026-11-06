package mcp

import (
	"context"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// planText is the plan in words: a client shows this to the person.
func planText(p ConfigPlanView) string {
	if p.Title == "" && len(p.Changes) == 0 {
		return "Nothing to change."
	}
	var text strings.Builder
	text.WriteString("Plan " + p.ID.String() + ": " + p.Title + " (" + string(p.Status) + ").")
	for _, c := range p.Changes {
		text.WriteString("\n" + c.Line)
	}
	return text.String()
}

func registerConfigTools(r registrar, svc ToolService) {
	plan := "Plan UUID."
	planTool := func(fn func(context.Context, domain.Actor, ConfigPlanRefInput) (ConfigPlanView, error)) func(context.Context, domain.Actor, ConfigPlanRefInput) (toolResult, error) {
		return func(ctx context.Context, by domain.Actor, in ConfigPlanRefInput) (toolResult, error) {
			in.Source = domain.ConfigFromMCP
			p, err := fn(ctx, by, in)
			return result(p, "%s", planText(p)), err
		}
	}
	register(r, ToolConfigSchema, "List the kinds of configuration resources with the JSON Schema of each spec, or one kind.",
		fieldDocs{"kind": "Resource kind, for example shape; empty lists every kind."},
		func(ctx context.Context, by domain.Actor, in ConfigKindInput) (toolResult, error) {
			s, err := svc.ConfigSchema(ctx, by, in)
			return list("schemas", s, "%d kinds.", len(s)), err
		})
	register(r, ToolConfigGet, "Read the configuration you may read, as resources of a kind, a name and a spec. Secrets are never shown, only references to them.",
		fieldDocs{"kind": "Only this kind.", "name": "Only this name."},
		func(ctx context.Context, by domain.Actor, in ConfigGetInput) (toolResult, error) {
			c, err := svc.ConfigGet(ctx, by, in)
			return list("resources", c, "%d resources.", len(c)), err
		})
	register(r, ToolConfigPlan, "Propose a change to the configuration. Nothing changes: the result is a stored plan with a readable diff, the permissions it needs, what it affects, its cost and whether it can be reverted. A person confirms it in the web UI.",
		fieldDocs{"title": "Short title of the change.", "request": "What the person asked for, in their words.", "changes": "The wanted resources: kind, name and the whole spec, or delete: true."},
		func(ctx context.Context, by domain.Actor, in ConfigPlanInput) (toolResult, error) {
			in.Source = domain.ConfigFromMCP
			p, err := svc.ConfigPlan(ctx, by, in)
			return result(p, "%s", planText(p)), err
		}, "changes")
	register(r, ToolConfigApply, "Ask for a plan to be applied. This never applies it: the plan goes to the Inbox, and a person confirms it in the web UI at confirm_url. Show the person the plan and the link.",
		fieldDocs{"plan": plan},
		func(ctx context.Context, by domain.Actor, in ConfigPlanRefInput) (toolResult, error) {
			c, err := svc.ConfigRequestApproval(ctx, by, in)
			return result(c, "%s\n%s", planText(c.Plan), c.Message), err
		}, "plan")
	register(r, ToolConfigRevert, "Make a new plan that puts back the previous values of an applied plan. It is confirmed like any plan.",
		fieldDocs{"plan": plan}, planTool(svc.ConfigRevert), "plan")
	register(r, ToolConfigExport, "Export the configuration you may read as YAML, without secrets.",
		nil, func(ctx context.Context, by domain.Actor, _ struct{}) (toolResult, error) {
			e, err := svc.ConfigExport(ctx, by)
			return result(e, "%d bytes of YAML.", len(e.YAML)), err
		})
	register(r, ToolConfigImport, "Turn YAML from config_export into a plan. What the file leaves out stays as it is. Nothing changes until a person confirms the plan.",
		fieldDocs{"yaml": "The YAML of config_export."},
		func(ctx context.Context, by domain.Actor, in ConfigImportInput) (toolResult, error) {
			in.Source = domain.ConfigFromMCP
			p, err := svc.ConfigImport(ctx, by, in)
			return result(p, "%s", planText(p)), err
		}, "yaml")
	register(r, ToolConfigPlans, "List change plans, newest first; with inbox, those waiting for your confirmation.",
		fieldDocs{"inbox": "Only the plans waiting for your confirmation.", "limit": "Maximum plans to return."},
		func(ctx context.Context, by domain.Actor, in ConfigPlansInput) (toolResult, error) {
			p, err := svc.ConfigPlans(ctx, by, in)
			return list("plans", p, "%d plans.", len(p)), err
		})
	register(r, ToolConfigGetPlan, "Show a change plan: its diff, status, what it needs and whether a secure field is still empty.",
		fieldDocs{"plan": plan}, planTool(svc.ConfigGetPlan), "plan")
	register(r, ToolConfigCancel, "Drop a change plan that was not applied.",
		fieldDocs{"plan": plan}, planTool(svc.ConfigCancel), "plan")
	register(r, ToolAssistantSend, "Tell the configuration assistant what you want. It answers in a job, so read the answer with assistant_get. It can propose plans; it cannot apply anything. Never send a secret: a message that looks like one is refused.",
		fieldDocs{"conversation": "Conversation UUID; empty starts a new one.", "message": "What you want, in your words.", "project": "Project the conversation's jobs run in; defaults to the first you may read.", "page": "The page you are on, as context."},
		func(ctx context.Context, by domain.Actor, in AssistantSendInput) (toolResult, error) {
			v, err := svc.AssistantSend(ctx, by, in)
			return result(v, "Conversation %s: the assistant is working.", v.ID), err
		}, "message")
	register(r, ToolAssistantGet, "Read one of your conversations with the assistant, with the plans it proposed.",
		fieldDocs{"conversation": "Conversation UUID."},
		func(ctx context.Context, by domain.Actor, in AssistantRefInput) (toolResult, error) {
			v, err := svc.AssistantGet(ctx, by, in)
			return result(v, "%d messages, %d plans.", len(v.Messages), len(v.Plans)), err
		}, "conversation")
	register(r, ToolAssistantList, "List your conversations with the assistant, newest first.",
		fieldDocs{"limit": "Maximum conversations to return."},
		func(ctx context.Context, by domain.Actor, in AssistantListInput) (toolResult, error) {
			c, err := svc.AssistantList(ctx, by, in)
			return list("conversations", c, "%d conversations.", len(c)), err
		})
}
