package advisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

type fixtureEnv struct {
	*Service
	clock   *testkit.Clock
	tracker *testkit.Tracker
	runs    *testkit.Playbooks
	project domain.Project
	human   domain.Actor
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixtureEnv {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	bus := testkit.NewSignalBus(st)
	g := testkit.NewGates(st, bus, clock.Now)
	tracker := testkit.NewTracker(clock.Now)
	tracker.Install("acme/app")
	must(t, tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	catalog, err := LoadCatalog(forecrew.Files)
	must(t, err)
	runs := &testkit.Playbooks{}
	svc := New(Config{Store: st, Gates: g, GitHub: tracker, Runs: runs, Catalog: catalog, Now: clock.Now, GoalPlaybook: func(domain.Project) string { return "goal" }})
	g.WithRecorder(gates.Recorders{svc})
	return &fixtureEnv{Service: svc, clock: clock, tracker: tracker, runs: runs, project: p,
		human: domain.Actor{AccountID: acc.ID, Kind: domain.ActorUser, Name: "ana"}}
}

func toolSignal(tool string, weight float64) Signal {
	return Signal{Detector: DetectorTooling, Key: "tooling:" + tool, Category: domain.CategoryTooling, Subject: tool, Weight: weight, Summary: "lacking " + tool,
		Evidence: []domain.ProposalEvidence{{Kind: "ask_human", Ref: "GATE-1", Detail: tool + ": command not found"}}}
}

func setupDraft(key string, tools ...string) Draft {
	return Draft{DedupeKey: key, Signals: []string{key}, Scope: domain.ScopeProject, Category: domain.CategoryTooling, Title: "Add it", Rationale: "Agents lack it",
		Benefit: "Fewer questions", Effort: domain.EffortS, Risk: domain.LevelLow, Tools: tools, Measure: DetectorTooling,
		Action: domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "Add the tool to the toolbox"}}}
}

func (f *fixtureEnv) proposal(t *testing.T, ref string) domain.Proposal {
	t.Helper()
	n, _, err := domain.ParseRefOrID(domain.RefProposal, ref)
	must(t, err)
	p, err := f.Store.Proposals().GetByNumber(t.Context(), f.project.AccountID, n)
	must(t, err)
	return p
}

// file files drafts for signals and fails on an error.
func (f *fixtureEnv) file(t *testing.T, key string, signals []Signal, drafts ...Draft) Filed {
	t.Helper()
	out, err := f.File(t.Context(), f.project, key, signals, drafts)
	must(t, err)
	return out
}

func TestFileOpensGateAndRun(t *testing.T) {
	f := newFixture(t)
	sig := toolSignal("trivy", 2)
	got := f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy"))
	if len(got.Created) != 1 || len(got.Dropped) != 0 {
		t.Fatalf("filed %+v", got)
	}
	p := f.proposal(t, got.Created[0])
	if p.Status != domain.ProposalProposed || p.EvidenceWeight != 2 || len(p.Evidence) != 1 || p.Measure == nil || p.Measure.Value != 2 {
		t.Fatalf("proposal %+v", p)
	}
	g, err := f.Gates.Get(t.Context(), p.AccountID, p.GateID)
	must(t, err)
	if g.Kind != domain.GateImprovementProposal || g.Status != domain.GatePending || !strings.Contains(g.Summary, "<untrusted-data") || !strings.Contains(g.Summary, "Fewer questions") {
		t.Fatalf("gate %+v", g)
	}
	starts := f.runs.Starts()
	if len(starts) != 1 || starts[0].Playbook != PlaybookProposal || starts[0].Request.ID != p.ID {
		t.Fatalf("starts %+v", starts)
	}
	// The engine calls an action again after a crash: nothing new is made.
	again := f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy"))
	all, err := f.Store.Proposals().List(t.Context(), p.AccountID, store.ProposalFilter{})
	must(t, err)
	if len(all) != 1 || len(again.Created) != 1 || again.Created[0] != p.Ref() {
		t.Fatalf("repeated: %+v, %d proposals", again, len(all))
	}
}

