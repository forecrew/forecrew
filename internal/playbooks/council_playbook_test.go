package playbooks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

const councilCommit = "c0ffee"

func councilConfig() *config.Config {
	role := func(rt string) config.Role { return config.Role{Runtime: rt, Tier: config.TierBest} }
	return &config.Config{
		Runtimes: map[string]config.Runtime{
			"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic"},
			"codex":  {Adapter: config.AdapterCodex, Vendor: "openai"},
		},
		ModelTiers: map[config.Tier]map[string]string{config.TierBest: {"anthropic": "a", "openai": "o"}},
		Roles: map[string]config.Role{
			"facilitator": role("claude"), "architect": role("claude"), "ux": role("codex"), "developer": role("claude"),
			"security-reviewer": role("codex"), "environment-engineer": role("claude"), "devils-advocate": role("codex"),
		},
	}
}

// councilEnv runs the shipped council playbook on an engine that can be
// stopped and started again; play is the scripted runtime, which answers the
// jobs as workers would.
type councilEnv struct {
	*env
	cfg     *config.Config
	seats   *councils.Set
	books   map[string]*workflow.Playbook
	jobs    *testkit.JobCreator
	worker  domain.Worker
	human   domain.Actor
	evid    *testkit.Evidence
	fetcher *councils.Fetcher

	engine *workflow.Engine
	stop   func()

	mu     sync.Mutex
	usage  domain.Usage // of every job
	invent func(path string) any
}

// labels are what the scripted worker offers.
var scriptedLabels = []string{domain.RuntimeLabel(config.AdapterClaudeCode), domain.RuntimeLabel(config.AdapterCodex)}

func newCouncilEnv(t *testing.T) *councilEnv {
	t.Helper()
	return councilEnvOn(t, newEnv(t, domain.ProjectOnboarded))
}

// councilEnvOn makes v, with its tracker and project, an env for councils.
func councilEnvOn(t *testing.T, v *env) *councilEnv {
	t.Helper()
	cfg := councilConfig()
	catalog, err := roles.New(forecrew.Files, cfg)
	must(t, err)
	seats, err := councils.Load(forecrew.Files, catalog.Definition)
	must(t, err)
	books, err := workflow.LoadPlaybooks(forecrew.Files, "packs/playbooks")
	must(t, err)
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", councilCommit))
	e := &councilEnv{
		env: v, cfg: cfg, seats: seats, books: books, jobs: &testkit.JobCreator{Store: v.st}, evid: testkit.NewEvidence(), fetcher: &councils.Fetcher{},
		human: domain.Actor{AccountID: v.account, Kind: domain.ActorUser, Name: "ana"},
	}
	e.worker = domain.Worker{AccountID: v.account, Name: "box", TokenHash: []byte(domain.NewID().String())}
	must(t, v.st.Workers().Create(t.Context(), &e.worker))
	control := domain.Worker{AccountID: v.account, Name: "control", Pool: domain.PoolControl, TokenHash: []byte(domain.NewID().String())}
	must(t, v.st.Workers().Create(t.Context(), &control))
	must(t, v.st.Workers().Seen(t.Context(), v.account, control.ID, scriptedLabels, "1"))
	v.gates.WithRecorder(registers.New(v.st, v.bus, v.clock.Now))
	playbooks.Register(v.actions, playbooks.Deps{
		Store: v.st, Tracker: v.tracker, Signals: v.bus, Sizing: config.DefaultSizing(), Now: v.clock.Now,
		Config: cfg, Councils: seats, Gates: v.gates, Runs: late{e}, Evidence: e.evid, Fetcher: e.fetcher,
	})
	return e
}

// late starts runs on the env's current engine.
type late struct{ e *councilEnv }

func (l late) Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	return l.e.engine.Start(ctx, playbook, req)
}

// run starts the engine on st (the env's store by default) until the test or
// a call to stop ends it.
func (e *councilEnv) run(t *testing.T, st ...store.Store) {
	t.Helper()
	cfg := workflow.Config{
		Store: e.st, Bus: e.bus, Jobs: e.jobs, Gates: e.gates, Actions: e.actions, Playbooks: e.books,
		Now: e.clock.Now, Workers: 2, Sweep: 5 * time.Millisecond,
	}
	if len(st) > 0 {
		cfg.Store = st[0]
	}
	var err error
	e.engine, err = workflow.New(cfg)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = e.engine.Run(ctx) })
	var once sync.Once
	e.stop = func() { once.Do(func() { cancel(); wg.Wait() }) }
	t.Cleanup(e.stop)
}

