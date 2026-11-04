package playbooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/workflow"
)

// call runs an action of the env for a run of its project.
func (e *councilEnv) call(t *testing.T, action, key string, with map[string]any) (any, error) {
	t.Helper()
	if key == "" {
		key = domain.NewID().String()
	}
	run := domain.WorkflowRun{ID: domain.NewID(), AccountID: e.account, ProjectID: e.project.ID, Status: domain.RunRunning}
	return e.actions[action](t.Context(), workflow.ActionCall{Key: key, Run: run, With: with})
}

// stored adds a council directly, without a run.
func (e *councilEnv) stored(t *testing.T) domain.Council {
	t.Helper()
	c := domain.Council{AccountID: e.account, ProjectID: e.project.ID, Kind: domain.CouncilTechnical, Question: "Which queue?"}
	must(t, e.st.Councils().Create(t.Context(), &c))
	return c
}

func (e *councilEnv) job(t *testing.T, role, runtime string) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: e.account, ProjectID: e.project.ID, Role: role, Runtime: runtime}
	must(t, e.st.Jobs().Create(t.Context(), &j))
	return j
}

// cited stores a proposal citing sources and returns it.
func (e *councilEnv) cited(t *testing.T, c domain.Council, role string, sources ...map[string]any) domain.CouncilContribution {
	t.Helper()
	items := []any{}
	for _, s := range sources {
		s["claim"], s["confidence"] = "A claim.", "high"
		if _, ok := s["date"]; !ok {
			s["date"] = nil
		}
		items = append(items, s)
	}
	content, err := json.Marshal(map[string]any{"evidence": items})
	must(t, err)
	k := domain.CouncilContribution{AccountID: e.account, CouncilID: c.ID, Role: role, Runtime: "codex", Stage: domain.StageProposal, Round: 1, JobID: e.job(t, role, "codex").ID, Content: content}
	must(t, e.st.Councils().AddContribution(t.Context(), &k))
	return k
}

func (e *councilEnv) check(t *testing.T, c domain.Council, k domain.CouncilContribution) []domain.SourceCheck {
	t.Helper()
	_, err := e.call(t, "council.check_evidence", "", map[string]any{"council_id": c.ID.String(), "contribution_id": k.ID.String(), "commit": councilCommit})
	must(t, err)
	all := e.contributions(t, c.ID, "")
	for _, x := range all {
		if x.ID == k.ID {
			return x.EvidenceCheck
		}
	}
	t.Fatalf("contribution %s is gone", k.ID)
	return nil
}

// web is a fake internet: a public host with fixed pages.
func (e *councilEnv) web(pages map[string]string) *int {
	hits := 0
	e.fetcher.Lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "inner.example" {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	e.fetcher.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		hits++
		body, ok := pages[r.URL.String()]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		}
		h := http.Header{"Content-Type": {"text/html; charset=utf-8"}}
		if after, ok0 := strings.CutPrefix(body, "redirect:"); ok0 {
			h.Set("Location", after)
			return &http.Response{StatusCode: http.StatusFound, Body: http.NoBody, Header: h, Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: h, Request: r}, nil
	})
	return &hits
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func url(source string) map[string]any {
	return map[string]any{"kind": "url", "source": source, "date": "2026-01-02"}
}

func TestCheckEvidencePathsAndJobs(t *testing.T) {
	e := newCouncilEnv(t)
	must(t, e.tracker.SetFile("acme/app", councilCommit, "internal/queue/lease.go", []byte("package queue")))
	must(t, e.tracker.SetFile("acme/app", councilCommit, "docs/adr/1.md", []byte("# ADR")))
	must(t, e.tracker.SetFile("acme/app", "main", "only-on-main.go", []byte("package x")))
	c := e.stored(t)
	j := e.job(t, "architect", "codex")
	got := e.check(t, c, e.cited(t, c, "architect",
		map[string]any{"kind": "path", "source": "internal/queue/lease.go:40-58"},
		map[string]any{"kind": "path", "source": "gone.go"},
		map[string]any{"kind": "path", "source": "only-on-main.go"},
		map[string]any{"kind": "kb", "source": "docs/adr/1.md"},
		map[string]any{"kind": "kb", "source": "docs/adr/2.md"},
		map[string]any{"kind": "job", "source": j.Ref()},
		map[string]any{"kind": "job", "source": "JOB-9999"},
		map[string]any{"kind": "job", "source": "not a ref"},
		map[string]any{"kind": "path", "source": "gone.go"}, // cited twice, checked once
	))
	want := []domain.SourceStatus{domain.SourceOK, domain.SourceMissing, domain.SourceMissing, domain.SourceOK, domain.SourceMissing, domain.SourceOK, domain.SourceMissing, domain.SourceMissing}
	if len(got) != len(want) {
		t.Fatalf("checks = %+v", got)
	}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("%s = %s (%s), want %s", got[i].Source, got[i].Status, got[i].Detail, w)
		}
	}
}

