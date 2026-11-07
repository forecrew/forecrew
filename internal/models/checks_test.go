package models_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// checks is a world with a checker over a real job service.
type checks struct {
	*world
	jobs    *jobs.Service
	checker *models.Checker
	svc     *models.Service
	by      domain.Actor
}

func newChecks(t *testing.T) *checks {
	t.Helper()
	w := newWorld(t)
	js := jobs.New(w.st, testkit.NewSignalBus(w.st), nil, slog.New(slog.DiscardHandler), time.Now)
	cat, err := models.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	c := &checks{world: w, jobs: js, checker: models.NewChecker(w.st, js, cat), svc: models.New(w.st, hosted(fileConfig()), w.vault, nil),
		by: domain.Actor{AccountID: w.account, Kind: domain.ActorUser, Name: "ana"}}
	js.WithRegistrar(c.checker)
	return c
}

// add stores a connection with its key.
func (c *checks) add(t *testing.T, conn domain.ModelConnection) domain.ModelConnection {
	t.Helper()
	conn.AccountID, conn.UpdatedBy = c.account, "user:ana"
	if err := c.st.ModelConnections().Put(t.Context(), &conn); err != nil {
		t.Fatal(err)
	}
	name, _ := domain.StoreSecretName(conn.SecretRef)
	if err := c.vault.Put(t.Context(), domain.Actor{}, c.account, name, []byte("sk-key")); err != nil {
		t.Fatal(err)
	}
	return conn
}

// finish leases job id on a worker and completes it with output.
func (c *checks) finish(t *testing.T, id uuid.UUID, status domain.JobStatus, output any, change func(*store.JobResult)) domain.Job {
	t.Helper()
	ctx := t.Context()
	w := domain.Worker{AccountID: c.account, Name: "w-" + id.String(), Pool: domain.PoolControl, MaxJobs: 1, Labels: []string{domain.OrganizationModelLabel, domain.ModelCheckLabel, "runtime:claude-code"}}
	if err := c.st.Workers().Create(ctx, &w); err != nil {
		t.Fatal(err)
	}
	j, err := queue.New(c.st.Jobs()).Lease(ctx, c.account, w.ID, w.Labels)
	if err != nil || j.ID != id {
		t.Fatalf("lease = %+v, %v", j, err)
	}
	raw, _ := json.Marshal(output)
	r := store.JobResult{Status: status, Output: raw, Usage: domain.Usage{InputTokens: 1000, OutputTokens: 100}, CostUSD: 0.01}
	if change != nil {
		change(&r)
	}
	done, err := c.jobs.Complete(ctx, w, id, r)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func (c *checks) row(t *testing.T, model string) domain.ModelCheck {
	t.Helper()
	rows, err := c.checker.Results(t.Context(), c.account)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Model == model {
			return r
		}
	}
	t.Fatalf("no check of %q in %+v", model, rows)
	return domain.ModelCheck{}
}

func TestKeyCheckIsAJobOnAControlWorker(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))
	row, err := c.checker.Check(ctx, c.by, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := c.st.Jobs().Get(ctx, c.account, row.JobID)
	if j.Kind != domain.JobKindModelCheck || j.Pool != domain.PoolControl || j.Spec.ModelCheck.Connection != "cloud" || j.Spec.Env != nil ||
		!slicesHave(j.Needs, domain.OrganizationModelLabel, domain.ModelCheckLabel) || row.Status != domain.ModelCheckQueued {
		t.Fatalf("job = %+v, row %+v", j, row)
	}
	c.finish(t, j.ID, domain.JobSucceeded, domain.ModelCheckResult{Status: domain.ModelCheckReachable, Models: []string{"claude-sonnet-4-5", "evil\nname"}, AgentVersion: "2.0"}, nil)
	got := c.row(t, "")
	if got.Status != domain.ModelCheckReachable || len(got.Key.Models) != 1 || got.Key.AgentVersion != "2.0" {
		t.Fatalf("stored = %+v", got.Key)
	}

	// A check that fails, or says what it may not, fails.
	for _, out := range []any{domain.ModelCheckResult{Status: "great"}, "not json"} {
		row, err := c.checker.Check(ctx, c.by, "cloud")
		if err != nil {
			t.Fatal(err)
		}
		c.finish(t, row.JobID, domain.JobSucceeded, out, nil)
		if got := c.row(t, ""); got.Status != domain.ModelCheckFailed || !strings.Contains(got.Key.Detail, "JOB-") {
			t.Fatalf("stored = %+v", got.Key)
		}
	}
	if _, err := c.checker.Check(ctx, c.by, "nothing"); !errors.Is(err, models.ErrUnknownConnection) {
		t.Fatalf("unknown = %v", err)
	}
}