// start starts a council with the engine running.
func (e *councilEnv) start(t *testing.T, r councils.Request) domain.Council {
	t.Helper()
	if e.engine == nil {
		e.run(t)
	}
	r.AccountID, r.ProjectID = e.account, e.project.ID
	c, err := e.seats.Start(t.Context(), e.st, late{e}, r)
	must(t, err)
	return c
}

// three is a council of three members.
var three = councils.Request{Kind: domain.CouncilTechnical, Question: "Which queue?", Roles: []string{"architect", "ux"}}

// council returns the council as stored.
func (e *councilEnv) council(t *testing.T, id uuid.UUID) domain.Council {
	t.Helper()
	c, err := e.st.Councils().Get(t.Context(), e.account, id)
	must(t, err)
	return c
}

func (e *councilEnv) contributions(t *testing.T, id uuid.UUID, stage domain.CouncilStage) []domain.CouncilContribution {
	t.Helper()
	all, err := e.st.Councils().Contributions(t.Context(), e.account, id, stage)
	must(t, err)
	return all
}

// script answers a job from its step's path: the last segment is the job
// step's id. Nil from invent falls back to a valid output.
func (e *councilEnv) script(path string, req workflow.JobRequest) any {
	e.mu.Lock()
	invent := e.invent
	e.mu.Unlock()
	if invent != nil {
		if out := invent(path); out != nil {
			return out
		}
	}
	return validOutput(req)
}

func validOutput(req workflow.JobRequest) any {
	t := req.Council
	member := req.Role + "@" + t.Runtime
	switch councils.Step(t.Step) {
	case councils.StepFrame:
		return map[string]any{"question": "Which queue?", "constraints": []any{}, "must_contain": []any{"the store"}, "context": []any{}, "assumptions": []any{}, "escalation": nil}
	case councils.StepProposal, councils.StepRevision:
		out := map[string]any{
			"priority": orOf(t.Priority), "position": "Proposal of " + member, "recommendation": "pg",
			"options": []any{option("pg"), option("nats")}, "assumptions": []any{}, "risks": []any{}, "revision": nil, "escalation": nil,
			"evidence": []any{map[string]any{"claim": "It leases.", "kind": "path", "source": "lease.go:3", "date": nil, "confidence": "high"}},
		}
		if councils.Step(t.Step) == councils.StepRevision {
			out["revision"] = map[string]any{"changed": false, "reason": "Nothing answered it.", "prompted_by": []any{}}
		}
		return out
	case councils.StepCritique:
		critique := map[string]any{"member": "architect@claude-code", "agree": []any{}, "disagree": []any{}, "challenged_assumptions": []any{}}
		return map[string]any{"priority": orOf(t.Priority), "critiques": []any{critique}, "assumptions": []any{}, "escalation": nil}
	case councils.StepCheck:
		return map[string]any{"positions_moving": t.Round == 1, "reason": "Round " + fmt.Sprint(t.Round), "open_disagreements": []any{}}
	}
	return brief(true, false)
}

func orOf(priority string) string {
	if priority == "" {
		return "none"
	}
	return priority
}

func option(name string) map[string]any {
	return map[string]any{"name": name, "summary": "Use " + name + ".", "pros": []any{}, "cons": []any{}, "cost": "low", "risk": "low"}
}

func brief(reversible, exhausted bool) map[string]any {
	return map[string]any{
		"question": "Which queue?", "options": []any{option("pg"), option("nats")}, "recommendation": "pg",
		"votes":       []any{map[string]any{"member": "architect@claude-code", "option": "pg", "reason": "Simple."}},
		"dissent":     []any{map[string]any{"member": "devils-advocate@codex", "position": councils.DevilsAdvocateDissent + "Polling will not scale."}},
		"assumptions": []any{map[string]any{"statement": "Load stays low.", "confidence": "high", "impact": "low", "verify_by": "test"}}, "open_questions": []any{"How low?"},
		"reversible": reversible, "budget_exhausted": exhausted,
	}
}

