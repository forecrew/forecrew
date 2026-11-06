package verification_test

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/testsys"
	"github.com/forecrew/forecrew/internal/verification"
)

func req(env string, cell map[string]string, suite string) verification.Expected {
	return verification.Expected{Environment: env, Cell: cell, Suite: suite, Required: true}
}

func optional(e verification.Expected) verification.Expected { e.Required = false; return e }

func TestBuildStatus(t *testing.T) {
	linuxUnit, macUnit := req("linux-go", linux(), "unit"), req("mac-go", mac(), "unit")
	tests := []struct {
		name     string
		runs     []run
		expected []verification.Expected
		status   domain.VerificationStatus
		blocking []string
	}{
		{"all required results pass", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
			{env: "mac-go", cell: mac(), results: pass("a::T1")},
		}, []verification.Expected{linuxUnit, macUnit}, domain.VerificationPassed, nil},
		{"a failed required suite blocks", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
			{env: "mac-go", cell: mac(), results: join(pass("a::T1"), fail("a::T2"))},
		}, []verification.Expected{linuxUnit, macUnit}, domain.VerificationFailed, []string{"unit in mac-go (os=macos) failed (1 failed tests)"}},
		{"an errored required suite blocks", []run{
			{env: "linux-go", cell: linux(), status: domain.TestRunErrored},
		}, []verification.Expected{linuxUnit}, domain.VerificationFailed, []string{"errored: no usable result"}},
		{"a failed optional suite does not block", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
			{env: "mac-go", cell: mac(), results: fail("a::T1")},
		}, []verification.Expected{linuxUnit, optional(macUnit)}, domain.VerificationPassed, nil},
		{"a required cell without a result is incomplete", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
		}, []verification.Expected{linuxUnit, macUnit}, domain.VerificationIncomplete, []string{"unit in mac-go (os=macos) has no result yet"}},
		{"a missing optional cell is fine", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
		}, []verification.Expected{linuxUnit, optional(macUnit)}, domain.VerificationPassed, nil},
		{"failed beats incomplete", []run{
			{env: "linux-go", cell: linux(), results: fail("a::T1")},
		}, []verification.Expected{linuxUnit, macUnit}, domain.VerificationFailed, []string{"failed", "has no result yet"}},
		{"no runs at all is incomplete", nil, nil, domain.VerificationIncomplete, []string{"no test results for " + sha}},
		{"without the matrix the stored runs are all required", []run{
			{env: "linux-go", cell: linux(), results: fail("a::T1")},
		}, nil, domain.VerificationFailed, []string{"failed"}},
		{"runs of another commit are not read", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
			{env: "mac-go", cell: mac(), results: fail("a::T1"), sha: "ffff000"},
		}, []verification.Expected{linuxUnit}, domain.VerificationPassed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			for _, r := range tt.runs {
				f.add(r)
			}
			rep := f.build(verification.Inputs{Expected: tt.expected})
			if rep.Status != tt.status {
				t.Errorf("status = %s, want %s (blocking %q)", rep.Status, tt.status, rep.Blocking)
			}
			blockingHas(t, rep, tt.blocking...)
			if rep.SHA != sha || rep.Version != 1 {
				t.Errorf("sha %q version %d", rep.SHA, rep.Version)
			}
		})
	}
}

func TestBuildRunOfAnotherChangeIsIgnored(t *testing.T) {
	f := newFixture(t)
	other := domain.Change{AccountID: f.acc.ID, ProjectID: f.proj.ID, Repo: "acme/app", Number: 2, Branch: "b", BaseRef: "main", HeadSHA: sha, State: domain.ChangeOpen}
	must(t, f.st.Changes().Upsert(t.Context(), &other))
	f.add(run{env: "e", cell: linux(), results: fail("a::T1"), change: other.ID})
	f.add(run{env: "e", cell: linux(), suite: "lint", results: pass("a::T2"), change: f.change.ID})
	rep := f.build(verification.Inputs{})
	if len(rep.Results) != 1 || rep.Results[0].Suite != "lint" || rep.Status != domain.VerificationPassed {
		t.Fatalf("report = %+v", rep)
	}
}

