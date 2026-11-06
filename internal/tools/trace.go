package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// maxTraceSummary is the most characters of a node's summary.
const maxTraceSummary = 600

// GetTrace implements mcp.ToolService. It reads the existing tables; the
// chain is not stored anywhere.
func (s *Service) GetTrace(ctx context.Context, by domain.Actor, in mcp.GetTraceInput) (mcp.Trace, error) {
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		return mcp.Trace{}, fmt.Errorf("ref is required: %w", mcp.ErrInvalidInput)
	}
	b := &traceBuilder{s: s, ctx: ctx, by: by, acc: by.AccountID, seen: map[string]bool{}}
	goalID, focus, err := b.start(ref)
	if err != nil {
		return mcp.Trace{}, err
	}
	if goalID != uuid.Nil() {
		if err := b.goalChain(goalID); err != nil {
			return mcp.Trace{}, err
		}
	}
	b.t.Focus = focus
	b.finish()
	b.t.Untrusted = untrustedTrace(b.t)
	return b.t, nil
}

// traceBuilder collects the nodes and edges of a trace.
type traceBuilder struct {
	s   *Service
	ctx context.Context
	acc uuid.UUID
	by  domain.Actor
	// chain and jobs say whether the caller holds goal.read and job.read on
	// the goal's project: without goal.read the trace has no goal, spec, task,
	// change or merge, and without job.read no verification report.
	chain, jobs bool
	decisions   []domain.Decision
	t           mcp.Trace
	seen        map[string]bool
	// edges wait for the end: an edge needs both of its nodes, and a
	// decision comes before the task it is about.
	edges []mcp.TraceEdge
}

// allowed checks that the caller may read the project of the record asked
// for: the rest of the chain is in the same project.
func (b *traceBuilder) allowed(projectID uuid.UUID, err error) error {
	if err != nil {
		return err
	}
	return b.s.allow(b.ctx, b.by, domain.PermProjectRead, projectID)
}

// goalRead checks goal.read on the project of a goal, spec, task or change:
// a trace that starts there shows its chain.
func (b *traceBuilder) goalRead(projectID uuid.UUID, err error) error {
	if err != nil {
		return err
	}
	return b.s.allow(b.ctx, b.by, domain.PermGoalRead, projectID)
}

func (b *traceBuilder) node(n mcp.TraceNode) {
	if b.seen[n.ID] {
		return
	}
	b.seen[n.ID] = true
	if r := []rune(n.Summary); len(r) > maxTraceSummary {
		n.Summary = string(r[:maxTraceSummary])
	}
	b.t.Nodes = append(b.t.Nodes, n)
}

func (b *traceBuilder) edge(from, to, label string) {
	b.edges = append(b.edges, mcp.TraceEdge{From: from, To: to, Label: label})
}

// finish keeps the edges whose two nodes exist.
func (b *traceBuilder) finish() {
	for _, e := range b.edges {
		if b.seen[e.From] && b.seen[e.To] {
			b.t.Edges = append(b.t.Edges, e)
		}
	}
}