// play answers queued jobs until done holds.
func (e *councilEnv) play(t *testing.T, what string, done func() bool) {
	t.Helper()
	eventually(t, what, func() bool {
		e.answerOne(t)
		return done()
	})
}

// answerOne leases and finishes the next queued job, if any.
func (e *councilEnv) answerOne(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	j, err := e.st.Jobs().LeaseNext(ctx, e.account, e.worker.ID, scriptedLabels, time.Hour)
	if err != nil {
		return // none queued
	}
	req := e.request(t, j.ID)
	step, err := e.st.StepRuns().Get(ctx, e.account, j.ID)
	must(t, err)
	out, err := json.Marshal(e.script(step.StepID, req))
	must(t, err)
	e.mu.Lock()
	usage := e.usage
	e.mu.Unlock()
	_, err = e.st.Jobs().Complete(ctx, e.account, j.ID, e.worker.ID, store.JobResult{Status: domain.JobSucceeded, Output: out, Usage: usage})
	must(t, err)
	sig, err := domain.NewSignal(e.account, e.project.ID, &domain.JobCompleted{JobID: j.ID, Status: domain.JobSucceeded}, e.clock.Now())
	must(t, err)
	_, err = e.bus.Publish(ctx, sig)
	must(t, err)
}

func (e *councilEnv) request(t *testing.T, id uuid.UUID) workflow.JobRequest {
	t.Helper()
	for _, r := range e.jobs.Requests() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no request for job %s", id)
	return workflow.JobRequest{}
}

// gate plays until the council's gate is pending and returns it.
func (e *councilEnv) gate(t *testing.T) domain.Gate {
	t.Helper()
	var g domain.Gate
	e.play(t, "the council_decision gate", func() bool {
		gs, err := e.gates.ListPending(t.Context(), e.account, e.project.ID)
		must(t, err)
		for _, x := range gs {
			if x.Kind == domain.GateCouncilDecision {
				g = x
				return true
			}
		}
		return false
	})
	return g
}

func (e *councilEnv) decide(t *testing.T, g domain.Gate, tool, feedback string) {
	t.Helper()
	_, err := e.gates.Decide(t.Context(), e.human, tool, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: feedback})
	must(t, err)
}

func (e *councilEnv) waitCouncil(t *testing.T, id uuid.UUID, status domain.CouncilStatus) domain.Council {
	t.Helper()
	var c domain.Council
	e.play(t, "council "+string(status), func() bool {
		c = e.council(t, id)
		return c.Status == status
	})
	return c
}

func (e *councilEnv) signal(t *testing.T, id uuid.UUID) domain.CouncilDecidedSignal {
	t.Helper()
	var sigs []domain.Signal
	eventually(t, "council.decided", func() bool {
		var err error
		sigs, err = e.bus.Lookup(t.Context(), e.account, domain.CouncilKey(id))
		must(t, err)
		return len(sigs) > 0
	})
	if len(sigs) != 1 {
		t.Fatalf("signals = %+v, want one", sigs)
	}
	p, err := sigs[0].Decode()
	must(t, err)
	return *p.(*domain.CouncilDecidedSignal)
}

// requests returns the requests of a step, in order.
func (e *councilEnv) requests(step councils.Step) []workflow.JobRequest {
	var out []workflow.JobRequest
	for _, r := range e.jobs.Requests() {
		if r.Council != nil && councils.Step(r.Council.Step) == step {
			out = append(out, r)
		}
	}
	return out
}

func (e *councilEnv) decisions(t *testing.T) []domain.Decision {
	t.Helper()
	all, err := e.st.Decisions().List(t.Context(), e.account, store.DecisionFilter{})
	must(t, err)
	return all
}

