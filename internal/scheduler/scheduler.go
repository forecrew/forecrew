// Package scheduler starts playbooks on the cron schedules of forecrew.yaml.
//
// It keeps no state of its own. The run of a slot has an ID derived from the
// playbook, the project and the slot, and starting an existing ID returns
// that run (workflow.StartRequest.ID), so asking again is harmless: a restart
// finds the latest slot already started, or starts it once. The slots missed
// while forecrewd was down are not made up one by one: only the latest
// counts, and a slot waits while a run of the same schedule is active. A schedule that has no run for its latest slot when forecrewd
// starts therefore runs at once, once.
package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/cron"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// DefaultEvery is how often the schedules are looked at; cron's resolution
// is a minute.
const DefaultEvery = time.Minute

// Starter starts a run of a playbook; *workflow.Engine implements it.
type Starter interface {
	Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error)
}

// entry is a schedule with its expression parsed.
type entry struct {
	config.Schedule
	cron cron.Schedule
}

// Scheduler starts the runs of the schedules.
type Scheduler struct {
	Store   store.Store
	Starter Starter
	Log     *slog.Logger
	// Now is the clock; the real UTC time when nil.
	Now func() time.Time
	// Every is how often Run looks at the schedules; DefaultEvery when zero.
	Every time.Duration

	entries []entry
}

// New returns a scheduler for schedules, whose cron expressions it parses.
func New(schedules []config.Schedule, st store.Store, starter Starter) (*Scheduler, error) {
	s := &Scheduler{Store: st, Starter: starter}
	for _, c := range schedules {
		parsed, err := cron.Parse(c.Cron)
		if err != nil {
			return nil, fmt.Errorf("schedule %s: %w", c, err)
		}
		s.entries = append(s.entries, entry{Schedule: c, cron: parsed})
	}
	return s, nil
}

func (s *Scheduler) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// RunID is the ID of the run a schedule starts for a project at slot.
func RunID(playbook string, projectID uuid.UUID, slot time.Time) uuid.UUID {
	sum := sha256.Sum256(fmt.Appendf(nil, "forecrew-schedule\x00%s\x00%s\x00%d", playbook, projectID, slot.UTC().Unix()))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x80 // version 8: application-defined
	id[8] = id[8]&0x3f | 0x80 // variant RFC 9562
	return id
}

// Tick starts what is due: for every schedule, the run of its latest slot
// for each onboarded project of its repo, unless that run exists. It
// returns how many runs it started and the errors of the ones it could not,
// the others being started all the same.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	now := s.now()
	started := 0
	var errs []error
	for _, e := range s.entries {
		slot, ok := e.cron.Prev(now)
		if !ok {
			continue
		}
		projects, err := s.projectsOf(ctx, e.Project)
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s: list projects: %w", e.Schedule, err))
			continue
		}
		for _, p := range projects {
			if p.Status != domain.ProjectOnboarded {
				continue
			}
			n, err := s.start(ctx, e, p, slot)
			started += n
			if err != nil {
				errs = append(errs, fmt.Errorf("schedule %s: project %s: %w", e.Schedule, p.Name, err))
			}
		}
	}
	return started, joinErrs(errs)
}

// projectsOf returns the projects a schedule runs for: those of a repo in every
// organization, or all of them for config.AllProjects.
func (s *Scheduler) projectsOf(ctx context.Context, repo string) ([]domain.Project, error) {
	if repo != config.AllProjects {
		return s.Store.Projects().ListByRepo(ctx, repo)
	}
	accounts, err := s.Store.Accounts().List(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Project
	for _, a := range accounts {
		ps, err := s.Store.Projects().List(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

// active reports whether a run of the playbook is going on for the project.
func (s *Scheduler) active(ctx context.Context, p domain.Project, playbook string) (bool, error) {
	runs, err := s.Store.WorkflowRuns().ListActive(ctx, p.AccountID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(runs, func(r domain.WorkflowRun) bool { return r.Playbook == playbook && r.ProjectID == p.ID }), nil
}

// start starts the run of slot for p and says whether it was new.
func (s *Scheduler) start(ctx context.Context, e entry, p domain.Project, slot time.Time) (int, error) {
	id := RunID(e.Playbook, p.ID, slot)
	_, err := s.Store.WorkflowRuns().Get(ctx, p.AccountID, id)
	switch {
	case err == nil:
		return 0, nil // started before: by an earlier tick, or before a restart
	case !errors.Is(err, store.ErrNotFound):
		return 0, err
	}
	// A slot waits for the run of the one before: the next tick tries again.
	if busy, err := s.active(ctx, p, e.Playbook); err != nil || busy {
		return 0, err
	}
	run, err := s.Starter.Start(ctx, e.Playbook, workflow.StartRequest{
		ID: id, AccountID: p.AccountID, ProjectID: p.ID, Inputs: map[string]any{"repo": p.Repo},
	})
	if err != nil {
		return 0, err
	}
	s.log().InfoContext(ctx, "scheduled playbook started", "playbook", e.Playbook, "project", p.Name, "slot", slot.Format(time.RFC3339), "run_id", run.ID)
	return 1, nil
}

// Run ticks at once, so that a slot missed while forecrewd was down is made
// up, and then every Every until ctx is done. A tick that fails is logged;
// the next one tries again.
func (s *Scheduler) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = DefaultEvery
	}
	if len(s.entries) == 0 {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log().ErrorContext(ctx, "scheduled playbooks", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func joinErrs(errs []error) error { return errors.Join(errs...) }
