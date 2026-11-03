package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// NewHandler returns the MCP server over streamable HTTP, protected by bearer
// token authentication. Mount it at /mcp. Each tool calls the matching
// ToolService method as the token's actor.
func NewHandler(svc ToolService, tokens store.MCPTokenRepo, version string, logger *slog.Logger) http.Handler {
	srv := sdk.NewServer(&sdk.Implementation{Name: "forecrew", Version: version}, nil)
	registerTools(srv, svc, logger)
	// Stateless: every request carries its own token, so there is no session
	// to hijack and nothing to lose on restart; all tools are request/response.
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true})
	return Authenticate(tokens, logger)(h)
}

func registerTools(srv *sdk.Server, svc ToolService, logger *slog.Logger) {
	r := registrar{srv: srv, logger: logger}
	project := "Project name or repo (owner/name)."
	gate := "Gate ref (GATE-7) or UUID."
	job := "Job ref (JOB-41) or UUID."
	goal := "Goal ref (GOAL-3) or UUID."

	register(r, ToolOnboardProject, "Register a GitHub repo as a project if needed and start onboarding: an analyst job reads the repo and proposes a profile for approval. Returns the project and the onboarding run.",
		fieldDocs{"repo": "Repository as owner/name.", "name": "Project name; defaults to the repo name.", "default_branch": "Default branch of the repo, used when the project is registered by this call; defaults to main."},
		func(ctx context.Context, by domain.Actor, in OnboardProjectInput) (toolResult, error) {
			out, err := svc.OnboardProject(ctx, by, in)
			return result(out, "Onboarding of %s started.", out.Project.Repo), err
		})
	register(r, ToolCreateGoal, "Create a goal for a project and start the goal playbook (spec, plan, implementation). Returns the new draft goal.",
		fieldDocs{"project": project, "title": "Short goal title.", "body": "Goal description in markdown: what to build and why.", "platforms": "Platforms the goal targets, for example web or ios. Optional."},
		func(ctx context.Context, by domain.Actor, in CreateGoalInput) (toolResult, error) {
			g, err := svc.CreateGoal(ctx, by, in)
			return result(g, "%s created: %s (%s).", g.Ref(), g.Title, g.Status), err
		})
	register(r, ToolJobStatus, "Show a job: role, status, attempts, branch, result and failure reason.",
		fieldDocs{"job": job}, func(ctx context.Context, by domain.Actor, in JobRefInput) (toolResult, error) {
			j, err := svc.JobStatus(ctx, by, in)
			return result(j, "%s (%s) is %s%s.", j.Ref, j.Role, j.Status, because(j.Reason)), err
		})
	register(r, ToolJobLogs, "Read a job's event log (output, tool calls, status changes), oldest first. Pass next_seq of the previous call as after_seq to continue.",
		fieldDocs{"job": job, "after_seq": "Only events with a greater sequence number.", "limit": "Maximum events to return; default 200, maximum 1000."},
		func(ctx context.Context, by domain.Actor, in JobLogsInput) (toolResult, error) {
			l, err := svc.JobLogs(ctx, by, in)
			l.Events = NonNil(l.Events)
			return result(l, "%d events, next_seq %d.", len(l.Events), l.NextSeq), err
		})
	register(r, ToolListWorkers, "List the account's workers with labels, last seen time, whether each is online and the jobs it runs.",
		nil, func(ctx context.Context, by domain.Actor, _ struct{}) (toolResult, error) {
			w, err := svc.ListWorkers(ctx, by)
			online := 0
			for _, x := range w {
				if x.Online {
					online++
				}
			}
			return list("workers", w, "%d workers, %d online.", len(w), online), err
		})
	register(r, ToolListPendingGates, "List gates waiting for a human decision (profile, spec and merge approvals, and jobs that need you) with the GitHub link of what each is about, optionally for one project.",
		fieldDocs{"project": project}, func(ctx context.Context, by domain.Actor, in ProjectFilterInput) (toolResult, error) {
			g, err := svc.ListPendingGates(ctx, by, in)
			return list("gates", g, "%d pending gates.", len(g)), err
		})
	register(r, ToolApprove, "Approve a pending gate. On a needs_you gate this retries the stuck work.",
		fieldDocs{"gate": gate, "feedback": "Optional note recorded with the decision."}, gateTool(svc.Approve))
	register(r, ToolRequestChanges, "Ask for changes on a pending profile, spec, merge or test plan gate; feedback is required and goes to the next revision. Not valid on needs_you gates: use approve to retry or reject to stop.",
		fieldDocs{"gate": gate, "feedback": "What to change. Required."}, gateTool(svc.RequestChanges), "feedback")
	register(r, ToolReject, "Reject a pending gate. On a needs_you gate this stops the work.",
		fieldDocs{"gate": gate, "feedback": "Optional reason recorded with the decision."}, gateTool(svc.Reject))
	register(r, ToolDispatchJob, "Queue an ad-hoc job of a read-only role on a project, optionally tied to a goal or task and limited to workers with the needed labels.",
		fieldDocs{
			"project": project, "role": "Name of a read-only role, for example reviewer.",
			"base_ref": "Git ref to start from; defaults to the project's default branch.", "branch": "Branch to check out and inspect; it is never pushed.",
			"needs":              "Worker labels the job needs, for example runtime:claude-code or linux.",
			"pool":               "Worker pool to run in, for example control; defaults to the role's pool. The pool's policy must allow the role.",
			"include_repo_facts": "Include the analyzer's repo facts in the context.",
			"goal":               goal, "task": "Task ref (TASK-12) or UUID.", "feedback": "Review feedback for the job.", "prompt": "Instructions for the job: what it should do.",
		}, func(ctx context.Context, by domain.Actor, in DispatchJobInput) (toolResult, error) {
			j, err := svc.DispatchJob(ctx, by, in)
			return result(j, "%s queued (%s).", j.Ref(), j.Role), err
		})
	register(r, ToolListProjects, "List the account's projects.",
		nil, func(ctx context.Context, by domain.Actor, _ struct{}) (toolResult, error) {
			p, err := svc.ListProjects(ctx, by)
			return list("projects", p, "%d projects.", len(p)), err
		})
	register(r, ToolGetProject, "Show a project: repo, status and approved profile.",
		fieldDocs{"project": project}, func(ctx context.Context, by domain.Actor, in ProjectInput) (toolResult, error) {
			p, err := svc.GetProject(ctx, by, in)
			return result(p, "%s (%s) is %s.", p.Name, p.Repo, p.Status), err
		})
	register(r, ToolListGoals, "List goals, optionally for one project and status.",
		fieldDocs{"project": project, "status": "Goal status to filter on."}, func(ctx context.Context, by domain.Actor, in ListGoalsInput) (toolResult, error) {
			g, err := svc.ListGoals(ctx, by, in)
			return list("goals", g, "%d goals.", len(g)), err
		})
	register(r, ToolGetGoal, "Show a goal with its latest spec, tasks (state, issue and PR links, check status, iterations, cost), gates with their decisions and total cost.",
		fieldDocs{"goal": goal}, func(ctx context.Context, by domain.Actor, in GoalRefInput) (toolResult, error) {
			d, err := svc.GetGoal(ctx, by, in)
			d.Tasks, d.Gates = NonNil(d.Tasks), NonNil(d.Gates)
			return result(d, "%s %s is %s: %d tasks, $%.2f.", d.Ref, d.Goal.Title, d.Goal.Status, len(d.Tasks), d.CostUSD), err
		})
	register(r, ToolListJobs, "List recent jobs, newest first, optionally for one project, goal and status.",
		fieldDocs{"project": project, "goal": goal, "status": "Job status to filter on.", "limit": "Maximum jobs to return; default 50."},
		func(ctx context.Context, by domain.Actor, in ListJobsInput) (toolResult, error) {
			j, err := svc.ListJobs(ctx, by, in)
			return list("jobs", j, "%d jobs.", len(j)), err
		})
	register(r, ToolCancelJob, "Cancel a queued or running job. A worker running it stops, and the playbook step waiting for it fails through its normal failure path. Finished jobs cannot be canceled.",
		fieldDocs{"job": job}, func(ctx context.Context, by domain.Actor, in JobRefInput) (toolResult, error) {
			j, err := svc.CancelJob(ctx, by, in)
			return result(j, "%s is %s.", j.Ref, j.Status), err
		})
	register(r, ToolListTokens, "List the account's API and MCP tokens with creation, last use and revocation times, and which one you are using. Secrets are never shown.",
		nil, func(ctx context.Context, by domain.Actor, _ struct{}) (toolResult, error) {
			t, err := svc.ListTokens(ctx, by)
			active := 0
			for _, x := range t {
				if x.RevokedAt.IsZero() {
					active++
				}
			}
			return list("tokens", t, "%d tokens, %d active.", len(t), active), err
		})
	register(r, ToolRevokeToken, "Revoke an API and MCP token by name or ID; it stops working at once, as do the web sessions it backs. Revoking the token you are using is allowed and ends your own access.",
		fieldDocs{"token": "Token name or UUID."}, //nolint:gosec // a field description, not a credential
		func(ctx context.Context, by domain.Actor, in RevokeTokenInput) (toolResult, error) {
			out, err := svc.RevokeToken(ctx, by, in)
			return result(out, "%s", revokeMessage(out.Token.Name, out.Self)), err
		})
}

