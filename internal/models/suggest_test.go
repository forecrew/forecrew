package models_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
)

// fixed is the same input for every preset: three connections, and their
// models' tests.
func fixed(t *testing.T) []models.Candidate {
	t.Helper()
	cat := catalog(t)
	id := func(n byte) domain.ModelConnection {
		c := domain.ModelConnection{ID: domain.NewID(), Enabled: true}
		c.ID[15] = n
		return c
	}
	anthropic, deepseek, local := id(1), id(2), id(3)
	anthropic.Name, anthropic.Vendor, anthropic.Adapter = "anthropic", "anthropic", config.AdapterClaudeCode
	deepseek.Name, deepseek.Vendor, deepseek.Adapter = "deepseek", "deepseek", config.AdapterOpencode
	local.Name, local.Vendor, local.Adapter, local.BaseURL = "local", "ollama", config.AdapterOpencode, "http://127.0.0.1:11434/v1"
	local.Models = map[string]domain.ModelInfo{"my-coder": {Maker: "acme", Tier: "strong"}}
	off := id(4)
	off.Name, off.Vendor, off.Enabled = "off", "openai", false
	key := func(c domain.ModelConnection, status string) domain.ModelCheck {
		return domain.ModelCheck{ConnectionID: c.ID, Connection: c.Name, Status: status}
	}
	test := func(c domain.ModelConnection, model, level string) domain.ModelCheck {
		return domain.ModelCheck{ConnectionID: c.ID, Connection: c.Name, Model: model, Status: domain.SuitabilityDone, Test: &domain.SuitabilityResult{Level: level}}
	}
	stale := test(anthropic, "claude-haiku-4-5", domain.LevelCode)
	stale.Stale = true
	checks := []domain.ModelCheck{
		key(anthropic, domain.ModelCheckReachable), key(deepseek, domain.ModelCheckReachable), key(local, domain.ModelCheckReachable), key(off, domain.ModelCheckReachable),
		test(anthropic, "claude-opus-4-5", domain.LevelCode), test(anthropic, "claude-sonnet-4-5", domain.LevelCode), stale,
		test(deepseek, "deepseek-chat", domain.LevelCode), test(deepseek, "deepseek-reasoner", domain.LevelRead),
		test(local, "my-coder", domain.LevelCode), test(local, "no-tier", domain.LevelCode),
		test(off, "gpt-5", domain.LevelCode),
	}
	c, notes := models.Candidates(cat, []domain.ModelConnection{anthropic, deepseek, local, off}, checks)
	if len(notes) != 1 || !strings.Contains(notes[0], "no-tier") {
		t.Fatalf("notes = %v", notes)
	}
	return c
}

var needs = []models.RoleNeed{
	{Name: "architect", Tier: config.TierBest, Writes: false},
	{Name: "developer", Tier: config.TierStrong, Writes: true},
	{Name: "reviewer", Tier: config.TierStrong, Writes: false},
	{Name: "triage", Tier: config.TierFast, Writes: false},
}

func roles(s models.Suggestion) map[string]string {
	out := map[string]string{}
	for name, r := range s.Roles {
		out[name] = r.Connection + "/" + s.Tiers[r.Tier][map[string]string{"anthropic": "anthropic", "deepseek": "deepseek", "local": "ollama"}[r.Connection]]
	}
	return out
}

func TestSuggestionPresets(t *testing.T) {
	c := fixed(t)
	for preset, want := range map[string]map[string]string{
		// The tier each role is written for, then the price.
		models.PresetBalanced:  {"architect": "deepseek/deepseek-reasoner", "developer": "local/my-coder", "reviewer": "deepseek/deepseek-chat", "triage": "local/my-coder"},
		models.PresetLowCost:   {"architect": "local/my-coder", "developer": "local/my-coder", "reviewer": "deepseek/deepseek-chat", "triage": "local/my-coder"},
		models.PresetLocalOnly: {"architect": "local/my-coder", "developer": "local/my-coder", "triage": "local/my-coder"},
	} {
		s, err := models.Suggest(c, needs, preset, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := roles(s); !maps.Equal(got, want) {
			t.Errorf("%s = %v, want %v", preset, got, want)
		}
		again, _ := models.Suggest(slices.Clone(c), slices.Clone(needs), preset, false)
		if !maps.Equal(roles(again), roles(s)) {
			t.Errorf("%s is not deterministic", preset)
		}
		if preset == models.PresetLocalOnly && (len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "acme")) {
			t.Errorf("local only notes = %v", s.Notes)
		}
	}
	if _, err := models.Suggest(c, needs, "fastest", false); err == nil {
		t.Error("unknown preset")
	}
	if s, _ := models.Suggest(c, needs, "", false); s.Preset != models.PresetBalanced {
		t.Errorf("default = %s", s.Preset)
	}
}