func TestCheckEvidenceOfAJobOfAnotherProject(t *testing.T) {
	e := newCouncilEnv(t)
	other := domain.Project{AccountID: e.account, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, e.st.Projects().Create(t.Context(), &other))
	j := domain.Job{AccountID: e.account, ProjectID: other.ID, Role: "architect", Runtime: "codex"}
	must(t, e.st.Jobs().Create(t.Context(), &j))
	c := e.stored(t)
	got := e.check(t, c, e.cited(t, c, "architect", map[string]any{"kind": "job", "source": j.Ref()}))
	if got[0].Status != domain.SourceMissing {
		t.Fatalf("checks = %+v", got)
	}
}

func TestCheckEvidenceURLs(t *testing.T) {
	e := newCouncilEnv(t)
	hits := e.web(map[string]string{
		"https://public.example/doc":   "<h1>A page</h1>",
		"https://public.example/inner": "redirect:http://169.254.169.254/latest/meta-data",
	})
	c := e.stored(t)
	k := e.cited(t, c, "architect",
		url("https://public.example/doc"), url("http://10.0.0.5/admin"), url("https://inner.example/x"),
		url("https://public.example/inner"), url("https://public.example/404"))
	got := e.check(t, c, k)
	want := []domain.SourceStatus{domain.SourceOK, domain.SourceRefused, domain.SourceRefused, domain.SourceRefused, domain.SourceUnreachable}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("%s = %s (%s), want %s", got[i].Source, got[i].Status, got[i].Detail, w)
		}
	}
	if *hits != 3 {
		t.Errorf("requests = %d: private addresses must not be contacted", *hits)
	}
	if strings.Contains(got[2].Detail, "10.1.2.3") {
		t.Errorf("detail tells what a name resolves to: %q", got[2].Detail)
	}

	// The page is archived as an artifact of the job.
	a, err := e.st.Artifacts().Get(t.Context(), e.account, got[0].ArtifactID)
	must(t, err)
	if a.JobID != k.JobID || a.ContentType != "text/html" || a.Size != 15 || !domain.ValidArtifactName(a.Name) || a.SHA256 == "" {
		t.Fatalf("artifact = %+v", a)
	}
	body, err := e.evid.Get(t.Context(), a.StorageKey)
	must(t, err)
	defer func() { _ = body.Close() }()
	if b, _ := io.ReadAll(body); string(b) != "<h1>A page</h1>" {
		t.Fatalf("archived = %q", b)
	}

	// Fetched once per council: another member citing it reuses the check.
	before := *hits
	again := e.check(t, c, e.cited(t, c, "ux", url("https://public.example/doc"), url("http://10.0.0.5/admin")))
	if *hits != before || again[0].ArtifactID != got[0].ArtifactID || again[1].Status != domain.SourceRefused {
		t.Fatalf("second citation: %d requests, %+v", *hits-before, again)
	}
}

func TestCheckEvidenceCapsURLs(t *testing.T) {
	e := newCouncilEnv(t)
	pages := map[string]string{}
	var sources []map[string]any
	for i := range playbooks.MaxCheckedURLs + 2 {
		u := fmt.Sprintf("https://public.example/%d", i)
		pages[u] = "page"
		sources = append(sources, url(u))
	}
	hits := e.web(pages)
	c := e.stored(t)
	got := e.check(t, c, e.cited(t, c, "architect", sources...))
	if *hits != playbooks.MaxCheckedURLs || got[len(got)-1].Status != domain.SourceUnreachable || !strings.Contains(got[len(got)-1].Detail, "limit") {
		t.Fatalf("%d requests, last = %+v", *hits, got[len(got)-1])
	}
	// What was over the limit can still be fetched for another member.
	again := e.check(t, c, e.cited(t, c, "ux", sources[len(sources)-1]))
	if again[0].Status != domain.SourceOK {
		t.Fatalf("checks = %+v", again)
	}
}

