// Package registers keeps the assumption and decision registers
// (docs/contracts/data-model.md): every assumption an agent states becomes
// a tracked item with a policy by impact, and every human or council
// decision is recorded with its reasons.
//
// Policy by impact: a low-impact assumption is only logged (an audit entry),
// a medium one is listed as open in get_goal and in the gate summaries, and
// a high one opens an assumption_review gate of its own (one per assumption,
// so a decision maps onto one assumption) that the playbook waits for before
// it opens the merge gate.
package registers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
)

// Service is the register. Safe for concurrent use.
type Service struct {
	st  store.Store
	pub gates.Publisher
	now func() time.Time
}

// New returns a register on st. pub publishes the gate.resolved signals of
// the gates the register resolves itself (a merge gate whose assumption was
// invalidated); now defaults to the real UTC time.
func New(st store.Store, pub gates.Publisher, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{st: st, pub: pub, now: now}
}

// Source says where stated assumptions come from and what rests on them.
// The goal and the task (when set) become dependents; Extra adds others.
type Source struct {
	// MadeBy is the role (or council member) that stated them.
	MadeBy string
	GoalID, SpecID, TaskID,
	JobID, CouncilID uuid.UUID
	Extra []domain.Dependent
}

// Normalize returns the form in which statements are compared: lower case,
// white space collapsed, trailing punctuation dropped.
func Normalize(statement string) string {
	s := strings.ToLower(strings.Join(strings.Fields(statement), " "))
	return strings.TrimRight(s, " .,;:!?")
}

// Record registers stated assumptions of a project in one transaction. A
// statement the goal already has (same Normalize form; for a source without
// a goal, among the project's assumptions without one) adds the source's
// dependents to the existing item instead of making a new one. It returns
// one item per statement, in order, and opens an assumption_review gate for
// every new high-impact one.
func (s *Service) Record(ctx context.Context, accountID, projectID uuid.UUID, src Source, stated []domain.StatedAssumption) ([]domain.Assumption, error) {
	var out []domain.Assumption
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) (err error) {
		out, err = s.record(ctx, tx, accountID, projectID, src, stated)
		return err
	})
	return out, err
}

func (s *Service) record(ctx context.Context, tx store.Store, accountID, projectID uuid.UUID, src Source, stated []domain.StatedAssumption) ([]domain.Assumption, error) {
	if err := tx.Assumptions().Lock(ctx, accountID, projectID, src.GoalID); err != nil {
		return nil, fmt.Errorf("lock the assumptions of the goal: %w", err)
	}
	deps, err := dependents(ctx, tx, accountID, src)
	if err != nil {
		return nil, err
	}
	filter := store.AssumptionFilter{ProjectID: projectID, GoalID: src.GoalID}
	existing, err := tx.Assumptions().List(ctx, accountID, filter)
	if err != nil {
		return nil, fmt.Errorf("list assumptions: %w", err)
	}
	existing = slices.DeleteFunc(existing, func(a domain.Assumption) bool { return a.GoalID != src.GoalID })
	out := make([]domain.Assumption, 0, len(stated))
	for _, st := range stated {
		if err := st.Validate(); err != nil {
			return nil, err
		}
		norm := Normalize(st.Statement)
		same := func(a domain.Assumption) bool { return Normalize(a.Statement) == norm }
		// A statement a human invalidated is not the same item: restating it
		// is a new, high-impact one that is reviewed again.
		if i := slices.IndexFunc(existing, func(a domain.Assumption) bool {
			return same(a) && a.Status != domain.AssumptionInvalidated
		}); i >= 0 {
			if err := s.restate(ctx, tx, &existing[i], st, src, deps); err != nil {
				return nil, err
			}
			out = append(out, existing[i])
			continue
		}
		var rejected *domain.Assumption
		if i := slices.IndexFunc(existing, same); i >= 0 {
			rejected = &existing[i]
			st.Impact = domain.LevelHigh
		}
		a, err := s.create(ctx, tx, accountID, projectID, src, st, deps, rejected)
		if err != nil {
			return nil, err
		}
		existing = append(existing, a)
		out = append(out, a)
	}
	return out, nil
}

