package jobmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/schema"
	"github.com/forecrew/forecrew/internal/store"
)

// MaxBodyBytes is the largest request the gateway reads.
const MaxBodyBytes = 64 << 10

// Defaults of Config.
const (
	DefaultRecheck       = 2 * time.Second
	DefaultProgressEvery = 30 * time.Second
)

// Subscriber delivers published signals; workflow.Bus implements it.
type Subscriber interface {
	Subscribe(ctx context.Context) <-chan domain.Signal
}

// AssumptionRecorder registers an assumption a job made (T32's register).
type AssumptionRecorder interface {
	// RecordAssumption registers a for job j. It returns the item, the
	// existing one when the goal had the statement already, and whether it
	// was. An invalid assumption wraps domain.ErrInvalidRecord.
	RecordAssumption(ctx context.Context, j domain.Job, a domain.StatedAssumption) (domain.Assumption, bool, error)
}

// Config holds what the gateway and its tools use.
type Config struct {
	Store store.Store
	Gates gates.Service
	// Bus wakes ask_human when a gate is resolved; without it the gate is
	// only re-read every Recheck.
	Bus Subscriber
	// Attachments holds the files evidence_attach marked until the worker's
	// next heartbeat takes them.
	Attachments *Attachments
	// Evidence says whether artifact storage is configured; without it
	// evidence_attach answers unavailable.
	Evidence bool
	// Assumptions registers the assumption of an unanswered ask_human; nil
	// leaves it in the job log only.
	Assumptions AssumptionRecorder
	// Search and Notes serve kb_search and notes_write; nil makes those
	// tools answer unavailable.
	Search *knowledge.Search
	Notes  *knowledge.Notes
	// Proposals serves kb_propose and request_access; nil makes those tools
	// answer unavailable.
	Proposals *proposals.Service
	Logger    *slog.Logger
	Version   string
	Now       func() time.Time
	// After returns a channel that fires after d (time.After); tests
	// replace it to drive ask_human timeouts.
	After func(d time.Duration) <-chan time.Time
	// Recheck is how often a waiting ask_human re-reads its gate and the
	// job's lease (DefaultRecheck).
	Recheck time.Duration
	// ProgressEvery is how often a waiting ask_human sends a progress
	// notification when the call asked for them (DefaultProgressEvery).
	ProgressEvery time.Duration
}

// Gateway serves the job MCP endpoint.
type Gateway struct {
	cfg    Config
	limits *limiter
	mcp    http.Handler
}

// New returns a gateway.
func New(cfg Config) *Gateway {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Recheck <= 0 {
		cfg.Recheck = DefaultRecheck
	}
	if cfg.ProgressEvery <= 0 {
		cfg.ProgressEvery = DefaultProgressEvery
	}
	if cfg.Attachments == nil {
		cfg.Attachments = NewAttachments(cfg.Now)
	}
	g := &Gateway{cfg: cfg, limits: newLimiter(cfg.Now)}
	// Stateless: every request carries the token and is checked on its
	// own, so a token dies for the very next call; ask_human waits inside
	// its request.
	g.mcp = sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		c, _ := r.Context().Value(callerKey{}).(*caller)
		return g.server(c)
	}, &sdk.StreamableHTTPOptions{Stateless: true})
	return g
}

// Attachments returns the queue the worker API takes attachments from.
func (g *Gateway) Attachments() *Attachments { return g.cfg.Attachments }

