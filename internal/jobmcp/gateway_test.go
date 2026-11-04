package jobmcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// env is a gateway on a fake store with one project, two workers and a
// fake timer for ask_human.
type env struct {
	clock   *testkit.Clock
	st      *testkit.Store
	gates   *gates.Keeper
	gw      *jobmcp.Gateway
	srv     *httptest.Server
	logs    *bytes.Buffer
	project domain.Project
	task    domain.Task
	w, w2   domain.Worker

	mu       sync.Mutex
	timeouts []time.Duration
	fire     chan time.Time
	records  []domain.StatedAssumption
}

func newEnv(t *testing.T, mod ...func(*jobmcp.Config)) *env {
	t.Helper()
	ctx := t.Context()
	e := &env{clock: testkit.NewClock(t0), logs: &bytes.Buffer{}, fire: make(chan time.Time)}
	e.st = testkit.NewStore(e.clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, e.st.Accounts().Create(ctx, &acc))
	e.project = domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, e.st.Projects().Create(ctx, &e.project))
	goal := domain.Goal{AccountID: acc.ID, ProjectID: e.project.ID, Title: "g", CreatedBy: "user:ana"}
	must(t, e.st.Goals().Create(ctx, &goal))
	spec := domain.Spec{AccountID: acc.ID, GoalID: goal.ID, Revision: 1}
	must(t, e.st.Specs().Create(ctx, &spec))
	e.task = domain.Task{AccountID: acc.ID, GoalID: goal.ID, SpecID: spec.ID, Seq: 1, Title: "t", IssueNumber: 42}
	must(t, e.st.Tasks().Create(ctx, &e.task))
	e.w = domain.Worker{AccountID: acc.ID, Name: "box", TokenHash: []byte("w1")}
	e.w2 = domain.Worker{AccountID: acc.ID, Name: "box2", TokenHash: []byte("w2")}
	must(t, e.st.Workers().Create(ctx, &e.w))
	must(t, e.st.Workers().Create(ctx, &e.w2))
	bus := testkit.NewSignalBus(e.st)
	e.gates = gates.New(e.st, bus, e.clock.Now)
	cfg := jobmcp.Config{
		Store: e.st, Gates: e.gates, Bus: bus, Evidence: true, Assumptions: e,
		Logger: slog.New(slog.NewJSONHandler(e.logs, nil)), Now: e.clock.Now, Recheck: 5 * time.Millisecond,
		After: func(d time.Duration) <-chan time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.timeouts = append(e.timeouts, d)
			return e.fire
		},
	}
	for _, m := range mod {
		m(&cfg)
	}
	e.gw = jobmcp.New(cfg)
	e.srv = httptest.NewServer(e.gw.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

// RecordAssumption implements jobmcp.AssumptionRecorder.
func (e *env) RecordAssumption(_ context.Context, _ domain.Job, a domain.StatedAssumption) (domain.Assumption, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, a)
	return domain.Assumption{Number: 1, Status: domain.AssumptionUnverified}, false, nil
}

var allTools = []string{"Read", runtime.JobMCPTool("report_progress"), runtime.JobMCPTool("evidence_attach"), runtime.JobMCPTool("ask_human"), runtime.JobMCPTool("assumption_record")}

// lease creates an agent job with the given allowed tools, leases it to w
// and issues its token.
func (e *env) lease(t *testing.T, w domain.Worker, allowed ...string) (domain.Job, string) {
	t.Helper()
	ctx := t.Context()
	j := domain.Job{AccountID: e.project.AccountID, ProjectID: e.project.ID, Role: "developer", Runtime: "claude-code", TaskID: e.task.ID,
		GoalID: e.task.GoalID, Spec: domain.JobSpec{Repo: "acme/app", AllowedTools: allowed}}
	must(t, e.st.Jobs().Create(ctx, &j))
	return e.relet(t, j, w)
}

// relet leases the queued job j to w and issues a token.
func (e *env) relet(t *testing.T, j domain.Job, w domain.Worker) (domain.Job, string) {
	t.Helper()
	leased, err := e.st.Jobs().LeaseNext(t.Context(), j.AccountID, w.ID, []string{"runtime:claude-code"}, domain.LeaseDuration)
	must(t, err)
	if leased.ID != j.ID {
		t.Fatalf("leased %s, want %s", leased.Ref(), j.Ref())
	}
	tok, hash := jobmcp.NewToken()
	must(t, e.st.Jobs().SetJobMCPTokenHash(t.Context(), j.AccountID, j.ID, w.ID, leased.Attempts, hash))
	return leased, tok
}