// TestCheckEvidenceCapsACouncil: however many contributions cite them, a
// council gets MaxCouncilURLs fetched.
func TestCheckEvidenceCapsACouncil(t *testing.T) {
	e := newCouncilEnv(t)
	pages := map[string]string{}
	var batches [][]map[string]any
	for b := range playbooks.MaxCouncilURLs/playbooks.MaxCheckedURLs + 1 {
		var batch []map[string]any
		for i := range playbooks.MaxCheckedURLs {
			u := fmt.Sprintf("https://public.example/%d/%d", b, i)
			pages[u] = "page"
			batch = append(batch, url(u))
		}
		batches = append(batches, batch)
	}
	hits := e.web(pages)
	c := e.stored(t)
	var last []domain.SourceCheck
	for i, batch := range batches {
		last = e.check(t, c, e.cited(t, c, fmt.Sprintf("role-%d", i), batch...))
	}
	if *hits != playbooks.MaxCouncilURLs || last[0].Status != domain.SourceUnreachable || !strings.Contains(last[0].Detail, "per council") {
		t.Fatalf("%d requests, last = %+v", *hits, last[0])
	}
	// What was fetched stays known past the cap.
	again := e.check(t, c, e.cited(t, c, "ux", batches[0][0]))
	if again[0].Status != domain.SourceOK {
		t.Fatalf("checks = %+v", again)
	}
}

func TestCheckEvidenceStorageFailures(t *testing.T) {
	e := newCouncilEnv(t)
	e.web(map[string]string{"https://public.example/doc": "page"})
	c := e.stored(t)
	e.evid.PutErr = errors.New("disk full")
	got := e.check(t, c, e.cited(t, c, "architect", url("https://public.example/doc")))
	if got[0].Status != domain.SourceOK || got[0].ArtifactID != uuid.Nil() || !strings.Contains(got[0].Detail, "not archived") {
		t.Fatalf("checks = %+v", got)
	}
}

func TestCheckEvidenceWithoutStorage(t *testing.T) {
	e := newCouncilEnv(t)
	e.web(map[string]string{"https://public.example/doc": "page"})
	playbooks.Register(e.actions, playbooks.Deps{Store: e.st, Tracker: e.tracker, Signals: e.bus, Now: e.clock.Now, Fetcher: e.fetcher})
	c := e.stored(t)
	got := e.check(t, c, e.cited(t, c, "architect", url("https://public.example/doc")))
	if got[0].Status != domain.SourceOK || !strings.Contains(got[0].Detail, "no evidence storage") {
		t.Fatalf("checks = %+v", got)
	}
}

// downFiles fails file reads, as GitHub does when it is down.
type downFiles struct{ github.Tracker }

func (downFiles) GetFile(context.Context, string, string, string) ([]byte, error) {
	return nil, errors.New("github is down")
}

func TestCheckEvidenceRepoUnreadable(t *testing.T) {
	e := newCouncilEnv(t)
	playbooks.Register(e.actions, playbooks.Deps{Store: e.st, Tracker: downFiles{e.tracker}, Signals: e.bus, Now: e.clock.Now})
	c := e.stored(t)
	got := e.check(t, c, e.cited(t, c, "architect", map[string]any{"kind": "path", "source": "a.go"}))
	if got[0].Status != domain.SourceUnreachable {
		t.Fatalf("checks = %+v", got)
	}
}

// TestCheckEvidenceIsIdempotent: a repeated call fetches nothing again.
func TestCheckEvidenceIsIdempotent(t *testing.T) {
	e := newCouncilEnv(t)
	hits := e.web(map[string]string{"https://public.example/doc": "page"})
	c := e.stored(t)
	k := e.cited(t, c, "architect", url("https://public.example/doc"))
	e.check(t, c, k)
	e.check(t, c, k)
	if *hits != 1 {
		t.Fatalf("requests = %d", *hits)
	}
}

