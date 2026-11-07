package workerapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

// pollAgents polls with labels, the given capabilities and agents, and
// returns the status and the install header.
func (e *env) pollAgents(t *testing.T, token string, labels, capabilities []string, agents []domain.WorkerAgent) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"labels": labels, "free_slots": 1, "version": "v1", "capabilities": capabilities, "agents": agents})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.http.URL+"/worker/v1/poll", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(domain.AgentInstallHeader)
}

var installs = []string{domain.AgentInstallCapability}

// TestInstallRequestReachesOnlyWorkersOfTheJobsPool: a job of the control
// pool on codex, which no worker has, is installed by a control worker and
// never by a build worker.
func TestInstallRequestReachesOnlyWorkersOfTheJobsPool(t *testing.T) {
	e := newEnv(t)
	_, build := e.addPoolWorker(t, "build", domain.PoolBuild)
	_, ctl := e.addPoolWorker(t, "ctl", domain.PoolControl)
	e.queue(t, domain.Job{Pool: domain.PoolControl, Role: "reviewer", Runtime: "codex"})
	if status, asked := e.pollAgents(t, build, []string{"linux"}, installs, nil); status != http.StatusNoContent || asked != "" {
		t.Fatalf("build worker: %d, asked %q", status, asked)
	}
	if status, asked := e.pollAgents(t, ctl, []string{"linux"}, installs, nil); status != http.StatusNoContent || asked != "codex" {
		t.Fatalf("control worker: %d, asked %q, want codex", status, asked)
	}
}

func TestInstallRequestOnlyWhenNoWorkerCanRunIt(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "w")
	e.queue(t, domain.Job{Role: "developer", Runtime: "codex", Needs: []string{"go"}})
	cases := []struct {
		name         string
		labels       []string
		capabilities []string
		agents       []domain.WorkerAgent
		want         string
	}{
		{"without the capability", []string{"go"}, nil, nil, ""},
		{"setting the label itself", []string{"go", domain.AgentInstallLabel}, nil, nil, ""},
		{"lacking another need", []string{"linux"}, installs, nil, ""},
		{"after a failed install", []string{"go"}, installs, []domain.WorkerAgent{{Adapter: "codex", Error: domain.AgentInstallChecksum}}, ""},
		{"asked", []string{"go"}, installs, []domain.WorkerAgent{{Adapter: "codex"}}, "codex"},
	}
	for _, c := range cases {
		if _, asked := e.pollAgents(t, token, c.labels, c.capabilities, c.agents); asked != c.want {
			t.Errorf("%s: asked %q, want %q", c.name, asked, c.want)
		}
	}
	// Another online worker of the pool can run it: nobody installs.
	_, other := e.addWorker(t, "other")
	if status, _ := e.pollAgents(t, other, []string{"go", "runtime:codex"}, nil, nil); status != http.StatusOK {
		t.Fatalf("the other worker did not lease the job: %d", status)
	}
	e.queue(t, domain.Job{Role: "developer", Runtime: "codex", Needs: []string{"go"}})
	if _, asked := e.pollAgents(t, token, []string{"go"}, installs, nil); asked != "" {
		t.Errorf("asked %q while an online worker has codex", asked)
	}
}

func TestPollRecordsWellFormedAgents(t *testing.T) {
	e := newEnv(t)
	w, token := e.addWorker(t, "w")
	agents := []domain.WorkerAgent{
		{Adapter: "claude-code", Source: domain.AgentSourceInstalled, Version: "2.1.306"},
		{Adapter: "codex", Error: domain.AgentInstallChecksum},
		{Adapter: "opencode", Source: "<script>"},
		{Adapter: "Bad Name"},
		{Adapter: "x", Error: "anything a worker writes"},
	}
	e.pollAgents(t, token, nil, nil, agents)
	got, err := e.st.Workers().Get(t.Context(), e.account, w.ID)
	if err != nil || len(got.Agents) != 2 || got.Agents[0] != agents[0] || got.Agents[1] != agents[1] {
		t.Fatalf("agents = %+v, %v", got.Agents, err)
	}
	// A poll without agents (an older worker) leaves them.
	e.poll(t, token)
	if got, _ = e.st.Workers().Get(t.Context(), e.account, w.ID); len(got.Agents) != 2 {
		t.Fatalf("agents = %+v", got.Agents)
	}
}