func TestChecksNeedAProject(t *testing.T) {
	c := newChecks(t)
	other := domain.Account{Name: "empty"}
	if err := c.st.Accounts().Create(t.Context(), &other); err != nil {
		t.Fatal(err)
	}
	cn := conn("cloud", config.AdapterClaudeCode, "anthropic")
	cn.AccountID, cn.UpdatedBy = other.ID, "user:ana"
	if err := c.st.ModelConnections().Put(t.Context(), &cn); err != nil {
		t.Fatal(err)
	}
	if _, err := c.checker.Check(t.Context(), domain.Actor{AccountID: other.ID, Kind: domain.ActorUser, Name: "x"}, "cloud"); !errors.Is(err, models.ErrNoProject) {
		t.Fatalf("err = %v", err)
	}
}

func slicesHave(have []string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(strings.Join(have, ","), w) {
			return false
		}
	}
	return true
}

func TestCheckGrantSendsTheKeyToTheConnectionOnly(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	proxy := conn("proxy", config.AdapterClaudeCode, "anthropic")
	proxy.BaseURL = "https://gateway.example.com/anthropic/"
	for _, tt := range []struct {
		conn              domain.ModelConnection
		url, auth, keyEnv string
	}{
		{conn("cloud", config.AdapterClaudeCode, "anthropic"), "https://api.anthropic.com/v1/models", models.AuthXAPIKey, "ANTHROPIC_API_KEY"},
		{proxy, "https://gateway.example.com/anthropic/v1/models", models.AuthXAPIKey, "ANTHROPIC_API_KEY"},
		{conn("gpt", config.AdapterCodex, "someone"), "https://api.openai.com/v1/models", models.AuthBearer, "CODEX_API_KEY"},
		{conn("mixed", config.AdapterClaudeCode, "openai"), "https://api.anthropic.com/v1/models", models.AuthXAPIKey, "ANTHROPIC_API_KEY"},
		{domain.ModelConnection{Name: "local", Adapter: config.AdapterOpencode, Vendor: "ollama", KeyEnv: "OLLAMA_API_KEY", BaseURL: "http://10.0.0.5:11434/v1", SecretRef: "store:models/local", Enabled: true},
			"http://10.0.0.5:11434/v1/models", models.AuthNone, "OLLAMA_API_KEY"},
		{domain.ModelConnection{Name: "bedrock", Adapter: config.AdapterOpencode, Vendor: "bedrock", KeyEnv: "BEDROCK_API_KEY", BaseURL: "https://bedrock.example.com/openai/v1", SecretRef: "store:models/bedrock", Enabled: true},
			"", "", "BEDROCK_API_KEY"},
	} {
		cn := c.add(t, tt.conn)
		row, err := c.checker.Check(ctx, c.by, cn.Name)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := c.st.Jobs().Get(ctx, c.account, row.JobID)
		spec, env, waiting, err := c.svc.CheckGrant(ctx, j)
		if err != nil || waiting != "" || spec.URL != tt.url || spec.Auth != tt.auth || spec.KeyEnv != tt.keyEnv || !maps.Equal(env, map[string]string{tt.keyEnv: "sk-key"}) {
			t.Errorf("%s: spec %+v env %v waiting %q err %v", cn.Name, spec, env, waiting, err)
		}
	}
	// A connection gone, or without its key, waits.
	j := domain.Job{AccountID: c.account, Spec: domain.JobSpec{ModelCheck: &domain.ModelCheckSpec{Connection: "gone"}}}
	if _, _, waiting, err := c.svc.CheckGrant(ctx, j); err != nil || !strings.Contains(waiting, "gone") {
		t.Fatalf("gone = %q, %v", waiting, err)
	}
	nokey := conn("nokey", config.AdapterClaudeCode, "anthropic")
	nokey.AccountID, nokey.UpdatedBy = c.account, "user:ana"
	if err := c.st.ModelConnections().Put(ctx, &nokey); err != nil {
		t.Fatal(err)
	}
	j.Spec.ModelCheck.Connection = "nokey"
	if _, env, waiting, err := c.svc.CheckGrant(ctx, j); err != nil || waiting == "" || env != nil {
		t.Fatalf("no key = %q, %v", waiting, err)
	}
	if _, _, waiting, _ := c.svc.CheckGrant(ctx, domain.Job{}); waiting == "" {
		t.Fatal("no check")
	}
}

