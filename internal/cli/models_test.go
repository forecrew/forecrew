package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
)

func TestModelsCommands(t *testing.T) {
	e := newRemoteEnv(t)
	job := domain.NewID()
	e.svc.ModelsCheckFunc = func(context.Context, domain.Actor, mcp.ModelConnectionRefInput) (mcp.ModelCheckView, error) {
		return mcp.ModelCheckView{Connection: "cloud", JobID: job, Status: domain.ModelCheckQueued}, nil
	}
	e.check(t, []string{"models", "check", "cloud"}, "started as job "+job.String(), mcp.ToolModelsCheck, mcp.ModelConnectionRefInput{Connection: "cloud"})

	cost := 0.25
	e.svc.ModelsTestFunc = func(_ context.Context, _ domain.Actor, in mcp.ModelsTestInput) (mcp.ModelsTestOutput, error) {
		out := mcp.ModelsTestOutput{Connection: in.Connection, Model: in.Model, MaxTurns: 15, TimeoutSeconds: 300, Estimate: mcp.ModelsEstimate{CostUSD: &cost, Basis: "catalog"}}
		if in.Run {
			out.Test = &mcp.ModelCheckView{JobID: job}
		}
		return out, nil
	}
	// It shows the cost and asks first.
	out, _, code := e.runIn(t, "no\n", "models", "test", "cloud", "m")
	if code != 0 || !strings.Contains(out, "Typical cost: about $0.2500") || !strings.Contains(out, "Worst-case cost: unknown") || !strings.Contains(out, "Not run.") {
		t.Fatalf("declined: %d %q", code, out)
	}
	if last := e.svc.Calls()[len(e.svc.Calls())-1]; last.Input.(mcp.ModelsTestInput).Run {
		t.Fatal("a declined test ran")
	}
	out, _, code = e.runIn(t, "yes\n", "models", "test", "cloud", "m")
	if code != 0 || !strings.Contains(out, "Test started as job") {
		t.Fatalf("confirmed: %d %q", code, out)
	}
	e.check(t, []string{"models", "test", "cloud", "m", "--yes"}, "started", mcp.ToolModelsTest, mcp.ModelsTestInput{Connection: "cloud", Model: "m", Run: true})
	e.svc.ModelsTestFunc = func(_ context.Context, _ domain.Actor, in mcp.ModelsTestInput) (mcp.ModelsTestOutput, error) {
		return mcp.ModelsTestOutput{Connection: in.Connection, Model: in.Model, Estimate: mcp.ModelsEstimate{Basis: "no price"}}, nil
	}
	if out, _, _ := e.runIn(t, "no\n", "models", "test", "cloud", "x"); !strings.Contains(out, "Typical cost: unknown (no price)") {
		t.Errorf("unknown cost: %q", out)
	}

	e.svc.ModelsResultsFunc = func(context.Context, domain.Actor, mcp.ModelsResultsInput) ([]mcp.ModelCheckView, error) {
		return []mcp.ModelCheckView{
			{Connection: "cloud", Status: domain.ModelCheckReachable, Key: &domain.ModelCheckResult{Models: []string{"a", "b"}}},
			{Connection: "cloud", Model: "m", Status: domain.SuitabilityDone, Stale: true,
				Test: &domain.SuitabilityResult{Level: domain.LevelRead, Abilities: map[string]bool{"read": true, "mcp": true, "json": true}}},
			{Connection: "local", Status: domain.ModelCheckRefused, Key: &domain.ModelCheckResult{Detail: "the provider refused the key"}},
		}, nil
	}
	e.check(t, []string{"models", "results", "--connection", "cloud"}, "2 models", mcp.ToolModelsResults, mcp.ModelsResultsInput{Connection: "cloud"})
	if out, _, _ := e.run(t, "models", "results"); !strings.Contains(out, "read EDIT COMMAND mcp json (agent changed") || !strings.Contains(out, "refused the key") {
		t.Errorf("results = %q", out)
	}

	e.svc.ModelsSuggestFunc = func(context.Context, domain.Actor, mcp.ModelsSuggestInput) (mcp.ModelsSuggestion, error) {
		return mcp.ModelsSuggestion{Notes: []string{"Only models of anthropic passed"}, Plan: &mcp.ConfigPlanView{ID: job, Title: "Models suggested"}}, nil
	}
	e.check(t, []string{"models", "suggest", "--preset", "low_cost"}, "forecrew config apply "+job.String(), mcp.ToolModelsSuggest,
		mcp.ModelsSuggestInput{Preset: "low_cost", Source: domain.ConfigFromCLI})
	e.svc.ModelsSuggestFunc = func(context.Context, domain.Actor, mcp.ModelsSuggestInput) (mcp.ModelsSuggestion, error) {
		return mcp.ModelsSuggestion{Notes: []string{"nothing tested"}}, nil
	}
	if out, _, _ := e.run(t, "models", "suggest"); !strings.Contains(out, "Note: nothing tested") || !strings.Contains(out, "Nothing to change.") {
		t.Errorf("suggest = %q", out)
	}

	e.svc.ModelsCatalogFunc = func(context.Context, domain.Actor, mcp.ModelsCatalogInput) (mcp.ModelsCatalog, error) {
		return mcp.ModelsCatalog{Providers: []models.Provider{{Name: "deepseek", Title: "DeepSeek API", Adapter: "opencode", Country: "CN", KeyEnv: "DEEPSEEK_API_KEY"}},
			Models: []models.CatalogModel{{ID: "deepseek-chat", Maker: "deepseek", Tier: "strong", Context: 128000, Price: models.Price{Input: 0.28, Output: 0.42}}}}, nil
	}
	e.check(t, []string{"models", "catalog"}, "DeepSeek API", mcp.ToolModelsCatalog, mcp.ModelsCatalogInput{})
	if out, _, _ := e.run(t, "models", "catalog"); !strings.Contains(out, "in CN") || !strings.Contains(out, "$0.28 / $0.42") {
		t.Errorf("catalog = %q", out)
	}
}