type headerTransport struct{ header http.Header }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	maps.Copy(r.Header, h.header)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) connect(t *testing.T, token string, opts *sdk.ClientOptions) *sdk.ClientSession {
	t.Helper()
	hc := &http.Client{Transport: headerTransport{http.Header{"Authorization": {"Bearer " + token}}}}
	client := sdk.NewClient(&sdk.Implementation{Name: "agent", Version: "1"}, opts)
	cs, err := client.Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: e.srv.URL, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// status posts an initialize request with token and returns the HTTP status.
func (e *env) status(t *testing.T, token string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func call(t *testing.T, cs *sdk.ClientSession, tool string, args any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return res, out
}

func errorCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// audits returns the data of the job_mcp.call entries of job, oldest first.
func (e *env) audits(t *testing.T, j domain.Job) []map[string]any {
	t.Helper()
	entries, err := e.st.Audit().List(t.Context(), j.AccountID, store.AuditFilter{SubjectID: j.ID})
	must(t, err)
	var out []map[string]any
	for _, en := range slices.Backward(entries) {

		if en.Action != "job_mcp.call" {
			continue
		}
		if en.Actor != "worker:box" && en.Actor != "worker:box2" || en.SubjectType != "job" || en.ProjectID != j.ProjectID {
			t.Errorf("audit entry = %+v", en)
		}
		var d map[string]any
		must(t, json.Unmarshal(en.Data, &d))
		out = append(out, d)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	j, tok := e.lease(t, e.w, allTools...)
	if got := e.status(t, tok); got != http.StatusOK {
		t.Fatalf("live token: %d", got)
	}
	other, _ := jobmcp.NewToken()
	for name, token := range map[string]string{"missing": "", "not a job token": "fcm_abc", "unknown": other} {
		if got := e.status(t, token); got != http.StatusUnauthorized {
			t.Errorf("%s token: %d, want 401", name, got)
		}
	}

	// Expired lease: the reaper has not run yet, the token is dead anyway.
	e.clock.Advance(domain.LeaseDuration)
	if got := e.status(t, tok); got != http.StatusUnauthorized {
		t.Errorf("expired lease: %d, want 401", got)
	}

	// Another worker: the job was requeued and leased again; the old token
	// is dead, the new one works for the new worker only.
	_, err := e.st.Jobs().RequeueExpired(ctx, j.AccountID)
	must(t, err)
	j2, tok2 := e.relet(t, j, e.w2)
	if got := e.status(t, tok); got != http.StatusUnauthorized {
		t.Errorf("token of the earlier lease: %d, want 401", got)
	}
	if got := e.status(t, tok2); got != http.StatusOK {
		t.Errorf("token of the new lease: %d", got)
	}

	// Revoked: the worker holding the lease is revoked.
	must(t, e.st.Workers().Revoke(ctx, j.AccountID, e.w2.ID))
	if got := e.status(t, tok2); got != http.StatusUnauthorized {
		t.Errorf("revoked worker: %d, want 401", got)
	}

	// Completed job.
	k, tok3 := e.lease(t, e.w, allTools...)
	if got := e.status(t, tok3); got != http.StatusOK {
		t.Fatalf("live token: %d", got)
	}
	_, err = e.st.Jobs().Complete(ctx, k.AccountID, k.ID, e.w.ID, store.JobResult{Status: domain.JobSucceeded})
	must(t, err)
	if got := e.status(t, tok3); got != http.StatusUnauthorized {
		t.Errorf("completed job: %d, want 401", got)
	}
	// Canceled job.
	c, tok4 := e.lease(t, e.w, allTools...)
	_, err = e.st.Jobs().Cancel(ctx, c.AccountID, c.ID)
	must(t, err)
	if got := e.status(t, tok4); got != http.StatusUnauthorized {
		t.Errorf("canceled job: %d, want 401", got)
	}
	_ = j2
}

func TestTooLarge(t *testing.T) {
	e := newEnv(t)
	_, tok := e.lease(t, e.w, allTools...)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL, strings.NewReader(strings.Repeat("x", jobmcp.MaxBodyBytes+1)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestOnlyTheRolesToolsAreOffered(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, "Read", runtime.JobMCPTool("report_progress"))
	cs := e.connect(t, tok, nil)
	list, err := cs.ListTools(t.Context(), nil)
	must(t, err)
	if len(list.Tools) != 1 || list.Tools[0].Name != "report_progress" || list.Tools[0].OutputSchema == nil {
		t.Fatalf("tools = %+v", list.Tools)
	}
	_, err = cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "ask_human", Arguments: map[string]any{"question": "q"}})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("call of a tool the role lacks: %v", err)
	}
	a := e.audits(t, j)
	if len(a) != 1 || a[0]["tool"] != "ask_human" || a[0]["status"] != "refused" {
		t.Fatalf("audit = %+v", a)
	}
}

func TestReportProgress(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	res, out := call(t, cs, "report_progress", map[string]any{"message": "adding the handler", "percent": 40})
	if res.IsError || out["ok"] != true {
		t.Fatalf("result = %+v %v", res, out)
	}
	events, err := e.st.JobEvents().List(t.Context(), j.AccountID, j.ID, 0, 0)
	must(t, err)
	if len(events) != 1 || events[0].Type != domain.EventMessage || events[0].Text != "adding the handler" || string(events[0].Data) != `{"percent":40}` {
		t.Fatalf("events = %+v", events)
	}
	// Input that does not match the schema is refused and audited.
	res, out = call(t, cs, "report_progress", map[string]any{"message": "x", "percent": 101})
	if !res.IsError || errorCode(out) != jobmcp.CodeInvalidInput {
		t.Fatalf("invalid input = %+v %v", res, out)
	}
	a := e.audits(t, j)
	if len(a) != 2 || a[0]["status"] != "ok" || a[0]["input_bytes"].(float64) == 0 || a[1]["status"] != "error" || a[1]["code"] != "invalid_input" {
		t.Fatalf("audit = %+v", a)
	}
	if _, ok := a[0]["duration_ms"]; !ok {
		t.Fatalf("audit without duration: %+v", a[0])
	}
}

func TestEvidenceAttach(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	_, out := call(t, cs, "evidence_attach", map[string]any{"path": "shots/home.png", "description": "home page"})
	if out["name"] != "evidence/shots/home.png" || out["status"] != "pending" {
		t.Fatalf("attach = %v", out)
	}
	_, out = call(t, cs, "evidence_attach", map[string]any{"path": "report.xml", "name": "reports/unit.xml"})
	if out["name"] != "reports/unit.xml" {
		t.Fatalf("named attach = %v", out)
	}
	_, out = call(t, cs, "evidence_attach", map[string]any{"path": "evidence:shots/a.png"})
	if out["name"] != "evidence/scratch/shots/a.png" {
		t.Fatalf("scratch attach = %v", out)
	}
	for _, bad := range []map[string]any{{"path": "evidence:../x"}, {"path": "evidence:/x"}, {"path": "evidence:"}, {"path": "../etc/passwd"}, {"path": "/etc/passwd"}, {"path": "a/../../b"}, {"path": ".git/config"}, {"path": "x", "name": ".hidden"}} {
		res, out := call(t, cs, "evidence_attach", bad)
		if !res.IsError || errorCode(out) != jobmcp.CodeInvalidInput {
			t.Errorf("attach %v = %v", bad, out)
		}
	}
	got := e.gw.Attachments().Pending(j)
	want := []jobmcp.Attachment{{Path: "shots/home.png", Name: "evidence/shots/home.png"}, {Path: "report.xml", Name: "reports/unit.xml"}, {Path: "evidence:shots/a.png", Name: "evidence/scratch/shots/a.png"}}
	if !slices.Equal(got, want) {
		t.Fatalf("attachments = %+v", got)
	}

	off := newEnv(t, func(c *jobmcp.Config) { c.Evidence = false })
	_, tok = off.lease(t, off.w, allTools...)
	res, out := call(t, off.connect(t, tok, nil), "evidence_attach", map[string]any{"path": "a.png"})
	if !res.IsError || errorCode(out) != jobmcp.CodeUnavailable {
		t.Fatalf("without storage = %v", out)
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	for i := range jobmcp.CallBurst {
		if res, _ := call(t, cs, "report_progress", map[string]any{"message": "x"}); res.IsError {
			t.Fatalf("call %d refused", i)
		}
	}
	res, out := call(t, cs, "report_progress", map[string]any{"message": "x"})
	if !res.IsError || errorCode(out) != jobmcp.CodeRateLimited {
		t.Fatalf("over the burst = %v", out)
	}
	if a := e.audits(t, j); a[len(a)-1]["status"] != "rate_limited" {
		t.Fatalf("audit = %+v", a[len(a)-1])
	}
	// The bucket refills at one call a second; evidence_attach stops at 50
	// calls whatever the rate.
	for i := range 50 {
		e.clock.Advance(time.Second)
		if res, out := call(t, cs, "evidence_attach", map[string]any{"path": "a.txt"}); res.IsError {
			t.Fatalf("attach %d refused: %v", i, out)
		}
	}
	e.clock.Advance(time.Second)
	if res, out := call(t, cs, "evidence_attach", map[string]any{"path": "a.txt"}); !res.IsError || errorCode(out) != jobmcp.CodeRateLimited {
		t.Fatalf("attach 51 = %v", out)
	}
	if res, _ := call(t, cs, "report_progress", map[string]any{"message": "x"}); res.IsError {
		t.Fatal("other tools stay allowed")
	}
}

// TestDeniedCallsCannotFillTheAuditLog: refused and rate-limited calls are
// answered as ever, but past the limit they share one summary row.
func TestDeniedCallsCannotFillTheAuditLog(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, runtime.JobMCPTool("report_progress"))
	cs := e.connect(t, tok, nil)
	for range jobmcp.MaxDeniedAudits + 5 {
		if _, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "ask_human", Arguments: map[string]any{}}); err == nil {
			t.Fatal("a tool the role lacks was called")
		}
	}
	a := e.audits(t, j)
	if len(a) != jobmcp.MaxDeniedAudits+1 || a[0]["status"] != "refused" || a[len(a)-1]["status"] != "audit_limit" {
		t.Fatalf("%d audit rows, last %+v", len(a), a[len(a)-1])
	}
	// Rate-limited calls share the limit; they still get their error, and
	// the calls that do run are still audited.
	for range jobmcp.CallBurst {
		if res, _ := call(t, cs, "report_progress", map[string]any{"message": "x"}); res.IsError {
			t.Fatal("call refused")
		}
	}
	if res, out := call(t, cs, "report_progress", map[string]any{"message": "x"}); !res.IsError || errorCode(out) != jobmcp.CodeRateLimited {
		t.Fatalf("over the rate = %v", out)
	}
	if got := len(e.audits(t, j)); got != jobmcp.MaxDeniedAudits+1+jobmcp.CallBurst {
		t.Fatalf("%d audit rows", got)
	}
}