// restate adds the dependents of src to the registered assumption a that
// is stated again. A higher impact than the registered one raises it: an
// agent cannot keep an item at low by stating it first, and an open item
// that becomes high-impact is reviewed.
func (s *Service) restate(ctx context.Context, tx store.Store, a *domain.Assumption, st domain.StatedAssumption, src Source, deps []domain.Dependent) error {
	// An item of the goal as a whole lists no task, and that is what makes
	// every task rest on it: stating it again from a task must not take it
	// away from the others.
	if !slices.ContainsFunc(a.Dependents, func(d domain.Dependent) bool { return d.Kind == domain.DependentTask }) {
		deps = slices.DeleteFunc(slices.Clone(deps), func(d domain.Dependent) bool { return d.Kind == domain.DependentTask })
	}
	if err := tx.Assumptions().AddDependents(ctx, a.AccountID, a.ID, deps); err != nil {
		return fmt.Errorf("add dependents to %s: %w", a.Ref(), err)
	}
	a.Dependents = store.MergeDependents(a.Dependents, deps)
	if slices.Index(domain.Levels(), st.Impact) <= slices.Index(domain.Levels(), a.Impact) {
		return nil
	}
	if err := tx.Assumptions().RaiseImpact(ctx, a.AccountID, a.ID, st.Impact); err != nil {
		return fmt.Errorf("raise the impact of %s: %w", a.Ref(), err)
	}
	from := a.Impact
	a.Impact = st.Impact
	err := s.audit(ctx, tx, domain.SystemActor(a.AccountID, domain.ForecrewdName), *a, "assumption.impact_raised",
		map[string]any{"ref": a.Ref(), "from": from, "impact": a.Impact, "stated_by": src.MadeBy})
	if err != nil || a.Impact != domain.LevelHigh || !a.Status.Open() {
		return err
	}
	return s.openReview(ctx, tx, *a, src, nil)
}

// create registers a new assumption and, when its impact is high, opens
// its review. rejected is the invalidated assumption it restates, if any.
func (s *Service) create(ctx context.Context, tx store.Store, accountID, projectID uuid.UUID, src Source, st domain.StatedAssumption, deps []domain.Dependent, rejected *domain.Assumption) (domain.Assumption, error) {
	a := domain.Assumption{
		AccountID: accountID, ProjectID: projectID, GoalID: src.GoalID, MadeBy: src.MadeBy,
		JobID: src.JobID, SpecID: src.SpecID, CouncilID: src.CouncilID, StatedAssumption: st,
		Status: domain.AssumptionUnverified, Dependents: slices.Clone(deps),
	}
	if err := a.Validate(); err != nil {
		return a, err
	}
	if err := tx.Assumptions().Create(ctx, &a); err != nil {
		return a, fmt.Errorf("register assumption: %w", err)
	}
	data := map[string]any{"ref": a.Ref(), "impact": a.Impact, "made_by": a.MadeBy}
	if rejected != nil {
		data["restates"] = rejected.Ref()
	}
	if err := s.audit(ctx, tx, domain.SystemActor(accountID, domain.ForecrewdName), a, "assumption.recorded", data); err != nil {
		return a, err
	}
	if a.Impact == domain.LevelHigh {
		return a, s.openReview(ctx, tx, a, src, rejected)
	}
	return a, nil
}

// dependents returns the dependents of a source: its goal and task by ref,
// then Extra.
func dependents(ctx context.Context, tx store.Store, accountID uuid.UUID, src Source) ([]domain.Dependent, error) {
	deps := []domain.Dependent{}
	if src.GoalID != uuid.Nil() {
		g, err := tx.Goals().Get(ctx, accountID, src.GoalID)
		if err != nil {
			return nil, fmt.Errorf("get goal: %w", err)
		}
		deps = append(deps, domain.Dependent{Kind: domain.DependentGoal, ID: g.Ref()})
	}
	if src.TaskID != uuid.Nil() {
		t, err := tx.Tasks().Get(ctx, accountID, src.TaskID)
		if err != nil {
			return nil, fmt.Errorf("get task: %w", err)
		}
		deps = append(deps, domain.Dependent{Kind: domain.DependentTask, ID: t.Ref()})
	}
	return store.MergeDependents(deps, src.Extra), nil
}

