package workerapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

var errDown = errors.New("store down")

// brokenStore fails worker lookups, Seen, worker reads or job leasing and
// extension, as set; afterLease runs after each successful lease.
type brokenStore struct {
	store.Store
	lookup, seen, get, extend bool
	afterLease                func()
}

func (s brokenStore) Workers() store.WorkerRepo { return brokenWorkers{s.Store.Workers(), s} }
func (s brokenStore) Jobs() store.JobRepo       { return brokenJobs{s.Store.Jobs(), s} }

type brokenWorkers struct {
	store.WorkerRepo
	s brokenStore
}

func (w brokenWorkers) GetByTokenHash(ctx context.Context, hash []byte) (domain.Worker, error) {
	if w.s.lookup {
		return domain.Worker{}, errDown
	}
	return w.WorkerRepo.GetByTokenHash(ctx, hash)
}

func (w brokenWorkers) Seen(ctx context.Context, accountID, id uuid.UUID, labels []string, version string) error {
	if w.s.seen {
		return errDown
	}
	return w.WorkerRepo.Seen(ctx, accountID, id, labels, version)
}

func (w brokenWorkers) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Worker, error) {
	if w.s.get {
		return domain.Worker{}, errDown
	}
	return w.WorkerRepo.Get(ctx, accountID, id)
}

type brokenJobs struct {
	store.JobRepo
	s brokenStore
}

func (j brokenJobs) ExtendLease(ctx context.Context, accountID, jobID, workerID uuid.UUID, d time.Duration) (domain.Job, error) {
	if j.s.extend {
		return domain.Job{}, errDown
	}
	return j.JobRepo.ExtendLease(ctx, accountID, jobID, workerID, d)
}

func (j brokenJobs) LeaseNext(ctx context.Context, accountID, workerID uuid.UUID, labels []string, d time.Duration) (domain.Job, error) {
	if j.s.extend {
		return domain.Job{}, errDown
	}
	job, err := j.JobRepo.LeaseNext(ctx, accountID, workerID, labels, d)
	if err == nil && j.s.afterLease != nil {
		j.s.afterLease()
	}
	return job, err
}

func TestStoreFailures(t *testing.T) {
	tests := []struct {
		name   string
		broken brokenStore
		action string
	}{
		{"lookup", brokenStore{lookup: true}, "poll"},
		{"seen on poll", brokenStore{seen: true}, "poll"},
		{"lease", brokenStore{extend: true}, "poll"},
		{"worker check", brokenStore{get: true}, "poll"},
		{"seen on heartbeat", brokenStore{seen: true}, "heartbeat"},
		{"extend", brokenStore{extend: true}, "heartbeat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			_, token := e.addWorker(t, "box")
			j := e.newJob(t, jobOpts{})
			broken := tt.broken
			broken.Store = e.st
			cfg := e.server.cfg
			cfg.Store = broken
			h := New(cfg).Handler()
			path, body := "/worker/v1/poll", `{"free_slots":1,"labels":["runtime:claude-code"]}`
			if tt.action == "heartbeat" {
				path, body = jobPath(j.ID, "heartbeat"), ""
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"internal"`) {
				t.Fatalf("response = %d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), errDown.Error()) {
				t.Fatalf("internal error leaked: %s", rec.Body)
			}
			if got := e.job(t, j.ID); got.Status != domain.JobQueued {
				t.Fatalf("job left %s", got.Status)
			}
		})
	}
}

func TestPollClientGone(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PollWait = time.Minute })
	_, token := e.addWorker(t, "box")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/worker/v1/poll", strings.NewReader(`{"free_slots":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("response = %d %s, want 204", rec.Code, rec.Body)
	}
}

func TestPollClientGoneAfterLease(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := e.server.cfg
	cfg.Store = brokenStore{Store: e.st, afterLease: cancel}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/worker/v1/poll", strings.NewReader(`{"free_slots":1,"labels":["runtime:claude-code"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	New(cfg).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("response = %d %s, want 204", rec.Code, rec.Body)
	}
	got := e.job(t, j.ID)
	if got.Status != domain.JobQueued || got.Attempts != 1 || got.WorkerID != uuid.Nil() {
		t.Fatalf("job = %+v, want it released", got)
	}
	if e.tracker.Tokens != 0 {
		t.Fatalf("minted %d tokens for a gone worker", e.tracker.Tokens)
	}
}