func TestContractSchemaInlinesRefs(t *testing.T) {
	in, out := jobmcp.ContractSchema("ask_human")
	if strings.Contains(string(in), "$ref") || !strings.Contains(string(in), `"verify_by"`) || !strings.Contains(string(out), `"no_answer"`) {
		t.Fatalf("schemas = %s %s", in, out)
	}
	defer func() {
		if recover() == nil {
			t.Error("unknown tool did not panic")
		}
	}()
	jobmcp.ContractSchema("nope")
}

func TestRegisterRefusesBadTools(t *testing.T) {
	in, out := jobmcp.ContractSchema("report_progress")
	h := func(context.Context, *jobmcp.Call, json.RawMessage) (jobmcp.Result, error) {
		return jobmcp.Result{}, nil
	}
	for name, tool := range map[string]jobmcp.Tool{
		"bad name":   {Name: "Bad.Name", RoleTool: "x", Description: "d", Input: in, Output: out, Handler: h},
		"incomplete": {Name: "x_y", RoleTool: "x.y", Input: in, Output: out, Handler: h},
		"not object": {Name: "x_z", RoleTool: "x.z", Description: "d", Input: json.RawMessage(`{"type":"string"}`), Output: out, Handler: h},
		"duplicate":  {Name: "report_progress", RoleTool: "y", Description: "d", Input: in, Output: out, Handler: h},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			jobmcp.Register(tool)
		}()
	}
	if tools := jobmcp.Tools(); len(tools) < 3 {
		t.Fatalf("tools = %d", len(tools))
	}
}

