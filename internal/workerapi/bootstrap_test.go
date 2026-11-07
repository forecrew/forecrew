package workerapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// machineEnv adds an instance of the fleet to env: a provider, a shape and a
// booting machine with a bootstrap token.
type machineEnv struct {
	*env
	shape   domain.Shape
	machine domain.Machine
	token   string
}

func newMachineEnv(t *testing.T, edit func(*domain.Shape)) *machineEnv {
	t.Helper()
	e := newEnv(t)
	ctx := t.Context()
	p := domain.CapacityProvider{AccountID: e.account, Name: "home", Kind: domain.ProviderFake, Mode: domain.ProviderModeDirect, Status: domain.ProviderActive}
	must(t, e.st.CapacityProviders().Create(ctx, &p))
	s := domain.Shape{AccountID: e.account, ProviderID: p.ID, Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Labels: []string{"docker"}, Image: "linux", Enabled: true}
	if edit != nil {
		edit(&s)
	}
	must(t, e.st.Shapes().Create(ctx, &s))
	m := domain.Machine{AccountID: e.account, ProviderID: p.ID, ShapeID: s.ID}
	must(t, e.st.Machines().Create(ctx, &m))
	token := "fcb_" + m.ID.String()
	tok := domain.BootstrapToken{MachineID: m.ID, AccountID: e.account, TokenHash: mcp.HashToken(token), ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.BootstrapTokens().Issue(ctx, &tok))
	return &machineEnv{env: e, shape: s, machine: m, token: token}
}

func (e *machineEnv) booting(t *testing.T) {
	t.Helper()
	must(t, e.st.Machines().UpdateState(t.Context(), e.machine.ID, domain.MachineRequested, domain.MachineBooting, ""))
}

func (e *machineEnv) bootstrap(t *testing.T, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.http.URL+bootstrapPath, bytes.NewBufferString(`{"version": "v9"}`))
	must(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	must(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

func TestBootstrap(t *testing.T) {
	e := newMachineEnv(t, func(s *domain.Shape) { s.MaxJobs = 2 })
	if code, _ := e.bootstrap(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := e.bootstrap(t, "fcb_unknown"); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", code)
	}
	// The guest can be faster than the provider's report.
	code, out := e.bootstrap(t, e.token)
	if code != http.StatusAccepted || out["retry_after_seconds"] != float64(5) {
		t.Fatalf("%d %v", code, out)
	}
	e.booting(t)
	code, out = e.bootstrap(t, e.token)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	worker, _ := out["worker_token"].(string)
	if !strings.HasPrefix(worker, "fcw_") || out["max_jobs"] != float64(2) || !strings.HasPrefix(out["name"].(string), "machine-") {
		t.Fatalf("answer = %v", out)
	}
	if labels := out["labels"].([]any); len(labels) != 4 {
		t.Fatalf("labels = %v", labels)
	}
	w, err := e.st.Workers().GetByTokenHash(t.Context(), mcp.HashToken(worker))
	must(t, err)
	m, err := e.st.Machines().Get(t.Context(), e.machine.ID)
	must(t, err)
	if !w.Ephemeral || w.Version != "v9" || m.WorkerID != w.ID || m.State != domain.MachineReady {
		t.Fatalf("worker = %+v, machine = %+v", w, m)
	}
	// It works once, and the worker token is a worker token.
	if code, _ := e.bootstrap(t, e.token); code != http.StatusUnauthorized {
		t.Fatalf("second use: %d", code)
	}
	if code, _ := e.poll(t, worker, "linux"); code != http.StatusNoContent {
		t.Fatalf("poll: %d", code)
	}
	for _, s := range []string{e.token, worker} {
		if strings.Contains(e.logs.String(), s) {
			t.Fatal("a token in the logs")
		}
	}
}

func TestBootstrapExpiredOrFinished(t *testing.T) {
	e := newMachineEnv(t, nil)
	e.booting(t)
	must(t, e.st.Machines().UpdateState(t.Context(), e.machine.ID, domain.MachineBooting, domain.MachineFailed, "broke"))
	if code, _ := e.bootstrap(t, e.token); code != http.StatusUnauthorized {
		t.Fatalf("failed machine: %d", code)
	}
}

// bootWorker brings the machine up and returns its worker and token.
func (e *machineEnv) bootWorker(t *testing.T) (domain.Worker, string) {
	t.Helper()
	e.booting(t)
	code, out := e.bootstrap(t, e.token)
	if code != http.StatusOK {
		t.Fatalf("bootstrap: %d %v", code, out)
	}
	token := out["worker_token"].(string)
	w, err := e.st.Workers().GetByTokenHash(t.Context(), mcp.HashToken(token))
	must(t, err)
	return w, token
}

func (e *machineEnv) queue(t *testing.T, r domain.Resources) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: e.account, ProjectID: e.project.ID, Role: "developer", Runtime: "claude-code", Resources: r,
		Spec: domain.JobSpec{Repo: repo, Prompt: "go", Timeout: domain.Duration(time.Hour)}}
	must(t, e.st.Jobs().Create(t.Context(), &j))
	return j
}

func TestMachineWorkerLeases(t *testing.T) {
	e := newMachineEnv(t, nil)
	_, token := e.bootWorker(t)
	big := e.queue(t, domain.Resources{MinMemoryMB: 1 << 20})
	fits := e.queue(t, domain.Resources{MinCPU: 2})

	code, got := e.poll(t, token)
	if code != http.StatusOK || got.Job.JobID != fits.ID {
		t.Fatalf("poll = %d %s; the big job is %s", code, got.Job.JobID, big.ID)
	}
	m, err := e.st.Machines().Get(t.Context(), e.machine.ID)
	must(t, err)
	if m.State != domain.MachineBusy || m.JobsTaken != 1 || m.JobID != fits.ID || m.DeadlineAt.IsZero() {
		t.Fatalf("machine = %+v", m)
	}
	e.queue(t, domain.Resources{})
	if code, _ := e.poll(t, token); code != http.StatusNoContent {
		t.Fatalf("poll past max_jobs = %d", code)
	}
}

// Of concurrent polls of one machine only max_jobs lease, whatever the race.
func TestMachineConcurrentPolls(t *testing.T) {
	e := newMachineEnv(t, func(s *domain.Shape) { s.MaxJobs = 2 })
	_, token := e.bootWorker(t)
	for range 6 {
		e.queue(t, domain.Resources{})
	}
	var mu sync.Mutex
	won := 0
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if code, _ := e.poll(t, token); code == http.StatusOK {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	m, err := e.st.Machines().Get(t.Context(), e.machine.ID)
	must(t, err)
	if won != 2 || m.JobsTaken != 2 {
		t.Fatalf("%d leases, jobs_taken %d", won, m.JobsTaken)
	}
}
