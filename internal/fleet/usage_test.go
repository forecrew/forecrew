package fleet_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
)

func TestMeter(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	org := domain.NewID()
	own := domain.Shape{AccountID: org, PricePerMinute: 0.5}
	hosted := domain.Shape{PricePerMinute: 0.013}
	for name, c := range map[string]struct {
		m             domain.Machine
		s             domain.Shape
		end           time.Time
		start         time.Time
		minutes, cost float64
		ownership     domain.Ownership
	}{
		// An own machine is billed from its start at the provider.
		"own": {domain.Machine{AccountID: org, AssignedAt: t0, BootingAt: t0.Add(time.Minute)}, own, t0.Add(11 * time.Minute), t0.Add(time.Minute), 10, 5, domain.OwnershipOwn},
		// A warm machine from its assignment.
		"warm": {domain.Machine{AccountID: org, AssignedAt: t0.Add(time.Hour), BootingAt: t0}, hosted, t0.Add(time.Hour + 30*time.Second), t0.Add(time.Hour), 0.5, 0.0065, domain.OwnershipHosted},
		// A machine that never started costs nothing.
		"never started": {domain.Machine{AccountID: org, AssignedAt: t0}, hosted, t0.Add(time.Hour), t0.Add(time.Hour), 0, 0, domain.OwnershipHosted},
		// Minutes to the thousandth, and cost from them.
		"rounding": {domain.Machine{AccountID: org, AssignedAt: t0, BootingAt: t0}, hosted, t0.Add(1234567 * time.Microsecond), t0, 0.021, 0.000273, domain.OwnershipHosted},
	} {
		t.Run(name, func(t *testing.T) {
			u := fleet.Meter(c.m, c.s, c.end)
			if !u.StartedAt.Equal(c.start) || !u.EndedAt.Equal(c.end) || u.Minutes != c.minutes || u.Cost != c.cost || u.Ownership != c.ownership || u.AccountID != org {
				t.Fatalf("record = %+v", u)
			}
			if err := u.Validate(); err != nil && c.m.ID != uuid.Nil() {
				t.Fatal(err)
			}
		})
	}
}

// runOne takes a machine through a whole job on the fake provider, with the
// clock moving: up after boot, busy for work, and gone at the next pass.
func (e *env) runOne(account, project uuid.UUID, boot, work time.Duration) domain.Machine {
	e.t.Helper()
	j := e.job(account, project, nil)
	before := len(e.fake.Requests())
	e.reconcile()
	reqs := e.fake.Requests()
	if len(reqs) != before+1 {
		e.t.Fatalf("%d creates, want %d", len(reqs), before+1)
	}
	req := reqs[len(reqs)-1]
	e.clock.Advance(boot)
	w := e.boot(req)
	got, err := e.lease(w)
	must(e.t, err)
	if got.ID != j.ID {
		e.t.Fatalf("leased %s", got.Ref())
	}
	e.clock.Advance(work)
	e.finishJob(w, got)
	e.reconcile() // drains it
	e.reconcile() // destroys it
	m := e.machine(req.MachineID)
	if m.State != domain.MachineDestroyed {
		e.t.Fatalf("machine is %s", m.State)
	}
	return m
}

