package models

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/schema"
	"github.com/forecrew/forecrew/internal/store"
)

// TestRole is the role of a suitability test's job. It is no role of the
// packs: the job runs on the connection and model it names, never on a
// role's assignment, and only on the fixture repo.
const TestRole = "model-test"

// Limits of a suitability test.
const (
	TestMaxTurns = 15
	TestTimeout  = 5 * time.Minute
)

// Tokens a suitability test is estimated to take.
const (
	testInputTokens  = 60000
	testOutputTokens = 4000
)

// TestTool is the job MCP tool a suitability test calls.
const TestTool = "report_progress"

// auditCall is the audit action of a job MCP call (jobmcp.AuditCall).
const auditCall = "job_mcp.call"

// TestPrompt is what the agent of a suitability test is asked. The fixture is
// packs/fixtures/model-test, which the worker writes the code word into.
const TestPrompt = `This is a short test of what you can do here. The work tree is a small fixture, not a real project. Do these five things, then answer.

1. Read the file notes.txt. After "code word:" it holds one word. Put that word in code_word.
2. Change greeting.txt so that it holds exactly one line: hello, forecrew
3. Run the command ` + "`sh check.sh`" + ` in the work tree, and put the line it prints in command_output.
4. Call the tool report_progress of the forecrew MCP server with the message: suitability test
5. Answer with JSON that matches the output schema: code_word, command_output, and done set to true.
`

// TestSchema is the output schema of a suitability test.
var TestSchema = json.RawMessage(`{"type":"object","required":["code_word","command_output","done"],"additionalProperties":false,` +
	`"properties":{"code_word":{"type":"string"},"command_output":{"type":"string"},"done":{"type":"boolean"}}}`)

// TestTools are the tools the agent of a suitability test may use.
var TestTools = []string{"Bash", "Edit", "Glob", "Grep", "Read", "Write", runtime.JobMCPTool(TestTool)}

// Errors of the checks.
var (
	// ErrNoProject means the organization has no project for the check's
	// job to belong to.
	ErrNoProject = errors.New("no project")
	// ErrUnknownConnection means the organization has no such connection.
	ErrUnknownConnection = errors.New("unknown connection")
)

// JobCreator creates jobs, in the transaction of st; *jobs.Service is one.
type JobCreator interface {
	CreateIn(ctx context.Context, st store.Store, by domain.Actor, j domain.Job) (domain.Job, error)
}

// Checker runs key checks and suitability tests, and records what they find
// when their jobs end (it is a jobs.Registrar).
type Checker struct {
	st   store.Store
	jobs JobCreator
	cat  *Catalog
}

// NewChecker returns a checker; cat is the model catalog, nil for the one
// this binary ships with.
func NewChecker(st store.Store, jobs JobCreator, cat *Catalog) *Checker {
	if cat == nil {
		cat, _ = DefaultCatalog() // embedded and tested; a nil catalog knows no model
	}
	return &Checker{st: st, jobs: jobs, cat: cat}
}

// connection returns the organization's connection called name.
func connection(ctx context.Context, st store.Store, accountID uuid.UUID, name string) (domain.ModelConnection, error) {
	rows, err := st.ModelConnections().List(ctx, accountID)
	if err != nil {
		return domain.ModelConnection{}, err
	}
	i := slices.IndexFunc(rows, func(c domain.ModelConnection) bool { return c.Name == name })
	if i < 0 {
		return domain.ModelConnection{}, fmt.Errorf("connection %s: %w", name, ErrUnknownConnection)
	}
	return rows[i], nil
}

// home is the project a check's job belongs to, because jobs need one: the
// organization's first. The job never sees its repo.
func home(ctx context.Context, st store.Store, accountID uuid.UUID) (domain.Project, error) {
	projects, err := st.Projects().List(ctx, accountID)
	if err != nil {
		return domain.Project{}, err
	}
	if len(projects) == 0 {
		return domain.Project{}, fmt.Errorf("checks run as jobs of a project, and the organization has none yet: %w", ErrNoProject)
	}
	return projects[0], nil
}

// Check starts the key check of a connection: a model_check job in the
// control pool, on a worker that keeps its own keys away from it.
func (c *Checker) Check(ctx context.Context, by domain.Actor, name string) (domain.ModelCheck, error) {
	var out domain.ModelCheck
	err := c.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		out, err = c.check(ctx, tx, by, name)
		return err
	})
	return out, err
}