func TestFileWeeklyCap(t *testing.T) {
	f := newFixture(t)
	var signals []Signal
	var drafts []Draft
	for _, tool := range []string{"trivy", "semgrep", "gitleaks", "syft"} {
		s := toolSignal(tool, 2)
		signals, drafts = append(signals, s), append(drafts, setupDraft(s.Key, tool))
	}
	got := f.file(t, "k1", signals, drafts...)
	if len(got.Created) != 3 || len(got.Dropped) != 1 || !strings.Contains(got.Dropped[0], "weekly cap") {
		t.Fatalf("filed %+v", got)
	}
	// Repeating the call and a later run in the same week add nothing.
	if again := f.file(t, "k1", signals, drafts...); len(again.Created) != 3 {
		t.Fatalf("repeated: %+v", again)
	}
	if later := f.file(t, "k2", signals, drafts[3]); len(later.Created) != 0 {
		t.Fatalf("a later run: %+v", later)
	}
	f.clock.Advance(capWindow + time.Hour)
	if next := f.file(t, "k3", signals, drafts[3]); len(next.Created) != 1 {
		t.Fatalf("next week: %+v", next)
	}
}

func TestFileDropsWhatBreaksARule(t *testing.T) {
	sig := toolSignal("trivy", 2)
	other := toolSignal("nomad", 2)
	withAction := func(d Draft, a domain.ProposalAction) Draft { d.Action = a; return d }
	bad := func(d Draft, change func(*Draft)) Draft { change(&d); return d }
	config := func(path, content string) domain.ProposalAction {
		return domain.ProposalAction{ConfigPR: &domain.ConfigPRAction{Files: []domain.ConfigFile{{Path: path, Content: content}}}}
	}
	base := setupDraft(sig.Key, "trivy")
	cases := map[string]struct {
		draft Draft
		want  string
	}{
		"no signal":      {setupDraft("tooling:unknown", "trivy"), "no listed signal"},
		"unknown cited":  {bad(base, func(d *Draft) { d.Signals = []string{"tooling:gone"} }), "not a listed signal"},
		"outside":        {setupDraft(sig.Key, "kubescape"), "kubescape is not in the catalog"},
		"no catalog":     {setupDraft(sig.Key), "no tool of the catalog"},
		"in the action":  {withAction(setupDraft(other.Key, "trivy"), domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "Install nomad on the workers"}}), "nomad, which is not in the catalog"},
		"two actions":    {withAction(base, domain.ProposalAction{Setup: base.Action.Setup, Goal: &domain.GoalAction{Title: "x"}}), "exactly one"},
		"platform":       {bad(base, func(d *Draft) { d.Scope = domain.ScopePlatform }), "text of an issue"},
		"outside config": {withAction(base, config("deploy/x.yaml", "a: 1")), "under .forecrew/"},
		"no contract":    {withAction(base, config(".forecrew/notes.yaml", "a: 1")), "has no contract"},
		"bad contract":   {withAction(base, config(".forecrew/matrix.yaml", "axes: 3")), "matrix"},
		"too long":       {bad(base, func(d *Draft) { d.Title = strings.Repeat("x", maxTitle+1) }), "too long"},
		"too large":      {withAction(base, config(".forecrew/profile.yaml", strings.Repeat("x", maxConfigFile+1))), "too large"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			got := f.file(t, "k", []Signal{sig, other}, c.draft)
			if len(got.Created) != 0 || len(got.Dropped) != 1 || !strings.Contains(got.Dropped[0], c.want) {
				t.Fatalf("filed %+v, want a drop with %q", got, c.want)
			}
		})
	}
}

// A tool the catalog has may be named; one it lacks may stand in the
// rationale, as worth researching, and not in the action.
func TestFileKeepsResearchInTheRationale(t *testing.T) {
	f := newFixture(t)
	sig := toolSignal("nomad", 2)
	d := setupDraft(sig.Key, "trivy")
	d.Rationale = "nomad is worth researching."
	if got := f.file(t, "k", []Signal{sig}, d); len(got.Created) != 1 {
		t.Fatalf("filed %+v", got)
	}
}

func TestFileUpdatesAnOpenProposal(t *testing.T) {
	f := newFixture(t)
	sig := toolSignal("trivy", 2)
	first := f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy"))
	sig.Weight, sig.Evidence = 5, append(sig.Evidence, domain.ProposalEvidence{Kind: "ask_human", Ref: "GATE-9", Detail: "again"})
	got := f.file(t, "k2", []Signal{sig}, setupDraft(sig.Key, "trivy"))
	if len(got.Created) != 0 || len(got.Updated) != 1 || got.Updated[0] != first.Created[0] {
		t.Fatalf("filed %+v", got)
	}
	if p := f.proposal(t, first.Created[0]); p.EvidenceWeight != 5 || len(p.Evidence) != 2 || p.Measure.Value != 2 {
		t.Fatalf("proposal %+v", p)
	}
}