func TestBuildResultFields(t *testing.T) {
	f := newFixture(t)
	r1 := f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1", "a::T2"), versions: map[string]string{"go": "go version go1.27 linux/amd64\nextra", "node": "v26"}})
	f.add(run{env: "linux-go", cell: linux(), suite: "lint", results: pass("a::L1"), versions: map[string]string{"go": "other"}})
	f.add(run{env: "", cell: nil, suite: "docs", results: pass("D1")})
	rep := f.build(verification.Inputs{Expected: []verification.Expected{req("linux-go", linux(), "unit")}})
	if len(rep.Results) != 3 {
		t.Fatalf("results = %+v", rep.Results)
	}
	x := rep.Results[0]
	if x.Environment != "linux-go" || x.Suite != "unit" || !x.Required || x.Status != "passed" || x.Counts.Passed != 2 || x.DurationMS != 1200 ||
		x.TestRun != r1.ID.String() || !strings.HasPrefix(x.Job, "JOB-") || x.Attempts != 1 || x.Cell["os"] != "linux" {
		t.Errorf("result = %+v", x)
	}
	if rep.Results[1].Suite != "docs" && rep.Results[1].Suite != "lint" || rep.Results[1].Required {
		t.Errorf("an unexpected result is not required: %+v", rep.Results[1:])
	}
	env := rep.Environments["linux-go"].Versions
	if env["go"] != "go version go1.27 linux/amd64" || env["node"] != "v26" || len(env) != 2 {
		t.Errorf("versions = %v", env)
	}
	if _, ok := rep.Environments[""]; !ok {
		t.Errorf("the worker's host is missing: %v", rep.Environments)
	}
}

func TestBuildFlaky(t *testing.T) {
	cell := req("linux-go", linux(), "unit")
	tests := []struct {
		name   string
		runs   []run
		status domain.VerificationStatus
		flaky  []verification.Flaky
		result string
	}{
		{"fail then pass is flaky and does not block", []run{
			{env: "linux-go", cell: linux(), results: join(pass("a::T1"), fail("a::T2"))},
			{env: "linux-go", cell: linux(), results: pass("a::T1", "a::T2")},
		}, domain.VerificationPassed, []verification.Flaky{{Test: "a::T2", Environments: []string{"linux-go"}}}, "passed"},
		{"fail then fail again blocks", []run{
			{env: "linux-go", cell: linux(), results: fail("a::T2")},
			{env: "linux-go", cell: linux(), results: fail("a::T2")},
		}, domain.VerificationFailed, []verification.Flaky{}, "failed"},
		{"pass then fail is a failure, not flaky", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T2")},
			{env: "linux-go", cell: linux(), results: fail("a::T2")},
		}, domain.VerificationFailed, []verification.Flaky{}, "failed"},
		{"a different test failing in the rerun is not flaky", []run{
			{env: "linux-go", cell: linux(), results: join(pass("a::T1"), fail("a::T2"))},
			{env: "linux-go", cell: linux(), results: join(fail("a::T1"), pass("a::T2"))},
		}, domain.VerificationFailed, []verification.Flaky{{Test: "a::T2", Environments: []string{"linux-go"}}}, "failed"},
		{"a test missing from the rerun blocks", []run{
			{env: "linux-go", cell: linux(), results: fail("a::T2")},
			{env: "linux-go", cell: linux(), results: pass("a::T9")},
		}, domain.VerificationFailed, []verification.Flaky{}, "passed"},
		{"a test skipped in the rerun blocks", []run{
			{env: "linux-go", cell: linux(), results: fail("a::T2")},
			{env: "linux-go", cell: linux(), results: results(domain.TestSkipped, "a::T2")},
		}, domain.VerificationFailed, []verification.Flaky{}, "passed"},
		{"a compile error then a pass has no test to list", []run{
			{env: "linux-go", cell: linux(), status: domain.TestRunErrored},
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
		}, domain.VerificationPassed, []verification.Flaky{}, "passed"},
		{"the synthetic results of a failed command are not tests", []run{
			{env: "linux-go", cell: linux(), results: fail("(command)", "(report)", "(oversized)")},
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
		}, domain.VerificationPassed, []verification.Flaky{}, "passed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			for _, r := range tt.runs {
				f.add(r)
			}
			rep := f.build(verification.Inputs{Expected: []verification.Expected{cell}})
			if tt.status == domain.VerificationFailed && tt.result == "passed" {
				blockingHas(t, rep, "a::T2 failed in an earlier attempt")
			}
			if rep.Status != tt.status || rep.Results[0].Status != tt.result || rep.Results[0].Attempts != 2 || !reflect.DeepEqual(rep.Flaky, tt.flaky) {
				t.Errorf("status %s, result %+v, flaky %+v; blocking %q", rep.Status, rep.Results[0], rep.Flaky, rep.Blocking)
			}
		})
	}
}