func (c *Checker) check(ctx context.Context, tx store.Store, by domain.Actor, name string) (domain.ModelCheck, error) {
	conn, err := connection(ctx, tx, by.AccountID, name)
	if err != nil {
		return domain.ModelCheck{}, err
	}
	p, err := home(ctx, tx, by.AccountID)
	if err != nil {
		return domain.ModelCheck{}, err
	}
	j, err := c.jobs.CreateIn(ctx, tx, by, domain.Job{AccountID: by.AccountID, ProjectID: p.ID, Kind: domain.JobKindModelCheck, Pool: domain.PoolControl,
		Needs: []string{domain.OrganizationModelLabel, domain.ModelCheckLabel}, MaxAttempts: 2,
		Spec: domain.JobSpec{Repo: p.Repo, BaseRef: p.DefaultBranch, ModelCheck: &domain.ModelCheckSpec{Connection: conn.Name, Adapter: conn.Adapter}}})
	if err != nil {
		return domain.ModelCheck{}, err
	}
	row := domain.ModelCheck{AccountID: by.AccountID, ConnectionID: conn.ID, Connection: conn.Name, JobID: j.ID, Status: domain.ModelCheckQueued, CreatedBy: by.String()}
	return row, tx.ModelChecks().Put(ctx, &row)
}

// Estimate is what a suitability test is expected to cost.
type Estimate struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CostUSD is nil when the price of the model is unknown.
	CostUSD *float64 `json:"cost_usd"`
	Basis   string   `json:"basis"`
}

// EstimateTest estimates a suitability test of model on connection conn.
func (c *Checker) EstimateTest(conn domain.ModelConnection, model string) Estimate {
	e := Estimate{InputTokens: testInputTokens, OutputTokens: testOutputTokens}
	price, ok := c.price(conn, model)
	switch {
	case LocalEndpoint(conn.BaseURL):
		zero := 0.0
		e.CostUSD, e.Basis = &zero, "the model runs on your own network"
	case !ok:
		e.Basis = "the catalog does not know the price of " + model + ": enter it on the connection"
	default:
		cost := (float64(e.InputTokens)*price.Input + float64(e.OutputTokens)*price.Output) / 1e6
		e.CostUSD, e.Basis = &cost, fmt.Sprintf("about %d input and %d output tokens at the price of the model catalog", e.InputTokens, e.OutputTokens)
	}
	return e
}

// price is the price of model: entered on the connection, or the catalog's.
func (c *Checker) price(conn domain.ModelConnection, model string) (Price, bool) {
	if info, ok := conn.Models[model]; ok && info.Price != nil {
		return Price{Input: info.Price.Input, Output: info.Price.Output}, true
	}
	m, ok := c.cat.Model(model)
	return m.Price, ok
}

// Test starts the suitability test of model on a connection: a short agent
// job on the fixture repo, at most TestMaxTurns turns and TestTimeout.
func (c *Checker) Test(ctx context.Context, by domain.Actor, name, model string) (domain.ModelCheck, error) {
	var out domain.ModelCheck
	err := c.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		conn, err := connection(ctx, tx, by.AccountID, name)
		if err != nil {
			return err
		}
		out, err = c.test(ctx, tx, by, conn, model)
		return err
	})
	return out, err
}

func (c *Checker) test(ctx context.Context, tx store.Store, by domain.Actor, conn domain.ModelConnection, model string) (domain.ModelCheck, error) {
	if !conn.Enabled {
		return domain.ModelCheck{}, fmt.Errorf("connection %s is turned off: %w", conn.Name, ErrUnknownConnection)
	}
	p, err := home(ctx, tx, by.AccountID)
	if err != nil {
		return domain.ModelCheck{}, err
	}
	j, err := c.jobs.CreateIn(ctx, tx, by, domain.Job{AccountID: by.AccountID, ProjectID: p.ID, Kind: domain.JobKindAgent, Pool: domain.PoolControl,
		Role: TestRole, Runtime: conn.Adapter, MaxAttempts: 1, Needs: []string{domain.ModelCheckLabel},
		Spec: domain.JobSpec{Repo: p.Repo, BaseRef: p.DefaultBranch, Model: model, Prompt: TestPrompt, AllowedTools: slices.Clone(TestTools),
			MaxTurns: TestMaxTurns, Timeout: domain.Duration(TestTimeout), OrganizationModel: true,
			Suitability: &domain.SuitabilitySpec{Connection: conn.Name, Nonce: nonce(), Schema: TestSchema}}})
	if err != nil {
		return domain.ModelCheck{}, err
	}
	row := domain.ModelCheck{AccountID: by.AccountID, ConnectionID: conn.ID, Connection: conn.Name, Model: model, JobID: j.ID, Status: domain.SuitabilityQueued, CreatedBy: by.String()}
	return row, tx.ModelChecks().Put(ctx, &row)
}

