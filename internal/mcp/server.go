package mcp

import (
	"context"
	"encoding/json"
	"errors"
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

	register(r, ToolOnboardProject, "Register a GitHub repo as a project if needed and start onboarding: an analyst job reads the repo and proposes a profile for approval. Returns the project and the onboarding job.",
		fieldDocs{"repo": "Repository as owner/name.", "name": "Project name; defaults to the repo name."}, svc.OnboardProject)
	register(r, ToolCreateGoal, "Create a goal for a project and start the goal playbook (spec, plan, implementation). Returns the new draft goal.",
		fieldDocs{"project": project, "title": "Short goal title.", "body": "Goal description in markdown: what to build and why."}, svc.CreateGoal)
	register(r, ToolJobStatus, "Show a job: role, status, attempts, branch, result and failure reason.",
		fieldDocs{"job": job}, svc.JobStatus)
	register(r, ToolJobLogs, "Read a job's event log (output, tool calls, status changes), oldest first. Pass next_seq of the previous call as after_seq to continue.",
		fieldDocs{"job": job, "after_seq": "Only events with a greater sequence number.", "limit": "Maximum events to return; default 200, maximum 1000."}, svc.JobLogs)
	register(r, ToolListWorkers, "List the account's workers with labels, last seen time and whether each is online.",
		nil, func(ctx context.Context, by domain.Actor, _ struct{}) ([]WorkerInfo, error) {
			w, err := svc.ListWorkers(ctx, by)
			return nonNil(w), err
		})
	register(r, ToolListPendingGates, "List gates waiting for a human decision (profile, spec and merge approvals, and jobs that need you), optionally for one project.",
		fieldDocs{"project": project}, func(ctx context.Context, by domain.Actor, in ProjectFilterInput) ([]domain.Gate, error) {
			g, err := svc.ListPendingGates(ctx, by, in)
			return nonNil(g), err
		})
	register(r, ToolApprove, "Approve a pending gate. On a needs_you gate this retries the stuck work.",
		fieldDocs{"gate": gate, "feedback": "Optional note recorded with the decision."}, svc.Approve)
	register(r, ToolRequestChanges, "Ask for changes on a pending profile, spec or merge gate; feedback is required and goes to the next revision. Not valid on needs_you gates: use approve to retry or reject to stop.",
		fieldDocs{"gate": gate, "feedback": "What to change. Required."}, svc.RequestChanges, "feedback")
	register(r, ToolReject, "Reject a pending gate. On a needs_you gate this stops the work.",
		fieldDocs{"gate": gate, "feedback": "Optional reason recorded with the decision."}, svc.Reject)
	register(r, ToolDispatchJob, "Queue an ad-hoc job of a role on a project, optionally tied to a goal or task and limited to workers with the needed labels.",
		fieldDocs{
			"project": project, "role": "Role name, for example implementer or reviewer.",
			"base_ref": "Git ref to start from; defaults to the project's default branch.", "branch": "Branch the job works on.",
			"needs": "Worker labels the job needs, for example runtime:claude-code or linux.", "include_repo_facts": "Include the analyzer's repo facts in the context.",
			"goal": goal, "task": "Task ref (TASK-12) or UUID.", "feedback": "Instructions or review feedback for the job.",
		}, svc.DispatchJob)
	register(r, ToolListProjects, "List the account's projects.",
		nil, func(ctx context.Context, by domain.Actor, _ struct{}) ([]domain.Project, error) {
			p, err := svc.ListProjects(ctx, by)
			return nonNil(p), err
		})
	register(r, ToolGetProject, "Show a project: repo, status and profile.",
		fieldDocs{"project": project}, svc.GetProject)
	register(r, ToolListGoals, "List goals, optionally for one project and status.",
		fieldDocs{"project": project, "status": "Goal status to filter on."}, func(ctx context.Context, by domain.Actor, in ListGoalsInput) ([]domain.Goal, error) {
			g, err := svc.ListGoals(ctx, by, in)
			return nonNil(g), err
		})
	register(r, ToolGetGoal, "Show a goal with its latest spec, tasks, changes and pending gates.",
		fieldDocs{"goal": goal}, svc.GetGoal)
	register(r, ToolListJobs, "List recent jobs, newest first, optionally for one project and status.",
		fieldDocs{"project": project, "status": "Job status to filter on.", "limit": "Maximum jobs to return; default 50."}, func(ctx context.Context, by domain.Actor, in ListJobsInput) ([]domain.Job, error) {
			j, err := svc.ListJobs(ctx, by, in)
			return nonNil(j), err
		})
}

type registrar struct {
	srv    *sdk.Server
	logger *slog.Logger
}

// fieldDocs maps an input field's JSON name to its description.
type fieldDocs map[string]string

// register adds one tool whose input type is In; its JSON schema comes from
// In and the field descriptions from docs; the fields named in required are
// marked required on top of those In requires. call receives the token's actor.
func register[In, Out any](r registrar, name, description string, docs fieldDocs, call func(context.Context, domain.Actor, In) (Out, error), required ...string) {
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
		out, err := call(ctx, actor, in)
		if err != nil {
			return nil, nil, errors.New(toolErrorMessage(ctx, r.logger, name, actor, err))
		}
		data, err := json.Marshal(out)
		if err != nil {
			r.logger.ErrorContext(ctx, "encode tool result", "tool", name, "account_id", actor.AccountID, "error", err)
			return nil, nil, errors.New("internal error")
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, nil, nil
	})
}

// toolErrorMessage returns the text a client sees for a ToolService error:
// the message of known conditions, and a generic one (with the cause logged)
// for anything else, so internals and secrets never reach the client.
func toolErrorMessage(ctx context.Context, logger *slog.Logger, tool string, actor domain.Actor, err error) string {
	for _, known := range []error{ErrNotFound, ErrInvalidInput, domain.ErrInvalidRef, domain.ErrInvalidTransition, domain.ErrInvalidDecision} {
		if errors.Is(err, known) {
			return err.Error()
		}
	}
	logger.ErrorContext(ctx, "tool failed", "tool", tool, "account_id", actor.AccountID, "actor", actor.String(), "error", err)
	return "internal error, try again later"
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