func TestBuildFlakyInSeveralEnvironments(t *testing.T) {
	f := newFixture(t)
	for _, e := range []struct {
		env  string
		cell map[string]string
	}{{"mac-go", mac()}, {"linux-go", linux()}} {
		f.add(run{env: e.env, cell: e.cell, results: fail("a::T2")})
		f.add(run{env: e.env, cell: e.cell, results: pass("a::T2")})
	}
	rep := f.build(verification.Inputs{})
	want := []verification.Flaky{{Test: "a::T2", Environments: []string{"linux-go", "mac-go"}}}
	if !reflect.DeepEqual(rep.Flaky, want) || rep.Status != domain.VerificationPassed {
		t.Errorf("flaky = %+v, status %s", rep.Flaky, rep.Status)
	}
}

func TestBuildCriteria(t *testing.T) {
	unit := req("linux-go", linux(), "unit")
	tests := []struct {
		name   string
		runs   []run
		exp    []verification.Expected
		in     verification.Inputs
		status domain.VerificationStatus
		crit   map[string]string // criterion -> status
		block  []string
	}{
		{"covered by a passing test", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationPassed, map[string]string{"AC-1": "passed"}, nil},
		{"a criterion with no test blocks", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1", "AC-2"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "passed", "AC-2": "untested"}, []string{"AC-2 has no test"}},
		{"an untestable criterion with a reason does not block", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1", "AC-2"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}},
				Untestable: []verification.Untestable{{Criterion: "AC-2", Reason: "needs a human to read the page"}}},
			domain.VerificationPassed, map[string]string{"AC-1": "passed", "AC-2": "untested"}, nil},
		{"an untestable criterion without a reason blocks", nil, nil,
			verification.Inputs{Criteria: []string{"AC-2"}, Untestable: []verification.Untestable{{Criterion: "AC-2"}}},
			domain.VerificationFailed, map[string]string{"AC-2": "untested"}, []string{"AC-2 has no test"}},
		{"a failing test fails its criterion", []run{{env: "linux-go", cell: linux(), results: join(pass("a::T1"), fail("a::T2"))}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}, {Criterion: "AC-1", Suite: "unit", ID: "a::T2"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "failed"}, []string{"AC-1: a::T2 failed"}},
		{"a test missing from a run that ran blocks", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::Gone"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "untested"}, []string{"AC-1: a::Gone did not run"}},
		{"a test whose cell has not run yet is incomplete", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}},
			[]verification.Expected{unit, req("mac-go", mac(), "unit")},
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationIncomplete, map[string]string{"AC-1": "untested"}, []string{"AC-1: a::T1 has no result yet"}},
		{"skipped tests do not verify", []run{{env: "linux-go", cell: linux(), results: []domain.TestResult{{Classname: "a", Name: "T1", Status: domain.TestSkipped}}}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "untested"}, []string{"AC-1: all its tests were skipped"}},
		{"a test of a suite that is not in the matrix blocks", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "e2e", ID: "a::T1"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "untested"}, []string{"AC-1: a::T1 did not run"}},
		{"the worst result over the required cells counts", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
			{env: "mac-go", cell: mac(), results: fail("a::T1")},
		}, []verification.Expected{unit, req("mac-go", mac(), "unit")},
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "failed"}, []string{"AC-1: a::T1 failed"}},
		{"a failure in an optional cell does not count when a required one has the suite", []run{
			{env: "linux-go", cell: linux(), results: pass("a::T1")},
			{env: "mac-go", cell: mac(), results: fail("a::T1")},
		}, []verification.Expected{unit, optional(req("mac-go", mac(), "unit"))},
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationPassed, map[string]string{"AC-1": "passed"}, nil},
		{"a suite with optional cells only is read from all of them", []run{
			{env: "mac-go", cell: mac(), results: pass("a::T1")},
		}, []verification.Expected{optional(req("mac-go", mac(), "unit"))},
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationPassed, map[string]string{"AC-1": "passed"}, nil},
		{"a test without a classname", []run{{env: "linux-go", cell: linux(), results: pass("TestPlain")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "TestPlain"}}},
			domain.VerificationPassed, map[string]string{"AC-1": "passed"}, nil},
		{"a test of another criterion is not counted", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-9", Suite: "unit", ID: "a::T1"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "untested"}, []string{"AC-1 has no test"}},
		{"a failing test of a criterion the task lacks blocks", []run{{env: "linux-go", cell: linux(), results: join(pass("a::T1"), fail("pkg::TestGhost"))}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}, {Criterion: "AC-9", Suite: "unit", ID: "pkg::TestGhost"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "passed"}, []string{"pkg::TestGhost of no criterion of the task failed"}},
		{"an absent test of a criterion the task lacks blocks", []run{{env: "linux-go", cell: linux(), results: pass("a::T1")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}, {Criterion: "AC-9", Suite: "unit", ID: "pkg::TestGhost"}, {Criterion: "AC-8", Suite: "unit", ID: "pkg::TestGhost"}}},
			domain.VerificationFailed, map[string]string{"AC-1": "passed"}, []string{"pkg::TestGhost of no criterion of the task did not run"}},
		{"a passing test of a criterion the task lacks does not block", []run{{env: "linux-go", cell: linux(), results: pass("a::T1", "pkg::TestGhost")}}, nil,
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}, {Criterion: "AC-9", Suite: "unit", ID: "pkg::TestGhost"}}},
			domain.VerificationPassed, map[string]string{"AC-1": "passed"}, nil},
		{"a test of a criterion the task lacks waits for its cell", []run{{env: "linux-go", cell: linux(), results: pass("a::T1", "pkg::TestGhost")}},
			[]verification.Expected{unit, req("mac-go", mac(), "unit")},
			verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T1"}, {Criterion: "AC-9", Suite: "unit", ID: "pkg::TestGhost"}}},
			domain.VerificationIncomplete, map[string]string{"AC-1": "untested"}, []string{"pkg::TestGhost of no criterion of the task has no result yet"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			for _, r := range tt.runs {
				f.add(r)
			}
			tt.in.Expected = tt.exp
			for _, mt := range tt.in.Tests {
				tt.in.BaseCheck = append(tt.in.BaseCheck, verification.BaseTest{ID: mt.ID, FailedOnBase: true})
			}
			rep := f.build(tt.in)
			if rep.Status != tt.status {
				t.Errorf("status = %s, want %s (blocking %q)", rep.Status, tt.status, rep.Blocking)
			}
			got := map[string]string{}
			for id, c := range rep.Criteria {
				got[id] = c.Status
			}
			if !reflect.DeepEqual(got, tt.crit) {
				t.Errorf("criteria = %v, want %v", got, tt.crit)
			}
			blockingHas(t, rep, tt.block...)
		})
	}
}