func TestContributeIsIdempotent(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.stored(t)
	j := e.job(t, "architect", "codex")
	with := map[string]any{"council_id": c.ID.String(), "step": "proposal", "round": 1, "job_id": j.ID.String(), "output": validOutput(workflow.JobRequest{
		Role: "architect", Council: &workflow.CouncilTurn{Step: "proposal", Round: 1, Priority: "simplicity", Runtime: "codex"},
	})}
	a, err := e.call(t, "council.contribute", "", with)
	must(t, err)
	b, err := e.call(t, "council.contribute", "", with)
	must(t, err)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("second call = %+v, first %+v", b, a)
	}
	got := e.contributions(t, c.ID, domain.StageProposal)
	if len(got) != 1 || got[0].Role != "architect" || got[0].Runtime != "codex" || got[0].JobID != j.ID {
		t.Fatalf("contributions = %+v", got)
	}
}

// briefed stores a council with a valid brief.
func (e *councilEnv) briefed(t *testing.T) domain.Council {
	t.Helper()
	c := e.stored(t)
	raw, err := json.Marshal(brief(true, false))
	must(t, err)
	b, err := councils.ParseBrief(raw)
	must(t, err)
	c.Brief = &b
	must(t, e.st.Councils().Update(t.Context(), &c))
	return c
}

func TestOpenGateIsIdempotent(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	key := domain.NewID().String()
	with := map[string]any{"council_id": c.ID.String()}
	first, err := e.call(t, "council.open_gate", key, with)
	must(t, err)
	second, err := e.call(t, "council.open_gate", key, with)
	must(t, err)
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("gates %v, %v", first, second)
	}
}

// wantDecided checks the end of a council: decided, with want its one
// decision, announced once.
func (e *councilEnv) wantDecided(t *testing.T, c domain.Council, want domain.Decision) {
	t.Helper()
	if got := e.council(t, c.ID); got.Status != domain.CouncilDecided {
		t.Fatalf("status = %s", got.Status)
	}
	if d := e.decisions(t); len(d) != 1 || d[0].ID != want.ID || d[0].CouncilID != c.ID {
		t.Fatalf("decisions = %+v, want %s", d, want.Ref())
	}
	if s := e.signal(t, c.ID); s.DecisionID != want.ID {
		t.Fatalf("signal = %+v", s)
	}
}

// TestDecideByPolicyRecordsOneDecision covers a repeated call and the crashes
// around the decision: before the status change and before the signal.
func TestDecideByPolicyRecordsOneDecision(t *testing.T) {
	regs := func(e *councilEnv) *registers.Service { return registers.New(e.st, e.bus, e.clock.Now) }
	policy := func() *councilEnv {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilTechnical}
		return e
	}
	t.Run("repeated call", func(t *testing.T) {
		e := policy()
		c := e.briefed(t)
		for range 2 {
			_, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String()})
			must(t, err)
		}
		d := e.decisions(t)
		if len(d) != 1 || d[0].DecidedBy != domain.DecidedByPolicy {
			t.Fatalf("decisions = %+v", d)
		}
		e.wantDecided(t, c, d[0])
	})
	t.Run("crash before the status change", func(t *testing.T) {
		e := policy()
		c := e.briefed(t)
		d, err := regs(e).AcceptRecommendation(t.Context(), c)
		must(t, err)
		_, err = e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String()})
		must(t, err)
		e.wantDecided(t, c, d)
	})
	t.Run("crash before the signal, the policy changed since", func(t *testing.T) {
		e := policy()
		c := e.briefed(t)
		d, err := regs(e).AcceptRecommendation(t.Context(), c)
		must(t, err)
		must(t, e.st.Councils().UpdateStatus(t.Context(), e.account, c.ID, domain.CouncilRunning, domain.CouncilDecided))
		e.cfg.Policy.AcceptRecommendations = nil
		_, err = e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String()})
		must(t, err)
		e.wantDecided(t, c, d)
	})
}