// toolResult is what a tool returns to the client: a short text and the
// structured content, which must be a JSON object.
type toolResult struct {
	text string
	data any
}

func result(data any, format string, args ...any) toolResult {
	return toolResult{text: fmt.Sprintf(format, args...), data: data}
}

// list returns items under key, as [] when empty.
func list[T any](key string, items []T, format string, args ...any) toolResult {
	return result(map[string]any{key: NonNil(items)}, format, args...)
}

func because(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// gateTool adapts a gate decision method to a tool.
func gateTool(decide func(context.Context, domain.Actor, GateDecisionInput) (domain.Gate, error)) func(context.Context, domain.Actor, GateDecisionInput) (toolResult, error) {
	return func(ctx context.Context, by domain.Actor, in GateDecisionInput) (toolResult, error) {
		g, err := decide(ctx, by, in)
		return result(g, "%s is %s.", g.Ref(), g.Status), err
	}
}

type registrar struct {
	srv    *sdk.Server
	logger *slog.Logger
}

// fieldDocs maps an input field's JSON name to its description.
type fieldDocs map[string]string

// register adds one tool whose input type is In; its JSON schema comes from
// In and the field descriptions from docs; the fields named in required are
// marked required on top of those In requires. call receives the token's actor
// and returns the short text and the structured content.
func register[In any](r registrar, name, description string, docs fieldDocs, call func(context.Context, domain.Actor, In) (toolResult, error), required ...string) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic("mcp: schema for " + name + ": " + err.Error()) // programmer error, at startup
	}
	for field, doc := range docs {
		p, ok := schema.Properties[field]
		if !ok {
			panic("mcp: " + name + " has no field " + field)
		}
		p.Description = doc
	}
	for _, field := range required {
		if !slices.Contains(schema.Required, field) {
			schema.Required = append(schema.Required, field)
		}
	}
	tool := &sdk.Tool{Name: name, Description: description, InputSchema: schema}
	sdk.AddTool(r.srv, tool, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		actor, ok := ActorFromContext(ctx)
		if !ok {
			return nil, nil, errors.New("not authenticated")
		}
		res, err := call(ctx, actor, in)
		if err != nil {
			return nil, nil, errors.New(toolErrorMessage(ctx, r.logger, name, actor, err))
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: res.text}}}, res.data, nil
	})
}

// toolErrorMessage returns the text a client sees for a ToolService error:
// the message of known conditions, and a generic one (with the cause logged)
// for anything else, so internals and secrets never reach the client.
func toolErrorMessage(ctx context.Context, logger *slog.Logger, tool string, actor domain.Actor, err error) string {
	if _, ok := ErrorCode(err); ok {
		return err.Error()
	}
	logger.ErrorContext(ctx, "tool failed", "tool", tool, "account_id", actor.AccountID, "actor", actor.String(), "error", err)
	return "internal error, try again later"
}

// NonNil makes an empty list encode as [] rather than null.
func NonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