// Handler returns the endpoint; mount it at Path.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body over 64 KiB")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "bearer") || !validToken(token) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed job token")
			return
		}
		c, err := g.authenticate(r.Context(), mcp.HashToken(token))
		switch {
		case errors.Is(err, errDeadToken):
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown or expired job token")
			return
		case err != nil:
			g.cfg.Logger.ErrorContext(r.Context(), "look up job token", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		g.mcp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent; a failed write has no recovery.
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

type callerKey struct{}

// caller is the job and worker of an authenticated request.
type caller struct {
	job    domain.Job
	worker domain.Worker
	hash   []byte
}

// errDeadToken means a token that no live lease holds.
var errDeadToken = errors.New("job token is not valid")

// authenticate returns the job and worker a token belongs to, or
// errDeadToken unless the job is still running under the worker it was
// leased to, the lease has not expired, and that worker is not revoked.
func (g *Gateway) authenticate(ctx context.Context, hash []byte) (*caller, error) {
	j, err := g.cfg.Store.Jobs().GetByJobMCPTokenHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errDeadToken
	}
	if err != nil {
		return nil, err
	}
	if j.Status != domain.JobRunning || j.Kind.OrDefault() == domain.JobKindCommand || !g.cfg.Now().Before(j.LeaseExpiresAt) {
		return nil, errDeadToken
	}
	w, err := g.cfg.Store.Workers().Get(ctx, j.AccountID, j.WorkerID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && w.Revoked()) {
		return nil, errDeadToken
	}
	if err != nil {
		return nil, err
	}
	return &caller{job: j, worker: w, hash: hash}, nil
}

// alive reports whether the caller's token still holds its lease.
func (g *Gateway) alive(ctx context.Context, c *caller) (bool, error) {
	cur, err := g.authenticate(ctx, c.hash)
	if errors.Is(err, errDeadToken) {
		return false, nil
	}
	return err == nil && cur.job.ID == c.job.ID && cur.worker.ID == c.worker.ID, err
}

// server returns an MCP server with the tools the caller's role lists.
func (g *Gateway) server(c *caller) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: ServerName, Version: g.cfg.Version}, nil)
	if c == nil {
		return srv
	}
	names := map[string]bool{}
	for _, t := range offered(c.job.Spec.AllowedTools) {
		names[t.Name] = true
		srv.AddTool(&sdk.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Input, OutputSchema: t.Output},
			g.toolHandler(c, t))
	}
	srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if call, ok := req.(*sdk.CallToolRequest); ok && method == "tools/call" && call.Params != nil && !names[call.Params.Name] {
				// The SDK answers -32602 unknown tool; the call is audited.
				g.auditDenied(ctx, c, call.Params.Name, len(call.Params.Arguments), "refused", 0)
			}
			return next(ctx, method, req)
		}
	})
	return srv
}

// toolHandler wraps a tool with the rate limit, input validation, error
// mapping and the audit entry of every call.
func (g *Gateway) toolHandler(c *caller, t Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		start := time.Now()
		in := req.Params.Arguments
		if len(in) == 0 || string(in) == "null" {
			in = json.RawMessage("{}")
		}
		finish := func(res *sdk.CallToolResult, status, code string) (*sdk.CallToolResult, error) {
			g.audit(ctx, c, t.Name, len(req.Params.Arguments), status, code, time.Since(start))
			return res, nil
		}
		if !g.limits.allow(c.job, t) {
			te := &ToolError{Code: CodeRateLimited, Message: "too many calls; try later or stop calling this tool"}
			g.auditDenied(ctx, c, t.Name, len(req.Params.Arguments), "rate_limited", time.Since(start))
			return te.result(), nil
		}
		if err := checkInput(t, in); err != nil {
			return finish(err.result(), "error", err.Code)
		}
		call := &Call{Job: c.job, Worker: c.worker, Config: &g.cfg, gateway: g, caller: c, session: req.Session, progress: req.Params.GetProgressToken()}
		out, err := t.Handler(ctx, call, in)
		if te, ok := errors.AsType[*ToolError](err); ok {
			return finish(te.result(), "error", te.Code)
		}
		if err != nil {
			g.cfg.Logger.ErrorContext(ctx, "job MCP tool failed", "tool", t.Name, "job_id", c.job.ID, "account_id", c.job.AccountID, "error", err)
			return finish(internalError(), "error", "internal")
		}
		res, err := out.result()
		if err != nil {
			g.cfg.Logger.ErrorContext(ctx, "encode job MCP result", "tool", t.Name, "job_id", c.job.ID, "error", err)
			return finish(internalError(), "error", "internal")
		}
		return finish(res, "ok", "")
	}
}