// start resolves ref to the goal whose chain to build (nil for a record
// that belongs to none, which is added as a chain of one) and the ID of the
// node asked for.
func (b *traceBuilder) start(ref string) (goalID uuid.UUID, focus string, err error) {
	st := b.s.cfg.Store
	if repo, n, ok := strings.Cut(ref, "#"); ok {
		num, perr := strconv.Atoi(n)
		if perr != nil || num < 1 {
			return uuid.Nil(), "", fmt.Errorf("%q is not a change like owner/name#12: %w", ref, mcp.ErrInvalidInput)
		}
		c, err := st.Changes().GetByPR(b.ctx, b.acc, repo, num)
		return b.ofChange(c, err, ref)
	}
	if id, perr := uuid.Parse(ref); perr == nil {
		c, err := st.Changes().Get(b.ctx, b.acc, id)
		return b.ofChange(c, err, ref)
	}
	kind, _, perr := domain.ParseRef(ref)
	if perr != nil {
		return uuid.Nil(), "", fmt.Errorf("%q is not a goal, spec, task, council, assumption, decision or change ref: %w", ref, mcp.ErrInvalidInput)
	}
	switch kind {
	case domain.RefGoal:
		g, err := b.s.goal(b.ctx, b.acc, ref)
		return g.ID, g.Ref(), b.goalRead(g.ProjectID, b.allowed(g.ProjectID, err))
	case domain.RefSpec:
		sp, err := lookup(ref, domain.RefSpec,
			func(n int64) (domain.Spec, error) { return st.Specs().GetByNumber(b.ctx, b.acc, n) },
			func(id uuid.UUID) (domain.Spec, error) { return st.Specs().Get(b.ctx, b.acc, id) })
		return sp.GoalID, sp.Ref(), b.goalRead(sp.ProjectID, b.allowed(sp.ProjectID, err))
	case domain.RefTask:
		t, err := b.s.task(b.ctx, b.acc, ref)
		return t.GoalID, t.Ref(), b.goalRead(t.ProjectID, b.allowed(t.ProjectID, err))
	case domain.RefCouncil:
		c, err := b.s.council(b.ctx, b.acc, ref)
		if err = b.allowed(c.ProjectID, err); err == nil && c.GoalID == uuid.Nil() {
			b.node(councilNode(c))
		}
		return c.GoalID, c.Ref(), err
	case domain.RefAssumption:
		a, err := b.s.assumption(b.ctx, b.acc, ref)
		if err = b.allowed(a.ProjectID, err); err == nil && a.GoalID == uuid.Nil() {
			b.node(assumptionNode(a))
		}
		return a.GoalID, a.Ref(), err
	case domain.RefDecision:
		d, err := lookup(ref, domain.RefDecision,
			func(n int64) (domain.Decision, error) { return st.Decisions().GetByNumber(b.ctx, b.acc, n) },
			func(id uuid.UUID) (domain.Decision, error) { return st.Decisions().Get(b.ctx, b.acc, id) })
		if err = b.allowed(d.ProjectID, err); err == nil && d.GoalID == uuid.Nil() {
			b.node(decisionNode(d))
		}
		return d.GoalID, d.Ref(), err
	}
	return uuid.Nil(), "", fmt.Errorf("%s refs have no trace: %w", kind, mcp.ErrInvalidInput)
}

func (b *traceBuilder) ofChange(c domain.Change, err error, ref string) (uuid.UUID, string, error) {
	if errors.Is(err, store.ErrNotFound) {
		return uuid.Nil(), "", fmt.Errorf("change %s: %w", ref, mcp.ErrNotFound)
	}
	if err := b.goalRead(c.ProjectID, b.allowed(c.ProjectID, err)); err != nil {
		return uuid.Nil(), "", err
	}
	if c.TaskID == uuid.Nil() {
		return uuid.Nil(), "", fmt.Errorf("change %s is not tied to a task, so it has no goal: %w", ref, mcp.ErrNotFound)
	}
	t, err := b.s.cfg.Store.Tasks().Get(b.ctx, b.acc, c.TaskID)
	return t.GoalID, changeID(c), err
}

func changeID(c domain.Change) string { return c.Repo + "#" + strconv.Itoa(c.Number) }

// can reports whether by holds p on a project.
func (s *Service) can(ctx context.Context, by domain.Actor, p domain.Permission, projectID uuid.UUID) (bool, error) {
	return s.authz.Can(ctx, by, p, authz.Project(by.AccountID, projectID))
}

// goalChain adds the whole chain of a goal.
func (b *traceBuilder) goalChain(goalID uuid.UUID) error {
	st := b.s.cfg.Store
	g, err := st.Goals().Get(b.ctx, b.acc, goalID)
	if err != nil {
		return err
	}
	// The record asked for may be in another project than its goal: read the
	// goal's own project, and keep to it below.
	if err := b.s.allow(b.ctx, b.by, domain.PermProjectRead, g.ProjectID); err != nil {
		return err
	}
	p, err := st.Projects().Get(b.ctx, b.acc, g.ProjectID)
	if err != nil {
		return err
	}
	if b.chain, err = b.s.can(b.ctx, b.by, domain.PermGoalRead, g.ProjectID); err != nil {
		return err
	}
	if b.jobs, err = b.s.can(b.ctx, b.by, domain.PermJobRead, g.ProjectID); err != nil {
		return err
	}
	if b.chain {
		b.t.Goal = g.Ref()
		b.node(mcp.TraceNode{ID: g.Ref(), Kind: mcp.TraceGoal, Title: g.Title, Status: string(g.Status), Summary: g.Body, URL: issueURL(p.Repo, g.IssueNumber), At: g.CreatedAt})
	}
	if err := b.upstream(g); err != nil {
		return err
	}
	return b.downstream(g, p)
}