// nonce is the code word of a test.
func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand never fails
	return "fc" + hex.EncodeToString(b)
}

// Results lists the checks and tests of an organization.
func (c *Checker) Results(ctx context.Context, accountID uuid.UUID) ([]domain.ModelCheck, error) {
	return c.st.ModelChecks().List(ctx, accountID)
}

// JobCompleted implements jobs.Registrar: it records what a check or a test
// found.
func (c *Checker) JobCompleted(ctx context.Context, tx store.Store, j domain.Job) error {
	return c.ended(ctx, tx, j)
}

// JobFailed implements jobs.FailureRegistrar.
func (c *Checker) JobFailed(ctx context.Context, tx store.Store, j domain.Job) error {
	return c.ended(ctx, tx, j)
}

func (c *Checker) ended(ctx context.Context, tx store.Store, j domain.Job) error {
	if j.Kind != domain.JobKindModelCheck && j.Spec.Suitability == nil {
		return nil
	}
	row, err := tx.ModelChecks().GetByJob(ctx, j.AccountID, j.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // a newer check took its place, or its connection went
	}
	if err != nil {
		return err
	}
	if j.Kind == domain.JobKindModelCheck {
		row.Key = keyResult(j)
		row.Status = row.Key.Status
		if err := tx.ModelChecks().Put(ctx, &row); err != nil {
			return err
		}
		return c.rerun(ctx, tx, j, row)
	}
	row.Test, err = c.grade(ctx, tx, j)
	if err != nil {
		return err
	}
	row.Status, row.Stale = domain.SuitabilityDone, false
	if j.Status != domain.JobSucceeded {
		row.Status = domain.SuitabilityFailed
	}
	return tx.ModelChecks().Put(ctx, &row)
}

// keyStatuses are the statuses a worker may report.
var keyStatuses = []string{domain.ModelCheckReachable, domain.ModelCheckRefused, domain.ModelCheckUnreachable, domain.ModelCheckWrongAddress, domain.ModelCheckUnsupported}

// keyResult reads the output of a model_check job. The worker keeps model
// names plain; they are checked again here, and a detail is never the
// provider's text.
func keyResult(j domain.Job) *domain.ModelCheckResult {
	failed := &domain.ModelCheckResult{Status: domain.ModelCheckFailed, Detail: "the check did not finish: see its job " + j.Ref()}
	var r domain.ModelCheckResult
	if j.Status != domain.JobSucceeded || json.Unmarshal(j.Output, &r) != nil || !slices.Contains(keyStatuses, r.Status) {
		return failed
	}
	r.Models = slices.DeleteFunc(r.Models, func(m string) bool { return len(m) > maxModelName || !modelName.MatchString(m) })
	if len(r.Models) > maxProbeModels {
		r.Models = r.Models[:maxProbeModels]
	}
	r.Detail = oneLine(r.Detail, 300)
	r.AgentVersion = oneLine(r.AgentVersion, 100)
	return &r
}

// oneLine keeps the first line of s, at most n bytes.
func oneLine(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "")
	}
	return s
}

