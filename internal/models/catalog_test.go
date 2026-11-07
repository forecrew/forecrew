package models_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/runtime"
)

func catalog(t *testing.T) *models.Catalog {
	t.Helper()
	c, err := models.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCatalogShipsValid(t *testing.T) {
	c := catalog(t)
	want := []string{"anthropic", "openai", "google", "deepseek", "moonshot", "zai", "openrouter", "bedrock", "azure", "vertex", "ollama", "lmstudio"}
	for _, name := range want {
		if _, ok := c.Provider(name); !ok {
			t.Errorf("provider %s is missing", name)
		}
	}
	registered := runtime.DefaultRegistry(nil, nil)
	for _, p := range c.Providers {
		if _, err := registered.Get(p.Adapter); err != nil {
			t.Errorf("provider %s: adapter %s is not registered: %v", p.Name, p.Adapter, err)
		}
		if own := runtime.KeyEnv(p.Adapter); own != "" && own != p.KeyEnv {
			t.Errorf("provider %s: %s reads its key from %s, not %s", p.Name, p.Adapter, own, p.KeyEnv)
		}
		if p.Where() == "" {
			t.Error("no place")
		}
	}
	for _, m := range c.Models {
		if !config.Tier(m.Tier).Valid() || c.MakerTitle(m.Maker) == m.Maker {
			t.Errorf("model %s: tier %s maker %s", m.ID, m.Tier, m.Maker)
		}
	}
}

func TestCatalogRefusesBadEntries(t *testing.T) {
	head := "version: 1\nmakers: [{name: a, title: A, prefixes: [aa]}, {name: b, title: B, prefixes: [bb]}]\n"
	prov := "providers: [{name: p, title: P, adapter: opencode, key_env: P_KEY, homepage: https://p.example, country: US}]\n"
	for name, tt := range map[string]struct{ yaml, want string }{
		"schema":         {head + prov + "models: [{id: x}]\n", "invalid"},
		"unknown key":    {head + prov + "models: []\nextra: 1\n", "invalid"},
		"twice":          {head + "providers: [{name: p, title: P, adapter: opencode, key_env: P_KEY, homepage: https://p.example, country: US}, {name: p, title: P, adapter: opencode, key_env: P_KEY, homepage: https://p.example, country: US}]\nmodels: []\n", "twice"},
		"maker":          {head + prov + "models: [{id: x, maker: c, tier: fast, context: 1, price: {input: 0, output: 0}}]\n", "not listed"},
		"prefix":         {"version: 1\nmakers: [{name: a, title: A, prefixes: [x]}, {name: b, title: B, prefixes: [x]}]\n" + prov + "models: []\n", "prefix"},
		"maker twice":    {"version: 1\nmakers: [{name: a, title: A, prefixes: [x]}, {name: a, title: B, prefixes: [y]}]\n" + prov + "models: []\n", "twice"},
		"model twice":    {head + prov + "models: [{id: x, maker: a, tier: fast, context: 1, price: {input: 0, output: 0}}, {id: x, maker: a, tier: fast, context: 1, price: {input: 0, output: 0}}]\n", "twice"},
		"not normalized": {head + prov + "models: [{id: x-20250101, maker: a, tier: fast, context: 1, price: {input: 0, output: 0}}]\n", "write it as"},
		"base_url":       {head + "providers: [{name: p, title: P, adapter: opencode, base_url: http://example.com, key_env: P_KEY, homepage: https://p.example, country: US}]\nmodels: []\n", "base_url"},
		"adapter":        {head + "providers: [{name: p, title: P, adapter: aider, key_env: P_KEY, homepage: https://p.example, country: US}]\nmodels: []\n", "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := models.LoadCatalog(fstest.MapFS{models.CatalogFile: {Data: []byte(tt.yaml)}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := models.LoadCatalog(fstest.MapFS{}); err == nil {
		t.Error("a missing catalog loads")
	}
}

func TestNormalizeAndMaker(t *testing.T) {
	c := catalog(t)
	for model, want := range map[string][2]string{ // normalized, maker
		"claude-sonnet-4-5-20250929":                   {"claude-sonnet-4-5", "anthropic"},
		"anthropic/claude-sonnet-4.5":                  {"claude-sonnet-4-5", "anthropic"},
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": {"claude-sonnet-4-5", "anthropic"},
		"openrouter/anthropic/claude-3.7-sonnet":       {"claude-3-7-sonnet", "anthropic"},
		"deepseek/deepseek-chat":                       {"deepseek-chat", "deepseek"},
		"ollama/qwen3-coder:30b":                       {"qwen3-coder", "alibaba"},
		"llama3.3":                                     {"llama3-3", "meta"},
		"gpt-5-2025-08-07":                             {"gpt-5", "openai"},
		"meta.llama3-3-70b-instruct-v1:0":              {"llama3-3-70b-instruct", "meta"},
		"my-own-model":                                 {"my-own-model", ""},
		"z-ai/glm-4.6":                                 {"glm-4-6", "zai"},
		"moonshotai/kimi-k2-0905-preview":              {"kimi-k2-0905-preview", "moonshot"},
		"google/gemini-2.5-flash":                      {"gemini-2-5-flash", "google"},
	} {
		if got := models.Normalize(model); got != want[0] {
			t.Errorf("Normalize(%s) = %s, want %s", model, got, want[0])
		}
		if got := c.MakerOf(model); got != want[1] {
			t.Errorf("MakerOf(%s) = %s, want %s", model, got, want[1])
		}
	}
	if m, ok := c.Model("anthropic/claude-haiku-4.5"); !ok || m.Tier != "fast" || m.Price.Input != 1 {
		t.Errorf("Model = %+v %v", m, ok)
	}
	var none *models.Catalog
	if _, ok := none.Model("x"); ok || none.MakerOf("claude") != "" || none.MakerTitle("a") != "a" {
		t.Error("nil catalog")
	}
	if _, ok := none.Provider("anthropic"); ok {
		t.Error("nil catalog provider")
	}
	if p, _ := c.Provider("ollama"); p.Where() != "on your own machines" {
		t.Error(p.Where())
	}
	if p, _ := c.Provider("bedrock"); !strings.Contains(p.Where(), "region") {
		t.Error(p.Where())
	}
}