func TestSuggestionKeepsTheReviewerOffTheDevelopersMaker(t *testing.T) {
	c := fixed(t)
	// Only Anthropic and DeepSeek: the developer gets the cheaper strong
	// model, and the reviewer the other maker's even at a higher price.
	c = slices.DeleteFunc(c, func(x models.Candidate) bool { return x.Connection == "local" })
	s, err := models.Suggest(c, needs, models.PresetBalanced, false)
	if err != nil {
		t.Fatal(err)
	}
	got := roles(s)
	if got["developer"] != "deepseek/deepseek-chat" || got["reviewer"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("roles = %v", got)
	}

	// One maker only: an honest message, and the reviewer is left alone.
	one := slices.DeleteFunc(slices.Clone(c), func(x models.Candidate) bool { return x.Maker != "anthropic" })
	s, _ = models.Suggest(one, needs, models.PresetBalanced, false)
	if _, ok := s.Roles["reviewer"]; ok || !slices.ContainsFunc(s.Notes, func(n string) bool { return strings.Contains(n, "Only models of anthropic") }) || roles(s)["developer"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("one maker = %+v", s)
	}
	// Unless the policy allows it.
	s, _ = models.Suggest(one, needs, models.PresetBalanced, true)
	if roles(s)["reviewer"] == "" {
		t.Fatalf("allowed = %+v", s)
	}
}

func TestSuggestionWithNothingTestedOrNothingFitting(t *testing.T) {
	s, err := models.Suggest(nil, needs, models.PresetBalanced, false)
	if err != nil || len(s.Roles) != 0 || len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "test a model first") {
		t.Fatalf("nothing = %+v, %v", s, err)
	}
	readers := []models.Candidate{{Connection: "c", Vendor: "v", Model: "m", Maker: "x", Tier: config.TierStrong, Level: domain.LevelRead}}
	s, _ = models.Suggest(readers, needs, models.PresetBalanced, false)
	if _, ok := s.Roles["developer"]; ok || !slices.ContainsFunc(s.Notes, func(n string) bool { return strings.Contains(n, "role developer, which needs level code") }) {
		t.Fatalf("readers only = %+v", s)
	}
}

func TestMergeKeepsWhatTheSuggestionDidNotFill(t *testing.T) {
	cur := domain.ModelAssignment{
		Tiers: map[string]map[string]string{"strong": {"anthropic": "claude-sonnet-4-5"}, "fast": {"anthropic": "claude-haiku-4-5"}},
		Roles: map[string]domain.ModelRole{"ux": {Connection: "anthropic", Tier: "fast"}, "pm": {Connection: "anthropic", Tier: "strong"}, "file": {Connection: "claude", Tier: "strong"}},
	}
	s := models.Suggestion{Tiers: map[string]map[string]string{"strong": {"anthropic": "claude-opus-4-5"}}, Roles: map[string]domain.ModelRole{"developer": {Connection: "anthropic", Tier: "strong"}}}
	conns := map[string]domain.ModelConnection{"anthropic": {Name: "anthropic", Vendor: "anthropic"}}
	tiers, roles, notes := models.Merge(cur, s, conns)
	if tiers["fast"]["anthropic"] != "claude-haiku-4-5" || tiers["strong"]["anthropic"] != "claude-opus-4-5" || len(roles) != 4 || roles["file"].Connection != "claude" {
		t.Fatalf("tiers %v roles %v", tiers, roles)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Role pm") {
		t.Fatalf("notes = %v", notes)
	}
}