// rerun runs again the tests of a connection that an older agent version
// passed or failed: a key check found that workers run another version now.
func (c *Checker) rerun(ctx context.Context, tx store.Store, j domain.Job, key domain.ModelCheck) error {
	version := key.Key.AgentVersion
	if version == "" {
		return nil
	}
	rows, err := tx.ModelChecks().List(ctx, j.AccountID)
	if err != nil {
		return err
	}
	conn, err := connection(ctx, tx, j.AccountID, key.Connection)
	if errors.Is(err, ErrUnknownConnection) {
		return nil
	}
	if err != nil {
		return err
	}
	by := domain.Actor{AccountID: j.AccountID, Kind: domain.ActorSystem, Name: "model-checks"}
	for _, r := range rows {
		if r.ConnectionID != key.ConnectionID || r.Model == "" || r.Test == nil || r.Test.AgentVersion == "" || r.Test.AgentVersion == version || r.Stale {
			continue
		}
		// The old result stays, marked stale, until the new test ends.
		next, err := c.test(ctx, tx, by, conn, r.Model)
		switch {
		case errors.Is(err, ErrUnknownConnection): // turned off: nothing runs
			r.Stale = true
		case err != nil:
			return fmt.Errorf("test %s again: %w", r.Model, err)
		default:
			r.JobID, r.Status, r.Stale, r.CreatedBy = next.JobID, next.Status, true, next.CreatedBy
		}
		if err := tx.ModelChecks().Put(ctx, &r); err != nil {
			return err
		}
	}
	return nil
}

// grade turns what the worker reported of a test, and what the job MCP
// gateway saw, into abilities and a level.
func (c *Checker) grade(ctx context.Context, tx store.Store, j domain.Job) (*domain.SuitabilityResult, error) {
	res := &domain.SuitabilityResult{Abilities: map[string]bool{}, Level: domain.LevelNone,
		Tokens: j.Usage.InputTokens + j.Usage.OutputTokens + j.Usage.CacheReadTokens + j.Usage.CacheWriteTokens, CostUSD: j.CostUSD}
	for _, a := range domain.Abilities() {
		res.Abilities[a] = false
	}
	if !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		res.DurationMS = j.FinishedAt.Sub(j.StartedAt).Milliseconds()
	}
	var rep domain.SuitabilityReport
	if j.Status != domain.JobSucceeded || json.Unmarshal(j.Output, &rep) != nil {
		res.Error = "the test did not finish: see its job " + j.Ref()
		return res, nil
	}
	res.Turns, res.AgentVersion = rep.Turns, oneLine(rep.AgentVersion, 100)
	if rep.Turns > TestMaxTurns || time.Duration(res.DurationMS)*time.Millisecond > TestTimeout+time.Minute {
		res.Error = fmt.Sprintf("the test went past its limits of %d turns and %s", TestMaxTurns, TestTimeout)
		return res, nil
	}
	var out struct {
		CodeWord string `json:"code_word"`
		Done     bool   `json:"done"`
	}
	answered := len(rep.Output) > 0 && schema.Check("suitability", string(j.Spec.Suitability.Schema), mustDecode(rep.Output)) == nil && json.Unmarshal(rep.Output, &out) == nil
	res.Abilities[domain.AbilityJSON] = answered && out.Done
	res.Abilities[domain.AbilityRead] = rep.Read || (answered && out.CodeWord == j.Spec.Suitability.Nonce)
	res.Abilities[domain.AbilityEdit] = rep.Edited
	res.Abilities[domain.AbilityCommand] = rep.Ran
	calls, err := tx.Audit().List(ctx, j.AccountID, store.AuditFilter{SubjectID: j.ID, Action: auditCall, Limit: 200})
	if err != nil {
		return nil, err
	}
	res.Abilities[domain.AbilityMCP] = slices.ContainsFunc(calls, func(e domain.AuditEntry) bool {
		var d struct{ Tool, Status string }
		return json.Unmarshal(e.Data, &d) == nil && d.Tool == TestTool && d.Status == "ok"
	})
	res.Level = domain.LevelOf(res.Abilities)
	if rep.Error != "" {
		res.Error = oneLine(rep.Error, 300)
	}
	return res, nil
}

// mustDecode decodes JSON for schema.Check; what does not decode fails it.
func mustDecode(raw json.RawMessage) any {
	v, err := schema.DecodeJSON(raw)
	if err != nil {
		return struct{}{}
	}
	return v
}

