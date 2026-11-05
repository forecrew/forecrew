// Package api serves /api/v1 (docs/api.md): one JSON endpoint per MCP tool,
// plus the browser session endpoints of the web UI. Handlers only decode
// the request, call the mcp.ToolService and encode the answer; every rule
// lives in the service.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

var errNotAuthenticated = errors.New("not authenticated")

// MaxBodyBytes is the largest request body accepted.
const MaxBodyBytes = 1 << 20

// Config holds the dependencies of the API.
type Config struct {
	Tools mcp.ToolService
	// Artifacts serves artifact downloads; nil leaves the endpoint out.
	Artifacts ArtifactOpener
	Tokens    store.MCPTokenRepo
	Accounts  store.AccountRepo
	Logger    *slog.Logger
}

// Server is the /api/v1 handler.
type Server struct {
	cfg    Config
	auth   func(http.Handler) http.Handler
	routes []string // patterns registered by Handler
}

// New returns the API, to be mounted at /api/v1/.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, auth: mcp.Authenticate(cfg.Tokens, cfg.Logger)}
}

// Handler returns the routes under /api/v1/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	t := s.cfg.Tools
	s.routes = nil
	handle := func(pattern string, h http.Handler) {
		s.routes = append(s.routes, pattern)
		mux.Handle(pattern, h)
	}

	handle("POST /api/v1/session", http.HandlerFunc(s.login))
	handle("DELETE /api/v1/session", http.HandlerFunc(s.logout))
	handle("GET /api/v1/me", s.protect(http.HandlerFunc(s.me)))

	route := func(pattern string, h http.Handler) { handle(pattern, s.protect(h)) }
	route("POST /api/v1/projects", serve(s, http.StatusAccepted, bodyOf[mcp.OnboardProjectInput], t.OnboardProject))
	route("GET /api/v1/projects", serve(s, http.StatusOK, noInput, list(noArg(t.ListProjects))))
	route("GET /api/v1/projects/{project...}", serve(s, http.StatusOK, projectInput, t.GetProject))

	route("POST /api/v1/goals", serve(s, http.StatusCreated, bodyOf[mcp.CreateGoalInput], t.CreateGoal))
	route("GET /api/v1/goals", serve(s, http.StatusOK, listGoalsInput, list(t.ListGoals)))
	route("GET /api/v1/goals/{goal}", serve(s, http.StatusOK, goalInput, t.GetGoal))

	route("POST /api/v1/jobs", serve(s, http.StatusCreated, bodyOf[mcp.DispatchJobInput], t.DispatchJob))
	route("GET /api/v1/jobs", serve(s, http.StatusOK, listJobsInput, list(t.ListJobs)))
	route("GET /api/v1/jobs/{job}", serve(s, http.StatusOK, jobInput, t.JobStatus))
	route("GET /api/v1/jobs/{job}/logs", serve(s, http.StatusOK, jobLogsInput, t.JobLogs))
	route("POST /api/v1/jobs/{job}/cancel", serve(s, http.StatusOK, jobInput, t.CancelJob))
	route("GET /api/v1/jobs/{job}/artifacts", serve(s, http.StatusOK, jobInput, list(t.ListArtifacts)))
	route("GET /api/v1/test-runs", serve(s, http.StatusOK, listTestRunsInput, list(t.ListTestRuns)))

	route("POST /api/v1/councils", serve(s, http.StatusAccepted, bodyOf[mcp.StartCouncilInput], t.StartCouncil))
	route("GET /api/v1/councils/{council}", serve(s, http.StatusOK, councilInput, t.GetCouncil))
	route("POST /api/v1/councils/{council}/constraints", serve(s, http.StatusOK, constraintInput, t.AddCouncilConstraint))
	route("GET /api/v1/changes/{change}/verification", serve(s, http.StatusOK, verificationInput, t.VerificationReport))
	if s.cfg.Artifacts != nil {
		route("GET /api/v1/artifacts/{id}", http.HandlerFunc(s.artifact))
	}

	route("GET /api/v1/assumptions", serve(s, http.StatusOK, listAssumptionsInput, list(t.ListAssumptions)))
	route("POST /api/v1/assumptions/{assumption}/resolve", serve(s, http.StatusOK, resolveAssumptionInput, t.ResolveAssumption))
	route("GET /api/v1/decisions", serve(s, http.StatusOK, listDecisionsInput, list(t.ListDecisions)))
	route("GET /api/v1/knowledge", serve(s, http.StatusOK, searchKnowledgeInput, list(t.SearchKnowledge)))
	route("DELETE /api/v1/knowledge/notes/{note}", serve(s, http.StatusOK, noteInput, t.DeleteNote))
	route("POST /api/v1/knowledge/proposals", serve(s, http.StatusCreated, bodyOf[mcp.ProposeKnowledgeInput], t.ProposeKnowledge))

	route("GET /api/v1/workers", serve(s, http.StatusOK, noInput, list(noArg(t.ListWorkers))))

	route("GET /api/v1/gates", serve(s, http.StatusOK, gateFilterInput, list(t.ListPendingGates)))
	route("POST /api/v1/gates/{gate}/approve", serve(s, http.StatusOK, gateDecisionInput, t.Approve))
	route("POST /api/v1/gates/{gate}/request-changes", serve(s, http.StatusOK, gateDecisionInput, t.RequestChanges))
	route("POST /api/v1/gates/{gate}/reject", serve(s, http.StatusOK, gateDecisionInput, t.Reject))

	route("GET /api/v1/tokens", serve(s, http.StatusOK, noInput, list(noArg(t.ListTokens))))
	route("POST /api/v1/tokens/{token}/revoke", serve(s, http.StatusOK, tokenInput, t.RevokeToken))

	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, errors.New("no such endpoint"), http.StatusNotFound, "not_found")
	})
	return mux
}