// TestCouncilToADecision is the whole protocol: three members, one revision
// round, the synthesis, an approved gate.
func TestCouncilToADecision(t *testing.T) {
	e := newCouncilEnv(t)
	must(t, e.tracker.SetFile("acme/app", councilCommit, "lease.go", []byte("package queue")))
	c := e.start(t, three)
	if c.RunID != c.ID || c.Status != domain.CouncilRunning || c.Number == 0 {
		t.Fatalf("started = %+v", c)
	}

	g := e.gate(t)
	stored := e.council(t, c.ID)
	if g.SubjectRepo != "acme/app" || !strings.Contains(g.Title, c.Ref()+": Which queue?") || stored.GateID != g.ID || stored.Brief == nil {
		t.Fatalf("gate = %+v, council = %+v", g, stored)
	}
	for _, want := range []string{"Recommendation: pg", "<untrusted-data", "devils-advocate@codex", "Polling will not scale", "How low?", "Load stays low"} {
		if !strings.Contains(g.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, g.Summary)
		}
	}
	e.decide(t, g, mcp.ToolApprove, "")

	c = e.waitCouncil(t, c.ID, domain.CouncilDecided)
	e.checkDecision(t, c, g)
	e.checkContributions(t, c)
	e.waitRunOf(t, c.RunID, domain.RunSucceeded) // the run ends after the steps that follow the decision
}

// checkDecision checks the signal and the recorded decision of an approved
// gate.
func (e *councilEnv) checkDecision(t *testing.T, c domain.Council, g domain.Gate) {
	t.Helper()
	sig := e.signal(t, c.ID)
	if sig.Status != domain.CouncilDecided || sig.DecisionID == uuid.Nil() {
		t.Fatalf("signal = %+v", sig)
	}
	d := e.decisions(t)
	if len(d) != 1 || d[0].ID != sig.DecisionID || d[0].DecidedBy != e.human.String() {
		t.Fatalf("decisions = %+v", d)
	}
	rec := d[0]
	if rec.Choice != "pg" || rec.CouncilID != c.ID || rec.GateID != g.ID || len(rec.Options) != 2 || !strings.Contains(rec.Reasons, "architect@claude-code") || !strings.Contains(rec.Dissent, "Polling") {
		t.Fatalf("decision = %+v", rec)
	}
}

// checkContributions checks what three members left after one revision round.
func (e *councilEnv) checkContributions(t *testing.T, c domain.Council) {
	t.Helper()
	for _, s := range []struct {
		stage domain.CouncilStage
		want  int
	}{{domain.StageProposal, 3}, {domain.StageCritique, 3}, {domain.StageRevision, 3}, {domain.StageSynthesis, 1}} {
		if got := e.contributions(t, c.ID, s.stage); len(got) != s.want {
			t.Errorf("%s: %d contributions, want %d", s.stage, len(got), s.want)
		}
	}
	if n := len(e.requests(councils.StepCheck)); n != 2 {
		t.Errorf("checks = %d, want 2: positions stopped moving in the second", n)
	}
	seen := map[string]bool{}
	for _, k := range e.contributions(t, c.ID, domain.StageProposal) {
		seen[k.Runtime] = true
		if k.EvidenceCheck == nil || len(k.EvidenceCheck) != 1 || k.EvidenceCheck[0].Status != domain.SourceOK {
			t.Errorf("evidence check of %s = %+v", k.Role, k.EvidenceCheck)
		}
	}
	if len(seen) < 2 {
		t.Errorf("runtimes of the proposals = %v, want two vendors", seen)
	}
	roles := []string{}
	for _, r := range e.requests(councils.StepProposal) {
		roles = append(roles, r.Role)
		if len(r.Council.Roles) != 2 || r.BaseRef != councilCommit || r.Feedback != "" || string(r.Council.Framing) == "" {
			t.Errorf("proposal request = %+v, %+v", r, r.Council)
		}
	}
	slices.Sort(roles)
	if !slices.Equal(roles, []string{"architect", "devils-advocate", "ux"}) {
		t.Errorf("members = %v: the devil's advocate must sit", roles)
	}
}

// TestCouncilStopsWhenPositionsSettle: no revision round when the first
// check says positions do not move.
func TestCouncilStopsWhenPositionsSettle(t *testing.T) {
	e := newCouncilEnv(t)
	e.invent = func(path string) any {
		if strings.HasSuffix(path, "/check") {
			return map[string]any{"positions_moving": false, "reason": "Settled.", "open_disagreements": []any{}}
		}
		return nil
	}
	c := e.start(t, three)
	e.decide(t, e.gate(t), mcp.ToolApprove, "")
	e.waitCouncil(t, c.ID, domain.CouncilDecided)
	if got := e.contributions(t, c.ID, domain.StageRevision); len(got) != 0 {
		t.Fatalf("revisions = %+v", got)
	}
}