// Metering sums match the machines' lifetimes, and each machine is billed
// once, to its organization, at its shape's price.
func TestMeteringMatchesLifetimes(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", func(s *domain.Shape) { s.PricePerMinute = 0.25 })
	ms := []domain.Machine{
		e.runOne(e.account, e.project, 20*time.Second, 10*time.Minute),
		e.runOne(e.account, e.project, 40*time.Second, 3*time.Minute+500*time.Millisecond),
	}
	var minutes float64
	for _, m := range ms {
		life := m.DestroyedAt.Sub(m.BootingAt).Minutes()
		minutes += life
		got, err := e.st.Usage().List(e.ctx, e.account, store.UsageFilter{})
		must(t, err)
		var u *domain.UsageRecord
		for i := range got {
			if got[i].MachineID == m.ID {
				u = &got[i]
			}
		}
		if u == nil {
			t.Fatalf("no record of machine %s", m.ID)
		}
		if diff := u.Minutes - life; diff > 0.0005 || diff < -0.0005 || !u.StartedAt.Equal(m.BootingAt) || !u.EndedAt.Equal(m.DestroyedAt) {
			t.Fatalf("record %+v of a machine that lived %.4f minutes", u, life)
		}
		if u.Cost != fleet.Meter(m, domain.Shape{AccountID: e.account, PricePerMinute: 0.25}, m.DestroyedAt).Cost || u.Ownership != domain.OwnershipOwn {
			t.Fatalf("record %+v", u)
		}
	}
	sum, err := e.st.Usage().Sum(e.ctx, e.account, store.UsageFilter{})
	must(t, err)
	if d := sum.Minutes - minutes; d > 0.001 || d < -0.001 {
		t.Fatalf("sum = %.4f minutes, lifetimes %.4f", sum.Minutes, minutes)
	}
	if n := e.audited(e.account, fleet.ActionDestroyed); n != 2 {
		t.Fatalf("%d destroy entries", n)
	}
	// A report of the end after it was metered changes nothing.
	must(t, e.c.ReportState(e.ctx, e.prov, ms[0].ID, domain.MachineDestroyed, "", ""))
	again, err := e.st.Usage().Sum(e.ctx, e.account, store.UsageFilter{})
	must(t, err)
	if again != sum {
		t.Fatalf("sum moved from %+v to %+v", sum, again)
	}
	other, err := e.st.Usage().Sum(e.ctx, domain.NewID(), store.UsageFilter{})
	must(t, err)
	if other != (store.UsageTotal{}) {
		t.Fatalf("another organization was billed %+v", other)
	}
}

