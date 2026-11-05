package verification_test

import (
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/verification"
)

const sha = "abc1234def"

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fixture is a store with a project, a PR and a way to add test runs.
type fixture struct {
	t      *testing.T
	clock  *testkit.Clock
	st     *testkit.Store
	acc    domain.Account
	proj   domain.Project
	change domain.Change
	b      verification.Builder
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, st.Projects().Create(t.Context(), &p))
	c := domain.Change{AccountID: acc.ID, ProjectID: p.ID, Repo: "acme/app", Number: 1, Branch: "agent/task-1", BaseRef: "main", HeadSHA: sha, State: domain.ChangeOpen, URL: "https://github.com/acme/app/pull/1"}
	must(t, st.Changes().Upsert(t.Context(), &c))
	return &fixture{t: t, clock: clock, st: st, acc: acc, proj: p, change: c, b: verification.Builder{Store: st}}
}

// run is a stored test run for the fixture's PR head.
type run struct {
	env      string
	cell     map[string]string
	suite    string
	status   domain.TestRunStatus
	counts   domain.TestCounts
	versions map[string]string
	sha      string
	change   uuid.UUID
	results  []domain.TestResult
	// overlay makes it the run of a base check: new tests laid over a base.
	overlay bool
}

func pass(names ...string) []domain.TestResult { return results(domain.TestPassed, names...) }
func fail(names ...string) []domain.TestResult { return results(domain.TestFailed, names...) }

func results(st domain.TestStatus, names ...string) []domain.TestResult {
	var out []domain.TestResult
	for _, n := range names {
		class, name, ok := strings.Cut(n, "::")
		if !ok {
			class, name = "", n
		}
		out = append(out, domain.TestResult{Classname: class, Name: name, Status: st})
	}
	return out
}

func join(parts ...[]domain.TestResult) []domain.TestResult {
	var out []domain.TestResult
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// add stores r, one minute after the previous run.
func (f *fixture) add(r run) domain.TestRun {
	f.t.Helper()
	f.clock.Advance(time.Minute)
	j := domain.Job{AccountID: f.acc.ID, ProjectID: f.proj.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	if r.overlay {
		j.Spec.Overlay = &domain.Overlay{Ref: "head", Paths: []string{"a_test.go"}}
	}
	must(f.t, f.st.Jobs().Create(f.t.Context(), &j))
	if r.sha == "" {
		r.sha = sha
	}
	if r.suite == "" {
		r.suite = "unit"
	}
	if r.status == "" {
		r.status = domain.TestRunPassed
		for _, x := range r.results {
			if x.Status == domain.TestFailed || x.Status == domain.TestErrored {
				r.status = domain.TestRunFailed
			}
		}
	}
	if r.counts == (domain.TestCounts{}) {
		for _, x := range r.results {
			switch x.Status {
			case domain.TestPassed:
				r.counts.Passed++
			case domain.TestFailed:
				r.counts.Failed++
			case domain.TestSkipped:
				r.counts.Skipped++
			case domain.TestErrored:
				r.counts.Errored++
			}
		}
	}
	tr := domain.TestRun{AccountID: f.acc.ID, ProjectID: f.proj.ID, JobID: j.ID, ChangeID: r.change, SHA: r.sha, Suite: r.suite, Environment: r.env, Cell: r.cell,
		Status: r.status, Counts: r.counts, DurationMS: 1200, Versions: r.versions, Attempt: 1}
	must(f.t, f.st.TestRuns().Create(f.t.Context(), &tr, r.results))
	return tr
}

func (f *fixture) build(in verification.Inputs) verification.Report {
	f.t.Helper()
	rep, err := f.b.Build(f.t.Context(), f.change, sha, in)
	must(f.t, err)
	raw, err := rep.JSON()
	must(f.t, err)
	if _, err := verification.Parse(raw); err != nil {
		f.t.Fatalf("Parse(JSON) = %v", err)
	}
	return rep
}

func linux() map[string]string { return map[string]string{"os": "linux"} }
func mac() map[string]string   { return map[string]string{"os": "macos"} }

// want is the expectation of a table test.
func blockingHas(t *testing.T, rep verification.Report, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, b := range rep.Blocking {
			found = found || strings.Contains(b, w)
		}
		if !found {
			t.Errorf("blocking %q lacks %q", rep.Blocking, w)
		}
	}
	if len(want) == 0 && len(rep.Blocking) != 0 {
		t.Errorf("blocking = %q, want none", rep.Blocking)
	}
}