// TestCouncilRevisesAtMostMaxRounds: positions that never settle stop at the
// pack's max_rounds.
func TestCouncilRevisesAtMostMaxRounds(t *testing.T) {
	e := newCouncilEnv(t)
	e.invent = func(path string) any {
		if strings.HasSuffix(path, "/check") {
			return map[string]any{"positions_moving": true, "reason": "Still moving.", "open_disagreements": []any{}}
		}
		return nil
	}
	c := e.start(t, councils.Request{Kind: domain.CouncilTestDesign, Question: "What to verify?", Roles: []string{"architect", "ux"}})
	e.decide(t, e.gate(t), mcp.ToolApprove, "")
	e.waitCouncil(t, c.ID, domain.CouncilDecided)
	rounds := map[int]int{}
	for _, k := range e.contributions(t, c.ID, domain.StageRevision) {
		rounds[k.Round]++
	}
	if len(rounds) != 1 || rounds[1] != 3 {
		t.Fatalf("revisions by round = %v, want one round (max_rounds 1)", rounds)
	}
}

func TestCouncilBudget(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.usage = domain.Usage{InputTokens: 300_000, OutputTokens: 1000}
		c := e.start(t, three)
		g := e.gate(t)
		if !strings.Contains(g.Summary, "budget ran out") {
			t.Errorf("summary = %s", g.Summary)
		}
		// One framing and three proposals spend 800k: the debate is skipped.
		if n := len(e.contributions(t, c.ID, domain.StageCritique)); n != 0 {
			t.Errorf("critiques = %d", n)
		}
		s := e.requests(councils.StepSynthesis)
		if len(s) != 1 || !s[0].Council.BudgetExhausted {
			t.Errorf("synthesis = %+v", s)
		}
		if b := e.council(t, c.ID).Brief; b == nil || !b.BudgetExhausted {
			t.Errorf("brief = %+v: it must note the budget even when the facilitator does not", b)
		}
		e.decide(t, g, mcp.ToolApprove, "")
		e.waitCouncil(t, c.ID, domain.CouncilDecided)
	})
	t.Run("time", func(t *testing.T) {
		e := newCouncilEnv(t)
		c := e.start(t, three)
		e.play(t, "the proposal jobs", func() bool { return len(e.requests(councils.StepProposal)) == 3 })
		e.clock.Advance(2 * time.Hour) // the members take too long
		g := e.gate(t)
		if n := len(e.requests(councils.StepCheck)); n != 0 {
			t.Errorf("checks = %d", n)
		}
		if b := e.council(t, c.ID).Brief; !b.BudgetExhausted {
			t.Errorf("brief = %+v", b)
		}
		e.decide(t, g, mcp.ToolReject, "")
	})
}

func TestCouncilPolicy(t *testing.T) {
	t.Run("accepts a reversible brief", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilTechnical}
		c := e.start(t, three)
		c = e.waitCouncil(t, c.ID, domain.CouncilDecided)
		if c.GateID != uuid.Nil() {
			t.Fatalf("council = %+v", c)
		}
		d := e.decisions(t)
		if len(d) != 1 || d[0].DecidedBy != domain.DecidedByPolicy || d[0].CouncilID != c.ID || e.signal(t, c.ID).DecisionID != d[0].ID {
			t.Fatalf("decisions = %+v", d)
		}
	})
	t.Run("never an irreversible one", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilTechnical}
		e.invent = func(path string) any {
			if strings.HasSuffix(path, "/synthesis") {
				return brief(false, false)
			}
			return nil
		}
		c := e.start(t, three)
		g := e.gate(t)
		if !strings.Contains(g.Summary, "hard to undo") {
			t.Errorf("summary = %s", g.Summary)
		}
		e.decide(t, g, mcp.ToolApprove, "")
		e.waitCouncil(t, c.ID, domain.CouncilDecided)
	})
	t.Run("nor one a member disputes", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilTechnical}
		e.invent = func(path string) any {
			if strings.HasSuffix(path, "/synthesis") {
				b := brief(true, false)
				b["dissent"] = []any{map[string]any{"member": "ux@codex", "position": "It will not do."}}
				return b
			}
			return nil
		}
		e.start(t, three)
		e.gate(t)
	})
	t.Run("nor another kind's", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilProduct}
		e.start(t, three)
		e.gate(t)
	})
}