func (f *fixtureEnv) reject(t *testing.T, p domain.Proposal) {
	t.Helper()
	_, err := f.Gates.Resolve(t.Context(), f.human, p.GateID, domain.GateRejected, "not now")
	must(t, err)
}

func TestRejectedComesBackOnlyWithDoubledEvidenceOrAfter90Days(t *testing.T) {
	f := newFixture(t)
	sig := toolSignal("trivy", 2)
	p := f.proposal(t, f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy")).Created[0])
	f.reject(t, p)
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalRejected || p.Reason != "not now" || p.DecidedBy == "" {
		t.Fatalf("rejected: %+v", p)
	}
	f.clock.Advance(capWindow + time.Hour)
	sig.Weight = 3.9
	if got := f.file(t, "k2", []Signal{sig}, setupDraft(sig.Key, "trivy")); len(got.Created) != 0 || !strings.Contains(got.Dropped[0], "holds it back") {
		t.Fatalf("below double: %+v", got)
	}
	sig.Weight = 4
	got := f.file(t, "k3", []Signal{sig}, setupDraft(sig.Key, "trivy"))
	if len(got.Created) != 1 {
		t.Fatalf("doubled: %+v", got)
	}
	f.reject(t, f.proposal(t, got.Created[0]))
	f.clock.Advance(rejectedWindow - time.Hour)
	if got := f.file(t, "k4", []Signal{sig}, setupDraft(sig.Key, "trivy")); len(got.Created) != 0 {
		t.Fatalf("within 90 days: %+v", got)
	}
	f.clock.Advance(2 * time.Hour)
	if got := f.file(t, "k5", []Signal{sig}, setupDraft(sig.Key, "trivy")); len(got.Created) != 1 {
		t.Fatalf("after 90 days: %+v", got)
	}
}

func TestDetectorsOfACollectedProject(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	must(t, f.Store.Projects().SetProfile(ctx, f.project.AccountID, f.project.ID, []byte(`{"version":1,"name":"app","summary":"s","platforms":["linux"],
		"components":[{"path":".","stack":"go"}],"ci":{"workflows":[".github/workflows/ci.yml"],"runners":["ubuntu-latest"]}}`)))
	must(t, f.tracker.SetFile("acme/app", "main", ".github/workflows/ci.yml", []byte("steps:\n  - run: gitleaks detect\n")))
	must(t, f.tracker.SetFile("acme/app", "main", ".forecrew/matrix.yaml", []byte("axes:\n  os: [linux, windows]\n")))
	e := domain.KnowledgeEntry{AccountID: f.project.AccountID, ProjectID: f.project.ID, Provider: "repo", Path: generatedDep, Section: domain.SectionGenerated, Generated: true,
		Title: "Direct dependencies", Body: "## .\n\n- github.com/jackc/pgx/v5 v5 (go)\n", Version: "v1", Provenance: domain.Provenance{AuthorRole: "forecrewd", Source: "x", Created: t0}}
	must(t, f.Store.Knowledge().Upsert(ctx, &e))
	project, err := f.Store.Projects().Get(ctx, f.project.AccountID, f.project.ID)
	must(t, err)
	brief, err := f.Prepare(ctx, project)
	must(t, err)
	keys := map[string]bool{}
	for _, s := range brief.Signals {
		keys[s.Key] = true
	}
	for _, want := range []string{"security:dependency-scanning", "ci:platform:windows", "observability:."} {
		if !keys[want] {
			t.Errorf("no signal %s in %v", want, keys)
		}
	}
	if keys["security:secret-scanning"] {
		t.Errorf("gitleaks is in CI: %v", keys)
	}
	if brief.Room != 3 || !strings.Contains(brief.Text, "at most 3") || !strings.Contains(brief.Text, "gitleaks (cli, MIT)") || !strings.Contains(brief.Text, "<untrusted-data") {
		t.Errorf("brief %d:\n%s", brief.Room, brief.Text)
	}
}

// A repo that cannot be read costs only what comes from it.
func TestCollectSurvivesAnUnreadableRepo(t *testing.T) {
	f := newFixture(t)
	f.GitHub = failingTracker{f.tracker}
	d, err := f.Collect(t.Context(), f.project)
	must(t, err)
	if d.CIFiles != nil {
		t.Fatalf("CI files %v", d.CIFiles)
	}
}

type failingTracker struct{ github.Tracker }

func (failingTracker) GetFile(context.Context, string, string, string) ([]byte, error) {
	return nil, errors.New("boom")
}

func TestSnoozeEndsAndOutcomeIsMeasured(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	sig := toolSignal("trivy", 2)
	p := f.proposal(t, f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy")).Created[0])
	must(t, p.Decide(f.human, domain.ProposalSnooze, "", t0.Add(24*time.Hour), t0))
	must(t, f.Store.Proposals().Update(ctx, &p, domain.ProposalProposed))
	if _, err := f.Prepare(ctx, f.project); err != nil {
		t.Fatal(err)
	}
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalSnoozed {
		t.Fatalf("too early: %+v", p)
	}
	f.clock.Advance(25 * time.Hour)
	_, err := f.Prepare(ctx, f.project)
	must(t, err)
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalProposed || !p.SnoozedUntil.IsZero() {
		t.Fatalf("after the date: %+v", p)
	}
	_, err = f.Gates.Resolve(ctx, f.human, p.GateID, domain.GateApproved, "")
	must(t, err)
	must(t, f.PREnded(ctx, p.AccountID, p.ID, domain.SignalPRMerged))
	p = f.proposal(t, p.Ref())
	if p.Status != domain.ProposalDone {
		t.Fatalf("done: %+v", p)
	}
	f.clock.Advance(MeasureAfter - time.Hour)
	_, err = f.Prepare(ctx, f.project)
	must(t, err)
	if p = f.proposal(t, p.Ref()); p.Outcome != nil {
		t.Fatalf("measured too early: %+v", p.Outcome)
	}
	f.clock.Advance(2 * time.Hour)
	_, err = f.Prepare(ctx, f.project)
	must(t, err)
	p = f.proposal(t, p.Ref())
	if p.Outcome == nil || p.Outcome.Before != 2 || p.Outcome.After != 0 || p.Outcome.MeasuredAt.IsZero() {
		t.Fatalf("outcome %+v", p.Outcome)
	}
	text, err := f.RetroText(ctx, p.AccountID, p.ProjectID)
	must(t, err)
	if !strings.Contains(text, p.Ref()) || !strings.Contains(text, "2.0 before, 0.0 after") || !strings.Contains(text, "<untrusted-data") {
		t.Fatalf("retro:\n%s", text)
	}
	f.clock.Advance(retroWindow + time.Hour)
	if text, _ = f.RetroText(ctx, p.AccountID, p.ProjectID); text != "" {
		t.Fatalf("an old outcome: %s", text)
	}
}

func accepted(t *testing.T, f *fixtureEnv, a domain.ProposalAction) domain.Proposal {
	t.Helper()
	sig := toolSignal("trivy", 2)
	d := setupDraft(sig.Key, "trivy")
	d.Action = a
	got := f.file(t, "k1", []Signal{sig}, d)
	if len(got.Created) != 1 {
		t.Fatalf("filed %+v", got)
	}
	p := f.proposal(t, got.Created[0])
	_, err := f.Gates.Resolve(t.Context(), f.human, p.GateID, domain.GateApproved, "")
	must(t, err)
	return f.proposal(t, p.Ref())
}

func TestAcceptGoal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := accepted(t, f, domain.ProposalAction{Goal: &domain.GoalAction{Title: "Scan images", Body: "Add a scan", Platforms: []string{"Linux"}}})
	if p.Status != domain.ProposalAccepted {
		t.Fatalf("proposal %+v", p)
	}
	out, err := f.Carry(ctx, p.AccountID, p.ID)
	must(t, err)
	again, err := f.Carry(ctx, p.AccountID, p.ID) // after a crash
	must(t, err)
	p = f.proposal(t, p.Ref())
	g, err := f.Store.Goals().Get(ctx, p.AccountID, p.GoalID)
	must(t, err)
	if out.GoalRef != g.Ref() || again.GoalRef != g.Ref() || g.Kind != domain.GoalFeature || g.Title != "Scan images" || !strings.Contains(g.Body, "<untrusted-data") || g.CreatedBy != p.DecidedBy || g.Platforms[0] != "linux" {
		t.Fatalf("goal %+v, out %+v", g, out)
	}
	starts := f.runs.Starts()
	if last := starts[len(starts)-1]; last.Playbook != "goal" || last.Request.ID != g.ID || last.Request.GoalID != g.ID || last.Request.Inputs["goal_id"] != g.ID.String() {
		t.Fatalf("start %+v", last)
	}
	g.Status = domain.GoalDone
	must(t, f.GoalEnded(ctx, g))
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalDone {
		t.Fatalf("goal done: %+v", p)
	}
}

func TestCanceledGoalSupersedes(t *testing.T) {
	f := newFixture(t)
	p := accepted(t, f, domain.ProposalAction{Goal: &domain.GoalAction{Title: "Scan"}})
	_, err := f.Carry(t.Context(), p.AccountID, p.ID)
	must(t, err)
	p = f.proposal(t, p.Ref())
	must(t, f.GoalEnded(t.Context(), domain.Goal{ID: p.GoalID, AccountID: p.AccountID, Status: domain.GoalCanceled}))
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalSuperseded || !strings.Contains(p.Reason, "canceled") {
		t.Fatalf("proposal %+v", p)
	}
}

func TestAcceptConfigPR(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	profile := "version: 1\nname: app\nsummary: An app.\n"
	p := accepted(t, f, domain.ProposalAction{ConfigPR: &domain.ConfigPRAction{Files: []domain.ConfigFile{{Path: ".forecrew/profile.yaml", Content: profile}}}})
	out, err := f.Carry(ctx, p.AccountID, p.ID)
	must(t, err)
	if len(out.IfPR) != 1 {
		t.Fatalf("carried %+v", out)
	}
	again, err := f.Carry(ctx, p.AccountID, p.ID)
	must(t, err)
	if again.IfPR[0] != out.IfPR[0] {
		t.Fatalf("a second PR: %+v", again)
	}
	pr, err := f.tracker.GetPR(ctx, "acme/app", out.IfPR[0])
	must(t, err)
	if pr.Head != "agent/prop-"+strings.TrimPrefix(p.Ref(), "PROP-") || !strings.Contains(pr.Body, "A human merges") {
		t.Fatalf("PR %+v", pr)
	}
	data, err := f.tracker.GetFile(ctx, "acme/app", pr.Head, ".forecrew/profile.yaml")
	must(t, err)
	if string(data) != profile {
		t.Fatalf("file %q", data)
	}
	must(t, f.PREnded(ctx, p.AccountID, p.ID, domain.SignalPRClosed))
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalSuperseded {
		t.Fatalf("closed: %+v", p)
	}
}

func TestAcceptSetupWaitsForTheHuman(t *testing.T) {
	f := newFixture(t)
	p := accepted(t, f, domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "Add a worker"}})
	out, err := f.Carry(t.Context(), p.AccountID, p.ID)
	must(t, err)
	if p = f.proposal(t, p.Ref()); len(out.IfPR) != 0 || out.GoalRef != "" || p.Status != domain.ProposalAccepted {
		t.Fatalf("carried %+v, %+v", out, p)
	}
}

