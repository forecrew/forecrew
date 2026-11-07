package domain

import "testing"

func TestWorkerAgent(t *testing.T) {
	w := Worker{Agents: []WorkerAgent{{Adapter: "codex", Error: AgentInstallChecksum}}}
	if a, ok := w.Agent("codex"); !ok || a.Error != AgentInstallChecksum {
		t.Errorf("Agent(codex) = %+v, %v", a, ok)
	}
	if _, ok := w.Agent("opencode"); ok {
		t.Error("Agent(opencode) found")
	}
}