// Routes returns the patterns the last Handler call registered, the
// catch-all for unknown endpoints aside.
func (s *Server) Routes() []string { return slices.Clone(s.routes) }

// RouteAccess returns what each /api/v1 route needs
// (docs/contracts/permissions.md), by pattern. Routes that serve a tool
// need what the tool needs. Every route Handler registers has an entry; a
// task adding a route adds its entry.
func RouteAccess() map[string]mcp.Access {
	tool := mcp.ToolAccess()
	return map[string]mcp.Access{
		"POST /api/v1/session":   {Public: true},
		"DELETE /api/v1/session": {Public: true},
		"GET /api/v1/me":         {},

		"POST /api/v1/projects":             tool[mcp.ToolOnboardProject],
		"GET /api/v1/projects":              tool[mcp.ToolListProjects],
		"GET /api/v1/projects/{project...}": tool[mcp.ToolGetProject],

		"POST /api/v1/goals":       tool[mcp.ToolCreateGoal],
		"GET /api/v1/goals":        tool[mcp.ToolListGoals],
		"GET /api/v1/goals/{goal}": tool[mcp.ToolGetGoal],

		"POST /api/v1/jobs":                tool[mcp.ToolDispatchJob],
		"GET /api/v1/jobs":                 tool[mcp.ToolListJobs],
		"GET /api/v1/jobs/{job}":           tool[mcp.ToolJobStatus],
		"GET /api/v1/jobs/{job}/logs":      tool[mcp.ToolJobLogs],
		"POST /api/v1/jobs/{job}/cancel":   tool[mcp.ToolCancelJob],
		"GET /api/v1/jobs/{job}/artifacts": tool[mcp.ToolListArtifacts],
		"GET /api/v1/artifacts/{id}":       {Permission: domain.PermJobRead},
		"GET /api/v1/test-runs":            tool[mcp.ToolListTestRuns],

		"GET /api/v1/assumptions":                       tool[mcp.ToolListAssumptions],
		"POST /api/v1/assumptions/{assumption}/resolve": tool[mcp.ToolResolveAssumption],
		"GET /api/v1/decisions":                         tool[mcp.ToolListDecisions],

		"GET /api/v1/knowledge":                       tool[mcp.ToolSearchKnowledge],
		"DELETE /api/v1/knowledge/notes/{note}":       tool[mcp.ToolDeleteNote],
		"POST /api/v1/knowledge/proposals":            tool[mcp.ToolProposeKnowledge],
		"POST /api/v1/councils":                       tool[mcp.ToolStartCouncil],
		"GET /api/v1/councils/{council}":              tool[mcp.ToolGetCouncil],
		"POST /api/v1/councils/{council}/constraints": tool[mcp.ToolAddConstraint],

		"GET /api/v1/changes/{change}/verification": tool[mcp.ToolVerificationReport],

		"GET /api/v1/workers": tool[mcp.ToolListWorkers],

		"GET /api/v1/gates":                         tool[mcp.ToolListPendingGates],
		"POST /api/v1/gates/{gate}/approve":         tool[mcp.ToolApprove],
		"POST /api/v1/gates/{gate}/request-changes": tool[mcp.ToolRequestChanges],
		"POST /api/v1/gates/{gate}/reject":          tool[mcp.ToolReject],

		"GET /api/v1/tokens":                 tool[mcp.ToolListTokens],
		"POST /api/v1/tokens/{token}/revoke": tool[mcp.ToolRevokeToken],
	}
}

