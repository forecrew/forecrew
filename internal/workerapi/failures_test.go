package workerapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	// goneErr, if set, is returned by lookup and seen instead of errDown.
	goneErr    error
	afterLease func()
	// inSeen runs inside Seen, before it fails.
	inSeen func()
}

func (s brokenStore) err() error {
	if s.goneErr != nil {
		return s.goneErr
	}
	return errDown
}

func (s brokenStore) Workers() store.WorkerRepo { return brokenWorkers{s.Store.Workers(), s} }
func (s brokenStore) Jobs() store.JobRepo       { return brokenJobs{s.Store.Jobs(), s} }

type brokenWorkers struct {
	store.WorkerRepo
	s brokenStore
}

func (w brokenWorkers) GetByTokenHash(ctx context.Context, hash []byte) (domain.Worker, error) {
	if w.s.lookup {
		return domain.Worker{}, w.s.err()
	}
	return w.WorkerRepo.GetByTokenHash(ctx, hash)
}

func (w brokenWorkers) Seen(ctx context.Context, accountID, id uuid.UUID, labels []string, version string) error {
	if w.s.seen {
		if w.s.inSeen != nil {
			w.s.inSeen()
		}
		return w.s.err()
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

// A client that goes away while a store call runs is not a server error:
// the store reports the request context's error and poll answers 204 with
// nothing logged. A real store error still answers 500.
func TestPollClientGoneInStoreCall(t *testing.T) {
	wrapped := fmt.Errorf("query: %w", context.DeadlineExceeded)
	writeTimeout := fmt.Errorf("write failed: %w", os.ErrDeadlineExceeded)
	tests := []struct {
		name string
		err  error
		seen bool // fail Seen (cancelling inside it) rather than the lookup
	}{
		{"auth canceled", context.Canceled, false},
		{"auth deadline", wrapped, false},
		{"auth write timeout", writeTimeout, false},
		{"seen canceled", context.Canceled, true},
		{"seen deadline", wrapped, true},
		{"seen write timeout", writeTimeout, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			e := newEnv(t)
			_, token := e.addWorker(t, "box")
			ctx, cancel := context.WithCancel(t.Context())
			broken := brokenStore{Store: e.st, goneErr: tt.err}
			if tt.seen {
				broken.seen, broken.inSeen = true, cancel
			} else {
				broken.lookup = true
				cancel() // gone before the lookup
			}
			cfg := e.server.cfg
			cfg.Store = broken
			cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/worker/v1/poll", strings.NewReader(`{"free_slots":1}`))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			New(cfg).Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("response = %d %s, want 204", rec.Code, rec.Body)
			}
			if logs.Len() != 0 {
				t.Fatalf("logged: %s", logs.String())
			}
		})
	}
}

func TestClientGone(t *testing.T) {
	done, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"canceled", done, context.Canceled, true},
		{"deadline", done, context.DeadlineExceeded, true},
		{"write timeout", done, fmt.Errorf("write failed: %w", os.ErrDeadlineExceeded), true},
		{"real error", done, errDown, false},
		{"nil error", done, nil, false},
		{"live context", t.Context(), context.Canceled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientGone(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("clientGone = %v, want %v", got, tt.want)
			}
		})
	}
}

// Outside poll a done context (shutdown) must not become a 2xx the worker
// would take for success.
func TestNonPollContextDoneNot204(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := e.server.cfg
	cfg.Store = brokenStore{Store: e.st, lookup: true, goneErr: context.Canceled}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, jobPath(uuid.Nil(), "heartbeat"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	New(cfg).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d %s, want 500", rec.Code, rec.Body)
	}
}

func TestPollContextErrorWhileClientHere(t *testing.T) {
	// The store's context error with a live request is a real failure.
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	cfg := e.server.cfg
	cfg.Store = brokenStore{Store: e.st, seen: true, goneErr: context.Canceled}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/worker/v1/poll", strings.NewReader(`{"free_slots":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	New(cfg).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d %s, want 500", rec.Code, rec.Body)
	}
}
