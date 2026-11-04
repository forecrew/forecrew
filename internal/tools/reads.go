package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// JobStatus implements mcp.ToolService.
func (s *Service) JobStatus(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
	j, err := s.job(ctx, by.AccountID, in.Job)
	if err != nil {
		return mcp.JobInfo{}, err
	}
	v, err := s.cfg.Jobs.Status(ctx, by.AccountID, j.ID)
	if err != nil {
		return mcp.JobInfo{}, err
	}
	tests, err := s.testSummary(ctx, by.AccountID, v.Job)
	if err != nil {
		return mcp.JobInfo{}, err
	}
	return mcp.JobInfo{Job: v.Job, Ref: v.Ref(), Reason: v.Reason, WorkerName: v.WorkerName, Tests: tests}, nil
}

// JobLogs implements mcp.ToolService.
func (s *Service) JobLogs(ctx context.Context, by domain.Actor, in mcp.JobLogsInput) (mcp.JobLogsOutput, error) {
	if in.AfterSeq < 0 || in.Limit < 0 {
		return mcp.JobLogsOutput{}, fmt.Errorf("after_seq and limit cannot be negative: %w", mcp.ErrInvalidInput)
	}
	j, err := s.job(ctx, by.AccountID, in.Job)
	if err != nil {
		return mcp.JobLogsOutput{}, err
	}
	l, err := s.cfg.Jobs.Logs(ctx, by.AccountID, j.ID, in.AfterSeq, in.Limit)
	if err != nil {
		return mcp.JobLogsOutput{}, err
	}
	return mcp.JobLogsOutput{Events: l.Events, NextSeq: l.NextSeq}, nil
}

// ListWorkers implements mcp.ToolService.
func (s *Service) ListWorkers(ctx context.Context, by domain.Actor) ([]mcp.WorkerInfo, error) {
	views, err := s.cfg.Jobs.Workers(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.WorkerInfo, len(views))
	for i, v := range views {
		out[i] = mcp.WorkerInfo{Worker: v.Worker, Online: v.Online, RunningJobs: v.RunningJobs}
	}
	return out, nil
}