// failTool fails like a tool whose backend broke.
func init() {
	in, out := jobmcp.ContractSchema("council_read")
	jobmcp.Register(jobmcp.Tool{Name: "test_fail", RoleTool: "test.fail", Description: "Fails.", Input: in, Output: out,
		Handler: func(context.Context, *jobmcp.Call, json.RawMessage) (jobmcp.Result, error) {
			return jobmcp.Result{}, errors.New("database password=hunter2 unreachable")
		}})
}

func TestInternalErrorsStayInTheLog(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, runtime.JobMCPTool("test_fail"))
	res, err := e.connect(t, tok, nil).CallTool(t.Context(), &sdk.CallToolParams{Name: "test_fail", Arguments: map[string]any{}})
	must(t, err)
	text := res.Content[0].(*sdk.TextContent).Text
	if !res.IsError || strings.Contains(text, "hunter2") || !strings.Contains(e.logs.String(), "hunter2") {
		t.Fatalf("result %q, log %s", text, e.logs)
	}
	if a := e.audits(t, j); len(a) != 1 || a[0]["code"] != "internal" {
		t.Fatalf("audit = %+v", a)
	}
}

func TestAskHumanWithoutTask(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	j := domain.Job{AccountID: e.project.AccountID, ProjectID: e.project.ID, Role: "architect", Runtime: "claude-code", Spec: domain.JobSpec{AllowedTools: allTools}}
	must(t, e.st.Jobs().Create(ctx, &j))
	_, tok := e.relet(t, j, e.w)
	done, g := e.asked(t, e.connect(t, tok, nil), ask("1m"))
	if g.SubjectRepo != "acme/app" || g.SubjectNumber != 0 || g.TaskID != j.TaskID {
		t.Fatalf("gate = %+v", g)
	}
	e.fire <- t0
	wait(t, done)
}
