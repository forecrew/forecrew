package workerapi

import (
	"maps"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

func TestRuntimeEnv(t *testing.T) {
	cfg := &config.Config{
		Runtimes: map[string]config.Runtime{
			"claude":  {Adapter: config.AdapterClaudeCode, Credentials: map[string]string{"ANTHROPIC_API_KEY": "a"}},
			"claude2": {Adapter: config.AdapterClaudeCode, Credentials: map[string]string{"ANTHROPIC_API_KEY": "b"}},
			"codex":   {Adapter: config.AdapterCodex},
		},
		Roles: map[string]config.Role{
			"developer": {Runtime: "claude2"},
			"reviewer":  {Runtime: "codex"},
		},
	}
	env := RuntimeEnv(cfg)
	tests := []struct {
		name     string
		role     string
		runtime  string
		wantName string
		want     map[string]string
	}{
		{"role runtime", "developer", config.AdapterClaudeCode, "claude2", map[string]string{"ANTHROPIC_API_KEY": "b"}},
		{"first runtime of the adapter", "architect", config.AdapterClaudeCode, "claude", map[string]string{"ANTHROPIC_API_KEY": "a"}},
		{"role runtime of another adapter", "reviewer", config.AdapterClaudeCode, "claude", map[string]string{"ANTHROPIC_API_KEY": "a"}},
		{"no credentials", "reviewer", config.AdapterCodex, "codex", nil},
		{"unknown adapter", "developer", "other", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, got := env(domain.Job{Role: tt.role, Runtime: tt.runtime})
			if name != tt.wantName || !maps.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
				t.Fatalf("env = %q %v, want %q %v", name, got, tt.wantName, tt.want)
			}
		})
	}
	_, got := env(domain.Job{Role: "developer", Runtime: config.AdapterClaudeCode})
	got["ANTHROPIC_API_KEY"] = "changed"
	if cfg.Runtimes["claude2"].Credentials["ANTHROPIC_API_KEY"] != "b" {
		t.Fatal("env shares the config map")
	}
}

func TestNewDefaults(t *testing.T) {
	s := New(Config{})
	if name, env := s.cfg.Env(domain.Job{}); name != "" || env != nil {
		t.Fatalf("env = %q %v", name, env)
	}
	if s.cfg.PollWait != PollWait || s.cfg.PollRecheck != PollRecheck || len(s.cfg.Pools) != len(config.DefaultPools()) {
		t.Fatalf("config = %+v", s.cfg)
	}
}