// ListPendingGates implements mcp.ToolService.
func (s *Service) ListPendingGates(ctx context.Context, by domain.Actor, in mcp.ProjectFilterInput) ([]mcp.GateInfo, error) {
	projectID, err := s.optionalProject(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	pending, err := s.cfg.Gates.ListPending(ctx, by.AccountID, projectID)
	if err != nil {
		return nil, err
	}
	return s.gateInfos(ctx, by.AccountID, pending)
}

// gateInfos adds project, goal and task refs and the GitHub link to gates.
func (s *Service) gateInfos(ctx context.Context, accountID uuid.UUID, gs []domain.Gate) ([]mcp.GateInfo, error) {
	projects, err := s.cfg.Store.Projects().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	names := make(map[uuid.UUID]string, len(projects))
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	goalRefs, taskRefs := map[uuid.UUID]string{}, map[uuid.UUID]string{}
	out := make([]mcp.GateInfo, len(gs))
	for i, g := range gs {
		info := mcp.GateInfo{Gate: g, Ref: g.Ref(), Project: names[g.ProjectID], URL: issueURL(g.SubjectRepo, g.SubjectNumber)}
		if g.GoalID != uuid.Nil() {
			if info.GoalRef, err = memo(goalRefs, g.GoalID, func() (string, error) {
				goal, err := s.cfg.Store.Goals().Get(ctx, accountID, g.GoalID)
				return goal.Ref(), err
			}); err != nil {
				return nil, err
			}
		}
		if g.TaskID != uuid.Nil() {
			if info.TaskRef, err = memo(taskRefs, g.TaskID, func() (string, error) {
				task, err := s.cfg.Store.Tasks().Get(ctx, accountID, g.TaskID)
				return task.Ref(), err
			}); err != nil {
				return nil, err
			}
		}
		out[i] = info
	}
	return out, nil
}

func memo(cache map[uuid.UUID]string, id uuid.UUID, load func() (string, error)) (string, error) {
	if v, ok := cache[id]; ok {
		return v, nil
	}
	v, err := load()
	if err != nil {
		return "", err
	}
	cache[id] = v
	return v, nil
}

// ListProjects implements mcp.ToolService.
func (s *Service) ListProjects(ctx context.Context, by domain.Actor) ([]domain.Project, error) {
	return s.cfg.Store.Projects().List(ctx, by.AccountID)
}

// GetProject implements mcp.ToolService.
func (s *Service) GetProject(ctx context.Context, by domain.Actor, in mcp.ProjectInput) (domain.Project, error) {
	return s.project(ctx, by.AccountID, in.Project)
}

// ListGoals implements mcp.ToolService.
func (s *Service) ListGoals(ctx context.Context, by domain.Actor, in mcp.ListGoalsInput) ([]domain.Goal, error) {
	if in.Status != "" && !slices.Contains(domain.GoalStatuses(), in.Status) {
		return nil, fmt.Errorf("unknown goal status %q: %w", in.Status, mcp.ErrInvalidInput)
	}
	projectID, err := s.optionalProject(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.Goals().List(ctx, by.AccountID, store.GoalFilter{ProjectID: projectID, Status: in.Status})
}

// ListJobs implements mcp.ToolService.
func (s *Service) ListJobs(ctx context.Context, by domain.Actor, in mcp.ListJobsInput) ([]domain.Job, error) {
	if in.Status != "" && !slices.Contains(domain.JobStatuses(), in.Status) {
		return nil, fmt.Errorf("unknown job status %q: %w", in.Status, mcp.ErrInvalidInput)
	}
	if in.Limit < 0 {
		return nil, fmt.Errorf("limit cannot be negative: %w", mcp.ErrInvalidInput)
	}
	f := store.JobFilter{Status: in.Status, Limit: min(cmp.Or(in.Limit, DefaultJobLimit), MaxJobLimit)}
	var err error
	if f.ProjectID, err = s.optionalProject(ctx, by.AccountID, in.Project); err != nil {
		return nil, err
	}
	if in.Goal != "" {
		g, err := s.goal(ctx, by.AccountID, in.Goal)
		if err != nil {
			return nil, err
		}
		f.GoalID = g.ID
	}
	return s.cfg.Store.Jobs().List(ctx, by.AccountID, f)
}

// GetGoal implements mcp.ToolService.
func (s *Service) GetGoal(ctx context.Context, by domain.Actor, in mcp.GoalRefInput) (mcp.GoalDetail, error) {
	g, err := s.goal(ctx, by.AccountID, in.Goal)
	if err != nil {
		return mcp.GoalDetail{}, err
	}
	st := s.cfg.Store
	p, err := st.Projects().Get(ctx, by.AccountID, g.ProjectID)
	if err != nil {
		return mcp.GoalDetail{}, fmt.Errorf("project of %s: %w", g.Ref(), err)
	}
	d := mcp.GoalDetail{Goal: g, Ref: g.Ref(), Project: p.Name, URL: issueURL(p.Repo, g.IssueNumber)}
	switch spec, err := st.Specs().Latest(ctx, by.AccountID, g.ID); {
	case err == nil:
		d.Spec = &spec
	case !errors.Is(err, store.ErrNotFound):
		return mcp.GoalDetail{}, fmt.Errorf("spec of %s: %w", g.Ref(), err)
	}
	all, err := st.Gates().List(ctx, by.AccountID, store.GateFilter{ProjectID: g.ProjectID})
	if err != nil {
		return mcp.GoalDetail{}, fmt.Errorf("gates of %s: %w", g.Ref(), err)
	}
	mine := slices.DeleteFunc(all, func(x domain.Gate) bool { return x.GoalID != g.ID })
	if d.Gates, err = s.gateInfos(ctx, by.AccountID, mine); err != nil {
		return mcp.GoalDetail{}, fmt.Errorf("gates of %s: %w", g.Ref(), err)
	}
	jobs, err := st.Jobs().List(ctx, by.AccountID, store.JobFilter{GoalID: g.ID})
	if err != nil {
		return mcp.GoalDetail{}, fmt.Errorf("jobs of %s: %w", g.Ref(), err)
	}
	for _, j := range jobs {
		d.CostUSD += j.CostUSD
	}
	tasks, err := st.Tasks().List(ctx, by.AccountID, store.TaskFilter{GoalID: g.ID})
	if err != nil {
		return mcp.GoalDetail{}, fmt.Errorf("tasks of %s: %w", g.Ref(), err)
	}
	d.Tasks = make([]mcp.TaskDetail, len(tasks))
	for i, t := range tasks {
		td, err := s.taskDetail(ctx, p, t, d.Gates, jobs)
		if err != nil {
			return mcp.GoalDetail{}, err
		}
		d.Tasks[i] = td
	}
	return d, nil
}

// taskDetail adds links, pull requests, pending needs, iterations and cost
// to a task. gates are the goal's, jobs the goal's.
func (s *Service) taskDetail(ctx context.Context, p domain.Project, t domain.Task, gates []mcp.GateInfo, jobs []domain.Job) (mcp.TaskDetail, error) {
	td := mcp.TaskDetail{Task: t, Ref: t.Ref(), URL: issueURL(p.Repo, t.IssueNumber), Needs: []string{}}
	for _, g := range gates {
		if g.TaskID == t.ID && g.Status == domain.GatePending {
			td.Needs = append(td.Needs, g.Ref)
		}
	}
	for _, j := range jobs {
		if j.TaskID == t.ID {
			td.Iterations++
			td.CostUSD += j.CostUSD
		}
	}
	changes, err := s.cfg.Store.Changes().List(ctx, t.AccountID, store.ChangeFilter{TaskID: t.ID})
	if err != nil {
		return mcp.TaskDetail{}, fmt.Errorf("changes of %s: %w", t.Ref(), err)
	}
	td.Changes = make([]mcp.ChangeInfo, len(changes))
	for i, c := range changes {
		td.Changes[i] = mcp.ChangeInfo{Change: c}
		if c.State == domain.ChangeOpen {
			td.Changes[i].Checks = s.checks(ctx, c)
		}
	}
	return td, nil
}

// checks returns the aggregate check state of the head commit of c; GitHub
// being unreachable does not fail the whole goal view.
func (s *Service) checks(ctx context.Context, c domain.Change) string {
	cs, err := s.cfg.GitHub.CheckStatus(ctx, c.Repo, c.HeadSHA)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "read check status", "account_id", c.AccountID, "repo", c.Repo, "pr", c.Number, "error", err)
		return mcp.ChecksUnknown
	}
	return string(cs.State)
}