func TestBuildCriterionFields(t *testing.T) {
	f := newFixture(t)
	f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1")})
	rep := f.build(verification.Inputs{
		Criteria:   []string{"AC-10", "AC-2", "AC-2"},
		Tests:      []verification.MapTest{{Criterion: "AC-10", Suite: "unit", ID: "a::T1"}},
		Untestable: []verification.Untestable{{Criterion: "AC-2", Reason: "visual"}},
	})
	if c := rep.Criteria["AC-2"]; c.Untestable != "visual" || c.Status != "untested" || c.Tests == nil || len(c.Tests) != 0 {
		t.Errorf("AC-2 = %+v", c)
	}
	if c := rep.Criteria["AC-10"]; c.Tests["a::T1"] != "passed" || c.Untestable != "" {
		t.Errorf("AC-10 = %+v", c)
	}
	if len(rep.Criteria) != 2 {
		t.Errorf("criteria = %v", rep.Criteria)
	}
}

func TestBuildNewTests(t *testing.T) {
	tests := []struct {
		name   string
		checks []verification.BaseTest
		status domain.VerificationStatus
		want   map[string]bool
		block  []string
		// mapped are the tests of the test map; by default the checked ones.
		mapped []string
	}{
		{"every new test failed on base", []verification.BaseTest{{ID: "a::T1", FailedOnBase: true}}, domain.VerificationPassed, map[string]bool{"a::T1": true}, nil, nil},
		{"a new test that passed on base blocks", []verification.BaseTest{{ID: "a::T1", FailedOnBase: true}, {ID: "a::T2"}}, domain.VerificationFailed,
			map[string]bool{"a::T1": true, "a::T2": false}, []string{"new test a::T2 passed on the base branch"}, nil},
		{"a test listed twice passed on base if one entry says so", []verification.BaseTest{{ID: "a::T1", FailedOnBase: true}, {ID: "a::T1"}}, domain.VerificationFailed,
			map[string]bool{"a::T1": false}, []string{"a::T1 passed"}, nil},
		{"no new tests, no base check", nil, domain.VerificationPassed, map[string]bool{}, nil, nil},
		{"a new test with no base check entry is incomplete", nil, domain.VerificationIncomplete, map[string]bool{}, []string{"new test a::T1 has not been checked on the base branch yet"}, []string{"a::T1"}},
		{"one of two new tests unchecked", []verification.BaseTest{{ID: "a::T1", FailedOnBase: true}}, domain.VerificationIncomplete, map[string]bool{"a::T1": true},
			[]string{"a::T2 has not been checked"}, []string{"a::T1", "a::T2", "a::T2"}},
		{"unchecked next to one that passed on base is failed", []verification.BaseTest{{ID: "a::T1"}}, domain.VerificationFailed, map[string]bool{"a::T1": false},
			[]string{"a::T1 passed on the base", "a::T2 has not been checked"}, []string{"a::T1", "a::T2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1", "a::T2")})
			in := verification.Inputs{BaseCheck: tt.checks}
			mapped := tt.mapped
			for _, c := range tt.checks {
				if tt.mapped == nil {
					mapped = append(mapped, c.ID)
				}
			}
			for _, id := range mapped {
				in.Tests = append(in.Tests, verification.MapTest{Criterion: "AC-1", Suite: "unit", ID: id})
			}
			if len(mapped) > 0 {
				in.Criteria = []string{"AC-1"}
			}
			rep := f.build(in)
			if rep.Status != tt.status || !reflect.DeepEqual(rep.NewTests, tt.want) {
				t.Errorf("status %s, new tests %v; blocking %q", rep.Status, rep.NewTests, rep.Blocking)
			}
			blockingHas(t, rep, tt.block...)
		})
	}
}

