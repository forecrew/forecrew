package evidence_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type retentionEnv struct {
	clock   *testkit.Clock
	st      *testkit.Store
	ev      *testkit.Evidence
	account domain.Account
	project domain.Project
	job     domain.Job
	logs    *bytes.Buffer
	r       *evidence.Retention
}

func newRetentionEnv(t *testing.T, days int) *retentionEnv {
	t.Helper()
	ctx := t.Context()
	e := &retentionEnv{clock: testkit.NewClock(t0), ev: testkit.NewEvidence(), logs: &bytes.Buffer{}}
	e.st = testkit.NewStore(e.clock.Now)
	e.account = domain.Account{Name: "default"}
	must(t, e.st.Accounts().Create(ctx, &e.account))
	e.project = domain.Project{AccountID: e.account.ID, Name: "app", Repo: "acme/app"}
	must(t, e.st.Projects().Create(ctx, &e.project))
	e.job = domain.Job{AccountID: e.account.ID, ProjectID: e.project.ID, Role: "developer"}
	must(t, e.st.Jobs().Create(ctx, &e.job))
	e.r = &evidence.Retention{Store: e.st, Evidence: e.ev, Days: days, Logger: slog.New(slog.NewJSONHandler(e.logs, nil)), Now: e.clock.Now}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// add stores an artifact (row and object) created now.
func (e *retentionEnv) add(t *testing.T, name string) domain.Artifact {
	t.Helper()
	a := domain.Artifact{AccountID: e.account.ID, ProjectID: e.project.ID, JobID: e.job.ID, Name: name, ContentType: "text/plain", Size: 1,
		SHA256: strings.Repeat("a", 64), StorageKey: evidence.JobKey(e.account.ID, e.job.ID, name)}
	must(t, e.st.Artifacts().Put(t.Context(), &a))
	must(t, e.ev.Put(t.Context(), a.StorageKey, strings.NewReader("x"), 1, "text/plain"))
	return a
}

func TestSweep(t *testing.T) {
	e := newRetentionEnv(t, 30)
	ctx := t.Context()
	old1, old2 := e.add(t, "old1.log"), e.add(t, "old2.log")
	e.clock.Advance(20 * 24 * time.Hour)
	middle := e.add(t, "middle.log")
	e.clock.Advance(15 * 24 * time.Hour) // old ones are 35 days old, middle 15
	fresh := e.add(t, "fresh.log")

	n, err := e.r.Sweep(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	left, _ := e.st.Artifacts().ListByJob(ctx, e.account.ID, e.job.ID)
	if len(left) != 2 || left[0].ID != fresh.ID || left[1].ID != middle.ID {
		t.Fatalf("rows left = %+v", left)
	}
	if keys := e.ev.Keys(); len(keys) != 2 {
		t.Fatalf("objects left = %v", keys)
	}
	audit, err := e.st.Audit().List(ctx, e.account.ID, store.AuditFilter{})
	if err != nil || len(audit) != 2 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	for _, a := range audit {
		if a.Action != "artifact.deleted" || a.Actor != "system:forecrewd" || a.SubjectType != "artifact" || a.ProjectID != e.project.ID ||
			(a.SubjectID != old1.ID && a.SubjectID != old2.ID) || !strings.Contains(string(a.Data), `"retention_days":30`) {
			t.Errorf("audit entry = %+v (%s)", a, a.Data)
		}
	}

	// A second sweep has nothing to do; later the middle one expires too.
	if n, err := e.r.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v", n, err)
	}
	e.clock.Advance(16 * 24 * time.Hour)
	if n, err := e.r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("later sweep = %d, %v", n, err)
	}
}