// upstream adds councils, spec revisions and the decisions between them.
func (b *traceBuilder) upstream(g domain.Goal) error {
	st := b.s.cfg.Store
	councils, err := st.Councils().List(b.ctx, b.acc, store.CouncilFilter{GoalID: g.ID, ProjectID: g.ProjectID})
	if err != nil {
		return err
	}
	for _, c := range councils {
		b.node(councilNode(c))
		b.edge(g.Ref(), c.Ref(), "asked")
	}
	specs, err := b.specs(g)
	if err != nil {
		return err
	}
	prev, label := g.Ref(), "written"
	for _, sp := range specs {
		b.node(specNode(sp))
		b.edge(prev, sp.Ref(), label)
		prev, label = sp.Ref(), "revised"
	}
	ds, err := st.Decisions().List(b.ctx, b.acc, store.DecisionFilter{GoalID: g.ID, ProjectID: g.ProjectID})
	if err != nil {
		return err
	}
	b.decisions = ds
	for _, d := range ds {
		b.node(decisionNode(d))
		parent, err := b.decisionParent(g, d)
		if err != nil {
			return err
		}
		b.edge(parent, d.Ref(), "decided")
		if d.CouncilID != uuid.Nil() && len(specs) > 0 {
			b.edge(d.Ref(), specs[0].Ref(), "informs")
		}
	}
	return nil
}

// specs returns the spec revisions of the goal in its project, for a caller
// who reads the chain.
func (b *traceBuilder) specs(g domain.Goal) ([]domain.Spec, error) {
	if !b.chain {
		return nil, nil
	}
	all, err := b.s.cfg.Store.Specs().ListByGoal(b.ctx, b.acc, g.ID)
	return slices.DeleteFunc(all, func(sp domain.Spec) bool { return sp.ProjectID != g.ProjectID }), err
}

