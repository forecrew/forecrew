package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// noWorkers is a store whose worker reads fail.
type noWorkers struct{ store.Store }

func (noWorkers) Workers() store.WorkerRepo { return failingWorkers{} }

type failingWorkers struct{ store.WorkerRepo }

func (failingWorkers) List(context.Context, uuid.UUID) ([]domain.Worker, error) { return nil, errDown }
func (failingWorkers) Get(context.Context, uuid.UUID, uuid.UUID) (domain.Worker, error) {
	return domain.Worker{}, errDown
}

func TestStatus(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t, "Docker")

	noWorker := "no online worker in pool build with labels docker, runtime:claude-code"
	v, err := f.svc.Status(ctx, f.account, j.ID)
	must(t, err)
	if v.Reason != noWorker {
		t.Fatalf("reason = %q, want %q (worker never seen)", v.Reason, noWorker)
	}
	must(t, f.st.Workers().Seen(ctx, f.account, f.worker.ID, []string{"runtime:claude-code"}, "v1"))
	if v, _ = f.svc.Status(ctx, f.account, j.ID); v.Reason != noWorker {
		t.Fatalf("reason = %q, want no match (label missing)", v.Reason)
	}
	must(t, f.st.Workers().Seen(ctx, f.account, f.worker.ID, []string{"DOCKER", "Runtime:Claude-Code"}, "v1"))
	if v, _ = f.svc.Status(ctx, f.account, j.ID); v.Reason != "" {
		t.Fatalf("reason = %q, want none (labels match case-insensitively)", v.Reason)
	}
	f.clock.Advance(domain.OnlineWindow + time.Second)
	if v, _ = f.svc.Status(ctx, f.account, j.ID); v.Reason != noWorker {
		t.Fatalf("reason = %q, want no match (worker offline)", v.Reason)
	}

	f.lease(t, "docker")
	v, err = f.svc.Status(ctx, f.account, j.ID)
	must(t, err)
	if v.Status != domain.JobRunning || v.WorkerName != "box" || v.Reason != "" {
		t.Fatalf("running view = %+v", v)
	}
	_, err = f.svc.Release(ctx, f.worker, j.ID, "x")
	must(t, err)
	if v, _ = f.svc.Status(ctx, f.account, j.ID); v.Attempts != 1 || v.Status != domain.JobQueued {
		t.Fatalf("released view = %+v", v)
	}

	failed := f.newJob(t)
	f.lease(t)
	_, err = f.svc.Complete(ctx, f.worker, failed.ID, store.JobResult{Status: domain.JobFailed, Error: "boom"})
	must(t, err)
	if v, _ = f.svc.Status(ctx, f.account, failed.ID); v.Reason != "boom" {
		t.Fatalf("failed reason = %q", v.Reason)
	}

	_, err = f.svc.Status(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

// TestStatusOtherPool: an online worker with every label but of another
// pool does not run the job, and the reason names the job's pool.
func TestStatusOtherPool(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	review, err := f.svc.Create(ctx, system, domain.Job{
		AccountID: f.account, ProjectID: f.project.ID, Pool: domain.PoolControl, Role: "reviewer", Runtime: "codex",
		Spec: domain.JobSpec{Repo: "acme/app", Prompt: "review"},
	})
	must(t, err)
	must(t, f.st.Workers().Seen(ctx, f.account, f.worker.ID, []string{"runtime:codex"}, "v1"))
	v, err := f.svc.Status(ctx, f.account, review.ID)
	must(t, err)
	if want := "no online worker in pool control with labels runtime:codex"; v.Reason != want {
		t.Fatalf("reason = %q, want %q", v.Reason, want)
	}
	ctl := domain.Worker{AccountID: f.account, Name: "ctl", Pool: domain.PoolControl, TokenHash: []byte("c")}
	must(t, f.st.Workers().Create(ctx, &ctl))
	must(t, f.st.Workers().Seen(ctx, f.account, ctl.ID, []string{"runtime:codex"}, "v1"))
	if v, _ = f.svc.Status(ctx, f.account, review.ID); v.Reason != "" {
		t.Fatalf("reason = %q, want none (control worker online)", v.Reason)
	}
	if got := NoWorkerReason(domain.Job{Kind: domain.JobKindCommand}); got != "no online worker in pool build" {
		t.Fatalf("command reason = %q", got)
	}
}

func TestStatusErrors(t *testing.T) {
	f := newFixture(t)
	queued := f.newJob(t)
	running := f.newJob(t)
	f.lease(t)
	broken := New(noWorkers{f.st}, f.bus, nil, f.svc.logger, f.clock.Now)
	_, err := broken.Status(t.Context(), f.account, queued.ID)
	wantErr(t, err, errDown)
	_, err = broken.Status(t.Context(), f.account, running.ID)
	wantErr(t, err, errDown)
}

func TestLogs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t)
	var events []domain.JobEvent
	for i := range 5 {
		events = append(events, domain.JobEvent{Time: t0.Add(time.Duration(i) * time.Second), Type: domain.EventLog, Text: "line"})
	}
	_, err := f.st.JobEvents().Append(ctx, f.account, j.ID, events)
	must(t, err)

	page, err := f.svc.Logs(ctx, f.account, j.ID, 0, 2)
	must(t, err)
	if len(page.Events) != 2 || page.NextSeq != 2 {
		t.Fatalf("page 1 = %+v", page)
	}
	page, err = f.svc.Logs(ctx, f.account, j.ID, page.NextSeq, 0)
	must(t, err)
	if len(page.Events) != 3 || page.NextSeq != 5 || page.Events[0].Seq != 3 {
		t.Fatalf("page 2 = %+v", page)
	}
	page, err = f.svc.Logs(ctx, f.account, j.ID, 5, MaxLogLimit+1)
	must(t, err)
	if len(page.Events) != 0 || page.NextSeq != 5 {
		t.Fatalf("empty page = %+v", page)
	}
	_, err = f.svc.Logs(ctx, f.account, domain.NewID(), 0, 0)
	wantErr(t, err, store.ErrNotFound)
}

