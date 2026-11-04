package testkit

import (
	"bytes"
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func jobsT(d *storeData) *table[domain.Job]           { return &d.jobs }
func jobEventsT(d *storeData) *table[domain.JobEvent] { return &d.jobEvents }
func workersT(d *storeData) *table[domain.Worker]     { return &d.workers }

// Jobs implements store.Store.
func (s *Store) Jobs() store.JobRepo { return jobRepo{s} }

type jobRepo struct{ s *Store }

func (r jobRepo) Create(_ context.Context, j *domain.Job) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, j.AccountID, j.ProjectID), foreign(&d.goals, j.AccountID, j.GoalID), foreign(&d.specs, j.AccountID, j.SpecID), foreign(&d.tasks, j.AccountID, j.TaskID)); err != nil {
			return err
		}
		j.ID = newID(j.ID)
		j.Number = d.nextNumber(j.AccountID, domain.RefJob)
		j.Status = domain.JobQueued
		j.Kind = j.Kind.OrDefault()
		j.Pool = domain.OrDefaultPool(j.Pool)
		if j.MaxAttempts == 0 {
			j.MaxAttempts = domain.DefaultMaxAttempts
		}
		j.Spec.JobID, j.Spec.AccountID = j.ID, j.AccountID
		j.Spec = j.Spec.Redacted()
		j.CreatedAt, j.UpdatedAt = now, now
		d.jobs.insert(*j)
		return nil
	})
}

func (r jobRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Job, error) {
	return getRow(r.s, jobsT, accountID, id)
}

func (r jobRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Job, error) {
	return findRow(r.s, jobsT, func(j domain.Job) bool { return j.AccountID == accountID && j.Number == number })
}

func (r jobRepo) List(_ context.Context, accountID uuid.UUID, f store.JobFilter) ([]domain.Job, error) {
	return read(r.s, func(d *storeData) ([]domain.Job, error) {
		return d.jobs.newest(f.Limit, func(j domain.Job) bool {
			return j.AccountID == accountID && zeroOr(f.ProjectID, j.ProjectID) && zeroOr(f.GoalID, j.GoalID) &&
				zeroOr(f.TaskID, j.TaskID) && zeroOr(f.WorkerID, j.WorkerID) && zeroOr(f.Status, j.Status)
		}), nil
	})
}

func (r jobRepo) LeaseNext(_ context.Context, accountID, workerID uuid.UUID, labels []string, leaseFor time.Duration) (domain.Job, error) {
	var out domain.Job
	err := r.s.do(func(d *storeData, now time.Time) error {
		// Only jobs of the worker's pool. Unlike Postgres, the fake lets
		// tests lease for a worker it does not know, as one of pool build.
		pool := domain.PoolBuild
		if w, err := d.workers.find(func(w domain.Worker) bool { return w.AccountID == accountID && w.ID == workerID }); err == nil {
			pool = w.Pool
		}
		j, err := d.jobs.find(func(j domain.Job) bool {
			return j.AccountID == accountID && j.Status == domain.JobQueued && !j.RunAfter.After(now) &&
				j.Pool == pool && domain.MatchLabels(j.RequiredLabels(), labels)
		})
		if err != nil {
			return store.ErrNoJob
		}
		if err := j.Lease(workerID, now); err != nil {
			return err
		}
		j.LeaseExpiresAt = now.Add(leaseFor)
		out = *j
		return nil
	})
	return out, err
}

// checkLease returns ErrLeaseLost unless j is running under workerID.
func checkLease(j *domain.Job, workerID uuid.UUID) error {
	if j.Status != domain.JobRunning || j.WorkerID != workerID {
		return fmt.Errorf("job %s is %s: %w", j.Ref(), j.Status, store.ErrLeaseLost)
	}
	return nil
}

// leased applies fn to a job running under workerID, and returns the job.
// It returns ErrLeaseLost with the current job otherwise.
func (r jobRepo) leased(accountID, jobID, workerID uuid.UUID, fn func(j *domain.Job, now time.Time) error) (domain.Job, error) {
	var out domain.Job
	err := update(r.s, jobsT, accountID, jobID, func(j *domain.Job, now time.Time) error {
		if err := checkLease(j, workerID); err != nil {
			out = *j
			return err
		}
		if err := fn(j, now); err != nil {
			return err
		}
		out = *j
		return nil
	})
	return out, err
}

func (r jobRepo) ExtendLease(_ context.Context, accountID, jobID, workerID uuid.UUID, leaseFor time.Duration) (domain.Job, error) {
	return r.leased(accountID, jobID, workerID, func(j *domain.Job, now time.Time) error {
		j.LeaseExpiresAt = now.Add(leaseFor)
		j.UpdatedAt = now
		return nil
	})
}

func (r jobRepo) RequeueExpired(_ context.Context, accountID uuid.UUID) ([]domain.Job, error) {
	var out []domain.Job
	err := r.s.do(func(d *storeData, now time.Time) error {
		for i := range d.jobs.rows {
			j := &d.jobs.rows[i]
			if j.AccountID != accountID || j.Status != domain.JobRunning || j.LeaseExpiresAt.After(now) {
				continue
			}
			if err := j.Requeue("lease expired", now); err != nil {
				return err
			}
			out = append(out, *j)
		}
		return nil
	})
	return out, err
}