// CheckGrant fills in a model_check job at lease from its connection as it is
// now: the address of its list and its key, which goes there and nowhere
// else. waiting says why the job cannot run now.
func (s *Service) CheckGrant(ctx context.Context, j domain.Job) (spec domain.ModelCheckSpec, env map[string]string, waitingFor string, err error) {
	if j.Spec.ModelCheck == nil {
		return spec, nil, "the job checks no connection", nil
	}
	c, err := connection(ctx, s.st, j.AccountID, j.Spec.ModelCheck.Connection)
	if errors.Is(err, ErrUnknownConnection) {
		return spec, nil, "connection " + j.Spec.ModelCheck.Connection + " is gone", nil
	}
	if err != nil {
		return spec, nil, "", err
	}
	d, key, err := s.key(ctx, j.AccountID, Decision{Source: SourceOrganization, Connection: c.Name, connection: c})
	if err != nil || d.Waiting != "" {
		return spec, nil, d.Waiting, err
	}
	cat, _ := DefaultCatalog() // without it, the adapter's defaults
	spec = ListOf(cat, c)
	spec.KeyEnv = cmp.Or(c.KeyEnv, runtime.KeyEnv(c.Adapter))
	return spec, map[string]string{spec.KeyEnv: string(key)}, "", nil
}

// ListOf is where and how the models of connection c are listed: its
// base_url, or else where its agent sends the key (the catalog's provider of
// the vendor for opencode), and the provider's path. URL is empty when the
// provider has no list.
func ListOf(cat *Catalog, c domain.ModelConnection) domain.ModelCheckSpec {
	out := domain.ModelCheckSpec{Connection: c.Name, Adapter: c.Adapter}
	p, known := cat.Provider(c.Vendor)
	// Without a base_url, Claude Code and Codex send the key to their own
	// vendor whatever the connection's vendor says: so does the check.
	if own := map[string]string{config.AdapterClaudeCode: "anthropic", config.AdapterCodex: "openai"}[c.Adapter]; c.BaseURL == "" && own != "" {
		p, known = cat.Provider(own)
	}
	list := p.ListEndpoint
	base := cmp.Or(c.BaseURL, p.BaseURL)
	if !known {
		// What the agent itself speaks.
		switch c.Adapter {
		case config.AdapterClaudeCode:
			list, base = &ListEndpoint{Path: "/v1/models", Auth: AuthXAPIKey, Format: FormatAnthropic}, cmp.Or(c.BaseURL, "https://api.anthropic.com")
		case config.AdapterCodex:
			list, base = &ListEndpoint{Path: "/models", Auth: AuthBearer, Format: FormatOpenAI}, cmp.Or(c.BaseURL, "https://api.openai.com/v1")
		default:
			list = &ListEndpoint{Path: "/models", Auth: AuthBearer, Format: FormatOpenAI}
		}
	}
	if list == nil || base == "" {
		return out
	}
	out.URL, out.Auth, out.Format = strings.TrimRight(base, "/")+list.Path, list.Auth, list.Format
	return out
}

// testGrant is the grant of a suitability test: the connection and model it
// names, whatever the role's assignment. Its code is the fixture's, so the
// private code policy has nothing to say.
func (s *Service) testGrant(ctx context.Context, j domain.Job) (Grant, error) {
	c, err := connection(ctx, s.st, j.AccountID, j.Spec.Suitability.Connection)
	switch {
	case errors.Is(err, ErrUnknownConnection):
		return Grant{Waiting: "Connection " + j.Spec.Suitability.Connection + " is gone"}, nil
	case err != nil:
		return Grant{}, err
	case !c.Enabled:
		return Grant{Waiting: "Connection " + c.Name + " is turned off"}, nil
	case c.Adapter != j.Runtime:
		return Grant{Waiting: "Connection " + c.Name + " moved to " + c.Adapter + ": test it again"}, nil
	}
	d, key, err := s.key(ctx, j.AccountID, Decision{Source: SourceOrganization, Connection: c.Name, Adapter: c.Adapter, connection: c})
	if err != nil || d.Waiting != "" {
		return Grant{Waiting: d.Waiting}, err
	}
	name := cmp.Or(c.KeyEnv, runtime.KeyEnv(c.Adapter))
	g := Grant{Connection: c.Name, Env: map[string]string{name: string(key)}, Organization: true}
	if c.BaseURL != "" {
		g.Endpoint = &domain.Endpoint{BaseURL: c.BaseURL, KeyEnv: name, Organization: true}
	}
	return g, nil
}