func TestWorkers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	idle := domain.Worker{AccountID: f.account, Name: "idle", TokenHash: []byte("i")}
	must(t, f.st.Workers().Create(ctx, &idle))
	a, b := f.newJob(t), f.newJob(t)
	f.lease(t)
	f.lease(t)
	must(t, f.st.Workers().Seen(ctx, f.account, f.worker.ID, []string{"Go"}, "v1.2.3"))

	views, err := f.svc.Workers(ctx, f.account)
	must(t, err)
	if len(views) != 2 {
		t.Fatalf("views = %+v", views)
	}
	box, other := views[0], views[1]
	if box.Name != "box" || !box.Online || box.Version != "v1.2.3" || !slices.Equal(box.Labels, []string{"go"}) ||
		!slices.Equal(box.RunningJobs, []string{a.Ref(), b.Ref()}) {
		t.Fatalf("box = %+v", box)
	}
	if other.Name != "idle" || other.Online || other.RunningJobs == nil || len(other.RunningJobs) != 0 {
		t.Fatalf("idle = %+v", other)
	}

	f.clock.Advance(domain.OnlineWindow)
	if views, _ = f.svc.Workers(ctx, f.account); !views[0].Online {
		t.Fatal("offline at exactly the online window")
	}
	f.clock.Advance(time.Second)
	if views, _ = f.svc.Workers(ctx, f.account); views[0].Online {
		t.Fatal("online after the online window")
	}

	_, err = New(noWorkers{f.st}, f.bus, nil, f.svc.logger, f.clock.Now).Workers(ctx, f.account)
	wantErr(t, err, errDown)
}

type waitingFunc func(domain.Job) (string, string, bool, error)

func (f waitingFunc) WaitReason(_ context.Context, j domain.Job) (string, string, bool, error) {
	return f(j)
}

func TestStatusAsksTheFleetWhyAJobWaits(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t, "docker")
	var ok bool
	var err error
	f.svc.WithWaiting(waitingFunc(func(domain.Job) (string, string, bool, error) { return "booting", "Starting a machine", ok, err }))

	ok = true
	v, e := f.svc.Status(ctx, f.account, j.ID)
	must(t, e)
	if v.ReasonCode != "booting" || v.Reason != "Starting a machine" {
		t.Fatalf("view = %+v", v)
	}
	ok = false // not the fleet's: it waits for a worker
	v, e = f.svc.Status(ctx, f.account, j.ID)
	must(t, e)
	if v.ReasonCode != "" || !strings.HasPrefix(v.Reason, ReasonNoWorker) {
		t.Fatalf("view = %+v", v)
	}
	err = errDown
	if _, e = f.svc.Status(ctx, f.account, j.ID); !errors.Is(e, errDown) {
		t.Fatalf("err = %v", e)
	}
}