// JobCompleted registers the assumptions in the output of a succeeded agent
// job, in the transaction tx that finishes the job. The job is the source,
// its spec the spec, its goal and task the dependents. Output without
// assumptions, or with items that are not assumptions, registers nothing:
// the job's output schema is what checks them.
func (s *Service) JobCompleted(ctx context.Context, tx store.Store, j domain.Job) error {
	if j.Status != domain.JobSucceeded || j.Kind.OrDefault() != domain.JobKindAgent || len(j.Output) == 0 {
		return nil
	}
	var out struct {
		Assumptions []domain.StatedAssumption `json:"assumptions"`
	}
	if err := json.Unmarshal(j.Output, &out); err != nil {
		return nil //nolint:nilerr // output without a list of assumptions has none to register
	}
	stated := slices.DeleteFunc(out.Assumptions, func(a domain.StatedAssumption) bool { return a.Validate() != nil })
	if len(stated) == 0 {
		return nil
	}
	_, err := s.record(ctx, tx, j.AccountID, j.ProjectID, fromJob(j), stated)
	if err != nil {
		return fmt.Errorf("register the assumptions of %s: %w", j.Ref(), err)
	}
	return nil
}

func fromJob(j domain.Job) Source {
	return Source{MadeBy: j.Role, GoalID: j.GoalID, SpecID: j.SpecID, TaskID: j.TaskID, JobID: j.ID}
}

// RecordAssumption registers an assumption a job made while it ran (the job
// MCP tool assumption_record, jobmcp.AssumptionRecorder). It returns the
// item, which is the existing one when the goal had the statement already,
// and says whether it was. The job's row is locked first, as completing the
// job does: the record commits before the job finishes, or it is refused with
// store.ErrLeaseLost.
func (s *Service) RecordAssumption(ctx context.Context, j domain.Job, a domain.StatedAssumption) (domain.Assumption, bool, error) {
	var got domain.Assumption
	var dup bool
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Jobs().LockLeased(ctx, j.AccountID, j.ID, j.WorkerID); err != nil {
			return err
		}
		if err := tx.Assumptions().Lock(ctx, j.AccountID, j.ProjectID, j.GoalID); err != nil {
			return fmt.Errorf("lock the assumptions of the goal: %w", err)
		}
		before, err := tx.Assumptions().List(ctx, j.AccountID, store.AssumptionFilter{ProjectID: j.ProjectID, GoalID: j.GoalID})
		if err != nil {
			return err
		}
		out, err := s.record(ctx, tx, j.AccountID, j.ProjectID, fromJob(j), []domain.StatedAssumption{a})
		if err != nil {
			return err
		}
		got = out[0]
		dup = slices.ContainsFunc(before, func(x domain.Assumption) bool { return x.ID == got.ID })
		return nil
	})
	return got, dup, err
}

// reviewTitle is the title of the assumption_review gate of a.
func reviewTitle(a domain.Assumption) string {
	return "Review " + a.Ref() + ": " + contextpack.FirstLine(a.Statement, 100)
}

// ReviewedRef returns the number of the assumption an assumption_review gate
// is about, read from its title: "Review A-3: ..." is about A-3, whatever
// the rest of the title says (it holds the statement, which an agent wrote).
func ReviewedRef(g domain.Gate) (int64, bool) {
	rest, ok := strings.CutPrefix(g.Title, "Review ")
	if !ok || g.Kind != domain.GateAssumptionReview {
		return 0, false
	}
	ref, _, _ := strings.Cut(rest, ":")
	kind, n, err := domain.ParseRef(ref)
	return n, err == nil && kind == domain.RefAssumption
}