// TestDecideAfterTheGateRecordsNothing: the gate's resolution stored the one
// decision; decide finds it, also when it runs again.
func TestDecideAfterTheGateRecordsNothing(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	out, err := e.call(t, "council.open_gate", "", map[string]any{"council_id": c.ID.String()})
	must(t, err)
	gate := toMap(t, out)["gate_id"].(string)
	g, err := e.gates.Find(t.Context(), e.account, gate)
	must(t, err)
	e.decide(t, g, mcp.ToolApprove, "Go ahead")
	with := map[string]any{"council_id": c.ID.String(), "gate_id": gate, "decision": "approved"}
	for range 2 {
		_, err := e.call(t, "council.decide", "", with)
		must(t, err)
	}
	d := e.decisions(t)
	if len(d) != 1 || d[0].DecidedBy != e.human.String() || d[0].Choice != "pg" || d[0].GateID.String() != gate || !strings.HasPrefix(d[0].Reasons, "Go ahead\n") {
		t.Fatalf("decisions = %+v", d)
	}
	e.wantDecided(t, c, d[0])
}

// TestRejectionRecordsOneDecision: the gate's decision stands, the council
// fails without one, and a repeated call changes nothing.
func TestRejectionRecordsOneDecision(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	out, err := e.call(t, "council.open_gate", "", map[string]any{"council_id": c.ID.String()})
	must(t, err)
	gate := toMap(t, out)["gate_id"].(string)
	g, err := e.gates.Find(t.Context(), e.account, gate)
	must(t, err)
	e.decide(t, g, mcp.ToolReject, "No")
	for range 2 {
		if _, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String(), "gate_id": gate, "decision": "rejected"}); err == nil {
			t.Fatal("a rejection did not fail the step")
		}
	}
	if d := e.decisions(t); len(d) != 1 || d[0].Choice != string(domain.GateRejected) || d[0].CouncilID != c.ID {
		t.Fatalf("decisions = %+v", d)
	}
	if s := e.signal(t, c.ID); s.Status != domain.CouncilFailed {
		t.Fatalf("signal = %+v", s)
	}
}

// gated opens the gate of c through its action.
func (e *councilEnv) gated(t *testing.T, c domain.Council) domain.Gate {
	t.Helper()
	out, err := e.call(t, "council.open_gate", "", map[string]any{"council_id": c.ID.String()})
	must(t, err)
	g, err := e.gates.Find(t.Context(), e.account, toMap(t, out)["gate_id"].(string))
	must(t, err)
	return g
}

// TestDecideNeedsTheGatesDecision: a council is never decided without one,
// here a gate resolved by something that is no human.
func TestDecideNeedsTheGatesDecision(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	g := e.gated(t, c)
	_, err := e.gates.Resolve(t.Context(), domain.SystemActor(e.account, domain.ForecrewdName), g.ID, domain.GateApproved, "")
	must(t, err)
	if _, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String(), "gate_id": g.ID.String(), "decision": "approved"}); err == nil || !strings.Contains(err.Error(), "has no decision") {
		t.Fatalf("err = %v", err)
	}
	if got := e.council(t, c.ID); got.Status != domain.CouncilRunning {
		t.Fatalf("status = %s", got.Status)
	}
}

// wantUntouched checks that a refused call left c running and undecided.
func (e *councilEnv) wantUntouched(t *testing.T, c domain.Council, decisions int) {
	t.Helper()
	if got := e.council(t, c.ID); got.Status != domain.CouncilRunning {
		t.Errorf("status = %s", got.Status)
	}
	if d := e.decisions(t); len(d) != decisions {
		t.Errorf("decisions = %+v, want %d", d, decisions)
	}
	if sigs, err := e.bus.Lookup(t.Context(), e.account, domain.CouncilKey(c.ID)); err != nil || len(sigs) != 0 {
		t.Errorf("signals = %+v, %v", sigs, err)
	}
}

