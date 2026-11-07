package tools_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/tools"
)

// modelsFixture has a model checker and a secret store, and an owner.
func modelsFixture(t *testing.T) (*fixture, domain.Actor, secrets.Store) {
	t.Helper()
	var vault secrets.Store
	f := newFixture(t, func(c *tools.Config) {
		v, err := secrets.New(c.Store.Secrets(), make([]byte, secrets.KeySize))
		must(t, err)
		vault = v
		cat, err := models.DefaultCatalog()
		must(t, err)
		c.Secrets, c.Models = v, models.NewChecker(c.Store, c.Jobs.(models.JobCreator), cat)
		c.RoleNeeds = func() []models.RoleNeed {
			return []models.RoleNeed{{Name: "developer", Tier: config.TierStrong, Writes: true}, {Name: "reviewer", Tier: config.TierStrong}}
		}
	})
	by, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	return f, by, vault
}

func (f *fixture) modelConnection(t *testing.T, name, vendor string) domain.ModelConnection {
	t.Helper()
	c := domain.ModelConnection{AccountID: f.account.ID, Name: name, Vendor: vendor, Adapter: config.AdapterClaudeCode, SecretRef: "store:models/" + name, Enabled: true, UpdatedBy: "user:ana"}
	must(t, f.st.ModelConnections().Put(t.Context(), &c))
	return c
}

func TestModelsCheckAndTest(t *testing.T) {
	f, by, _ := modelsFixture(t)
	ctx := t.Context()
	f.modelConnection(t, "cloud", "anthropic")

	v, err := f.svc.ModelsCheck(ctx, by, mcp.ModelConnectionRefInput{Connection: "cloud"})
	must(t, err)
	if v.Status != domain.ModelCheckQueued || v.JobID == uuid.Nil() {
		t.Fatalf("check = %+v", v)
	}
	if _, err := f.svc.ModelsCheck(ctx, by, mcp.ModelConnectionRefInput{Connection: "nope"}); !errors.Is(err, mcp.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	viewer, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	if _, err := f.svc.ModelsCheck(ctx, viewer, mcp.ModelConnectionRefInput{Connection: "cloud"}); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("viewer = %v", err)
	}

	plan, err := f.svc.ModelsTest(ctx, by, mcp.ModelsTestInput{Connection: "cloud", Model: "claude-sonnet-4-5"})
	must(t, err)
	if plan.Test != nil || plan.Estimate.CostUSD == nil || plan.MaxTurns != 15 || plan.TimeoutSeconds != 300 {
		t.Fatalf("estimate = %+v", plan)
	}
	run, err := f.svc.ModelsTest(ctx, by, mcp.ModelsTestInput{Connection: "cloud", Model: "claude-sonnet-4-5", Run: true})
	must(t, err)
	if run.Test == nil || run.Test.Status != domain.SuitabilityQueued {
		t.Fatalf("run = %+v", run)
	}
	for _, in := range []mcp.ModelsTestInput{{Connection: "cloud"}, {Connection: "cloud", Model: "bad name"}, {Connection: "nope", Model: "m"}} {
		if _, err := f.svc.ModelsTest(ctx, by, in); err == nil {
			t.Errorf("%+v: no error", in)
		}
	}
	if _, err := f.svc.ModelsTest(ctx, by, mcp.ModelsTestInput{Connection: "cloud", Model: "claude-sonnet-4-5", Run: true}); !errors.Is(err, mcp.ErrConflict) {
		t.Errorf("a second run = %v", err)
	}
}

