package testkit

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func goalsT(d *storeData) *table[domain.Goal]     { return &d.goals }
func specsT(d *storeData) *table[domain.Spec]     { return &d.specs }
func tasksT(d *storeData) *table[domain.Task]     { return &d.tasks }
func changesT(d *storeData) *table[domain.Change] { return &d.changes }

// Goals implements store.Store.
func (s *Store) Goals() store.GoalRepo { return goalRepo{s} }

type goalRepo struct{ s *Store }

func (r goalRepo) Create(_ context.Context, g *domain.Goal) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.projects, g.AccountID, g.ProjectID); err != nil {
			return err
		}
		g.ID = newID(g.ID)
		g.Number = d.nextNumber(g.AccountID, domain.RefGoal)
		g.Platforms = domain.NormalizeLabels(g.Platforms)
		g.CreatedAt, g.UpdatedAt = now, now
		d.goals.insert(*g)
		return nil
	})
}

func (r goalRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Goal, error) {
	return getRow(r.s, goalsT, accountID, id)
}

func (r goalRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Goal, error) {
	return findRow(r.s, goalsT, func(g domain.Goal) bool { return g.AccountID == accountID && g.Number == number })
}

func (r goalRepo) List(_ context.Context, accountID uuid.UUID, f store.GoalFilter) ([]domain.Goal, error) {
	return listRows(r.s, goalsT, f.Limit, func(g domain.Goal) bool {
		return g.AccountID == accountID && zeroOr(f.ProjectID, g.ProjectID) && zeroOr(f.Status, g.Status)
	})
}

func (r goalRepo) Update(_ context.Context, g *domain.Goal) error {
	return update(r.s, goalsT, g.AccountID, g.ID, func(cur *domain.Goal, now time.Time) error {
		cur.Title, cur.Body, cur.IssueNumber, cur.UpdatedAt = g.Title, g.Body, g.IssueNumber, now
		*g = *cur
		return nil
	})
}

func (r goalRepo) SetSize(_ context.Context, accountID, id uuid.UUID, size domain.GoalSize) error {
	return update(r.s, goalsT, accountID, id, func(g *domain.Goal, now time.Time) error {
		if g.Size == nil {
			g.Size, g.UpdatedAt = &size, now
		}
		return nil
	})
}

func (r goalRepo) UpdateStatus(_ context.Context, accountID, id uuid.UUID, from, to domain.GoalStatus) error {
	return update(r.s, goalsT, accountID, id, func(g *domain.Goal, now time.Time) error {
		g.UpdatedAt = now
		return casStatus(&g.Status, from, to)
	})
}

// Specs implements store.Store.
func (s *Store) Specs() store.SpecRepo { return specRepo{s} }

type specRepo struct{ s *Store }

func (r specRepo) Create(_ context.Context, sp *domain.Spec) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, sp.AccountID, sp.ProjectID), foreign(&d.goals, sp.AccountID, sp.GoalID), foreign(&d.jobs, sp.AccountID, sp.JobID)); err != nil {
			return err
		}
		if d.specs.exists(func(x domain.Spec) bool { return x.GoalID == sp.GoalID && x.Revision == sp.Revision }) {
			return fmt.Errorf("spec revision %d: %w", sp.Revision, store.ErrConflict)
		}
		sp.ID = newID(sp.ID)
		sp.Number = d.nextNumber(sp.AccountID, domain.RefSpec)
		sp.CreatedAt = now
		d.specs.insert(*sp)
		return nil
	})
}

func (r specRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Spec, error) {
	return getRow(r.s, specsT, accountID, id)
}

func (r specRepo) ListByGoal(_ context.Context, accountID, goalID uuid.UUID) ([]domain.Spec, error) {
	specs, err := listRows(r.s, specsT, 0, func(sp domain.Spec) bool { return sp.AccountID == accountID && sp.GoalID == goalID })
	slices.SortFunc(specs, func(a, b domain.Spec) int { return cmp.Compare(a.Revision, b.Revision) })
	return specs, err
}

func (r specRepo) Latest(ctx context.Context, accountID, goalID uuid.UUID) (domain.Spec, error) {
	specs, _ := r.ListByGoal(ctx, accountID, goalID)
	if len(specs) == 0 {
		return domain.Spec{}, fmt.Errorf("spec of goal %s: %w", goalID, store.ErrNotFound)
	}
	return specs[len(specs)-1], nil
}

// Tasks implements store.Store.
func (s *Store) Tasks() store.TaskRepo { return taskRepo{s} }