// A machine that ended without a record is metered at a later pass, with an
// audit entry, and only once.
func TestBackfillMetersMissedMachines(t *testing.T) {
	e := newEnv(t)
	shape := e.shape(e.prov, "small", func(s *domain.Shape) { s.PricePerMinute = 1 })
	m := domain.Machine{AccountID: e.account, ProviderID: e.prov.ID, ShapeID: shape.ID}
	must(t, e.st.Machines().Create(e.ctx, &m))
	must(t, e.st.Machines().UpdateState(e.ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	e.clock.Advance(6 * time.Minute)
	must(t, e.st.Machines().UpdateState(e.ctx, m.ID, domain.MachineBooting, domain.MachineFailed, "lost"))
	must(t, e.st.Machines().UpdateState(e.ctx, m.ID, domain.MachineFailed, domain.MachineDestroyed, ""))

	e.reconcile()
	got, err := e.st.Usage().List(e.ctx, e.account, store.UsageFilter{})
	must(t, err)
	if len(got) != 1 || got[0].Minutes != 6 || got[0].Cost != 6 {
		t.Fatalf("records = %+v", got)
	}
	if n := e.audited(e.account, fleet.ActionMetered); n != 1 {
		t.Fatalf("%d metered entries", n)
	}
	e.clock.Advance(2 * time.Hour)
	e.reconcile()
	if n := e.audited(e.account, fleet.ActionMetered); n != 1 {
		t.Fatalf("%d metered entries after another pass", n)
	}
}

// The report sums the month's records by shape and ownership, and estimates
// what runs now apart.
func TestUsageReport(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", func(s *domain.Shape) { s.PricePerMinute = 0.5 })
	a := e.runOne(e.account, e.project, 30*time.Second, 4*time.Minute)
	b := e.runOne(e.account, e.project, 30*time.Second, 6*time.Minute)
	e.job(e.account, e.project, nil)
	e.reconcile() // a machine starts and keeps running
	e.clock.Advance(2 * time.Minute)

	month := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	u, err := e.c.Usage(e.ctx, e.account, month, month.AddDate(0, 1, 0), true)
	must(t, err)
	want := fleet.UsageTotals{Machines: 2, Minutes: 11, Cost: 5.5} // boot time is billed too
	if u.Total != want || u.Own != want || u.Hosted != (fleet.UsageTotals{}) || u.Currency != "CHF" {
		t.Fatalf("report = %+v", u)
	}
	if len(u.Shapes) != 1 || u.Shapes[0].Shape != "small" || u.Shapes[0].UsageTotals != want || u.Shapes[0].Ownership != domain.OwnershipOwn {
		t.Fatalf("shapes = %+v", u.Shapes)
	}
	if len(u.Records) != 2 || u.Records[0].MachineID != a.ID || u.Records[1].MachineID != b.ID {
		t.Fatalf("records = %+v", u.Records)
	}
	if u.Running != (fleet.UsageTotals{Machines: 1, Minutes: 2, Cost: 1}) {
		t.Fatalf("running = %+v", u.Running)
	}
	before, err := e.c.Usage(e.ctx, e.account, month.AddDate(0, -1, 0), month, false)
	must(t, err)
	if before.Total != (fleet.UsageTotals{}) || before.Running != (fleet.UsageTotals{}) || before.Records != nil {
		t.Fatalf("last month = %+v", before)
	}
}

// A machine of a location revoked under it fails and is never reported gone:
// it is billed up to its failure, once, and stops counting against limits.
func TestRevokedLocationMachineIsBilledToItsFailure(t *testing.T) {
	e := newEnv(t)
	agent, shape := e.agentProvider(e.account)
	shape.PricePerMinute = 0.5
	must(t, e.st.Shapes().Update(e.ctx, &shape))
	e.job(e.account, e.project, nil)
	e.reconcile()
	ms := e.machines(store.MachineFilter{ShapeID: shape.ID})
	if len(ms) != 1 {
		t.Fatalf("%d machines", len(ms))
	}
	must(t, e.c.ReportState(e.ctx, agent, ms[0].ID, domain.MachineBooting, "vm-1", ""))
	e.clock.Advance(10 * time.Minute)
	must(t, e.st.CapacityProviders().SetStatus(e.ctx, agent.ID, domain.ProviderRevoked))
	e.reconcile()
	m := e.machine(ms[0].ID)
	if m.State != domain.MachineFailed {
		t.Fatalf("machine = %s", m.State)
	}
	list, err := e.st.Usage().List(e.ctx, e.account, store.UsageFilter{})
	must(t, err)
	if len(list) != 1 || list[0].MachineID != m.ID || list[0].Minutes != 10 || list[0].Cost != 5 || !list[0].EndedAt.Equal(m.UpdatedAt) {
		t.Fatalf("records = %+v", list)
	}
	// Months on, it still counts 10 minutes, and a late report of its end
	// bills nothing more.
	e.clock.Advance(2 * time.Hour)
	e.reconcile()
	must(t, e.c.ReportState(e.ctx, agent, m.ID, domain.MachineDestroyed, "", ""))
	sum, err := e.st.Usage().Sum(e.ctx, e.account, store.UsageFilter{})
	must(t, err)
	if sum.Minutes != 10 {
		t.Fatalf("billed %.3f minutes", sum.Minutes)
	}
	month := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	u, err := e.c.Usage(e.ctx, e.account, month, month.AddDate(0, 1, 0), false)
	must(t, err)
	if u.Running != (fleet.UsageTotals{}) {
		t.Fatalf("a failed machine still runs up cost: %+v", u.Running)
	}
}

// A failed machine left without a record, as rows from before metering are,
// is billed by the catch-up up to its failure.
func TestBackfillMetersFailedMachines(t *testing.T) {
	e := newEnv(t)
	agent, _ := e.agentProvider(e.account) // nothing destroys its machines from here
	shape := e.shape(agent, "small", func(s *domain.Shape) { s.PricePerMinute = 1 })
	m := domain.Machine{AccountID: e.account, ProviderID: agent.ID, ShapeID: shape.ID}
	must(t, e.st.Machines().Create(e.ctx, &m))
	must(t, e.st.Machines().UpdateState(e.ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	e.clock.Advance(4 * time.Minute)
	must(t, e.st.Machines().UpdateState(e.ctx, m.ID, domain.MachineBooting, domain.MachineFailed, "lost"))
	e.clock.Advance(time.Hour)
	e.reconcile()
	got, err := e.st.Usage().List(e.ctx, e.account, store.UsageFilter{})
	must(t, err)
	if len(got) != 1 || got[0].Minutes != 4 {
		t.Fatalf("records = %+v", got)
	}
}