// TestDecideRefusesWithoutAGate calls the action directly, as a forged
// playbook value would: only what the policy accepts passes.
func TestDecideRefusesWithoutAGate(t *testing.T) {
	technical := []domain.CouncilKind{domain.CouncilTechnical}
	tests := []struct {
		name     string
		policy   []domain.CouncilKind
		change   func(b *domain.CouncilBrief)
		decision string
		want     string
	}{
		{name: "no policy", want: "does not accept"},
		{name: "another kind", policy: []domain.CouncilKind{domain.CouncilProduct}, want: "does not accept"},
		{name: "irreversible", policy: technical, change: func(b *domain.CouncilBrief) { b.Reversible = false }, want: "does not accept"},
		{name: "a real dissent", policy: technical, change: func(b *domain.CouncilBrief) {
			b.Dissent = append(b.Dissent, domain.BriefDissent{Member: "ux@codex", Position: "No."})
		}, want: "does not accept"},
		{name: "forged approval", decision: "approved", want: "without a gate"},
		{name: "forged approval the policy would allow", policy: technical, decision: "approved", want: "without a gate"},
		{name: "forged rejection", policy: technical, decision: "rejected", want: "without a gate"},
		{name: "unknown value", policy: technical, decision: "maybe", want: "without a gate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCouncilEnv(t)
			e.cfg.Policy.AcceptRecommendations = tt.policy
			c := e.briefed(t)
			if tt.change != nil {
				tt.change(c.Brief)
				must(t, e.st.Councils().Update(t.Context(), &c))
			}
			with := map[string]any{"council_id": c.ID.String()}
			if tt.decision != "" {
				with["decision"] = tt.decision
			}
			if _, err := e.call(t, "council.decide", "", with); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			e.wantUntouched(t, c, 0)
		})
	}
	t.Run("a council that asked a human", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = technical
		c := e.briefed(t)
		e.gated(t, c)
		if _, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String()}); err == nil || !strings.Contains(err.Error(), "does not accept") {
			t.Fatalf("err = %v", err)
		}
		e.wantUntouched(t, c, 0)
	})
	t.Run("no brief", func(t *testing.T) {
		e := newCouncilEnv(t)
		e.cfg.Policy.AcceptRecommendations = technical
		c := e.stored(t)
		if _, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String()}); err == nil {
			t.Fatal("a council without a brief was decided")
		}
		e.wantUntouched(t, c, 0)
	})
}

// TestDecideRefusesAForgedGate: the gate decides, not what the call says.
func TestDecideRefusesAForgedGate(t *testing.T) {
	type world struct {
		e    *councilEnv
		c    domain.Council
		gate domain.Gate
	}
	decide := func(w world, tool string) func(t *testing.T) int {
		return func(t *testing.T) int {
			w.e.decide(t, w.gate, tool, "why")
			return 1
		}
	}
	tests := []struct {
		name     string
		resolve  func(w world) func(t *testing.T) int
		gate     func(t *testing.T, w world) string
		decision any
		want     string
	}{
		{name: "empty decision", resolve: func(w world) func(*testing.T) int { return decide(w, mcp.ToolApprove) }, want: "unknown decision"},
		{name: "unknown decision", resolve: func(w world) func(*testing.T) int { return decide(w, mcp.ToolApprove) }, decision: "maybe", want: "unknown decision"},
		{name: "a pending gate", decision: "approved", want: "not approved"},
		{name: "a rejected gate called approved", resolve: func(w world) func(*testing.T) int { return decide(w, mcp.ToolReject) }, decision: "approved", want: "not approved"},
		{name: "changes called approved", resolve: func(w world) func(*testing.T) int { return decide(w, mcp.ToolRequestChanges) }, decision: "approved", want: "not approved"},
		{name: "an approved gate called rejected", resolve: func(w world) func(*testing.T) int { return decide(w, mcp.ToolApprove) }, decision: "rejected", want: "not rejected"},
		{name: "unknown gate", gate: func(*testing.T, world) string { return domain.NewID().String() }, decision: "approved", want: "is not the gate of"},
		{name: "another council's approved gate", gate: func(t *testing.T, w world) string {
			other := w.e.briefed(t)
			g := w.e.gated(t, other)
			w.e.decide(t, g, mcp.ToolApprove, "")
			return g.ID.String()
		}, decision: "approved", want: "is not the gate of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCouncilEnv(t)
			c := e.briefed(t)
			w := world{e: e, c: c, gate: e.gated(t, c)}
			decisions := 0
			if tt.resolve != nil {
				decisions = tt.resolve(w)(t)
			}
			id := w.gate.ID.String()
			if tt.gate != nil {
				id = tt.gate(t, w)
				decisions = len(e.decisions(t))
			}
			with := map[string]any{"council_id": c.ID.String(), "gate_id": id}
			if tt.decision != nil {
				with["decision"] = tt.decision
			}
			if _, err := e.call(t, "council.decide", "", with); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			e.wantUntouched(t, c, decisions)
			if got := e.council(t, c.ID); len(got.Constraints) != 0 {
				t.Errorf("constraints = %v", got.Constraints)
			}
		})
	}
}