type taskRepo struct{ s *Store }

func (r taskRepo) Create(_ context.Context, t *domain.Task) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, t.AccountID, t.ProjectID), foreign(&d.goals, t.AccountID, t.GoalID), foreign(&d.specs, t.AccountID, t.SpecID)); err != nil {
			return err
		}
		t.ID = newID(t.ID)
		t.Number = d.nextNumber(t.AccountID, domain.RefTask)
		t.CreatedAt, t.UpdatedAt = now, now
		d.tasks.insert(*t)
		return nil
	})
}

func (r taskRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Task, error) {
	return getRow(r.s, tasksT, accountID, id)
}

func (r taskRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Task, error) {
	return findRow(r.s, tasksT, func(t domain.Task) bool { return t.AccountID == accountID && t.Number == number })
}

func (r taskRepo) GetByIssue(_ context.Context, accountID, projectID uuid.UUID, issueNumber int) (domain.Task, error) {
	return findRow(r.s, tasksT, func(t domain.Task) bool {
		return t.AccountID == accountID && t.ProjectID == projectID && t.IssueNumber == issueNumber
	})
}

func (r taskRepo) List(_ context.Context, accountID uuid.UUID, f store.TaskFilter) ([]domain.Task, error) {
	tasks, err := listRows(r.s, tasksT, 0, func(t domain.Task) bool {
		return t.AccountID == accountID && zeroOr(f.ProjectID, t.ProjectID) && zeroOr(f.GoalID, t.GoalID) &&
			zeroOr(f.SpecID, t.SpecID) && zeroOr(f.Status, t.Status)
	})
	slices.SortStableFunc(tasks, func(a, b domain.Task) int {
		return cmp.Or(cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.Number, b.Number))
	})
	if f.Limit > 0 && len(tasks) > f.Limit {
		tasks = tasks[:f.Limit]
	}
	return tasks, err
}

func (r taskRepo) Update(_ context.Context, t *domain.Task) error {
	return update(r.s, tasksT, t.AccountID, t.ID, func(cur *domain.Task, now time.Time) error {
		cur.Title, cur.Body, cur.Seq, cur.IssueNumber, cur.Branch = t.Title, t.Body, t.Seq, t.IssueNumber, t.Branch
		cur.UpdatedAt = now
		*t = *cur
		return nil
	})
}

func (r taskRepo) UpdateStatus(_ context.Context, accountID, id uuid.UUID, from, to domain.TaskStatus) error {
	return update(r.s, tasksT, accountID, id, func(t *domain.Task, now time.Time) error {
		t.UpdatedAt = now
		return casStatus(&t.Status, from, to)
	})
}

// Changes implements store.Store.
func (s *Store) Changes() store.ChangeRepo { return changeRepo{s} }

type changeRepo struct{ s *Store }

func samePR(c domain.Change, account uuid.UUID, repo string, number int) bool {
	return c.AccountID == account && strings.EqualFold(c.Repo, repo) && c.Number == number
}

func (r changeRepo) Upsert(_ context.Context, c *domain.Change) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, c.AccountID, c.ProjectID), foreign(&d.tasks, c.AccountID, c.TaskID)); err != nil {
			return err
		}
		cur, err := d.changes.find(func(x domain.Change) bool { return samePR(x, c.AccountID, c.Repo, c.Number) })
		if err != nil {
			c.ID = newID(c.ID)
			c.CreatedAt, c.UpdatedAt = now, now
			d.changes.insert(*c)
			return nil
		}
		c.ID, c.CreatedAt, c.UpdatedAt = cur.ID, cur.CreatedAt, now
		if c.TaskID == uuid.Nil() {
			c.TaskID = cur.TaskID
		}
		*cur = *c
		return nil
	})
}

func (r changeRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Change, error) {
	return getRow(r.s, changesT, accountID, id)
}

func (r changeRepo) GetByPR(_ context.Context, accountID uuid.UUID, repo string, number int) (domain.Change, error) {
	return findRow(r.s, changesT, func(c domain.Change) bool { return samePR(c, accountID, repo, number) })
}

func (r changeRepo) List(_ context.Context, accountID uuid.UUID, f store.ChangeFilter) ([]domain.Change, error) {
	return listRows(r.s, changesT, f.Limit, func(c domain.Change) bool {
		return c.AccountID == accountID && zeroOr(f.ProjectID, c.ProjectID) && zeroOr(f.TaskID, c.TaskID) && zeroOr(f.State, c.State)
	})
}
