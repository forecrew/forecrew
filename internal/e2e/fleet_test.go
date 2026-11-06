//go:build e2e

package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/worker"
)

// addFleet gives the organization a direct provider with one shape in pool
// control, whose machines boot a real worker daemon with their bootstrap token.
func (w *world) addFleet() {
	w.t.Helper()
	ctx := w.t.Context()
	p := domain.CapacityProvider{AccountID: w.account.ID, Name: "home", Kind: domain.ProviderFake, Mode: domain.ProviderModeDirect, Status: domain.ProviderActive,
		Capabilities: domain.ProviderCapabilities{ImageFormats: []domain.ImageFormat{domain.ImageRootfs}}}
	must(w.t, w.st.CapacityProviders().Create(ctx, &p))
	// The shape's image must be registered, as a person would, for the resolver to find it.
	img := testkit.TestImage(uuid.Nil(), "forecrew-linux", "1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	must(w.t, w.st.Images().Create(ctx, &img))
	must(w.t, w.st.Shapes().Create(ctx, &domain.Shape{
		AccountID: w.account.ID, ProviderID: p.ID, Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Pool: domain.PoolControl, Labels: []string{"runtime:claude-code"}, Image: "forecrew-linux", Max: 2, Enabled: true,
	}))
	w.machines.Configure(func(f *testkit.FakeProvider) {
		f.Caps.ImageFormats = []domain.ImageFormat{domain.ImageRootfs}
		f.OnBoot = func(req fleet.CreateRequest) {
			client := worker.NewClient(w.url, req.Bootstrap.Token, nil)
			client.BootstrapWait = 10 * time.Millisecond
			g, err := client.Bootstrap(ctx, "e2e")
			if err != nil {
				w.t.Errorf("bootstrap: %v", err)
				return
			}
			w.runWorker(&workerProc{Name: g.Name}, g.WorkerToken, g.Labels)
		}
	})
}

// A job with no worker anywhere: a machine is created, boots, runs it and is
// destroyed, with the bootstrap token in no log.
func TestFleetMachineRunsAJob(t *testing.T) {
	w := newWorld(t, true)
	w.addFleet()
	var job domain.Job
	if code := w.api(http.MethodPost, "/jobs", mcp.DispatchJobInput{Project: "app", Role: "product-owner"}, &job); code != http.StatusCreated {
		t.Fatalf("dispatch: status %d", code)
	}
	eventually(t, "the job to succeed on a machine", func() bool {
		var info mcp.JobInfo
		w.api(http.MethodGet, "/jobs/"+job.Ref(), nil, &info)
		return info.Status == domain.JobSucceeded
	})
	eventually(t, "the machine to be destroyed", func() bool {
		m, err := w.st.Machines().List(t.Context(), store.MachineFilter{AccountID: w.account.ID, State: domain.MachineDestroyed})
		must(t, err)
		return len(m) == 1 && len(w.machines.Instances()) == 0
	})
	reqs := w.machines.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d machines for one job", len(reqs))
	}
	m, err := w.st.Machines().Get(t.Context(), reqs[0].MachineID)
	must(t, err)
	if m.JobID != job.ID || m.JobsTaken != 1 {
		t.Fatalf("machine = %+v", m)
	}
	if ran := w.ag.workersOf(job.ID); len(ran) != 1 || !strings.HasPrefix(ran[0], "machine-") {
		t.Fatalf("job ran on %v", ran)
	}
	if strings.Contains(w.logs.String(), reqs[0].Bootstrap.Token) {
		t.Fatal("the bootstrap token is in the logs")
	}

	// Over the API people see what happened, and the worker is gone.
	var machines []mcp.MachineInfo
	if code := w.api(http.MethodGet, "/machines?state=destroyed", nil, &machines); code != http.StatusOK || len(machines) != 1 || machines[0].JobRef != job.Ref() {
		t.Fatalf("machines: status %d, %+v", code, machines)
	}
	var overview fleet.Overview
	if code := w.api(http.MethodGet, "/fleet", nil, &overview); code != http.StatusOK || overview.Waiting != 0 || overview.Working != 0 {
		t.Fatalf("overview: status %d, %+v", code, overview)
	}
	var workers []mcp.WorkerInfo
	w.api(http.MethodGet, "/workers", nil, &workers)
	if i := slices.IndexFunc(workers, func(x mcp.WorkerInfo) bool { return x.Ephemeral }); i < 0 || !workers[i].Revoked() {
		t.Fatalf("workers = %+v", workers)
	}
}

// A machine that never comes up is replaced, and the job runs on the next.
func TestFleetBootFailure(t *testing.T) {
	w := newWorld(t, true)
	w.addFleet()
	w.machines.Configure(func(f *testkit.FakeProvider) { f.FailCreates = 1 })
	var job domain.Job
	if code := w.api(http.MethodPost, "/jobs", mcp.DispatchJobInput{Project: "app", Role: "product-owner"}, &job); code != http.StatusCreated {
		t.Fatalf("dispatch: status %d", code)
	}
	eventually(t, "the job to succeed on the replacement", func() bool {
		var info mcp.JobInfo
		w.api(http.MethodGet, "/jobs/"+job.Ref(), nil, &info)
		return info.Status == domain.JobSucceeded
	})
	all, err := w.st.Machines().List(t.Context(), store.MachineFilter{AccountID: w.account.ID})
	must(t, err)
	if len(all) != 2 {
		t.Fatalf("%d machines", len(all))
	}
}
