package fleet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
)

func TestOverview(t *testing.T) {
	e := newEnv(t)
	o, err := e.c.Overview(e.ctx, e.account)
	must(t, err)
	if len(o.Locations) != 1 || o.Locations[0].Name != "Home" || o.Locations[0].Status != fleet.StatusOffline {
		t.Fatalf("locations = %+v", o.Locations)
	}

	e.shape(e.prov, "small", func(s *domain.Shape) { s.PricePerMinute = 0.5 })
	windows := domain.Worker{AccountID: e.account, Name: "desk", MaxJobs: 2, TokenHash: []byte{1}}
	must(t, e.st.Workers().Create(e.ctx, &windows))
	must(t, e.st.Workers().Seen(e.ctx, e.account, windows.ID, []string{"windows", "only-here"}, "v1"))
	j := e.job(e.account, e.project, nil)
	static := e.job(e.account, e.project, func(j *domain.Job) { j.Needs = []string{"only-here"} })
	_, err = e.st.Jobs().LeaseNext(e.ctx, e.account, windows.ID, []string{"only-here", "runtime:codex"}, domain.LeaseDuration)
	must(t, err)
	e.reconcile()
	_ = static
	req := e.fake.Requests()
	if len(req) != 0 {
		// The job went to the static worker's second slot.
		t.Fatalf("%d creates", len(req))
	}
	_, err = e.st.Jobs().LeaseNext(e.ctx, e.account, windows.ID, []string{"only-here", "runtime:codex"}, domain.LeaseDuration)
	must(t, err)
	waiting := e.job(e.account, e.project, nil)
	e.reconcile()
	boot := e.fake.Requests()[0]
	w := e.boot(boot)
	_, err = e.lease(w)
	must(t, err)
	e.clock.Advance(4 * time.Minute)
	must(t, e.st.Workers().Seen(e.ctx, e.account, windows.ID, []string{"windows", "only-here"}, "v1"))
	e.reconcile()
	unmet := e.job(e.account, e.project, func(j *domain.Job) { j.Needs = []string{"gpu"} })
	_ = waiting

	o, err = e.c.Overview(e.ctx, e.account)
	must(t, err)
	if o.Working != 3 || o.Waiting != 1 || o.Queue[0].Ref != unmet.Ref() || o.Queue[0].Code != fleet.ReasonNoShape {
		t.Fatalf("overview = %+v", o)
	}
	if want := "1 job waiting: Nothing here runs a machine with gpu and runtime:codex."; o.Headline != want {
		t.Fatalf("headline = %q", o.Headline)
	}
	if len(o.Locations) != 2 {
		t.Fatalf("locations = %+v", o.Locations)
	}
	desk, home := o.Locations[0], o.Locations[1]
	if desk.Kind != fleet.LocationComputer || desk.Runs != "Windows, directly" || desk.Status != fleet.StatusOnline || desk.InUse != 2 || desk.Capacity != 2 || desk.Cost != "Free" {
		t.Fatalf("static worker = %+v", desk)
	}
	if home.Kind != fleet.LocationProvider || home.Runs != "Linux" || home.Status != fleet.StatusOnline || home.InUse != 1 || home.Cost != "~CHF 0.5/min" {
		t.Fatalf("location = %+v", home)
	}
	if o.Spend == nil || o.Spend.Amount != 2 || o.Spend.Currency != "CHF" {
		t.Fatalf("spend = %+v", o.Spend)
	}
	var onMachine *fleet.RunningJob
	for i := range o.Running {
		if o.Running[i].Location == "Home" {
			onMachine = &o.Running[i]
		}
	}
	if onMachine == nil || onMachine.Minutes != 4 || onMachine.Machine == "" || onMachine.Role != "developer" {
		t.Fatalf("running = %+v", o.Running)
	}
	_ = j
}

func TestOverviewHeadlines(t *testing.T) {
	e := newEnv(t)
	other, _ := e.org("other")
	o, err := e.c.Overview(e.ctx, other)
	must(t, err)
	if o.Headline != "No machines yet. Agents need somewhere to work." || o.Queue == nil || o.Running == nil {
		t.Fatalf("overview = %+v", o)
	}
	o, err = e.c.Overview(e.ctx, e.account)
	must(t, err)
	if o.Headline != "Everything is fine. 0 jobs running, none waiting." {
		t.Fatalf("headline = %q", o.Headline)
	}
	if _, err := e.c.Overview(e.ctx, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown organization: %v", err)
	}
}

func TestOverviewStatuses(t *testing.T) {
	e := newEnv(t)
	limit := 200.0
	e.hosted(func(s *domain.Shape) { s.PricePerMinute = 1; s.OS = domain.OSMacOS })
	e.policy(e.account, domain.RoutingOwnFirst, func(p *domain.FleetPolicy) { p.Quotas.MonthlyBudget = &limit })
	e.job(e.account, e.project, nil)
	e.reconcile()
	e.clock.Advance(time.Minute)
	must(t, e.st.CapacityProviders().SetStatus(e.ctx, e.prov.ID, domain.ProviderPaused))
	pending := domain.CapacityProvider{AccountID: e.account, Name: "aws", Kind: domain.ProviderAWS}
	must(t, e.st.CapacityProviders().Create(e.ctx, &pending))
	gone := e.provider(e.account, "gone", nil)
	must(t, e.st.CapacityProviders().SetStatus(e.ctx, gone.ID, domain.ProviderRevoked))
	o, err := e.c.Overview(e.ctx, e.account)
	must(t, err)
	got := map[string]string{}
	for _, l := range o.Locations {
		got[l.Name] = l.Status
	}
	want := map[string]string{"Home": fleet.StatusPaused, "Aws": fleet.StatusPending, "Gone": fleet.StatusRevoked, "Cloud": fleet.StatusOnline}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("%s = %q, want %q", name, got[name], status)
		}
	}
	if o.Spend == nil || o.Spend.Limit != limit {
		t.Fatalf("spend = %+v", o.Spend)
	}
	if o.Routing != domain.RoutingOwnFirst {
		t.Fatalf("routing = %s", o.Routing)
	}
}

func TestMachineName(t *testing.T) {
	m := domain.Machine{ID: domain.NewID()}
	s := m.ID.String()
	if got := fleet.MachineName(domain.Shape{Name: "linux-small"}, m); got != "linux-small-"+s[len(s)-3:] {
		t.Fatalf("name = %q", got)
	}
}