func TestCarryIgnoresWhatIsNotAccepted(t *testing.T) {
	f := newFixture(t)
	sig := toolSignal("trivy", 2)
	p := f.proposal(t, f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy")).Created[0])
	f.reject(t, p)
	if out, err := f.Carry(t.Context(), p.AccountID, p.ID); err != nil || len(out.IfPR) != 0 {
		t.Fatalf("carried %+v, %v", out, err)
	}
}

func TestRevisionRound(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	sig := toolSignal("trivy", 2)
	p := f.proposal(t, f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy")).Created[0])
	first := p.GateID
	if gate, err := f.Round(ctx, p.AccountID, p.ID); err != nil || gate != first {
		t.Fatalf("round %v, %v", gate, err)
	}
	_, err := f.Gates.Resolve(ctx, f.human, first, domain.GateChangesRequested, "Only for images")
	must(t, err)
	if p = f.proposal(t, p.Ref()); p.Status != domain.ProposalProposed {
		t.Fatalf("after feedback: %+v", p)
	}
	a, err := f.Assess(ctx, p.AccountID, p.ID)
	must(t, err)
	if a.Settled || len(a.IfRevise) != 1 || !strings.Contains(a.IfRevise[0], "Only for images") || strings.Count(a.IfRevise[0], "<untrusted-data") != 2 {
		t.Fatalf("assessment %+v", a)
	}
	d := setupDraft(sig.Key, "trivy")
	d.Title = "Scan images only"
	must(t, f.Reopen(ctx, "r1", p.AccountID, p.ID, &d))
	must(t, f.Reopen(ctx, "r1", p.AccountID, p.ID, &d)) // after a crash
	p = f.proposal(t, p.Ref())
	g, err := f.Gates.Get(ctx, p.AccountID, p.GateID)
	must(t, err)
	if p.Title != "Scan images only" || p.GateID == first || g.Status != domain.GatePending || !strings.Contains(g.Summary, "Revised") {
		t.Fatalf("revised %+v, gate %+v", p, g)
	}
	// A revision that breaks a rule leaves the proposal as it stood.
	_, err = f.Gates.Resolve(ctx, f.human, g.ID, domain.GateChangesRequested, "More")
	must(t, err)
	d.Tools = []string{"kubescape"}
	must(t, f.Reopen(ctx, "r2", p.AccountID, p.ID, &d))
	p = f.proposal(t, p.Ref())
	if g, err = f.Gates.Get(ctx, p.AccountID, p.GateID); err != nil || !strings.Contains(g.Summary, "refused") || p.Title != "Scan images only" {
		t.Fatalf("refused: %+v, %v", g, err)
	}
	_, err = f.Gates.Resolve(ctx, f.human, g.ID, domain.GateChangesRequested, "More")
	must(t, err)
	must(t, f.Reopen(ctx, "r3", p.AccountID, p.ID, nil))
	p = f.proposal(t, p.Ref())
	if g, _ = f.Gates.Get(ctx, p.AccountID, p.GateID); !strings.Contains(g.Summary, "no revision") {
		t.Fatalf("no revision: %+v", g)
	}
	_, err = f.Gates.Resolve(ctx, f.human, g.ID, domain.GateApproved, "")
	must(t, err)
	if a, err = f.Assess(ctx, p.AccountID, p.ID); err != nil || !a.Settled || len(a.IfRevise) != 0 {
		t.Fatalf("settled: %+v, %v", a, err)
	}
}

func TestAbandonSupersedesAndClosesTheGate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	sig := toolSignal("trivy", 2)
	p := f.proposal(t, f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy")).Created[0])
	must(t, f.Abandon(ctx, p.AccountID, p.ID))
	must(t, f.Abandon(ctx, p.AccountID, p.ID))
	p = f.proposal(t, p.Ref())
	g, err := f.Gates.Get(ctx, p.AccountID, p.GateID)
	must(t, err)
	if p.Status != domain.ProposalSuperseded || g.Status != domain.GateRejected {
		t.Fatalf("proposal %+v, gate %+v", p, g)
	}
}

func TestStartAfterGoal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	run := domain.WorkflowRun{AccountID: f.project.AccountID, ProjectID: f.project.ID}
	goal := uuid.New()
	started, err := f.StartAfterGoal(ctx, run, goal)
	must(t, err)
	starts := f.runs.Starts()
	if !started || len(starts) != 1 || starts[0].Playbook != PlaybookAdvise || starts[0].Request.Inputs["repo"] != "acme/app" {
		t.Fatalf("started %v, %+v", started, starts)
	}
	active := domain.WorkflowRun{AccountID: run.AccountID, ProjectID: run.ProjectID, Playbook: PlaybookAdvise, Status: domain.RunRunning, Inputs: []byte(`{}`)}
	must(t, f.Store.WorkflowRuns().Create(ctx, &active))
	if started, err = f.StartAfterGoal(ctx, run, goal); err != nil || started {
		t.Fatalf("with an active run: %v, %v", started, err)
	}
}

func TestLoadCatalog(t *testing.T) {
	c, err := LoadCatalog(forecrew.Files)
	must(t, err)
	if len(c) < 12 || !c.Has("gitleaks") || c.Has("nomad") || c["gitleaks"].License != "MIT" || c["gitleaks"].Homepage == "" {
		t.Fatalf("catalog %+v", c)
	}
}
