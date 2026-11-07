package fleet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
)

// A hosted machine runs one job and goes, whatever max_jobs its shape says:
// no job of a paying organization finds what the last one left.
func TestHostedMachineTakesOneJob(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(func(s *domain.Shape) { s.MaxJobs = 3; s.IdleSeconds = 600 })
	e.policy(e.account, domain.RoutingHosted, nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	req := cloud.Requests()[0]
	w := e.boot(req)
	if w.MaxJobs != 1 {
		t.Fatalf("worker max_jobs = %d", w.MaxJobs)
	}
	j, err := e.lease(w)
	must(t, err)
	e.job(e.account, e.project, nil)
	if _, err := e.lease(w); !errors.Is(err, store.ErrNoJob) && !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second lease: %v", err)
	}
	e.reconcile()
	if n := len(cloud.Requests()); n != 2 {
		t.Fatalf("%d creates: the second job gets a machine of its own", n)
	}
	e.finishJob(w, j)
	e.reconcile()
	e.reconcile()
	if m := e.machine(req.MachineID); m.State != domain.MachineDestroyed {
		t.Fatalf("a hosted machine after its job is %s", m.State)
	}
}

// The lease itself refuses a second job, even when the leasing query lets it
// through: the machine's row is what counts.
func TestHostedLeaseRefusedPastOneJob(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(func(s *domain.Shape) { s.MaxJobs = 3 })
	e.policy(e.account, domain.RoutingHosted, nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	w := e.boot(cloud.Requests()[0])
	_, err := e.lease(w)
	must(t, err)
	second := e.job(e.account, e.project, nil)
	err = fleet.RecordLease(e.ctx, e.st, w, second)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second job on a hosted machine: %v", err)
	}
}

// A hosted machine reaches the default list, the hosts of the organization's
// model connections and its own egress; an own location's keeps what the
// location allows.
func TestHostedEgress(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(func(s *domain.Shape) { s.WarmMin = 1 })
	e.policy(e.account, domain.RoutingHosted, func(p *domain.FleetPolicy) { p.Egress = []string{"pkgs.acme.example", "github.com"} })
	must(t, e.st.ModelConnections().Put(e.ctx, &domain.ModelConnection{AccountID: e.account, Name: "gateway", Vendor: "openai", Adapter: "codex",
		BaseURL: "https://llm.acme.example/v1", SecretRef: "store:llm", Enabled: true}))
	e.reconcile()
	warm := cloud.Requests()[0]
	if !slices.Equal(warm.NetworkAllow, fleet.HostedEgress) {
		t.Fatalf("a warm machine reaches %v", warm.NetworkAllow)
	}
	e.job(e.account, e.project, nil)
	e.reconcile()
	got := cloud.Updates()[e.machine(warm.MachineID).ExternalID].NetworkAllow
	want := append(slices.Clone(fleet.HostedEgress), "llm.acme.example", "pkgs.acme.example")
	if !slices.Equal(got, want) {
		t.Fatalf("assigned machine reaches %v, want %v", got, want)
	}

	own := e.shape(e.prov, "small", nil)
	e.policy(e.account, domain.RoutingOwnOnly, nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	for _, r := range e.fake.Requests() {
		if r.Shape.ID == own.ID && len(r.NetworkAllow) != 0 {
			t.Fatalf("own machine reaches %v", r.NetworkAllow)
		}
	}
}