// serve adapts a tool method to a handler: in decodes the request into the
// tool's input, fn runs the tool and its output is the JSON answer.
func serve[In, Out any](s *Server, status int, in func(*http.Request) (In, error), fn func(context.Context, domain.Actor, In) (Out, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		by, ok := mcp.ActorFromContext(r.Context())
		if !ok {
			s.fail(w, r, errNotAuthenticated, http.StatusUnauthorized, "unauthorized")
			return
		}
		input, err := in(r)
		if err != nil {
			s.badRequest(w, r, err)
			return
		}
		out, err := fn(r.Context(), by, input)
		if err != nil {
			s.failTool(w, r, by, err)
			return
		}
		s.write(w, r, status, out)
	})
}

// list makes a tool that returns a slice answer [] rather than null when empty.
func list[In, T any](fn func(context.Context, domain.Actor, In) ([]T, error)) func(context.Context, domain.Actor, In) ([]T, error) {
	return func(ctx context.Context, by domain.Actor, in In) ([]T, error) {
		out, err := fn(ctx, by, in)
		return mcp.NonNil(out), err
	}
}

// noArg adapts a tool without input.
func noArg[T any](fn func(context.Context, domain.Actor) (T, error)) func(context.Context, domain.Actor, struct{}) (T, error) {
	return func(ctx context.Context, by domain.Actor, _ struct{}) (T, error) { return fn(ctx, by) }
}

func noInput(*http.Request) (struct{}, error) { return struct{}{}, nil }

// decode reads the JSON request body into v, rejecting unknown fields.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON body")
	}
	return nil
}

func bodyOf[In any](r *http.Request) (In, error) {
	var in In
	err := decode(r, &in)
	return in, err
}

func projectInput(r *http.Request) (mcp.ProjectInput, error) {
	return mcp.ProjectInput{Project: r.PathValue("project")}, nil
}

func goalInput(r *http.Request) (mcp.GoalRefInput, error) {
	return mcp.GoalRefInput{Goal: r.PathValue("goal")}, nil
}

func jobInput(r *http.Request) (mcp.JobRefInput, error) {
	return mcp.JobRefInput{Job: r.PathValue("job")}, nil
}

func verificationInput(r *http.Request) (mcp.VerificationReportInput, error) {
	return mcp.VerificationReportInput{Change: r.PathValue("change"), Project: r.URL.Query().Get("project")}, nil
}

func councilInput(r *http.Request) (mcp.CouncilRefInput, error) {
	return mcp.CouncilRefInput{Council: r.PathValue("council")}, nil
}

func constraintInput(r *http.Request) (mcp.AddCouncilConstraintInput, error) {
	var body struct {
		Constraint string `json:"constraint"`
	}
	err := decode(r, &body)
	return mcp.AddCouncilConstraintInput{Council: r.PathValue("council"), Constraint: body.Constraint}, err
}

func tokenInput(r *http.Request) (mcp.RevokeTokenInput, error) {
	return mcp.RevokeTokenInput{Token: r.PathValue("token")}, nil
}