func TestHumanRoundAddsTheFeedbackOnce(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	g := e.gated(t, c)
	e.decide(t, g, mcp.ToolRequestChanges, "Keep Postgres")
	with := map[string]any{"council_id": c.ID.String(), "gate_id": g.ID.String(), "decision": "changes_requested",
		"members": []any{map[string]any{"role": "architect", "priority": "simplicity", "runtime": "codex"}}}
	for range 2 {
		out, err := e.call(t, "council.decide", "", with)
		must(t, err)
		if m := toMap(t, out); m["done"] != false || m["round"] != float64(1) {
			t.Fatalf("output = %+v", m)
		}
	}
	if got := e.council(t, c.ID).Constraints; len(got) != 1 || got[0] != "Keep Postgres" {
		t.Fatalf("constraints = %v", got)
	}
}

// TestSecondHumanRoundIsRefused: should a second request for changes reach
// the action, it fails instead of running a round the council may not have.
func TestSecondHumanRoundIsRefused(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	g := e.gated(t, c)
	e.decide(t, g, mcp.ToolRequestChanges, "Once more")
	k := domain.CouncilContribution{AccountID: e.account, CouncilID: c.ID, Role: "facilitator", Stage: domain.StageSynthesis, Round: 2, Content: []byte(`{}`)}
	must(t, e.st.Councils().AddContribution(t.Context(), &k))
	_, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String(), "gate_id": g.ID.String(), "decision": "changes_requested"})
	if err == nil || !strings.Contains(err.Error(), "had its human round") {
		t.Fatalf("err = %v", err)
	}
}

// TestActionsStopForAnEndedCouncil: what runs between a council's jobs fails
// once the council ended, which ends its run.
func TestActionsStopForAnEndedCouncil(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.briefed(t)
	must(t, e.st.Councils().UpdateStatus(t.Context(), e.account, c.ID, domain.CouncilRunning, domain.CouncilCanceled))
	id := c.ID.String()
	for action, with := range map[string]map[string]any{
		"council.contribute": {"council_id": id, "step": "frame", "round": 1},
		"council.round_done": {"council_id": id, "stage": "proposal"},
		"council.open_gate":  {"council_id": id},
	} {
		if _, err := e.call(t, action, "", with); err == nil || !strings.Contains(err.Error(), "is canceled") {
			t.Errorf("%s: err = %v", action, err)
		}
	}
}