func TestSweepKeepsForeverAtZeroDays(t *testing.T) {
	e := newRetentionEnv(t, 0)
	e.add(t, "a.log")
	e.clock.Advance(10 * 365 * 24 * time.Hour)
	if n, err := e.r.Sweep(t.Context()); err != nil || n != 0 || len(e.ev.Keys()) != 1 {
		t.Fatalf("Sweep = %d, %v, objects %v", n, err, e.ev.Keys())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.r.Run(ctx, time.Hour) // returns at once
}

func TestSweepManyAndOtherAccount(t *testing.T) {
	e := newRetentionEnv(t, 1)
	other := domain.Account{Name: "other"}
	must(t, e.st.Accounts().Create(t.Context(), &other))
	op := domain.Project{AccountID: other.ID, Name: "x", Repo: "o/x"}
	must(t, e.st.Projects().Create(t.Context(), &op))
	oa := domain.Artifact{AccountID: other.ID, ProjectID: op.ID, Name: "r.md", StorageKey: "reports/other"}
	must(t, e.st.Artifacts().Put(t.Context(), &oa))
	for i := range 250 {
		e.add(t, "f"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".log")
	}
	e.clock.Advance(48 * time.Hour)
	if n, err := e.r.Sweep(t.Context()); err != nil || n != 251 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if len(e.ev.Keys()) != 0 {
		t.Fatalf("objects left = %d", len(e.ev.Keys()))
	}
}

// A verification report keeps its data when its artifact expires; the
// Postgres store clears the link (see its integration test).
func TestSweepFailures(t *testing.T) {
	e := newRetentionEnv(t, 1)
	a, b := e.add(t, "a.log"), e.add(t, "b.log")
	e.clock.Advance(48 * time.Hour)

	// Storage down: nothing is deleted, rows stay for the next sweep.
	boom := errors.New("storage down")
	e.ev.DeleteErr = boom
	n, err := e.r.Sweep(t.Context())
	if !errors.Is(err, boom) || n != 0 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if left, _ := e.st.Artifacts().ListByJob(t.Context(), e.account.ID, e.job.ID); len(left) != 2 {
		t.Fatalf("rows left = %v", left)
	}
	if !strings.Contains(e.logs.String(), a.ID.String()) || !strings.Contains(e.logs.String(), b.ID.String()) {
		t.Errorf("failures not logged: %s", e.logs)
	}
	e.ev.DeleteErr = nil
	if n, err := e.r.Sweep(t.Context()); err != nil || n != 2 {
		t.Fatalf("retry = %d, %v", n, err)
	}
}

type brokenArtifacts struct {
	store.Store
	list, del, audit, accounts bool
}

var errDB = errors.New("db down")

func (s brokenArtifacts) Artifacts() store.ArtifactRepo {
	return brokenRepo{s.Store.Artifacts(), s}
}

func (s brokenArtifacts) Accounts() store.AccountRepo {
	if s.accounts {
		return brokenAccounts{s.Store.Accounts()}
	}
	return s.Store.Accounts()
}

func (s brokenArtifacts) Audit() store.AuditRepo { return brokenAudit{s.Store.Audit(), s} }

func (s brokenArtifacts) WithTx(ctx context.Context, fn func(context.Context, store.Store) error) error {
	return s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		return fn(ctx, brokenArtifacts{tx, s.list, s.del, s.audit, s.accounts})
	})
}

type brokenAccounts struct{ store.AccountRepo }

func (brokenAccounts) List(context.Context) ([]domain.Account, error) { return nil, errDB }

type brokenRepo struct {
	store.ArtifactRepo
	s brokenArtifacts
}

func (r brokenRepo) ListOlderThan(ctx context.Context, a uuid.UUID, t time.Time, limit int) ([]domain.Artifact, error) {
	if r.s.list {
		return nil, errDB
	}
	return r.ArtifactRepo.ListOlderThan(ctx, a, t, limit)
}

func (r brokenRepo) Delete(ctx context.Context, a, id uuid.UUID) error {
	if r.s.del {
		return errDB
	}
	return r.ArtifactRepo.Delete(ctx, a, id)
}

type brokenAudit struct {
	store.AuditRepo
	s brokenArtifacts
}

func (r brokenAudit) Append(ctx context.Context, e *domain.AuditEntry) error {
	if r.s.audit {
		return errDB
	}
	return r.AuditRepo.Append(ctx, e)
}

func TestSweepStoreFailures(t *testing.T) {
	for name, broken := range map[string]brokenArtifacts{
		"accounts": {accounts: true}, "list": {list: true}, "delete": {del: true}, "audit": {audit: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRetentionEnv(t, 1)
			e.add(t, "a.log")
			e.clock.Advance(48 * time.Hour)
			broken.Store = e.st
			e.r.Store = broken
			n, err := e.r.Sweep(t.Context())
			if !errors.Is(err, errDB) || n != 0 {
				t.Fatalf("Sweep = %d, %v", n, err)
			}
			// The audit entry and the row go together: a failed audit keeps the row.
			if left, _ := e.st.Artifacts().ListByJob(t.Context(), e.account.ID, e.job.ID); len(left) != 1 {
				t.Fatalf("rows left = %v", left)
			}
		})
	}
}

// Run sweeps once before it looks at the context, so a canceled context
// gives exactly one sweep.
func TestRunSweepsOnStart(t *testing.T) {
	e := newRetentionEnv(t, 1)
	e.add(t, "a.log")
	e.clock.Advance(48 * time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.r.Run(ctx, time.Hour)
	if len(e.ev.Keys()) != 0 || !strings.Contains(e.logs.String(), "expired artifacts deleted") {
		t.Errorf("objects %v, logs: %s", e.ev.Keys(), e.logs)
	}
}

func TestRunLogsFailures(t *testing.T) {
	e := newRetentionEnv(t, 1)
	e.add(t, "a.log")
	e.clock.Advance(48 * time.Hour)
	e.ev.DeleteErr = errors.New("down")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.r.Run(ctx, time.Hour)
	if !strings.Contains(e.logs.String(), "retention sweep failed") {
		t.Errorf("no failure logged: %s", e.logs)
	}
}

func TestRunSweepsAgain(t *testing.T) {
	e := newRetentionEnv(t, 1)
	e.add(t, "a.log")
	e.clock.Advance(48 * time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.r.Run(ctx, 5*time.Millisecond)
	deadline := time.After(5 * time.Second)
	for len(e.ev.Keys()) != 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not sweep")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