// TestCouncilHumanRound: changes requested add a constraint, every member
// revises in the round after the last one, and a second brief goes to a new
// gate, even when the policy would accept it.
func TestCouncilHumanRound(t *testing.T) {
	e := newCouncilEnv(t)
	e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilTechnical}
	e.invent = func(path string) any {
		if strings.HasPrefix(path, "decision/1/") && strings.HasSuffix(path, "/synthesis") {
			return brief(false, false)
		}
		return nil
	}
	c := e.start(t, three)
	first := e.gate(t)
	e.decide(t, first, mcp.ToolRequestChanges, "Keep it on one server")
	var second domain.Gate
	e.play(t, "the second gate", func() bool {
		gs, err := e.gates.ListPending(t.Context(), e.account, e.project.ID)
		must(t, err)
		if len(gs) == 1 && gs[0].ID != first.ID {
			second = gs[0]
			return true
		}
		return false
	})
	stored := e.council(t, c.ID)
	if !slices.Contains(stored.Constraints, "Keep it on one server") || stored.GateID != second.ID {
		t.Fatalf("council = %+v", stored)
	}
	rounds := map[int]int{}
	for _, k := range e.contributions(t, c.ID, domain.StageRevision) {
		rounds[k.Round]++
	}
	if rounds[1] != 3 || rounds[2] != 3 {
		t.Fatalf("revisions by round = %v, want the human's as round 2", rounds)
	}
	if got := e.contributions(t, c.ID, domain.StageSynthesis); len(got) != 2 || got[1].Round != 2 {
		t.Fatalf("briefs = %+v", got)
	}
	_, err := e.gates.Decide(t.Context(), e.human, mcp.ToolRequestChanges, mcp.GateDecisionInput{Gate: second.Ref(), Feedback: "Once more"})
	if err == nil {
		t.Fatal("a second request for changes was accepted")
	}
	e.decide(t, second, mcp.ToolApprove, "")
	e.waitCouncil(t, c.ID, domain.CouncilDecided)
	// One decision per gate, both of the council; the second ends it.
	d := e.decisions(t)
	if len(d) != 2 || d[0].GateID != first.ID || d[0].Choice != string(domain.GateChangesRequested) || d[0].CouncilID != c.ID ||
		d[1].GateID != second.ID || d[1].Choice != "pg" || d[1].CouncilID != c.ID || e.signal(t, c.ID).DecisionID != d[1].ID {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestCouncilRejected(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.start(t, three)
	e.decide(t, e.gate(t), mcp.ToolReject, "Not this")
	e.waitCouncil(t, c.ID, domain.CouncilFailed)
	if s := e.signal(t, c.ID); s.Status != domain.CouncilFailed || s.DecisionID != uuid.Nil() {
		t.Fatalf("signal = %+v", s)
	}
	run := e.waitRunOf(t, c.RunID, domain.RunFailed)
	if !strings.Contains(run.Error, "rejected") {
		t.Fatalf("run = %+v", run)
	}
	if d := e.decisions(t); len(d) != 1 || d[0].Choice != string(domain.GateRejected) || d[0].CouncilID != c.ID {
		t.Fatalf("decisions = %+v", d)
	}
}

func (e *councilEnv) waitRunOf(t *testing.T, id uuid.UUID, status domain.RunStatus) domain.WorkflowRun {
	t.Helper()
	var run domain.WorkflowRun
	eventually(t, "run "+string(status), func() bool {
		var err error
		run, err = e.st.WorkflowRuns().Get(t.Context(), e.account, id)
		must(t, err)
		return run.Status == status
	})
	return run
}

// TestCouncilRerunsAnInvalidOutput: a member gets its problems as feedback
// once, and a second invalid output asks a human; rejecting that cancels the
// council.
func TestCouncilRerunsAnInvalidOutput(t *testing.T) {
	e := newCouncilEnv(t)
	bad := map[string]any{"priority": "simplicity"}
	var mu sync.Mutex
	tries := 0
	e.invent = func(path string) any {
		if !strings.Contains(path, "proposals/0/") {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		tries++
		return bad
	}
	c := e.start(t, three)
	var g domain.Gate
	e.play(t, "the needs_you gate", func() bool {
		gs, err := e.gates.ListPending(t.Context(), e.account, e.project.ID)
		must(t, err)
		if len(gs) > 0 {
			g = gs[0]
		}
		return len(gs) > 0
	})
	if g.Kind != domain.GateNeedsYou || tries != 2 {
		t.Fatalf("gate = %+v after %d tries", g, tries)
	}
	var rerun workflow.JobRequest
	for _, r := range e.requests(councils.StepProposal) {
		if r.Feedback != "" {
			rerun = r
		}
	}
	if !strings.Contains(rerun.Feedback, "<untrusted-data") || !strings.Contains(rerun.Feedback, "Your output was refused") {
		t.Fatalf("feedback = %q", rerun.Feedback)
	}
	e.decide(t, g, mcp.ToolReject, "")
	e.waitCouncil(t, c.ID, domain.CouncilCanceled)
	if s := e.signal(t, c.ID); s.Status != domain.CouncilCanceled {
		t.Fatalf("signal = %+v", s)
	}
}

// TestCouncilSurvivesACrash kills the engine after the side effect of each
// kind of step, before it is recorded, and at the end checks that nothing
// happened twice.
func TestCouncilSurvivesACrash(t *testing.T) {
	for _, step := range []string{
		"framing/1/frame", "proposals/1/proposal_try/1/proposal", "proposals/1/proposal_try/1/proposed", "proposals/1/proposal_evidence",
		"critiques/1/critique_try/1/critique", "critiques/2/critique_try/1/critiqued", "debate/1/checks/0/check", "debate/1/checks/0/checked",
		"debate/1/revisions/2/revision_try/1/revised", "decision/1/synthesis_try/1/synthesis", "decision/1/synthesis_try/1/synthesized",
		"decision/1/gated/0/open_gate",
	} {
		t.Run(step, func(t *testing.T) {
			e := newCouncilEnv(t)
			cs := testkit.NewCrashStore(e.st, step)
			e.run(t, cs)
			c := e.start(t, three)
			e.play(t, "the crash", func() bool {
				select {
				case <-cs.Crashed():
					return true
				default:
					return false
				}
			})
			e.stop()
			e.run(t)
			g := e.gate(t)
			e.decide(t, g, mcp.ToolApprove, "")
			e.waitCouncil(t, c.ID, domain.CouncilDecided)

			e.checkNothingTwice(t, c)
		})
	}
}

// checkNothingTwice checks a council of three members with one revision
// round: every side effect happened once.
func (e *councilEnv) checkNothingTwice(t *testing.T, c domain.Council) {
	t.Helper()
	if n := len(e.jobs.Requests()); n != 1+3+3+2+3+1 { // frame, proposals, critiques, checks, revisions, synthesis
		t.Errorf("jobs = %d", n)
	}
	for _, s := range []domain.CouncilStage{domain.StageProposal, domain.StageCritique, domain.StageRevision} {
		if n := len(e.contributions(t, c.ID, s)); n != 3 {
			t.Errorf("%s: %d contributions", s, n)
		}
	}
	if d := e.decisions(t); len(d) != 1 {
		t.Errorf("decisions = %+v", d)
	}
	gs, err := e.st.Gates().List(t.Context(), e.account, store.GateFilter{ProjectID: e.project.ID})
	must(t, err)
	if len(gs) != 1 {
		t.Errorf("gates = %+v", gs)
	}
}

// TestCouncilDecideCrash: the decision and its signal survive a crash right
// after them.
func TestCouncilDecideCrash(t *testing.T) {
	e := newCouncilEnv(t)
	cs := testkit.NewCrashStore(e.st, "decision/1/decide")
	e.run(t, cs)
	c := e.start(t, three)
	e.decide(t, e.gate(t), mcp.ToolApprove, "")
	<-cs.Crashed()
	e.stop()
	e.run(t)
	e.waitRunOf(t, c.RunID, domain.RunSucceeded)
	if d := e.decisions(t); len(d) != 1 {
		t.Fatalf("decisions = %+v", d)
	}
	if s := e.signal(t, c.ID); s.DecisionID != e.decisions(t)[0].ID {
		t.Fatalf("signal = %+v", s)
	}
}

const parentPlaybook = `
inputs: [question]
steps:
  - id: ask
    type: action
    action: council.run
    with: {kind: technical, question: '${inputs.question}', constraints: [Stay on Postgres], roles: [architect, ux]}
  - id: answer
    type: wait
    with: {signal: council.decided, council_id: '${steps.ask.output.council_id}'}
`

// TestCouncilAsASubRun: a playbook starts a council and waits for it, also
// across a crash right after council.run and one at the wait.
func TestCouncilAsASubRun(t *testing.T) {
	for _, step := range []string{"ask", "answer"} {
		t.Run(step, func(t *testing.T) {
			e := newCouncilEnv(t)
			p, err := workflow.ParsePlaybook("parent", []byte(parentPlaybook))
			must(t, err)
			e.books["parent"] = p
			cs := testkit.NewCrashStore(e.st, step)
			e.run(t, cs)
			goal := domain.Goal{AccountID: e.account, ProjectID: e.project.ID, Title: "Queue", Status: domain.GoalDraft, CreatedBy: "user:ana"}
			must(t, e.st.Goals().Create(t.Context(), &goal))
			parent, err := e.engine.Start(t.Context(), "parent", workflow.StartRequest{AccountID: e.account, ProjectID: e.project.ID, GoalID: goal.ID, Inputs: map[string]any{"question": "Which queue?"}})
			must(t, err)
			<-cs.Crashed()
			e.stop()
			e.run(t)

			g := e.gate(t)
			e.decide(t, g, mcp.ToolApprove, "")
			e.waitRunOf(t, parent.ID, domain.RunSucceeded)
			all, err := e.st.Councils().List(t.Context(), e.account, store.CouncilFilter{ProjectID: e.project.ID})
			must(t, err)
			if len(all) != 1 || all[0].GoalID != goal.ID || all[0].Status != domain.CouncilDecided || all[0].Constraints[0] != "Stay on Postgres" {
				t.Fatalf("councils = %+v", all)
			}
			child, err := e.st.WorkflowRuns().Get(t.Context(), e.account, all[0].RunID)
			must(t, err)
			if child.GoalID != goal.ID {
				t.Fatalf("child run = %+v", child)
			}
			if n := len(e.requests(councils.StepFrame)); n != 1 {
				t.Fatalf("frame jobs = %d", n)
			}
		})
	}
}

// TestCanceledGoalStopsItsCouncil: goal.settle cancels the council, whose run
// ends after the stage under way and starts no more jobs.
func TestCanceledGoalStopsItsCouncil(t *testing.T) {
	e := newCouncilEnv(t)
	goal := domain.Goal{AccountID: e.account, ProjectID: e.project.ID, Title: "Queue", Status: domain.GoalDraft, CreatedBy: "user:ana"}
	must(t, e.st.Goals().Create(t.Context(), &goal))
	r := three
	r.GoalID = goal.ID
	c := e.start(t, r)
	eventually(t, "the proposals", func() bool {
		e.answerOne(t)
		return len(e.requests(councils.StepProposal)) == 3
	})
	_, err := e.call(t, "goal.settle", "", map[string]any{"goal_id": goal.ID.String()})
	must(t, err)
	if s := e.signal(t, c.ID); s.Status != domain.CouncilCanceled {
		t.Fatalf("signal = %+v", s)
	}
	// The members in flight finish their turn, and the run stops there.
	e.play(t, "the run to end", func() bool {
		run, err := e.st.WorkflowRuns().Get(t.Context(), e.account, c.RunID)
		must(t, err)
		return run.Status == domain.RunFailed
	})
	if got := e.council(t, c.ID); got.Status != domain.CouncilCanceled {
		t.Fatalf("status = %s", got.Status)
	}
	if n := len(e.requests(councils.StepCritique)); n != 0 {
		t.Fatalf("critiques = %d", n)
	}
}

func TestCouncilSubRunThatFails(t *testing.T) {
	e := newCouncilEnv(t)
	p, err := workflow.ParsePlaybook("parent", []byte(parentPlaybook))
	must(t, err)
	e.books["parent"] = p
	e.run(t)
	parent, err := e.engine.Start(t.Context(), "parent", workflow.StartRequest{AccountID: e.account, ProjectID: e.project.ID, Inputs: map[string]any{"question": "Which queue?"}})
	must(t, err)
	e.decide(t, e.gate(t), mcp.ToolReject, "")
	if run := e.waitRunOf(t, parent.ID, domain.RunFailed); !strings.Contains(run.Error, "council failed") {
		t.Fatalf("parent = %+v", run)
	}
}