func TestCouncilActionFailures(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.stored(t)
	other := domain.Project{AccountID: e.account, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, e.st.Projects().Create(t.Context(), &other))
	foreign := domain.Council{AccountID: e.account, ProjectID: other.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	must(t, e.st.Councils().Create(t.Context(), &foreign))
	id := c.ID.String()

	tests := []struct {
		name, action string
		with         map[string]any
		want         string
	}{
		{"start: bad with", "council.start", map[string]any{"bogus": 1}, "bogus"},
		{"start: unknown council", "council.start", map[string]any{"council_id": domain.NewID().String()}, "get council"},
		{"start: another project's", "council.start", map[string]any{"council_id": foreign.ID.String()}, "another project"},
		{"start: bad seats", "council.start", map[string]any{"council_id": id, "roles": []string{"ghost", "ux"}}, "ghost"},
		{"run: bad with", "council.run", map[string]any{"bogus": 1}, "bogus"},
		{"run: no question", "council.run", map[string]any{"kind": "technical"}, "question is required"},
		{"contribute: bad with", "council.contribute", map[string]any{"bogus": 1}, "bogus"},
		{"contribute: unknown step", "council.contribute", map[string]any{"council_id": id, "step": "vote", "output": map[string]any{}}, "unknown step"},
		{"contribute: unknown job", "council.contribute", map[string]any{"council_id": id, "step": "critique", "round": 1, "job_id": domain.NewID().String(),
			"output": validOutput(workflow.JobRequest{Role: "ux", Council: &workflow.CouncilTurn{Step: "critique", Priority: "ux"}})}, "get job"},
		{"evidence: bad with", "council.check_evidence", map[string]any{"bogus": 1}, "bogus"},
		{"evidence: unknown contribution", "council.check_evidence", map[string]any{"council_id": id, "contribution_id": domain.NewID().String()}, "no contribution"},
		{"round_done: bad with", "council.round_done", map[string]any{"bogus": 1}, "bogus"},
		{"round_done: unknown stage", "council.round_done", map[string]any{"council_id": id, "stage": "vote"}, "unknown stage"},
		{"open_gate: bad with", "council.open_gate", map[string]any{"bogus": 1}, "bogus"},
		{"open_gate: no brief", "council.open_gate", map[string]any{"council_id": id}, "has no brief"},
		{"decide: bad with", "council.decide", map[string]any{"bogus": 1}, "bogus"},
		{"settle: bad with", "council.settle", map[string]any{"bogus": 1}, "bogus"},
		{"settle: unknown council", "council.settle", map[string]any{"council_id": domain.NewID().String()}, "get council"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.call(t, tt.action, "", tt.with)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCouncilStartWithoutTheBranch(t *testing.T) {
	e := newCouncilEnv(t)
	c := e.stored(t)
	p := domain.Project{AccountID: e.account, Name: "empty", Repo: "acme/empty", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, e.st.Projects().Create(t.Context(), &p))
	e.project = p
	c.ProjectID = p.ID
	other := domain.Council{AccountID: e.account, ProjectID: p.ID, Kind: domain.CouncilTechnical, Question: "q"}
	must(t, e.st.Councils().Create(t.Context(), &other))
	e.tracker.Install("acme/empty")
	if _, err := e.call(t, "council.start", "", map[string]any{"council_id": other.ID.String()}); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// TestSettleKeepsADecidedCouncil: a run that fails after the decision does
// not undo it.
func TestSettleKeepsADecidedCouncil(t *testing.T) {
	e := newCouncilEnv(t)
	e.cfg.Policy.AcceptRecommendations = []domain.CouncilKind{domain.CouncilTechnical}
	c := e.briefed(t)
	_, err := e.call(t, "council.decide", "", map[string]any{"council_id": c.ID.String()})
	must(t, err)
	_, err = e.call(t, "council.settle", "", map[string]any{"council_id": c.ID.String()})
	must(t, err)
	if got := e.council(t, c.ID); got.Status != domain.CouncilDecided {
		t.Fatalf("status = %s", got.Status)
	}
	if s := e.signal(t, c.ID); s.Status != domain.CouncilDecided {
		t.Fatalf("signal = %+v", s)
	}
}

// TestStartPinsRuntimes: members go to the runtimes workers offer, and to
// their first choice when none does.
func TestStartPinsRuntimes(t *testing.T) {
	e := newCouncilEnv(t)
	c := domain.Council{AccountID: e.account, ProjectID: e.project.ID, Kind: domain.CouncilTechnical, Question: "q"}
	must(t, e.st.Councils().Create(t.Context(), &c))
	out, err := e.call(t, "council.start", "", map[string]any{"council_id": c.ID.String(), "roles": []string{"architect", "ux"}})
	must(t, err)
	m := toMap(t, out)
	if m["commit"] != councilCommit || m["facilitator"] != "facilitator" {
		t.Fatalf("output = %+v", m)
	}
	// The control worker lost codex: every control seat moves to claude-code.
	workers, err := e.st.Workers().List(t.Context(), e.account)
	must(t, err)
	for _, w := range workers {
		if w.Pool == domain.PoolControl {
			must(t, e.st.Workers().Seen(t.Context(), e.account, w.ID, []string{domain.RuntimeLabel(config.AdapterClaudeCode)}, "1"))
		}
	}
	out, err = e.call(t, "council.start", "", map[string]any{"council_id": c.ID.String(), "roles": []string{"architect", "ux"}})
	must(t, err)
	for _, member := range toMap(t, out)["members"].([]any) {
		if r := member.(map[string]any)["runtime"]; r != config.AdapterClaudeCode {
			t.Fatalf("members = %+v", toMap(t, out)["members"])
		}
	}
}
