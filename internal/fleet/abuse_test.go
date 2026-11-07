package fleet_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
)

// stopper cancels jobs in the store, as jobs.Service does, and remembers who
// asked.
type stopper struct {
	st  store.Store
	by  []domain.Actor
	err error
}

func (s *stopper) Cancel(ctx context.Context, by domain.Actor, accountID, jobID uuid.UUID) (domain.Job, error) {
	s.by = append(s.by, by)
	if s.err != nil {
		return domain.Job{}, s.err
	}
	return s.st.Jobs().Cancel(ctx, accountID, jobID)
}

// A machine stopped for abuse fails, its job is canceled rather than run
// again elsewhere, the organization's log says so and the operator gets one
// notice however often the agent reports it.
func TestReportAbuse(t *testing.T) {
	e := newEnv(t)
	stop := &stopper{st: e.st}
	e.c.StopJobsWith(stop)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	if _, err := e.lease(w); err != nil {
		t.Fatal(err)
	}

	if err := e.c.ReportAbuse(e.ctx, e.prov, req.MachineID, "spam", ""); !errors.Is(err, fleet.ErrBadReport) {
		t.Fatalf("unknown kind: %v", err)
	}
	other := e.provider(e.account, "other", nil)
	if err := e.c.ReportAbuse(e.ctx, other, req.MachineID, domain.AbuseCPU, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another provider's machine: %v", err)
	}
	for range 2 {
		must(t, e.c.ReportAbuse(e.ctx, e.prov, req.MachineID, domain.AbuseMining, "stratum key=sk-ant-abcdefghijkl"))
	}
	m := e.machine(req.MachineID)
	got, err := e.st.Jobs().Get(e.ctx, e.account, j.ID)
	must(t, err)
	if m.State != domain.MachineFailed || got.Status != domain.JobCanceled || len(stop.by) != 1 || stop.by[0].Kind != domain.ActorSystem {
		t.Fatalf("machine %s, job %s, stops %+v", m.State, got.Status, stop.by)
	}
	notices, err := e.st.FleetNotices().List(e.ctx, false, 0)
	must(t, err)
	if len(notices) != 1 || notices[0].JobID != j.ID || notices[0].AccountID != e.account || notices[0].Detail != "stratum key=[redacted]" {
		t.Fatalf("notices = %+v", notices)
	}
	if n := e.audited(e.account, fleet.ActionAbuse); n != 1 {
		t.Fatalf("%d abuse entries", n)
	}
	// The machine goes as any failed one.
	e.reconcile()
	if m := e.machine(req.MachineID); m.State != domain.MachineDestroyed {
		t.Fatalf("machine = %s", m.State)
	}
}

// Without a way to stop jobs, or when the job finished meanwhile, the notice
// still opens and names the job.
func TestReportAbuseLeavesFinishedJobs(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	leased, err := e.lease(w)
	must(t, err)
	e.finishJob(w, leased)
	stop := &stopper{st: e.st, err: errors.New("down")}
	e.c.StopJobsWith(stop)
	must(t, e.c.ReportAbuse(e.ctx, e.prov, req.MachineID, domain.AbuseCPU, "busy"))
	notices, _ := e.st.FleetNotices().List(e.ctx, false, 0)
	if len(notices) != 1 || notices[0].JobID != j.ID || len(stop.by) != 0 {
		t.Fatalf("notices %+v, stops %d", notices, len(stop.by))
	}
	// A running job the stopper cannot cancel is an error to retry.
	e2 := newEnv(t)
	e2.shape(e2.prov, "small", nil)
	e2.job(e2.account, e2.project, nil)
	e2.reconcile()
	req = e2.fake.Requests()[0]
	w = e2.boot(req)
	_, err = e2.lease(w)
	must(t, err)
	e2.c.StopJobsWith(&stopper{st: e2.st, err: errors.New("down")})
	if err := e2.c.ReportAbuse(e2.ctx, e2.prov, req.MachineID, domain.AbuseCPU, "busy"); err == nil {
		t.Fatal("a job that could not be stopped")
	}
}
