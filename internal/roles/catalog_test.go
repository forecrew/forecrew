package roles

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/fstest"
	"time"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Runtimes: map[string]config.Runtime{
			"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic"},
			"codex":  {Adapter: config.AdapterCodex, Vendor: "openai"},
		},
		ModelTiers: map[config.Tier]map[string]string{
			config.TierStrong: {"anthropic": "claude-strong", "openai": "gpt-strong"},
			config.TierBest:   {"anthropic": "claude-best", "openai": "gpt-best"},
		},
		Roles: map[string]config.Role{
			"onboarding-analyst":   {Runtime: "claude", Tier: config.TierStrong},
			"product-owner":        {Runtime: "claude", Tier: config.TierBest},
			"architect":            {Runtime: "claude", Tier: config.TierBest},
			"developer":            {Runtime: "claude", Tier: config.TierStrong},
			"reviewer":             {Runtime: "codex", Tier: config.TierBest},
			"test-architect":       {Runtime: "claude", Tier: config.TierBest},
			"environment-engineer": {Runtime: "claude", Tier: config.TierStrong},
			"test-engineer":        {Runtime: "claude", Tier: config.TierStrong},
		},
	}
}

func TestCatalogResolve(t *testing.T) {
	c, err := New(forecrew.Files, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := []string{"architect", "developer", "environment-engineer", "onboarding-analyst", "product-owner", "reviewer", "test-architect", "test-engineer"}
	if got := c.Names(); !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	ctx := context.Background()
	dev, err := c.Resolve(ctx, uuid.Nil(), uuid.Nil(), "developer")
	if err != nil {
		t.Fatalf("Resolve developer: %v", err)
	}
	if dev.Name != "developer" || dev.Runtime != config.AdapterClaudeCode || dev.Model != "claude-strong" ||
		dev.ReadOnly() || dev.Timeout != 60*time.Minute || dev.MaxTurns != 120 ||
		!slices.Contains(dev.AllowedTools, "Edit") || len(dev.OutputSchema) == 0 ||
		len(dev.DefinitionOfDone) == 0 || len(dev.EscalateWhen) == 0 || dev.Instructions == "" ||
		dev.Pool != "build" || dev.Toolbox != nil {
		t.Errorf("unexpected developer %+v", dev)
	}
	rev, err := c.Resolve(ctx, uuid.Nil(), uuid.Nil(), "reviewer")
	if err != nil {
		t.Fatalf("Resolve reviewer: %v", err)
	}
	// The config tier (best) decides, not the role file's (strong).
	if rev.Runtime != config.AdapterCodex || rev.Model != "gpt-best" || !rev.ReadOnly() || slices.Contains(rev.AllowedTools, "Edit") {
		t.Errorf("unexpected reviewer %+v", rev)
	}
	po, err := c.Resolve(ctx, uuid.Nil(), uuid.Nil(), "product-owner")
	if err != nil || po.Model != "claude-best" {
		t.Errorf("Resolve product-owner = %q, %v; want claude-best", po.Model, err)
	}
	for name, model := range map[string]string{"test-architect": "claude-best", "environment-engineer": "claude-strong", "test-engineer": "claude-strong"} {
		r, err := c.Resolve(ctx, uuid.Nil(), uuid.Nil(), name)
		if err != nil || r.Model != model || r.ReadOnly() || !slices.Contains(r.AllowedTools, "Write") || r.Pool != "build" {
			t.Errorf("Resolve %s = %+v, %v; want a writing role on %s", name, r, err, model)
		}
	}
	// The resolved role does not share slices with the catalog.
	dev.WritePaths[0] = "changed"
	again, _ := c.Resolve(ctx, uuid.Nil(), uuid.Nil(), "developer")
	if again.WritePaths[0] == "changed" {
		t.Error("Resolve returns shared write paths")
	}
}

func TestCatalogResolveErrors(t *testing.T) {
	cfg := testConfig()
	delete(cfg.Roles, "architect")
	cfg.Roles["onboarding-analyst"] = config.Role{Runtime: "gone", Tier: config.TierStrong}
	cfg.Roles["product-owner"] = config.Role{Runtime: "claude", Tier: config.TierFast}
	c, err := New(forecrew.Files, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		role string
		want error
	}{
		{"nobody", ErrUnknownRole},
		{"architect", ErrNotConfigured},
		{"onboarding-analyst", ErrNotConfigured},
		{"product-owner", ErrNotConfigured},
	}
	for _, tt := range tests {
		if _, err := c.Resolve(context.Background(), uuid.Nil(), uuid.Nil(), tt.role); !errors.Is(err, tt.want) {
			t.Errorf("Resolve(%s) error = %v, want %v", tt.role, err, tt.want)
		}
	}
	if _, err := c.Definition("nobody"); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("Definition error = %v, want ErrUnknownRole", err)
	}
}

func TestNewErrors(t *testing.T) {
	cfg := testConfig()
	cfg.Roles["tester"] = config.Role{Runtime: "claude", Tier: config.TierStrong}
	if _, err := New(forecrew.Files, cfg); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("New with unknown config role: error = %v, want ErrUnknownRole", err)
	}
	if _, err := New(fstest.MapFS{}, testConfig()); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("New without role files: error = %v, want ErrInvalidRole", err)
	}
	cfg = testConfig()
	cfg.Roles["reviewer"] = config.Role{Runtime: "claude", Tier: config.TierBest}
	if _, err := New(forecrew.Files, cfg); !errors.Is(err, ErrSameVendorReview) {
		t.Errorf("New with same-vendor review: error = %v, want ErrSameVendorReview", err)
	}
}

func TestNewSeparationOfDuties(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(*config.Config)
		wantErr bool
	}{
		{"different vendors", func(*config.Config) {}, false},
		{"same runtime", func(c *config.Config) { c.Roles["reviewer"] = c.Roles["developer"] }, true},
		{"same vendor, other runtime", func(c *config.Config) {
			c.Runtimes["claude2"] = config.Runtime{Adapter: config.AdapterClaudeCode, Vendor: "anthropic"}
			c.Roles["reviewer"] = config.Role{Runtime: "claude2", Tier: config.TierBest}
		}, true},
		{"same vendor allowed", func(c *config.Config) {
			c.Roles["reviewer"] = c.Roles["developer"]
			c.Policy.AllowSameVendorReview = true
		}, false},
		{"no reviewer", func(c *config.Config) { delete(c.Roles, "reviewer") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.edit(cfg)
			_, err := New(forecrew.Files, cfg)
			if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrSameVendorReview)) {
				t.Errorf("New error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}