// openReview opens the assumption_review gate of a high-impact assumption,
// on the issue of its task (else its goal) so that a command there reaches
// it. rejected is the invalidated assumption that a restates, if any.
func (s *Service) openReview(ctx context.Context, tx store.Store, a domain.Assumption, src Source, rejected *domain.Assumption) error {
	p, err := tx.Projects().Get(ctx, a.AccountID, a.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	req := gates.OpenRequest{
		AccountID: a.AccountID, ProjectID: a.ProjectID, Kind: domain.GateAssumptionReview, Title: reviewTitle(a),
		SubjectRepo: p.Repo, GoalID: a.GoalID, SpecID: a.SpecID, TaskID: src.TaskID, JobID: src.JobID,
	}
	switch {
	case src.TaskID != uuid.Nil():
		t, err := tx.Tasks().Get(ctx, a.AccountID, src.TaskID)
		if err != nil {
			return fmt.Errorf("get task: %w", err)
		}
		req.SubjectNumber = t.IssueNumber
	case a.GoalID != uuid.Nil():
		g, err := tx.Goals().Get(ctx, a.AccountID, a.GoalID)
		if err != nil {
			return fmt.Errorf("get goal: %w", err)
		}
		req.SubjectNumber = g.IssueNumber
	}
	req.Summary = reviewSummary(a, rejected, "")
	if _, err := s.keeper(tx).Open(ctx, req); err != nil {
		return fmt.Errorf("open the review of %s: %w", a.Ref(), err)
	}
	return nil
}

// reopenReview opens a new review of a after a human asked for changes at g,
// so that the run keeps waiting for a human and not for agents.
func (s *Service) reopenReview(ctx context.Context, tx store.Store, g domain.Gate, a domain.Assumption) error {
	req := gates.OpenRequest{
		AccountID: g.AccountID, ProjectID: g.ProjectID, Kind: domain.GateAssumptionReview, Title: reviewTitle(a),
		SubjectRepo: g.SubjectRepo, SubjectNumber: g.SubjectNumber, GoalID: g.GoalID, SpecID: g.SpecID, TaskID: g.TaskID, JobID: g.JobID,
		Summary: reviewSummary(a, nil, g.Feedback),
	}
	if _, err := s.keeper(tx).Open(ctx, req); err != nil {
		return fmt.Errorf("reopen the review of %s: %w", a.Ref(), err)
	}
	return nil
}

// reviewSummary is the text of a review gate. rejected is the invalidated
// assumption a restates; asked is what a human asked for at an earlier
// review. The statement and the evidence are text an agent wrote.
func reviewSummary(a domain.Assumption, rejected *domain.Assumption, asked string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is a high-impact assumption made by %s: work rests on it and nobody has checked it.\n\n", a.Ref(), contextpack.OneLine(a.MadeBy))
	if rejected != nil {
		fmt.Fprintf(&b, "It restates %s, which was invalidated. Evidence:\n%s\n", rejected.Ref(), contextpack.WrapUntrusted("evidence "+rejected.Ref(), rejected.Evidence))
	}
	if asked = strings.TrimSpace(asked); asked != "" {
		fmt.Fprintf(&b, "Changes were asked for at the last review:\n%s\n\n", asked)
	}
	b.WriteString("- **Approve** to accept it: you take the risk.\n")
	b.WriteString("- **Reject** to invalidate it: what depends on it is flagged and sent back.\n")
	b.WriteString("- **Request changes** with what to check; it stays unverified, a new review opens and it blocks the merge until it is resolved.\n\n")
	fmt.Fprintf(&b, "Confidence %s, impact %s, to be checked by %s.\n", a.Confidence, a.Impact, a.VerifyBy)
	if len(a.Dependents) > 0 {
		ids := make([]string, len(a.Dependents))
		for i, d := range a.Dependents {
			ids[i] = d.ID
		}
		fmt.Fprintf(&b, "Rests on it: %s.\n", strings.Join(ids, ", "))
	}
	b.WriteString("\n" + contextpack.WrapUntrusted("assumption "+a.Ref(), a.Statement))
	return b.String()
}

// audit appends an entry about assumption a.
func (s *Service) audit(ctx context.Context, tx store.Store, by domain.Actor, a domain.Assumption, action string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	err = tx.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: a.AccountID, ProjectID: a.ProjectID, Actor: by.String(), Credential: by.Credential.String(), Action: action,
		SubjectType: "assumption", SubjectID: a.ID, Data: raw,
	})
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// keeper returns a gate service on st that publishes to the register's bus.
func (s *Service) keeper(st store.Store) *gates.Keeper { return gates.New(st, s.pub, s.now) }

// LinkSpec adds the spec as a dependent of the assumptions the job stated.
func (s *Service) LinkSpec(ctx context.Context, accountID, jobID uuid.UUID, spec domain.Spec) error {
	as, err := s.st.Assumptions().List(ctx, accountID, store.AssumptionFilter{ProjectID: spec.ProjectID, JobID: jobID})
	if err != nil {
		return fmt.Errorf("list the assumptions of the job of %s: %w", spec.Ref(), err)
	}
	dep := []domain.Dependent{{Kind: domain.DependentSpec, ID: spec.Ref()}}
	for _, a := range as {
		if err := s.st.Assumptions().AddDependents(ctx, accountID, a.ID, dep); err != nil {
			return fmt.Errorf("link %s to %s: %w", spec.Ref(), a.Ref(), err)
		}
	}
	return nil
}