// answer is what a scripted agent that does everything reports.
func answer(nonce string) domain.SuitabilityReport {
	return domain.SuitabilityReport{Output: json.RawMessage(`{"code_word":"` + nonce + `","command_output":"check: ` + nonce + `-ran","done":true}`),
		Read: true, Edited: true, Ran: true, Turns: 7, AgentVersion: "2.0"}
}

// runTest starts a test of model m and ends it with what change makes of a
// full answer, calling the MCP tool when mcp is set.
func (c *checks) runTest(t *testing.T, change func(*domain.SuitabilityReport), mcp bool) domain.ModelCheck {
	t.Helper()
	return c.runTestOf(t, "cloud", change, mcp)
}

// runTestOn runs a test that passes on connection name.
func (c *checks) runTestOn(t *testing.T, name string) {
	t.Helper()
	c.runTestOf(t, name, nil, true)
}

func (c *checks) runTestOf(t *testing.T, name string, change func(*domain.SuitabilityReport), mcp bool) domain.ModelCheck {
	t.Helper()
	ctx := t.Context()
	row, err := c.checker.Test(ctx, c.by, name, "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := c.st.Jobs().Get(ctx, c.account, row.JobID)
	rep := answer(j.Spec.Suitability.Nonce)
	if change != nil {
		change(&rep)
	}
	if mcp {
		data, _ := json.Marshal(map[string]any{"tool": models.TestTool, "status": "ok"})
		if err := c.st.Audit().Append(ctx, &domain.AuditEntry{AccountID: c.account, ProjectID: j.ProjectID, Actor: "worker:w", Action: "job_mcp.call", SubjectType: "job", SubjectID: j.ID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	c.finish(t, j.ID, domain.JobSucceeded, rep, nil)
	rows, err := c.checker.Results(ctx, c.account)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Connection == name && r.Model == "claude-sonnet-4-5" {
			return r
		}
	}
	t.Fatalf("no test on %s", name)
	return domain.ModelCheck{}
}

func TestSuitabilityTest(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))

	row, err := c.checker.Test(ctx, c.by, "cloud", "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := c.st.Jobs().Get(ctx, c.account, row.JobID)
	if j.Role != models.TestRole || j.Runtime != config.AdapterClaudeCode || j.Spec.MaxTurns != models.TestMaxTurns || time.Duration(j.Spec.Timeout) != models.TestTimeout ||
		j.MaxAttempts != 1 || !j.Spec.OrganizationModel || j.Spec.Suitability.Nonce == "" || j.Spec.Model != "claude-sonnet-4-5" || len(j.Spec.OutputSchema) != 0 {
		t.Fatalf("job = %+v", j)
	}
	// Its grant is its connection's, whatever the role's assignment.
	g, err := c.svc.Grant(ctx, j)
	if err != nil || g.Waiting != "" || g.Connection != "cloud" || g.Env["ANTHROPIC_API_KEY"] != "sk-key" || !g.Organization {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	if _, _, ok, _ := c.svc.WaitReason(ctx, j); ok {
		t.Fatal("a test waits for a role's connection")
	}
	if err := c.svc.Settle(ctx, c.account); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.st.Jobs().Get(ctx, c.account, j.ID); got.Runtime != config.AdapterClaudeCode {
		t.Fatalf("settled = %+v", got)
	}
	c.finish(t, j.ID, domain.JobSucceeded, answer(j.Spec.Suitability.Nonce), nil) // no MCP call
}

func TestSuitabilityAbilities(t *testing.T) {
	c := newChecks(t)
	c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))
	all := c.runTest(t, nil, true)
	if all.Status != domain.SuitabilityDone || all.Test.Level != domain.LevelCode || all.Test.Tokens != 1100 || all.Test.Turns != 7 || all.Test.AgentVersion != "2.0" {
		t.Fatalf("all = %+v %+v", all, all.Test)
	}
	for _, a := range domain.Abilities() {
		if !all.Test.Abilities[a] {
			t.Errorf("%s failed", a)
		}
	}

	// Each ability failing alone.
	alone := map[string]struct {
		change func(*domain.SuitabilityReport)
		mcp    bool
		level  string
	}{
		domain.AbilityRead: {func(r *domain.SuitabilityReport) {
			r.Output, r.Read = json.RawMessage(`{"code_word":"guess","command_output":"x","done":true}`), false
		}, true, domain.LevelNone},
		domain.AbilityEdit:    {func(r *domain.SuitabilityReport) { r.Edited = false }, true, domain.LevelRead},
		domain.AbilityCommand: {func(r *domain.SuitabilityReport) { r.Ran = false }, true, domain.LevelRead},
		domain.AbilityMCP:     {nil, false, domain.LevelNone},
		domain.AbilityJSON: {func(r *domain.SuitabilityReport) {
			r.Output, r.Error = nil, "output does not match schema"
		}, true, domain.LevelNone},
	}
	for ability, tt := range alone {
		got := c.runTest(t, tt.change, tt.mcp)
		for _, a := range domain.Abilities() {
			if got.Test.Abilities[a] != (a != ability) {
				t.Errorf("%s alone: %s = %v", ability, a, got.Test.Abilities[a])
			}
		}
		if got.Test.Level != tt.level {
			t.Errorf("%s alone: level %s, want %s", ability, got.Test.Level, tt.level)
		}
	}

	// The limits hold: past 15 turns nothing counts.
	over := c.runTest(t, func(r *domain.SuitabilityReport) { r.Turns = 16 }, true)
	if over.Test.Level != domain.LevelNone || !strings.Contains(over.Test.Error, "15 turns") {
		t.Fatalf("over = %+v", over.Test)
	}
}

func TestATestThatFailsAndTheLevelPerRole(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))
	row, err := c.checker.Test(ctx, c.by, "cloud", "m")
	if err != nil {
		t.Fatal(err)
	}
	c.finish(t, row.JobID, domain.JobFailed, nil, func(r *store.JobResult) { r.Error = "boom" })
	got := c.row(t, "m")
	if got.Status != domain.SuitabilityFailed || got.Test.Level != domain.LevelNone || !strings.Contains(got.Test.Error, "did not finish") {
		t.Fatalf("failed = %+v %+v", got, got.Test)
	}
	for _, tt := range []struct {
		level         string
		reads, writes bool
	}{{domain.LevelCode, true, true}, {domain.LevelRead, true, false}, {domain.LevelNone, false, false}} {
		if domain.Serves(tt.level, false) != tt.reads || domain.Serves(tt.level, true) != tt.writes {
			t.Errorf("level %s", tt.level)
		}
	}
	// A turned-off connection is not tested.
	off := conn("off", config.AdapterClaudeCode, "anthropic")
	off.Enabled = false
	c.add(t, off)
	if _, err := c.checker.Test(ctx, c.by, "off", "m"); err == nil {
		t.Fatal("tested a connection that is off")
	}
}