func checkInput(t Tool, in json.RawMessage) *ToolError {
	inst, err := schema.DecodeJSON(in)
	if err == nil {
		err = schema.Check(contractFile, string(t.Input), inst)
	}
	if err != nil {
		return &ToolError{Code: CodeInvalidInput, Message: "input does not match the tool's schema: " + strings.TrimPrefix(err.Error(), schema.ErrInvalid.Error()+": ")}
	}
	return nil
}

func (r Result) result() (*sdk.CallToolResult, error) {
	data, err := json.Marshal(r.Data)
	if err != nil {
		return nil, err
	}
	text := r.Text
	if text == "" {
		text = string(data)
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}, StructuredContent: json.RawMessage(data)}, nil
}

// audit appends the audit entry of one call. A failure is logged: the
// call's own effects are already stored.
func (g *Gateway) audit(ctx context.Context, c *caller, tool string, inputBytes int, status, code string, took time.Duration) {
	data := map[string]any{"tool": tool, "input_bytes": inputBytes, "status": status, "duration_ms": took.Milliseconds()}
	if code != "" {
		data["code"] = code
	}
	raw, _ := json.Marshal(data) // a map of plain values always marshals
	by := domain.Actor{AccountID: c.job.AccountID, Kind: domain.ActorWorker, Name: c.worker.Name}
	err := g.cfg.Store.Audit().Append(context.WithoutCancel(ctx), &domain.AuditEntry{
		AccountID: c.job.AccountID, ProjectID: c.job.ProjectID, Actor: by.String(),
		Action: "job_mcp.call", SubjectType: "job", SubjectID: c.job.ID, Data: raw,
	})
	if err != nil {
		g.cfg.Logger.ErrorContext(ctx, "audit job MCP call", "tool", tool, "job_id", c.job.ID, "error", err)
	}
}

// auditDenied audits a refused or rate-limited call, up to MaxDeniedAudits
// of them per attempt; the first one past it writes a single audit_limit row
// and the rest write nothing, so a looping agent cannot fill the audit log.
func (g *Gateway) auditDenied(ctx context.Context, c *caller, tool string, inputBytes int, status string, took time.Duration) {
	audit, summary := g.limits.denied(c.job)
	switch {
	case audit:
		g.audit(ctx, c, tool, inputBytes, status, "", took)
	case summary:
		g.audit(ctx, c, tool, inputBytes, "audit_limit", "", took)
	}
}

// Call is one tool call of a job.
type Call struct {
	Job    domain.Job
	Worker domain.Worker
	Config *Config

	gateway  *Gateway
	caller   *caller
	session  *sdk.ServerSession
	progress any // the call's progressToken, nil without one
}

// Alive reports whether the job still holds the lease of the call's token.
func (c *Call) Alive(ctx context.Context) (bool, error) { return c.gateway.alive(ctx, c.caller) }

// Notify sends a progress notification when the call asked for them.
func (c *Call) Notify(ctx context.Context, progress float64, message string) {
	if c.progress == nil || c.session == nil {
		return
	}
	err := c.session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: c.progress, Progress: progress, Message: message})
	if err != nil {
		c.Config.Logger.DebugContext(ctx, "send progress notification", "job_id", c.Job.ID, "error", err)
	}
}

// Error codes of ToolError (job-mcp.md, Errors).
const (
	CodeInvalidInput = "invalid_input"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeRateLimited  = "rate_limited"
	CodeTooLarge     = "too_large"
	CodeUnavailable  = "unavailable"
)

// ToolError is a refusal the agent can act on: a result with isError and
// structuredContent {"error": {"code", "message"}}.
type ToolError struct {
	Code    string
	Message string
}

// Errorf returns a ToolError with a formatted message.
func Errorf(code, format string, args ...any) *ToolError {
	return &ToolError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

func (e *ToolError) result() *sdk.CallToolResult {
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}}) // plain strings
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: e.Message}}, StructuredContent: json.RawMessage(data)}
}

// internalError is the result of an unexpected failure; the cause is in the
// server log only.
func internalError() *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "internal error, try again later"}}}
}