// decisionParent is the node a decision is about: its council, else the
// spec or task of its gate, else the goal.
func (b *traceBuilder) decisionParent(g domain.Goal, d domain.Decision) (string, error) {
	st := b.s.cfg.Store
	if d.CouncilID != uuid.Nil() {
		c, err := st.Councils().Get(b.ctx, b.acc, d.CouncilID)
		if err == nil {
			return c.Ref(), nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	if d.GateID != uuid.Nil() {
		gate, err := st.Gates().Get(b.ctx, b.acc, d.GateID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return "", err
		case gate.SpecID != uuid.Nil():
			if sp, err := st.Specs().Get(b.ctx, b.acc, gate.SpecID); err == nil {
				return sp.Ref(), nil
			}
		case gate.TaskID != uuid.Nil():
			if t, err := st.Tasks().Get(b.ctx, b.acc, gate.TaskID); err == nil {
				return t.Ref(), nil
			}
		}
	}
	return g.Ref(), nil
}

// downstream adds tasks, their changes, verification and merge, and the
// assumptions of the goal.
func (b *traceBuilder) downstream(g domain.Goal, p domain.Project) error {
	st := b.s.cfg.Store
	specRefs := map[uuid.UUID]string{}
	specs, err := b.specs(g)
	if err != nil {
		return err
	}
	for _, sp := range specs {
		specRefs[sp.ID] = sp.Ref()
	}
	var tasks []domain.Task
	if b.chain {
		if tasks, err = st.Tasks().List(b.ctx, b.acc, store.TaskFilter{GoalID: g.ID, ProjectID: g.ProjectID}); err != nil {
			return err
		}
	}
	for _, t := range tasks {
		b.node(mcp.TraceNode{ID: t.Ref(), Kind: mcp.TraceTask, Title: t.Title, Status: string(t.Status), Summary: t.Body, URL: issueURL(p.Repo, t.IssueNumber), At: t.CreatedAt})
		b.edge(cmp.Or(specRefs[t.SpecID], g.Ref()), t.Ref(), "planned")
		if err := b.changes(t); err != nil {
			return err
		}
	}
	return b.assumptions(g)
}

// assumptions adds the assumptions of the goal, each attached to what it
// affects: its dependents, the decisions that rest on it, else the goal.
func (b *traceBuilder) assumptions(g domain.Goal) error {
	st := b.s.cfg.Store
	as, err := st.Assumptions().List(b.ctx, b.acc, store.AssumptionFilter{ProjectID: g.ProjectID, GoalID: g.ID})
	if err != nil {
		return err
	}
	for _, a := range as {
		b.node(assumptionNode(a))
		attached := false
		for _, d := range a.Dependents {
			if b.seen[d.ID] {
				b.edge(a.Ref(), d.ID, "affects")
				attached = true
			}
		}
		// A decision that rests on it is one more thing it affects.
		for _, d := range b.decisions {
			if b.seen[d.Ref()] && slices.Contains(d.Assumptions, a.ID) {
				b.edge(a.Ref(), d.Ref(), "affects")
				attached = true
			}
		}
		if !attached {
			b.edge(a.Ref(), g.Ref(), "affects")
		}
	}
	return nil
}

// changes adds the pull requests of a task with their newest verification
// report and, for a merged one, the merge.
func (b *traceBuilder) changes(t domain.Task) error {
	st := b.s.cfg.Store
	cs, err := st.Changes().List(b.ctx, b.acc, store.ChangeFilter{TaskID: t.ID, ProjectID: t.ProjectID})
	if err != nil {
		return err
	}
	slices.SortFunc(cs, func(x, y domain.Change) int { return cmp.Compare(x.Number, y.Number) })
	for _, c := range cs {
		id := changeID(c)
		b.node(mcp.TraceNode{ID: id, Kind: mcp.TraceChange, Title: "Pull request #" + strconv.Itoa(c.Number), Status: string(c.State), URL: c.URL, At: c.CreatedAt,
			Attrs: map[string]string{"branch": c.Branch}})
		b.edge(t.Ref(), id, "built")
		last := id
		var r domain.VerificationReport
		if b.jobs {
			r, err = st.VerificationReports().Latest(b.ctx, b.acc, c.ID)
		} else {
			err = store.ErrNotFound
		}
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			vid := mcp.TraceVerification + ":" + id
			b.node(mcp.TraceNode{ID: vid, Kind: mcp.TraceVerification, Title: "Verification of #" + strconv.Itoa(c.Number), Status: string(r.Status), At: r.UpdatedAt,
				Attrs: map[string]string{"sha": r.SHA}})
			b.edge(id, vid, "verified")
			last = vid
		}
		if c.State == domain.ChangeMerged {
			mid := mcp.TraceMerge + ":" + id
			b.node(mcp.TraceNode{ID: mid, Kind: mcp.TraceMerge, Title: "Merged #" + strconv.Itoa(c.Number), Status: string(c.State), URL: c.URL, At: c.UpdatedAt})
			b.edge(last, mid, "merged")
		}
	}
	return nil
}

func councilNode(c domain.Council) mcp.TraceNode {
	n := mcp.TraceNode{ID: c.Ref(), Kind: mcp.TraceCouncil, Title: c.Question, Status: string(c.Status), At: c.CreatedAt, Attrs: map[string]string{"kind": string(c.Kind)}}
	if c.Brief != nil {
		n.Summary = c.Brief.Recommendation
	}
	return n
}

func specNode(sp domain.Spec) mcp.TraceNode {
	return mcp.TraceNode{ID: sp.Ref(), Kind: mcp.TraceSpec, Title: "Spec revision " + strconv.Itoa(sp.Revision), Summary: sp.Body, At: sp.CreatedAt}
}

func decisionNode(d domain.Decision) mcp.TraceNode {
	return mcp.TraceNode{ID: d.Ref(), Kind: mcp.TraceDecision, Title: d.Question, Status: d.Choice, Summary: d.Reasons, At: d.CreatedAt,
		Attrs: map[string]string{"decided_by": d.DecidedBy, "dissent": d.Dissent}}
}

func assumptionNode(a domain.Assumption) mcp.TraceNode {
	return mcp.TraceNode{ID: a.Ref(), Kind: mcp.TraceAssumption, Title: a.Statement, Status: string(a.Status), At: a.CreatedAt,
		Attrs: map[string]string{"impact": string(a.Impact), "confidence": string(a.Confidence), "verify_by": string(a.VerifyBy), "made_by": a.MadeBy, "evidence": a.Evidence}}
}

// untrustedTrace lists the nodes as one wrapped block: titles, summaries
// and attributes were written by agents, and an MCP client may be one.
func untrustedTrace(t mcp.Trace) string {
	var text strings.Builder
	for _, n := range t.Nodes {
		fmt.Fprintf(&text, "- %s (%s, %s): %s\n", n.ID, n.Kind, cmp.Or(n.Status, "-"), contextpack.OneLine(n.Title))
	}
	for _, e := range t.Edges {
		fmt.Fprintf(&text, "%s -> %s (%s)\n", e.From, e.To, e.Label)
	}
	if text.Len() == 0 {
		return ""
	}
	return contextpack.WrapUntrusted("trace of "+t.Focus, text.String())
}