func TestBuildInvalidSHA(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"", "xyz", "abc", "ABC12$", strings.Repeat("a", 65)} {
		if _, err := f.b.Build(t.Context(), f.change, bad, verification.Inputs{}); !errors.Is(err, verification.ErrInvalid) {
			t.Errorf("sha %q: err = %v", bad, err)
		}
	}
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	rep, err := f.b.Build(t.Context(), f.change, "ABC1234DEF", verification.Inputs{})
	if err != nil || rep.SHA != sha || len(rep.Results) != 1 {
		t.Errorf("an upper case sha: %+v, %v", rep, err)
	}
}

func TestBuildIsRepeatable(t *testing.T) {
	f := newFixture(t)
	f.add(run{env: "linux-go", cell: linux(), results: fail("a::T2")})
	f.add(run{env: "linux-go", cell: linux(), results: pass("a::T2")})
	in := verification.Inputs{Criteria: []string{"AC-1"}, Tests: []verification.MapTest{{Criterion: "AC-1", Suite: "unit", ID: "a::T2"}}}
	a, b := f.build(in), f.build(in)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("two builds differ:\n%+v\n%+v", a, b)
	}
}

func TestExpect(t *testing.T) {
	repo := fstest.MapFS{
		testsys.MatrixFile: {Data: []byte(`version: 1
axes: {os: [linux, macos]}
required: [{os: linux}]
environments:
  - {match: {os: macos}, environment: mac-go}
  - {match: {}, environment: linux-go}
suites:
  - {name: unit, command: [make, test], level: unit, reports: [r.xml]}
  - {name: e2e, command: make e2e, level: e2e, reports: [e.xml], required: false}
`)},
		testsys.EnvironmentsDir + "/linux-go.yaml": {Data: []byte("version: 1\nname: linux-go\nprovider: machine\nneeds: [go]\n")},
		testsys.EnvironmentsDir + "/mac-go.yaml":   {Data: []byte("version: 1\nname: mac-go\nprovider: machine\nneeds: [go]\n")},
	}
	m, envs, err := matrix.Load(func(name string) ([]byte, error) { return fs.ReadFile(repo, name) })
	must(t, err)
	all, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeAll})
	must(t, err)
	required, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeRequired})
	must(t, err)
	var got []string
	for _, e := range verification.Expect(all, required) {
		got = append(got, e.Environment+"/"+e.Suite+"/"+verification.CellKey(e.Cell)+"/"+map[bool]string{true: "required", false: "optional"}[e.Required])
	}
	want := []string{"linux-go/unit/os=linux/required", "linux-go/e2e/os=linux/optional", "mac-go/unit/os=macos/optional", "mac-go/e2e/os=macos/optional"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expect = %v, want %v", got, want)
	}
}