func TestTestGrantWaitsWhenTheConnectionChanged(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	cn := c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))
	row, err := c.checker.Test(ctx, c.by, "cloud", "m")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := c.st.Jobs().Get(ctx, c.account, row.JobID)
	for change, want := range map[string]func(*domain.ModelConnection){
		"moved to codex": func(x *domain.ModelConnection) { x.Adapter = config.AdapterCodex },
		"turned off":     func(x *domain.ModelConnection) { x.Enabled = false },
	} {
		x := cn
		want(&x)
		if err := c.st.ModelConnections().Put(ctx, &x); err != nil {
			t.Fatal(err)
		}
		if g, err := c.svc.Grant(ctx, j); err != nil || !strings.Contains(g.Waiting, strings.Fields(change)[0]) {
			t.Errorf("%s: grant = %+v, %v", change, g, err)
		}
	}
	if err := c.st.ModelConnections().Delete(ctx, c.account, "cloud"); err != nil {
		t.Fatal(err)
	}
	if g, _ := c.svc.Grant(ctx, j); !strings.Contains(g.Waiting, "gone") {
		t.Errorf("gone: %+v", g)
	}
}

// TestANewAgentVersionTestsAgain: a key check that finds another agent
// version runs the tests of the connection again.
func TestANewAgentVersionTestsAgain(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))
	first := c.runTest(t, nil, true)
	row, err := c.checker.Check(ctx, c.by, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	c.finish(t, row.JobID, domain.JobSucceeded, domain.ModelCheckResult{Status: domain.ModelCheckReachable, AgentVersion: "2.0"}, nil)
	if got := c.row(t, "claude-sonnet-4-5"); got.JobID != first.JobID {
		t.Fatalf("same version tested again: %+v", got)
	}
	row, err = c.checker.Check(ctx, c.by, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	c.finish(t, row.JobID, domain.JobSucceeded, domain.ModelCheckResult{Status: domain.ModelCheckReachable, AgentVersion: "3.0"}, nil)
	got := c.row(t, "claude-sonnet-4-5")
	if got.JobID == first.JobID || got.Status != domain.SuitabilityQueued || !got.Stale || got.Test == nil || got.Test.AgentVersion != "2.0" {
		t.Fatalf("not tested again: %+v", got)
	}
	rerun, _ := c.st.Jobs().Get(ctx, c.account, got.JobID)
	c.finish(t, got.JobID, domain.JobSucceeded, answer(rerun.Spec.Suitability.Nonce), nil)
	if got := c.row(t, "claude-sonnet-4-5"); got.Stale || got.Status != domain.SuitabilityDone {
		t.Fatalf("after the rerun: %+v", got)
	}
	// A test of a connection turned off only goes stale.
	cn := c.add(t, conn("other", config.AdapterClaudeCode, "anthropic"))
	c.runTestOn(t, "other")
	cn.Enabled = false
	if err := c.st.ModelConnections().Put(ctx, &cn); err != nil {
		t.Fatal(err)
	}
	row, err = c.checker.Check(ctx, c.by, "other")
	if err != nil {
		t.Fatal(err)
	}
	c.finish(t, row.JobID, domain.JobSucceeded, domain.ModelCheckResult{Status: domain.ModelCheckReachable, AgentVersion: "3.0"}, nil)
	rows, _ := c.checker.Results(ctx, c.account)
	for _, r := range rows {
		if r.Connection == "other" && r.Model != "" && (!r.Stale || r.Status != domain.SuitabilityDone) {
			t.Fatalf("off = %+v", r)
		}
	}
}

func TestEstimateOfATest(t *testing.T) {
	c := newChecks(t)
	cloud := conn("cloud", config.AdapterClaudeCode, "anthropic")
	e := c.checker.EstimateTest(cloud, "claude-sonnet-4-5")
	if e.CostUSD == nil || *e.CostUSD < 0.2 || *e.CostUSD > 0.3 || !strings.HasPrefix(e.Basis, "typical") {
		t.Fatalf("catalog = %+v", e)
	}
	// 15 turns × 200000 tokens at $3, and 15 × 4000 output tokens at $15.
	if e.WorstInputTokens != 3000000 || e.WorstCostUSD == nil || *e.WorstCostUSD != 9.9 || !strings.HasPrefix(e.WorstBasis, "worst case") {
		t.Fatalf("worst = %+v", e)
	}
	if e := c.checker.EstimateTest(cloud, "mine"); e.CostUSD != nil || !strings.Contains(e.Basis, "price") {
		t.Fatalf("unknown = %+v", e)
	}
	cloud.Models = map[string]domain.ModelInfo{"mine": {Price: &domain.ModelPrice{Input: 1, Output: 1}}}
	if e := c.checker.EstimateTest(cloud, "mine"); e.CostUSD == nil || *e.CostUSD != 0.064 || e.WorstCostUSD != nil || !strings.Contains(e.WorstBasis, "context size") {
		t.Fatalf("entered = %+v", e)
	}
	local := cloud
	local.BaseURL = "http://127.0.0.1:11434/v1"
	if e := c.checker.EstimateTest(local, "claude-sonnet-4-5"); e.CostUSD == nil || *e.CostUSD != 0 {
		t.Fatalf("local = %+v", e)
	}
}

func TestListOfWithoutACatalog(t *testing.T) {
	for _, tt := range []struct {
		c   domain.ModelConnection
		url string
	}{
		{conn("a", config.AdapterClaudeCode, "x"), "https://api.anthropic.com/v1/models"},
		{conn("o", config.AdapterCodex, "x"), "https://api.openai.com/v1/models"},
		{conn("oc", config.AdapterOpencode, "x"), ""},
		{domain.ModelConnection{Name: "oc2", Adapter: config.AdapterOpencode, Vendor: "x", BaseURL: "https://m.example.com/v1"}, "https://m.example.com/v1/models"},
	} {
		if got := models.ListOf(nil, tt.c); got.URL != tt.url {
			t.Errorf("%s: %+v", tt.c.Name, got)
		}
	}
}

func TestOneTestAtATimeAndPlainModelNames(t *testing.T) {
	c := newChecks(t)
	ctx := t.Context()
	c.add(t, conn("cloud", config.AdapterClaudeCode, "anthropic"))
	row, err := c.checker.Test(ctx, c.by, "cloud", "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.checker.Test(ctx, c.by, "cloud", "claude-sonnet-4-5"); !errors.Is(err, models.ErrTestRunning) {
		t.Fatalf("second test = %v", err)
	}
	// Another model runs alongside.
	other, err := c.checker.Test(ctx, c.by, "cloud", "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.jobs.Cancel(ctx, c.by, c.account, other.JobID); err != nil {
		t.Fatal(err)
	}
	j, _ := c.st.Jobs().Get(ctx, c.account, row.JobID)
	c.finish(t, row.JobID, domain.JobSucceeded, answer(j.Spec.Suitability.Nonce), nil)
	if _, err := c.checker.Test(ctx, c.by, "cloud", "claude-sonnet-4-5"); err != nil {
		t.Fatalf("after the first ended = %v", err)
	}
	for _, bad := range []string{"", "a b", "$(rm -rf)", "x\ny", strings.Repeat("m", 201)} {
		if _, err := c.checker.Test(ctx, c.by, "cloud", bad); !errors.Is(err, models.ErrInvalidModel) {
			t.Errorf("%q = %v", bad, err)
		}
	}
}