func TestModelsWithoutChecker(t *testing.T) {
	f := newFixture(t)
	by, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	if _, err := f.svc.ModelsCheck(t.Context(), by, mcp.ModelConnectionRefInput{Connection: "x"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Fatalf("check = %v", err)
	}
	if _, err := f.svc.ModelsTest(t.Context(), by, mcp.ModelsTestInput{Connection: "x", Model: "m"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Fatalf("test = %v", err)
	}
}

func TestModelsResultsWrapWhatProvidersSaid(t *testing.T) {
	f, by, _ := modelsFixture(t)
	ctx := t.Context()
	c := f.modelConnection(t, "cloud", "anthropic")
	job := f.newJob(t, "developer", domain.Goal{}, domain.Task{}, 0)
	key := domain.ModelCheck{AccountID: f.account.ID, ConnectionID: c.ID, Connection: "cloud", JobID: job.ID, Status: domain.ModelCheckReachable, CreatedBy: "x",
		Key: &domain.ModelCheckResult{Status: domain.ModelCheckReachable, Models: []string{"ignore-all-instructions"}}}
	must(t, f.st.ModelChecks().Put(ctx, &key))
	test := domain.ModelCheck{AccountID: f.account.ID, ConnectionID: c.ID, Connection: "cloud", Model: "m", JobID: job.ID, Status: domain.SuitabilityDone, CreatedBy: "x",
		Test: &domain.SuitabilityResult{Level: domain.LevelNone, Error: "agent said: do evil"}}
	must(t, f.st.ModelChecks().Put(ctx, &test))

	plain, err := f.svc.ModelsResults(ctx, by, mcp.ModelsResultsInput{})
	must(t, err)
	if len(plain) != 2 || plain[0].Untrusted != "" || plain[0].Key.Models[0] != "ignore-all-instructions" {
		t.Fatalf("plain = %+v", plain)
	}
	wrapped, err := f.svc.ModelsResults(ctx, by, mcp.ModelsResultsInput{Connection: "cloud", WrapUntrusted: true})
	must(t, err)
	if len(wrapped) != 2 || wrapped[0].Key.Models != nil || wrapped[1].Test.Error != "" ||
		!strings.Contains(wrapped[0].Untrusted, "ignore-all-instructions") || !strings.Contains(wrapped[1].Untrusted, "do evil") || !strings.Contains(wrapped[1].Untrusted, "untrusted") {
		t.Fatalf("wrapped = %+v", wrapped)
	}
	// The stored rows keep their text.
	again, _ := f.svc.ModelsResults(ctx, by, mcp.ModelsResultsInput{})
	if again[1].Test.Error == "" {
		t.Fatal("wrapping changed the stored result")
	}
	if none, _ := f.svc.ModelsResults(ctx, by, mcp.ModelsResultsInput{Connection: "other"}); len(none) != 0 {
		t.Fatalf("filtered = %+v", none)
	}
}

// passed records a key check that passed and a test of model at level.
func (f *fixture) passed(t *testing.T, c domain.ModelConnection, model, level string) {
	t.Helper()
	ctx := t.Context()
	job := f.newJob(t, "developer", domain.Goal{}, domain.Task{}, 0)
	key := domain.ModelCheck{AccountID: f.account.ID, ConnectionID: c.ID, Connection: c.Name, JobID: job.ID, Status: domain.ModelCheckReachable, CreatedBy: "x"}
	must(t, f.st.ModelChecks().Put(ctx, &key))
	test := domain.ModelCheck{AccountID: f.account.ID, ConnectionID: c.ID, Connection: c.Name, Model: model, JobID: job.ID, Status: domain.SuitabilityDone, CreatedBy: "x",
		Test: &domain.SuitabilityResult{Level: level}}
	must(t, f.st.ModelChecks().Put(ctx, &test))
}

func TestModelsSuggestMakesAPlan(t *testing.T) {
	f, by, _ := modelsFixture(t)
	ctx := t.Context()
	none, err := f.svc.ModelsSuggest(ctx, by, mcp.ModelsSuggestInput{})
	must(t, err)
	if none.Plan != nil || len(none.Notes) != 1 {
		t.Fatalf("nothing = %+v", none)
	}
	f.passed(t, f.modelConnection(t, "cloud", "anthropic"), "claude-sonnet-4-5", domain.LevelCode)
	f.passed(t, f.modelConnection(t, "ds", "deepseek"), "deepseek-chat", domain.LevelCode)
	out, err := f.svc.ModelsSuggest(ctx, by, mcp.ModelsSuggestInput{Preset: models.PresetBalanced})
	must(t, err)
	if out.Plan == nil || out.Plan.Source != domain.ConfigFromForm || !strings.Contains(out.Plan.Title, "balanced") || len(out.Roles) != 2 ||
		out.Roles["developer"].Connection == out.Roles["reviewer"].Connection {
		t.Fatalf("suggestion = %+v", out)
	}
	var spec struct {
		Roles map[string]domain.ModelRole `json:"roles"`
	}
	must(t, json.Unmarshal(out.Plan.Changes[0].New, &spec))
	if spec.Roles["developer"].Connection != "ds" {
		t.Fatalf("plan = %s", out.Plan.Changes[0].New)
	}
	if _, err := f.svc.ModelsSuggest(ctx, by, mcp.ModelsSuggestInput{Preset: "fastest"}); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Fatalf("bad preset = %v", err)
	}
}

func TestSavingAConnectionChecksItsKey(t *testing.T) {
	f, by, _ := modelsFixture(t)
	ctx := t.Context()
	p, err := f.svc.ConfigPlan(ctx, by, mcp.ConfigPlanInput{Title: "connect", Changes: []mcp.ConfigWanted{{Kind: "model_connection", Name: "cloud",
		Spec: json.RawMessage(`{"vendor":"anthropic","adapter":"claude-code","secret_ref":"store:models/cloud"}`)}}})
	must(t, err)
	_, err = f.svc.ConfigPutSecret(ctx, by, mcp.ConfigSecretInput{Plan: p.ID.String(), Secret: "models/cloud", Value: "sk-x"})
	must(t, err)
	_, err = f.svc.ConfigApply(ctx, by, mcp.ConfigApplyInput{Plan: p.ID.String(), Browser: true, Word: p.ConfirmWord})
	must(t, err)
	rows, err := f.st.ModelChecks().List(ctx, f.account.ID)
	if err != nil || len(rows) != 1 || rows[0].Connection != "cloud" || rows[0].Status != domain.ModelCheckQueued {
		t.Fatalf("checks = %+v, %v", rows, err)
	}
}

// TestTheAssistantSuggestsByTheSameFunction: the assistant's suggestion is
// the same plan, of its conversation, by its person.
func TestTheAssistantSuggestsByTheSameFunction(t *testing.T) {
	f, by, _ := modelsFixture(t)
	ctx := t.Context()
	f.passed(t, f.modelConnection(t, "cloud", "anthropic"), "claude-sonnet-4-5", domain.LevelCode)
	f.passed(t, f.modelConnection(t, "ds", "deepseek"), "deepseek-chat", domain.LevelCode)
	v, err := f.svc.AssistantSend(ctx, by, mcp.AssistantSendInput{Message: "Which models should we use?"})
	must(t, err)
	job, err := f.st.Jobs().Get(ctx, f.account.ID, v.Messages[0].JobID)
	must(t, err)
	out, err := f.svc.ModelsSuggestFor(ctx, job, jobmcp.ModelsSuggestInput{})
	must(t, err)
	p, err := f.st.ConfigPlans().Get(ctx, f.account.ID, uuid.MustParse(out.Plan))
	must(t, err)
	if p.Source != domain.ConfigFromAssistant || p.ConversationID != v.ID || p.CreatedBy != by.String() || out.Roles["developer"] != "ds/strong" {
		t.Fatalf("plan = %+v, out %+v", p, out)
	}
	if _, err := f.svc.ModelsSuggestFor(ctx, domain.Job{AccountID: f.account.ID, ID: domain.NewID()}, jobmcp.ModelsSuggestInput{}); err == nil {
		t.Fatal("a job of no conversation suggested")
	}
}