func (r jobRepo) Complete(_ context.Context, accountID, jobID, workerID uuid.UUID, res store.JobResult) (domain.Job, error) {
	return r.leased(accountID, jobID, workerID, func(j *domain.Job, now time.Time) error {
		if res.Status != domain.JobSucceeded && res.Status != domain.JobFailed {
			return fmt.Errorf("complete with %q: %w", res.Status, domain.ErrInvalidTransition)
		}
		if err := j.Finish(res.Status, now); err != nil {
			return err
		}
		j.Output, j.HeadSHA, j.Error, j.Usage, j.CostUSD = res.Output, res.HeadSHA, res.Error, res.Usage, res.CostUSD
		return nil
	})
}

func (r jobRepo) Release(_ context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error) {
	return r.leased(accountID, jobID, workerID, func(j *domain.Job, now time.Time) error {
		return j.Release(reason, now)
	})
}

func (r jobRepo) Cancel(_ context.Context, accountID, jobID uuid.UUID) (domain.Job, error) {
	var out domain.Job
	err := update(r.s, jobsT, accountID, jobID, func(j *domain.Job, now time.Time) error {
		if err := j.Finish(domain.JobCanceled, now); err != nil {
			return err
		}
		out = *j
		return nil
	})
	return out, err
}

// JobEvents implements store.Store.
func (s *Store) JobEvents() store.JobEventRepo { return jobEventRepo{s} }

type jobEventRepo struct{ s *Store }

func (r jobEventRepo) Append(_ context.Context, accountID, jobID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error) {
	return r.append(accountID, jobID, uuid.Nil(), events)
}

func (r jobEventRepo) AppendLeased(_ context.Context, accountID, jobID, workerID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error) {
	return r.append(accountID, jobID, workerID, events)
}

func (r jobEventRepo) append(accountID, jobID, workerID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error) {
	out := make([]domain.JobEvent, 0, len(events))
	err := r.s.do(func(d *storeData, _ time.Time) error {
		j, err := d.jobs.get(accountID, jobID)
		if err != nil {
			return err
		}
		if workerID != uuid.Nil() {
			if err := checkLease(j, workerID); err != nil {
				return err
			}
		}
		seq := int64(len(d.jobEvents.filter(0, func(e domain.JobEvent) bool { return e.JobID == jobID })))
		for _, e := range events {
			seq++
			e.ID, e.AccountID, e.JobID, e.Seq = domain.NewID(), accountID, jobID, seq
			d.jobEvents.insert(e)
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func (r jobEventRepo) List(_ context.Context, accountID, jobID uuid.UUID, afterSeq int64, limit int) ([]domain.JobEvent, error) {
	return listRows(r.s, jobEventsT, limit, func(e domain.JobEvent) bool {
		return e.AccountID == accountID && e.JobID == jobID && e.Seq > afterSeq
	})
}

// Workers implements store.Store.
func (s *Store) Workers() store.WorkerRepo { return workerRepo{s} }

type workerRepo struct{ s *Store }

func (r workerRepo) Create(_ context.Context, w *domain.Worker) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.workers.exists(func(x domain.Worker) bool { return x.AccountID == w.AccountID && x.Name == w.Name }) {
			return fmt.Errorf("worker %s: %w", w.Name, store.ErrConflict)
		}
		w.ID = newID(w.ID)
		w.Pool = domain.OrDefaultPool(w.Pool)
		w.CreatedAt = now
		d.workers.insert(*w)
		return nil
	})
}

func (r workerRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Worker, error) {
	return getRow(r.s, workersT, accountID, id)
}

func (r workerRepo) GetByTokenHash(_ context.Context, hash []byte) (domain.Worker, error) {
	return findRow(r.s, workersT, func(w domain.Worker) bool { return bytes.Equal(w.TokenHash, hash) })
}

func (r workerRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Worker, error) {
	return listRows(r.s, workersT, 0, func(w domain.Worker) bool { return w.AccountID == accountID })
}

func (r workerRepo) Seen(_ context.Context, accountID, id uuid.UUID, labels []string, version string) error {
	return update(r.s, workersT, accountID, id, func(w *domain.Worker, now time.Time) error {
		w.LastSeenAt, w.Labels, w.Version = now, domain.NormalizeLabels(labels), version
		return nil
	})
}

func (r workerRepo) SetPool(_ context.Context, accountID, id uuid.UUID, pool string) error {
	return update(r.s, workersT, accountID, id, func(w *domain.Worker, _ time.Time) error {
		w.Pool = domain.OrDefaultPool(pool)
		return nil
	})
}

func (r workerRepo) Revoke(_ context.Context, accountID, id uuid.UUID) error {
	return update(r.s, workersT, accountID, id, func(w *domain.Worker, now time.Time) error {
		if w.RevokedAt.IsZero() {
			w.RevokedAt = now
		}
		return nil
	})
}
