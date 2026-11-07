package fleet_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func hashOf(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func TestExchange(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", func(s *domain.Shape) { s.Pool = domain.PoolControl; s.MaxJobs = 3; s.Labels = []string{"docker"} })
	e.job(e.account, e.project, func(j *domain.Job) { j.Pool = domain.PoolControl; j.Runtime = ""; j.Kind = domain.JobKindCommand })
	e.reconcile()
	req := e.fake.Requests()[0]
	hash := hashOf(req.Bootstrap.Token)

	if _, err := fleet.Exchange(e.ctx, e.st, []byte("unknown"), "v1"); !errors.Is(err, fleet.ErrBootstrapRefused) {
		t.Fatalf("unknown token: %v", err)
	}
	g, err := fleet.Exchange(e.ctx, e.st, hash, "v1")
	must(t, err)
	if g.Wait || g.MaxJobs != 3 || g.Name == "" {
		t.Fatalf("grant = %+v", g)
	}
	m := e.machine(req.MachineID)
	w, err := e.st.Workers().GetByTokenHash(e.ctx, hashOf(g.WorkerToken))
	must(t, err)
	if w.ID != m.WorkerID || w.Pool != domain.PoolControl || !w.Ephemeral || w.Version != "v1" || m.State != domain.MachineReady {
		t.Fatalf("worker = %+v, machine = %+v", w, m)
	}
	if _, err := fleet.Exchange(e.ctx, e.st, hash, "v1"); !errors.Is(err, fleet.ErrBootstrapRefused) {
		t.Fatalf("second use: %v", err)
	}
}

func TestExchangeGuestFasterThanReport(t *testing.T) {
	e := newEnv(t)
	s := e.shape(e.prov, "small", nil)
	m := domain.Machine{AccountID: e.account, ProviderID: e.prov.ID, ShapeID: s.ID}
	must(t, e.st.Machines().Create(e.ctx, &m))
	hash := hashOf("fcb_test")
	tok := domain.BootstrapToken{MachineID: m.ID, AccountID: e.account, TokenHash: hash, ExpiresAt: e.clock.Now().Add(time.Minute)}
	must(t, e.st.BootstrapTokens().Issue(e.ctx, &tok))
	g, err := fleet.Exchange(e.ctx, e.st, hash, "v1")
	must(t, err)
	if !g.Wait || e.machine(m.ID).State != domain.MachineRequested {
		t.Fatalf("grant = %+v", g)
	}
	must(t, e.st.Machines().UpdateState(e.ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	e.clock.Advance(2 * time.Minute)
	if _, err := fleet.Exchange(e.ctx, e.st, hash, "v1"); !errors.Is(err, fleet.ErrBootstrapRefused) {
		t.Fatalf("expired: %v", err)
	}
}

func TestExchangeRefusesFinishedMachines(t *testing.T) {
	for _, state := range []domain.MachineState{domain.MachineDraining, domain.MachineFailed} {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		e.job(e.account, e.project, nil)
		e.reconcile()
		req := e.fake.Requests()[0]
		must(t, e.st.Machines().UpdateState(e.ctx, req.MachineID, domain.MachineBooting, state, ""))
		if _, err := fleet.Exchange(e.ctx, e.st, hashOf(req.Bootstrap.Token), "v1"); !errors.Is(err, fleet.ErrBootstrapRefused) {
			t.Fatalf("%s: %v", state, err)
		}
	}
}

// Of two polls racing for the last job, the one that loses gives the lease
// up and the job stays queued.
func TestRecordLeaseLoser(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	other := e.job(e.account, e.project, nil)
	_, err := e.st.Machines().TakeJob(e.ctx, req.MachineID, other.ID, time.Time{})
	must(t, err)
	err = e.st.WithTx(e.ctx, func(ctx context.Context, tx store.Store) error {
		leased, err := e.leaseIgnoringShape(ctx, tx, w)
		if err != nil {
			return err
		}
		return fleet.RecordLease(ctx, tx, w, leased)
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("lease = %v", err)
	}
	if cur, _ := e.st.Jobs().Get(e.ctx, e.account, j.ID); cur.Status != domain.JobQueued {
		t.Fatalf("job = %s", cur.Status)
	}
}

// leaseIgnoringShape stands for a poll that passed the query before the
// machine took its last job.
func (e *env) leaseIgnoringShape(_ context.Context, _ store.Store, w domain.Worker) (domain.Job, error) {
	return domain.Job{AccountID: w.AccountID, Spec: domain.JobSpec{}, ID: domain.NewID()}, nil
}

func TestRecordLeaseOnDrainingMachine(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	must(t, e.st.Machines().UpdateState(e.ctx, req.MachineID, domain.MachineReady, domain.MachineDraining, ""))
	err := e.st.WithTx(e.ctx, func(ctx context.Context, tx store.Store) error { return fleet.RecordLease(ctx, tx, w, j) })
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("lease = %v", err)
	}
	if m := e.machine(req.MachineID); m.JobsTaken != 0 {
		t.Fatalf("the lease was not rolled back: %+v", m)
	}
}

func TestRecordLeaseNoMachine(t *testing.T) {
	e := newEnv(t)
	w := domain.Worker{AccountID: e.account, Name: "x", Ephemeral: true}
	if err := fleet.RecordLease(e.ctx, e.st, w, domain.Job{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeProvider(t *testing.T) {
	f := testkit.NewFakeProvider()
	booted := make(chan fleet.CreateRequest, 1)
	f.Configure(func(p *testkit.FakeProvider) {
		p.BootDelay = time.Millisecond
		p.OnBoot = func(r fleet.CreateRequest) { booted <- r }
	})
	id := domain.NewID()
	first, err := f.Create(t.Context(), fleet.CreateRequest{MachineID: id})
	must(t, err)
	second, err := f.Create(t.Context(), fleet.CreateRequest{MachineID: id}) // idempotent for the ID
	must(t, err)
	if n := len(f.Instances()); n != 1 || first.ExternalID == second.ExternalID {
		t.Fatalf("%d instances", n)
	}
	if r := <-booted; r.MachineID != id {
		t.Fatalf("booted %v", r.MachineID)
	}
	if err := f.Update(t.Context(), "nothing", fleet.UpdateRequest{}); err == nil {
		t.Fatal("updated nothing")
	}
	must(t, f.Destroy(t.Context(), "nothing"))
	must(t, f.Destroy(t.Context(), second.ExternalID))
	if got := f.Destroyed(); len(got) != 1 {
		t.Fatalf("destroyed %v", got)
	}
}

func TestProviderOrganization(t *testing.T) {
	st := testkit.NewStore(nil)
	ctx := t.Context()
	a := domain.Account{Name: "Acme"}
	must(t, st.Accounts().Create(ctx, &a))
	id, err := fleet.ProviderOrganization(ctx, st, "")
	must(t, err)
	if id != a.ID {
		t.Fatalf("a single organization: %s", id)
	}
	b := domain.Account{Name: "Other", Slug: "other"}
	must(t, st.Accounts().Create(ctx, &b))
	if _, err = fleet.ProviderOrganization(ctx, st, ""); !errors.Is(err, fleet.ErrOrgRequired) {
		t.Fatalf("several organizations: %v", err)
	}
	id, err = fleet.ProviderOrganization(ctx, st, fleet.OrgInstance)
	must(t, err)
	if id != uuid.Nil() {
		t.Fatalf("instance-wide: %s", id)
	}
	id, err = fleet.ProviderOrganization(ctx, st, "other")
	must(t, err)
	if id != b.ID {
		t.Fatalf("by slug: %s", id)
	}
	if _, err := fleet.ProviderOrganization(ctx, st, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown slug: %v", err)
	}
}
