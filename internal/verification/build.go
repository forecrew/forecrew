package verification

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testreport"
)

// ErrInvalid is wrapped by the errors of bad input.
var ErrInvalid = errors.New("invalid verification input")

// ReportVersion is the version of the report format.
const ReportVersion = 1

// maxFailures bounds the failed results read per run when looking for flaky
// tests; a run with more failures than this is not flaky material anyway.
const maxFailures = 1000

// maxVersion bounds a tool version text.
const maxVersion = 200

var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Expected is a result the report waits for: one suite in one cell of an
// environment, from the matrix at the verified commit.
type Expected struct {
	Environment string            `json:"environment"`
	Cell        map[string]string `json:"cell"`
	Suite       string            `json:"suite"`
	// Required results block the merge when they fail or are missing.
	Required bool `json:"required"`
}

// MapTest is a test of the test engineer's test map.
type MapTest struct {
	Criterion string `json:"criterion"`
	Suite     string `json:"suite"`
	ID        string `json:"id"` // <classname>::<name>
}

// Untestable is a criterion no automated test can check, with the reason.
type Untestable struct {
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// BaseTest is a new test with whether it failed on the base branch (the
// output of tests.base_check_result).
type BaseTest struct {
	ID           string `json:"id"`
	FailedOnBase bool   `json:"failed_on_base"`
}

// Inputs are what Build needs besides the stored test runs.
type Inputs struct {
	// Expected lists the results the matrix asks for. Without it the stored
	// runs are all there is, and all of them are required.
	Expected []Expected
	// Task says the change belongs to a task: a task with no criteria
	// blocks, since nothing says what the change must do.
	Task bool
	// Criteria are the ids of the task's acceptance criteria.
	Criteria   []string
	Tests      []MapTest
	Untestable []Untestable
	BaseCheck  []BaseTest
}

// Expect lists the results of a matrix: every suite in every cell, required
// where the matrix says so.
func Expect(cells, required []matrix.Cell) []Expected {
	isRequired := map[string]bool{}
	for _, c := range required {
		for _, j := range c.Jobs() {
			isRequired[j.Key] = true
		}
	}
	var out []Expected
	for _, c := range cells {
		for _, j := range c.Jobs() {
			out = append(out, Expected{Environment: j.Environment.Name, Cell: j.Cell, Suite: j.Suite, Required: isRequired[j.Key]})
		}
	}
	return out
}

// Builder builds reports from the stored test runs.
type Builder struct {
	Store store.Store
}

// group is the test runs of one suite in one cell of an environment.
type group struct {
	exp  Expected
	runs []domain.TestRun // oldest first
}

func groupKey(environment string, cell map[string]string, suite string) string {
	return environment + "\x00" + CellKey(cell) + "\x00" + suite
}

// label names a result for humans.
func label(environment string, cell map[string]string, suite string) string {
	parts := []string{suite}
	if environment != "" {
		parts = append(parts, "in "+environment)
	}
	if k := CellKey(cell); k != "" {
		parts = append(parts, "("+k+")")
	}
	return strings.Join(parts, " ")
}

// Build returns the verification of the head sha of change. Only the stored
// test runs of the sha are read, and each run's environment, cell and suite
// are taken from the run, never from the caller: the provider that ran it
// computed them. Rules:
//
//   - the latest attempt of a cell and suite counts; a test that failed in
//     an earlier attempt and passes in the latest is flaky: listed, not
//     blocking;
//   - a required result that failed or errored blocks;
//   - a criterion with no test, unless it is listed untestable with a reason,
//     blocks, and so does one whose tests fail or did not run;
//   - a new test that passed on base blocks;
//   - the status is failed when something blocks, else incomplete when
//     required results are still missing, else passed.
func (b Builder) Build(ctx context.Context, change domain.Change, sha string, in Inputs) (Report, error) {
	sha = strings.ToLower(sha)
	if !shaPattern.MatchString(sha) {
		return Report{}, fmt.Errorf("%w: %q is not a commit SHA", ErrInvalid, sha)
	}
	groups, stored, err := b.groups(ctx, change, sha, in.Expected)
	if err != nil {
		return Report{}, err
	}
	bd := &builder{b: b, ctx: ctx, change: change, groups: groups, runResults: map[uuid.UUID]map[string]string{}}
	rep := Report{Version: ReportVersion, SHA: sha, NewTests: map[string]bool{}, Environments: map[string]EnvironmentInfo{}}
	bd.results(&rep)
	if in.Task && len(in.Criteria) == 0 {
		bd.hard = append(bd.hard, "the task lists no acceptance criteria")
	}
	if err := bd.flaky(&rep); err != nil {
		return Report{}, err
	}
	if err := bd.criteria(&rep, in); err != nil {
		return Report{}, err
	}
	bd.newTests(&rep, in.BaseCheck, in.Tests)
	if stored == 0 {
		bd.pending = append(bd.pending, "no test results for "+sha)
	}
	rep.Blocking = append(bd.hard, bd.pending...)
	switch {
	case len(bd.hard) > 0:
		rep.Status = domain.VerificationFailed
	case len(bd.pending) > 0:
		rep.Status = domain.VerificationIncomplete
	default:
		rep.Status = domain.VerificationPassed
	}
	return rep.normalize(), nil
}

// groups reads the runs of sha and groups them by environment, cell and
// suite, the expected results first and in their order, the others by key.
// stored counts the runs that were taken into account.
func (b Builder) groups(ctx context.Context, change domain.Change, sha string, expected []Expected) (out []*group, stored int, err error) {
	runs, err := Runs(ctx, b.Store, change.AccountID, change.ProjectID, change.ID, sha)
	if err != nil {
		return nil, 0, err
	}
	byKey := map[string]*group{}
	for _, e := range expected {
		k := groupKey(e.Environment, e.Cell, e.Suite)
		if _, dup := byKey[k]; !dup {
			byKey[k] = &group{exp: e}
			out = append(out, byKey[k])
		}
	}
	var extra []*group
	for _, r := range runs {
		stored++
		k := groupKey(r.Environment, r.Cell, r.Suite)
		g, ok := byKey[k]
		if !ok {
			g = &group{exp: Expected{Environment: r.Environment, Cell: r.Cell, Suite: r.Suite, Required: len(expected) == 0}}
			byKey[k] = g
			extra = append(extra, g)
		}
		g.runs = append(g.runs, r)
	}
	slices.SortStableFunc(extra, func(a, b *group) int {
		return cmp.Compare(groupKey(a.exp.Environment, a.exp.Cell, a.exp.Suite), groupKey(b.exp.Environment, b.exp.Cell, b.exp.Suite))
	})
	return append(out, extra...), stored, nil
}

// Runs returns the test runs of sha that say something about it, oldest
// first: the runs of the change (a zero change: only runs that belong to no
// pull request) and of none, without another pull request's runs and without
// base checks. A base check lays the new tests over the base branch, which
// with stacked PRs is another PR's head, so its runs (jobs with an overlay)
// say nothing about sha.
func Runs(ctx context.Context, st store.Store, accountID, projectID, changeID uuid.UUID, sha string) ([]domain.TestRun, error) {
	runs, err := st.TestRuns().List(ctx, accountID, store.TestRunFilter{ProjectID: projectID, SHA: sha})
	if err != nil {
		return nil, fmt.Errorf("list test runs of %s: %w", sha, err)
	}
	slices.SortStableFunc(runs, func(a, b domain.TestRun) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.Attempt, b.Attempt), strings.Compare(a.ID.String(), b.ID.String()))
	})
	out := runs[:0]
	for _, r := range runs {
		if r.ChangeID != uuid.Nil() && r.ChangeID != changeID {
			continue // a run reported for another PR
		}
		if BaseCheck(ctx, st, r) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// BaseCheck says whether the run belongs to a base check job.
func BaseCheck(ctx context.Context, st store.Store, r domain.TestRun) bool {
	j, err := st.Jobs().Get(ctx, r.AccountID, r.JobID)
	return err == nil && j.Spec.Overlay != nil
}

// Latest keeps the newest attempt of each suite in each cell of each
// environment, in the order of runs (which Runs gives oldest first).
func Latest(runs []domain.TestRun) []domain.TestRun {
	last := map[string]int{}
	for i, r := range runs {
		last[groupKey(r.Environment, r.Cell, r.Suite)] = i
	}
	var out []domain.TestRun
	for i, r := range runs {
		if last[groupKey(r.Environment, r.Cell, r.Suite)] == i {
			out = append(out, r)
		}
	}
	return out
}

func (g *group) latest() (domain.TestRun, bool) {
	if len(g.runs) == 0 {
		return domain.TestRun{}, false
	}
	return g.runs[len(g.runs)-1], true
}

type builder struct {
	b      Builder
	ctx    context.Context
	change domain.Change
	groups []*group
	// hard and pending are the reasons that fail the report and the ones
	// that leave it incomplete.
	hard, pending []string
	// runResults caches the worst status per test id of a run.
	runResults map[uuid.UUID]map[string]string
}

// results fills the results and environments and the reasons from them.
func (bd *builder) results(rep *Report) {
	for _, g := range bd.groups {
		x := Result{Environment: g.exp.Environment, Cell: g.exp.Cell, Suite: g.exp.Suite, Required: g.exp.Required, Status: ResultMissing, Attempts: len(g.runs)}
		name := label(x.Environment, x.Cell, x.Suite)
		run, ok := g.latest()
		if ok {
			x.Status, x.Counts, x.DurationMS, x.TestRun = string(run.Status), run.Counts, run.DurationMS, run.ID.String()
			if j, err := bd.b.Store.Jobs().Get(bd.ctx, run.AccountID, run.JobID); err == nil {
				x.Job = j.Ref()
			}
			env := rep.Environments[x.Environment]
			if env.Versions == nil {
				env.Versions = map[string]string{}
			}
			for tool, v := range run.Versions {
				if _, seen := env.Versions[tool]; !seen {
					env.Versions[tool] = cutLine(v, maxVersion)
				}
			}
			rep.Environments[x.Environment] = env
		}
		rep.Results = append(rep.Results, x)
		if !x.Required {
			continue
		}
		switch x.Status {
		case ResultMissing:
			bd.pending = append(bd.pending, name+" has no result yet")
		case string(domain.TestRunFailed):
			bd.hard = append(bd.hard, fmt.Sprintf("%s failed (%d failed tests)", name, x.Counts.Failed+x.Counts.Errored))
		case string(domain.TestRunErrored):
			bd.hard = append(bd.hard, name+" errored: no usable result")
		}
	}
}

// cutLine returns the first line of s, at most limit runes.
func cutLine(s string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return line
}

// resultsOf returns the worst status per test id of a run.
func (bd *builder) resultsOf(run domain.TestRun) (map[string]string, error) {
	if m, ok := bd.runResults[run.ID]; ok {
		return m, nil
	}
	rs, err := bd.b.Store.TestRuns().Results(bd.ctx, run.AccountID, run.ID)
	if err != nil {
		return nil, fmt.Errorf("results of test run %s: %w", run.ID, err)
	}
	m := map[string]string{}
	for _, r := range rs {
		id, st := testID(r), string(r.Status)
		if prev, ok := m[id]; !ok || rank(st) > rank(prev) {
			m[id] = st
		}
	}
	bd.runResults[run.ID] = m
	return m, nil
}

// testID names a result as the test map does: classname::name, or the name.
func testID(r domain.TestResult) string {
	if r.Classname == "" {
		return r.Name
	}
	return r.Classname + "::" + r.Name
}

// rank orders test statuses by how bad they are.
func rank(status string) int {
	switch status {
	case string(domain.TestErrored):
		return 4
	case string(domain.TestFailed):
		return 3
	case TestMissing:
		return 2
	case string(domain.TestSkipped):
		return 1
	}
	return 0
}

// synthetic names the results the worker adds for a command that failed or
// a report that could not be read: they are not tests.
var synthetic = []string{testreport.CommandResult, testreport.ReportResult, testreport.OversizedResult}

// flaky lists the tests that failed in an earlier attempt of a cell and
// passed in its latest one. A test that failed earlier and is skipped or
// missing in the latest attempt of a required cell blocks: it did not pass.
func (bd *builder) flaky(rep *Report) error {
	envs := map[string]map[string]bool{}
	for _, g := range bd.groups {
		run, ok := g.latest()
		if !ok || len(g.runs) < 2 {
			continue
		}
		now, err := bd.resultsOf(run)
		if err != nil {
			return err
		}
		failed, err := bd.failedBefore(g.runs[:len(g.runs)-1])
		if err != nil {
			return err
		}
		for _, id := range failed {
			switch st := now[id]; {
			case st == string(domain.TestPassed):
				if envs[id] == nil {
					envs[id] = map[string]bool{}
				}
				envs[id][g.exp.Environment] = true
			case st == string(domain.TestFailed) || st == string(domain.TestErrored):
				// the latest attempt fails it, which the result reports
			case g.exp.Required:
				bd.hard = append(bd.hard, fmt.Sprintf("%s failed in an earlier attempt of %s and did not pass in the latest",
					id, label(g.exp.Environment, g.exp.Cell, g.exp.Suite)))
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(envs)) {
		rep.Flaky = append(rep.Flaky, Flaky{Test: id, Environments: slices.Sorted(maps.Keys(envs[id]))})
	}
	return nil
}

// failedBefore returns the ids of the tests that failed or errored in runs.
func (bd *builder) failedBefore(runs []domain.TestRun) ([]string, error) {
	var ids []string
	for _, earlier := range runs {
		fails, err := bd.b.Store.TestRuns().Failures(bd.ctx, earlier.AccountID, earlier.ID, maxFailures)
		if err != nil {
			return nil, fmt.Errorf("failures of test run %s: %w", earlier.ID, err)
		}
		for _, f := range fails {
			if !slices.Contains(synthetic, f.Name) {
				ids = append(ids, testID(f))
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// testStatus is the worst result of a test over the results of its suite
// that are required (all of them, if none is). pending says a result it
// needs has no run yet.
func (bd *builder) testStatus(suite, id string) (status string, pending bool, err error) {
	var applicable []*group
	for _, g := range bd.groups {
		if g.exp.Suite == suite {
			applicable = append(applicable, g)
		}
	}
	if slices.ContainsFunc(applicable, func(g *group) bool { return g.exp.Required }) {
		applicable = slices.DeleteFunc(applicable, func(g *group) bool { return !g.exp.Required })
	}
	status = TestMissing
	if len(applicable) == 0 {
		return status, false, nil
	}
	status = string(domain.TestPassed)
	for _, g := range applicable {
		run, ok := g.latest()
		st := TestMissing
		if !ok {
			pending = true
		} else {
			res, err := bd.resultsOf(run)
			if err != nil {
				return "", false, err
			}
			if s, found := res[id]; found {
				st = s
			}
		}
		if rank(st) > rank(status) {
			status = st
		}
	}
	return status, pending, nil
}

// criteria checks every criterion of the task against the test map.
func (bd *builder) criteria(rep *Report, in Inputs) error {
	untestable := map[string]string{}
	for _, u := range in.Untestable {
		if u.Reason != "" {
			untestable[u.Criterion] = u.Reason
		}
	}
	rep.Criteria = map[string]Criterion{}
	ids := slices.Clone(in.Criteria)
	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Or(cmp.Compare(criterionNumber(a), criterionNumber(b)), cmp.Compare(a, b))
	})
	for _, id := range slices.Compact(ids) {
		c, err := bd.criterion(id, in.Tests, untestable[id])
		if err != nil {
			return err
		}
		rep.Criteria[id] = c
	}
	return nil
}

// criterion checks one criterion and records what blocks.
func (bd *builder) criterion(id string, tests []MapTest, untestable string) (Criterion, error) {
	c := Criterion{Tests: map[string]string{}, Untestable: untestable}
	var failed, absent, pending []string
	skippedOnly := true
	for _, t := range tests {
		if t.Criterion != id {
			continue
		}
		st, wait, err := bd.testStatus(t.Suite, t.ID)
		if err != nil {
			return c, err
		}
		c.Tests[t.ID] = st
		switch {
		case st == string(domain.TestFailed) || st == string(domain.TestErrored):
			failed = append(failed, t.ID)
		case st == TestMissing && wait:
			pending = append(pending, t.ID)
		case st == TestMissing:
			absent = append(absent, t.ID)
		}
		skippedOnly = skippedOnly && st != string(domain.TestPassed)
	}
	c.Status = CriterionUntested
	switch {
	case len(c.Tests) == 0:
		if untestable == "" {
			bd.hard = append(bd.hard, id+" has no test")
		}
	case len(failed) > 0:
		c.Status = string(domain.TestFailed)
		bd.hard = append(bd.hard, fmt.Sprintf("%s: %s failed", id, listTests(failed)))
	case len(absent) > 0:
		bd.hard = append(bd.hard, fmt.Sprintf("%s: %s did not run", id, listTests(absent)))
	case len(pending) > 0:
		bd.pending = append(bd.pending, fmt.Sprintf("%s: %s has no result yet", id, listTests(pending)))
	case skippedOnly:
		bd.hard = append(bd.hard, id+": all its tests were skipped")
	default:
		c.Status = string(domain.TestPassed)
	}
	return c, nil
}

// listTests names up to three tests.
func listTests(ids []string) string {
	slices.Sort(ids)
	if len(ids) > 3 {
		return strings.Join(ids[:3], ", ") + fmt.Sprintf(" and %d more", len(ids)-3)
	}
	return strings.Join(ids, ", ")
}

// newTests records the fail-on-base check: a new test that passed on base
// blocks, and one that has not been checked yet leaves the report incomplete.
// A test listed twice counts as passed on base if any entry says so.
func (bd *builder) newTests(rep *Report, checks []BaseTest, tests []MapTest) {
	for _, c := range checks {
		failed := c.FailedOnBase
		if prev, ok := rep.NewTests[c.ID]; ok {
			failed = failed && prev
		}
		rep.NewTests[c.ID] = failed
	}
	for _, id := range slices.Sorted(maps.Keys(rep.NewTests)) {
		if !rep.NewTests[id] {
			bd.hard = append(bd.hard, fmt.Sprintf("new test %s passed on the base branch", id))
		}
	}
	var unchecked []string
	for _, t := range tests {
		if _, ok := rep.NewTests[t.ID]; !ok && !slices.Contains(unchecked, t.ID) {
			unchecked = append(unchecked, t.ID)
		}
	}
	if len(unchecked) > 0 {
		bd.pending = append(bd.pending, fmt.Sprintf("new test %s has not been checked on the base branch yet", listTests(unchecked)))
	}
}