func noteInput(r *http.Request) (mcp.DeleteNoteInput, error) {
	return mcp.DeleteNoteInput{Note: r.PathValue("note")}, nil
}

func searchKnowledgeInput(r *http.Request) (mcp.SearchKnowledgeInput, error) {
	q := r.URL.Query()
	limit, err := intParam(q.Get("limit"), "limit")
	return mcp.SearchKnowledgeInput{
		Project: q.Get("project"), Query: q.Get("q"), Section: q.Get("section"), Kind: domain.KnowledgeKind(q.Get("kind")),
		Provider: q.Get("provider"), Limit: int(limit),
	}, err
}

func gateFilterInput(r *http.Request) (mcp.ProjectFilterInput, error) {
	return mcp.ProjectFilterInput{Project: r.URL.Query().Get("project")}, nil
}

func listGoalsInput(r *http.Request) (mcp.ListGoalsInput, error) {
	q := r.URL.Query()
	return mcp.ListGoalsInput{Project: q.Get("project"), Status: domain.GoalStatus(q.Get("status"))}, nil
}

func listJobsInput(r *http.Request) (mcp.ListJobsInput, error) {
	q := r.URL.Query()
	limit, err := intParam(q.Get("limit"), "limit")
	return mcp.ListJobsInput{
		Project: q.Get("project"), Goal: q.Get("goal"), Status: domain.JobStatus(q.Get("status")), Limit: int(limit),
	}, err
}

func listTestRunsInput(r *http.Request) (mcp.ListTestRunsInput, error) {
	q := r.URL.Query()
	limit, err := intParam(q.Get("limit"), "limit")
	if err != nil {
		return mcp.ListTestRunsInput{}, err
	}
	change, err := intParam(q.Get("change"), "change")
	return mcp.ListTestRunsInput{
		Job: q.Get("job"), Project: q.Get("project"), Change: int(change), SHA: q.Get("sha"), Limit: int(limit),
	}, err
}

func jobLogsInput(r *http.Request) (mcp.JobLogsInput, error) {
	q := r.URL.Query()
	after, err := intParam(q.Get("after_seq"), "after_seq")
	if err != nil {
		return mcp.JobLogsInput{}, err
	}
	limit, err := intParam(q.Get("limit"), "limit")
	return mcp.JobLogsInput{Job: r.PathValue("job"), AfterSeq: after, Limit: int(limit)}, err
}

func gateDecisionInput(r *http.Request) (mcp.GateDecisionInput, error) {
	var body struct {
		Feedback string `json:"feedback"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return mcp.GateDecisionInput{}, err
		}
	}
	return mcp.GateDecisionInput{Gate: r.PathValue("gate"), Feedback: body.Feedback}, nil
}

func listAssumptionsInput(r *http.Request) (mcp.ListAssumptionsInput, error) {
	q := r.URL.Query()
	return mcp.ListAssumptionsInput{
		Project: q.Get("project"), Goal: q.Get("goal"), Task: q.Get("task"), Status: domain.AssumptionStatus(q.Get("status")),
		Impact: domain.Level(q.Get("impact")), VerifyBy: domain.VerifyBy(q.Get("verify_by")),
	}, nil
}

func resolveAssumptionInput(r *http.Request) (mcp.ResolveAssumptionInput, error) {
	var body struct {
		Status   domain.AssumptionStatus `json:"status"`
		Evidence string                  `json:"evidence"`
	}
	if err := decode(r, &body); err != nil {
		return mcp.ResolveAssumptionInput{}, err
	}
	return mcp.ResolveAssumptionInput{Assumption: r.PathValue("assumption"), Status: body.Status, Evidence: body.Evidence}, nil
}

func listDecisionsInput(r *http.Request) (mcp.ListDecisionsInput, error) {
	q := r.URL.Query()
	return mcp.ListDecisionsInput{Project: q.Get("project"), Goal: q.Get("goal"), Council: q.Get("council")}, nil
}

func intParam(v, name string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	return n, nil
}

// isJSON reports whether the request body is declared as application/json.
func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}
