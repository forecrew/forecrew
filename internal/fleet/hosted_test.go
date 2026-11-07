package fleet_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

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

// hostedOf counts the hosted machines each organization has.
func (e *env) hostedOf(accounts ...uuid.UUID) []int {
	e.t.Helper()
	out := make([]int, len(accounts))
	for i, a := range accounts {
		out[i] = len(e.machines(store.MachineFilter{AccountID: a, Active: true}))
	}
	return out
}

// Two busy organizations share the hosted fleet, however many jobs the first
// queued before the other; a paid one gets the bigger share.
func TestFairShare(t *testing.T) {
	for name, c := range map[string]struct {
		planB domain.FleetPlan
		want  []int
	}{
		"same plan":  {domain.PlanFree, []int{2, 2}},
		"paid first": {domain.PlanPaid, []int{1, 3}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.hosted(func(s *domain.Shape) { s.Max = 4 })
			orgB, projectB := e.org("beta")
			e.policy(e.account, domain.RoutingHosted, nil)
			e.policy(orgB, domain.RoutingHosted, func(p *domain.FleetPolicy) { p.Plan = c.planB })
			for range 6 {
				e.job(e.account, e.project, nil)
			}
			e.clock.Advance(time.Minute)
			var waiting domain.Job
			for range 6 {
				waiting = e.job(orgB, projectB, nil)
			}
			e.reconcile()
			if got := e.hostedOf(e.account, orgB); !slices.Equal(got, c.want) {
				t.Fatalf("machines = %v, want %v", got, c.want)
			}
			if r := e.reason(waiting); r.Code != fleet.ReasonQueue {
				t.Fatalf("a job past the fleet's room: %+v", r)
			}
		})
	}
}

// The free plan's limits stop hosted machines with a reason that says so;
// a paid organization and own machines are not held by them.
func TestFreePlanLimits(t *testing.T) {
	e := newEnv(t)
	e.c = fleet.New(fleet.Config{Store: e.st, Now: e.clock.Now, Currency: "CHF", ServerURL: "https://forecrew.example.com",
		Direct:   func(p domain.CapacityProvider) fleet.Provider { return e.drivers[p.ID] },
		FreePlan: domain.FleetQuotas{ConcurrentMachines: 1, MonthlyMinutes: 30}})
	cloud, _ := e.hosted(nil)
	orgB, projectB := e.org("beta")
	e.policy(e.account, domain.RoutingHosted, nil)
	e.policy(orgB, domain.RoutingHosted, func(p *domain.FleetPolicy) { p.Plan = domain.PlanPaid })
	e.job(e.account, e.project, nil)
	second := e.job(e.account, e.project, nil)
	e.job(orgB, projectB, nil)
	e.job(orgB, projectB, nil)
	e.reconcile()
	if got := e.hostedOf(e.account, orgB); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("machines = %v", got)
	}
	wantReason(t, e.reason(second), fleet.ReasonLimit, "The free plan runs 1 hosted machine at once")

	// Half an hour later the month's minutes are used up.
	for _, req := range cloud.Requests() {
		if e.machine(req.MachineID).AccountID == e.account {
			e.boot(req)
		}
	}
	e.clock.Advance(31 * time.Minute)
	for _, m := range e.machines(store.MachineFilter{AccountID: e.account, Active: true}) {
		must(t, e.st.Machines().UpdateState(e.ctx, m.ID, m.State, domain.MachineFailed, "gone"))
	}
	e.reconcile()
	if got := e.hostedOf(e.account); got[0] != 0 {
		t.Fatalf("%d machines past the month's minutes", got[0])
	}
	wantReason(t, e.reason(second), fleet.ReasonBudget, "The free plan's 30 minutes of hosted machines this month are used up")
}