// TestBuildNeedsARealRun: placeholders for the cells that have not run are
// not results, and neither are the runs of the base check.
func TestBuildNeedsARealRun(t *testing.T) {
	exp := []verification.Expected{req("linux-go", linux(), "unit")}
	tests := []struct {
		name string
		runs []run
		exp  []verification.Expected
	}{
		{"expected cells, no run", nil, exp},
		{"expected cells, only runs of another commit", []run{{env: "linux-go", cell: linux(), results: pass("a::T1"), sha: "ffff000"}}, exp},
		{"only a run of the base check", []run{{env: "linux-go", cell: linux(), results: pass("a::T1"), overlay: true}}, exp},
		{"only optional cells expected", nil, []verification.Expected{optional(exp[0])}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			for _, r := range tt.runs {
				f.add(r)
			}
			rep := f.build(verification.Inputs{Expected: tt.exp})
			if rep.Status != domain.VerificationIncomplete {
				t.Errorf("status = %s, blocking %q", rep.Status, rep.Blocking)
			}
			blockingHas(t, rep, "no test results for "+sha)
		})
	}
}

// TestBuildIgnoresBaseCheckRuns: with stacked PRs the base is another PR's
// head, so a passing overlay run has the sha of the PR under test and must not
// erase its failure.
func TestBuildIgnoresBaseCheckRuns(t *testing.T) {
	f := newFixture(t)
	f.add(run{env: "linux-go", cell: linux(), results: fail("a::T1")})
	f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1"), overlay: true})
	rep := f.build(verification.Inputs{Expected: []verification.Expected{req("linux-go", linux(), "unit")}})
	if rep.Status != domain.VerificationFailed || rep.Results[0].Attempts != 1 || rep.Results[0].Status != "failed" || len(rep.Flaky) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestBuildTaskWithoutCriteria(t *testing.T) {
	f := newFixture(t)
	f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1")})
	rep := f.build(verification.Inputs{Task: true})
	if rep.Status != domain.VerificationFailed {
		t.Fatalf("status = %s", rep.Status)
	}
	blockingHas(t, rep, "the task lists no acceptance criteria")
	if rep := f.build(verification.Inputs{}); rep.Status != domain.VerificationPassed {
		t.Errorf("a change without a task: %s, %q", rep.Status, rep.Blocking)
	}
}
