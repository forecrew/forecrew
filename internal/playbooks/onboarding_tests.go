package playbooks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"uuid"

	"github.com/forecrew/forecrew/internal/actionsprovider"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/matrix"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
	"github.com/forecrew/forecrew/internal/workflow"
)

// PlaybookOnboardingTests is the playbook that proposes the tests of an
// onboarded project and runs their baseline (packs/playbooks).
const PlaybookOnboardingTests = "onboarding-tests"

// testRoles are the roles of the test part of onboarding; it needs both to
// have an entry under roles in forecrew.yaml.
var testRoles = []string{"test-architect", "environment-engineer"}

const (
	testsBranchPrefix = "agent/onboarding-tests-"
	labelBaseline     = "forecrew:baseline"
	// maxIssueTests bounds the failing tests an issue names.
	maxIssueTests = 20
	// maxIssueText bounds the name of a test or an environment in an issue.
	maxIssueText = 120
)

// testsConfigured reports whether forecrew.yaml configures the roles of the
// test part. Without them onboarding stops after the profile, as before.
func (a *actions) testsConfigured() bool {
	if a.Config == nil || a.Runs == nil {
		return false
	}
	for _, r := range testRoles {
		if _, ok := a.Config.Roles[r]; !ok {
			return false
		}
	}
	return true
}

// startTestsOutput is the output of onboarding.start_tests.
type startTestsOutput struct {
	Started bool      `json:"started"`
	RunID   uuid.UUID `json:"run_id,omitzero"`
}

// startTests is the action onboarding.start_tests. When the test roles are
// configured it starts the onboarding-tests playbook as a run of its own, with
// the call's key as its ID, so a repeated call finds it; that run moves the
// project to onboarded when its baseline ran. Without the roles there is no
// test part: the project is onboarded here.
func (a *actions) startTests(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo    string `json:"repo"`
		Project string `json:"project"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if !a.testsConfigured() {
		if _, err := a.finishOnboarding(ctx, call); err != nil {
			return nil, err
		}
		return startTestsOutput{}, nil
	}
	id, err := uuid.Parse(call.Key)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	run, err := a.Runs.Start(ctx, PlaybookOnboardingTests, workflow.StartRequest{
		ID: id, AccountID: call.Run.AccountID, ProjectID: call.Run.ProjectID,
		Inputs: map[string]any{"repo": with.Repo, "project": with.Project},
	})
	if err != nil {
		return nil, fmt.Errorf("start the tests of %s: %w", with.Project, err)
	}
	return startTestsOutput{Started: true, RunID: run.ID}, nil
}

// finishOnboarding is the action onboarding.finish. It moves the run's project from onboarding to onboarded; a
// project that is onboarded already stays (a refresh, or a call repeated).
func (a *actions) finishOnboarding(ctx context.Context, call workflow.ActionCall) (any, error) {
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	switch p.Status {
	case domain.ProjectOnboarded:
	case domain.ProjectOnboarding:
		if err := a.Store.Projects().UpdateStatus(ctx, p.AccountID, p.ID, domain.ProjectOnboarding, domain.ProjectOnboarded); err != nil {
			return nil, fmt.Errorf("onboard %s: %w", p.Name, err)
		}
	default:
		return nil, fmt.Errorf("project %s is %s, not onboarding", p.Name, p.Status)
	}
	return map[string]string{"status": string(domain.ProjectOnboarded)}, nil
}

// testsBranchAction is the action onboarding.tests_branch. It outputs
// {branch}, a branch of its own for the run of onboarding-tests: the branch of
// an earlier run is merged but not deleted, and a job would continue it, so
// a refresh would never see that there is nothing to change. The name follows
// from the run's ID, so a repeated call gives the same one.
func (a *actions) testsBranchAction(_ context.Context, call workflow.ActionCall) (any, error) {
	if err := workflow.DecodeWith(call.With, &struct{}{}); err != nil {
		return nil, err
	}
	id := strings.ReplaceAll(call.Run.ID.String(), "-", "")
	return map[string]string{"branch": testsBranchPrefix + id[len(id)-12:]}, nil
}

// testsFeedback is the action onboarding.tests_feedback. It builds the
// feedback of a test architect or environment engineer job from the
// reviewer's with.human feedback and the validator's with.errors (each may be
// empty); the result is empty when there is nothing to say.
func (a *actions) testsFeedback(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Human  string `json:"human"`
		Errors string `json:"errors"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var parts []string
	if h := strings.TrimSpace(with.Human); h != "" {
		parts = append(parts, "A reviewer asked for changes to the test plan:\n"+h)
	}
	if e := strings.TrimSpace(with.Errors); e != "" {
		parts = append(parts, "What you left does not work. Fix .forecrew/matrix.yaml and the environment files:\n"+e)
	}
	return strings.Join(parts, "\n\n"), nil
}

// testsCheckOutput is the output of onboarding.tests_check.
type testsCheckOutput struct {
	Valid  bool   `json:"valid"`
	Errors string `json:"errors"` // empty when valid
}

// testsCheck is the action onboarding.tests_check. It decides whether the
// test plan can go on to review: with.errors is the output of matrix.plan, and
// with.smoke is the output of tests.smoke_result, with.architect and
// with.engineer are the escalations of the two jobs (null when they did not
// stop). The errors, as text for the next try, are what is
// wrong; the loop that runs it escalates when its tries run out.
func (a *actions) testsCheck(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Errors    string  `json:"errors"`
		Smoke     string  `json:"smoke"`
		Architect *string `json:"architect"`
		Engineer  *string `json:"engineer"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var problems []string
	for _, e := range []struct {
		role string
		text *string
	}{{"The test architect", with.Architect}, {"The environment engineer", with.Engineer}} {
		if e.text != nil && strings.TrimSpace(*e.text) != "" {
			problems = append(problems, e.role+" stopped: "+strings.TrimSpace(*e.text))
		}
	}
	for _, e := range []string{with.Errors, with.Smoke} {
		if e = strings.TrimSpace(e); e != "" {
			problems = append(problems, e)
		}
	}
	if len(problems) == 0 {
		return testsCheckOutput{Valid: true}, nil
	}
	return testsCheckOutput{Errors: strings.Join(problems, "\n")}, nil
}

// testsHeadOutput is the output of onboarding.tests_head.
type testsHeadOutput struct {
	Ref string `json:"ref"`
	// IfPushed is [head_sha] when the two jobs left a commit, else [].
	IfPushed []string `json:"if_pushed"`
}

// testsHead is the action onboarding.tests_head. with.head_sha is the head
// of the environment engineer's job: when it is set, the jobs changed the
// repo, and ref is that commit; when it is empty (nothing to change: the repo
// has its matrix and environments), ref is the head of the default branch, and
// there is nothing to review.
func (a *actions) testsHead(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo    string `json:"repo"`
		HeadSHA string `json:"head_sha"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.HeadSHA != "" {
		return testsHeadOutput{Ref: with.HeadSHA, IfPushed: []string{with.HeadSHA}}, nil
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	sha, err := a.Tracker.HeadSHA(ctx, with.Repo, p.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", p.DefaultBranch, with.Repo, err)
	}
	return testsHeadOutput{Ref: sha, IfPushed: []string{}}, nil
}

// baselineRefOutput is the output of onboarding.baseline_ref.
type baselineRefOutput struct {
	SHA string `json:"sha"`
}

// baselineRef is the action onboarding.baseline_ref: the commit the baseline
// runs on, with.merged (the merge commit of the tests PR) or, when there was
// no PR, the head of the default branch.
func (a *actions) baselineRef(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string `json:"repo"`
		Merged string `json:"merged"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Merged != "" {
		return baselineRefOutput{SHA: with.Merged}, nil
	}
	out, err := a.testsHead(ctx, workflow.ActionCall{Key: call.Key, Run: call.Run, With: map[string]any{"repo": with.Repo}})
	if err != nil {
		return nil, err
	}
	return baselineRefOutput{SHA: out.(testsHeadOutput).Ref}, nil
}

// smokeOutput is the output of tests.smoke_plan.
type smokeOutput struct {
	// Requests are the smoke jobs, one per new or changed environment, as
	// the items of a foreach of command job steps.
	Requests []smokeJob `json:"requests"`
	// IfActions is [repo] when an environment of the matrix runs on GitHub
	// Actions, so the workflow must be installed, else [].
	IfActions []string `json:"if_actions"`
}

// smokeJob is an item for the `from` of a command job step.
type smokeJob struct {
	Environment domain.Environment `json:"environment"`
	Needs       []string           `json:"needs"`
	Command     domain.CommandSpec `json:"command"`
}

// smokeCommand runs the versions commands of env, so a tool that is missing
// from the image fails the job's command; an environment without any runs a
// command that does nothing.
func smokeCommand(env domain.Environment) domain.CommandSpec {
	var cmds []string
	for _, tool := range slices.Sorted(maps.Keys(env.Versions)) {
		cmds = append(cmds, env.Versions[tool])
	}
	if len(cmds) == 0 {
		return domain.CommandSpec{Shell: "echo smoke"}
	}
	return domain.CommandSpec{Shell: strings.Join(cmds, " && ")}
}

// testsSmokePlan is the action tests.smoke_plan. It reads the matrix at
// with.ref and outputs a command job request for every environment that
// with.environments (the environment engineer's output) lists as added or
// changed: building it records its versions commands, and the job's command
// runs them again, so a failing one is an error of the plan (tests.smoke_result).
// The request is the one of the first cell that uses the environment, so it
// lands on the worker the real runs do. A workflow_dispatch needs the workflow
// on the default branch, so while it is not there the environments on GitHub
// Actions get no smoke job: the baseline runs them once the PR is merged.
// A matrix that does not load gives no requests: matrix.plan reports why.
func (a *actions) testsSmokePlan(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo string `json:"repo"`
		Ref  string `json:"ref"`
		// Environments is the list of the environment engineer's output;
		// only name and change are read.
		Environments []struct {
			Name     string `json:"name"`
			Change   string `json:"change"`
			Provider string `json:"provider"`
			Reason   string `json:"reason"`
		} `json:"environments"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Repo == "" || with.Ref == "" {
		return nil, errors.New("with: repo and ref are required")
	}
	out := smokeOutput{Requests: []smokeJob{}, IfActions: []string{}}
	m, envs, err := matrix.Load(a.repoReader(ctx, with.Repo, with.Ref))
	if errors.Is(err, matrix.ErrInvalid) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	cells, err := matrix.Expand(m, envs, matrix.Scope{Set: matrix.ScopeAll})
	if err != nil {
		if errors.Is(err, matrix.ErrInvalid) {
			return out, nil
		}
		return nil, err
	}
	dispatchable, err := a.workflowOnDefaultBranch(ctx, call.Run, with.Repo)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, e := range with.Environments {
		wanted[e.Name] = e.Change == "added" || e.Change == "changed"
	}
	seen := map[string]bool{}
	for _, c := range cells {
		onActions := c.Environment.Provider == domain.ProviderActions
		if onActions && len(out.IfActions) == 0 {
			out.IfActions = append(out.IfActions, with.Repo)
		}
		if !wanted[c.Environment.Name] || seen[c.Environment.Name] || (onActions && !dispatchable) {
			continue
		}
		seen[c.Environment.Name] = true
		out.Requests = append(out.Requests, smokeJob{Environment: c.Environment, Needs: c.Needs, Command: smokeCommand(c.Environment)})
	}
	return out, nil
}

// workflowOnDefaultBranch reports whether the forecrew-matrix workflow is on
// the default branch of repo.
func (a *actions) workflowOnDefaultBranch(ctx context.Context, run domain.WorkflowRun, repo string) (bool, error) {
	p, err := a.Store.Projects().Get(ctx, run.AccountID, run.ProjectID)
	if err != nil {
		return false, fmt.Errorf("get project: %w", err)
	}
	_, err = a.Tracker.GetFile(ctx, repo, p.DefaultBranch, actionsprovider.WorkflowPath)
	switch {
	case errors.Is(err, github.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read %s of %s: %w", actionsprovider.WorkflowPath, repo, err)
	}
	return true, nil
}

// smokeResultOutput is the output of tests.smoke_result.
type smokeResultOutput struct {
	Errors string `json:"errors"` // empty when every versions command ran
}

// testsSmokeResult is the action tests.smoke_result. It reads the test runs
// the smoke jobs of with.runs stored (job IDs, outputs of job steps or the
// rounds of a foreach over one, as for tests.base_check_result) and outputs, as
// text for the roles, every environment whose command, the versions commands,
// exited with an error.
func (a *actions) testsSmokeResult(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Runs []jobRefs `json:"runs"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var problems []string
	for i, refs := range with.Runs {
		if len(refs) == 0 {
			return nil, fmt.Errorf("with: runs[%d]: no job_id; give job IDs or the outputs of job steps", i)
		}
		for _, id := range refs {
			stored, err := a.Store.TestRuns().List(ctx, call.Run.AccountID, store.TestRunFilter{JobID: id})
			if err != nil {
				return nil, fmt.Errorf("list test runs of job %s: %w", id, err)
			}
			for _, r := range stored {
				if r.ExitCode != 0 {
					problems = append(problems, fmt.Sprintf("The versions commands of the environment %s exited with %d: a tool they name is missing or broken.", issueCode(cmp.Or(r.Environment, "host")), r.ExitCode))
				}
			}
		}
	}
	return smokeResultOutput{Errors: strings.Join(slices.Compact(problems), "\n")}, nil
}

// testPlanOutput and environmentsOutput are what onboarding.tests_proposal
// reads of the two jobs' outputs (packs/schemas/test-plan.json and
// environments.json).
type testPlanOutput struct {
	Summary string `json:"summary"`
	Axes    []struct {
		Name   string   `json:"name"`
		Values []string `json:"values"`
		Reason string   `json:"reason"`
	} `json:"axes"`
	MatrixChanges []struct {
		Part   string `json:"part"`
		Name   string `json:"name"`
		Change string `json:"change"`
		Reason string `json:"reason"`
	} `json:"matrix_changes"`
	Assumptions   []domain.StatedAssumption `json:"assumptions"`
	OpenQuestions []string                  `json:"open_questions"`
}

type environmentsOutput struct {
	Summary      string `json:"summary"`
	Environments []struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Change   string `json:"change"`
		Reason   string `json:"reason"`
	} `json:"environments"`
	Assumptions []domain.StatedAssumption `json:"assumptions"`
}

// testsProposal is the action onboarding.tests_proposal. It writes the title
// and body of the tests PR from with.plan (the test architect's output) and
// with.environments (the environment engineer's): the strategy, the axes and
// matrix changes with their reasons, the environments, the assumptions and the
// open questions. Text the agents wrote shows as list items of one line.
func (a *actions) testsProposal(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Plan         json.RawMessage `json:"plan"`
		Environments json.RawMessage `json:"environments"`
		Workflow     []string        `json:"workflow"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var plan testPlanOutput
	if err := json.Unmarshal(with.Plan, &plan); err != nil {
		return nil, fmt.Errorf("with: plan: %w", err)
	}
	var envs environmentsOutput
	if err := json.Unmarshal(with.Environments, &envs); err != nil {
		return nil, fmt.Errorf("with: environments: %w", err)
	}
	one := contextpack.OneLine
	var b strings.Builder
	b.WriteString("Adds the test strategy (`.forecrew/test-strategy.md`), the test matrix (`.forecrew/matrix.yaml`) and the environments (`.forecrew/environments/`) Forecrew proposes for this project.\n")
	b.WriteString("Merge this PR to approve them, close it to reject them, or ask for changes in Forecrew. After the merge Forecrew runs the existing tests on every target, and files the failures it finds as issues.\n")
	section(&b, "Strategy", []string{strings.TrimSpace(plan.Summary)}, "")
	var axes []string
	for _, x := range plan.Axes {
		axes = append(axes, fmt.Sprintf("`%s` (%s): %s", one(x.Name), one(strings.Join(x.Values, ", ")), one(x.Reason)))
	}
	section(&b, "Axes", axes, "- ")
	var changes []string
	for _, c := range plan.MatrixChanges {
		changes = append(changes, fmt.Sprintf("%s `%s` %s: %s", one(c.Part), one(c.Name), one(c.Change), one(c.Reason)))
	}
	section(&b, "Matrix changes", changes, "- ")
	section(&b, "Environments", []string{strings.TrimSpace(envs.Summary)}, "")
	var list []string
	for _, e := range envs.Environments {
		list = append(list, fmt.Sprintf("`%s` (%s, %s): %s", one(e.Name), one(e.Provider), one(e.Change), one(e.Reason)))
	}
	section(&b, "Environment files", list, "- ")
	if len(with.Workflow) > 0 {
		section(&b, "Workflow", []string{"Adds `.github/workflows/forecrew-matrix.yml`, which runs the jobs of the environments on GitHub Actions. It runs repo code with read access only."}, "")
	}
	section(&b, "Assumptions", assumptionLines(slices.Concat(plan.Assumptions, envs.Assumptions)), "- ")
	var questions []string
	for _, q := range plan.OpenQuestions {
		questions = append(questions, one(q))
	}
	section(&b, "Open questions", questions, "- ")
	return proposalOutput{Title: "Add the Forecrew test plan", Body: b.String()}, nil
}

// issuesOutput is the output of baseline.file_issues.
type issuesOutput struct {
	Issues []issueRef `json:"issues"`
}

type issueRef struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Created bool   `json:"created"`
}

// baselineIssues is the action baseline.file_issues. It files the failures of
// the baseline report with.report_id as issues of the project's repo,
// labeled forecrew:baseline: one per suite and environment that failed or
// errored (with the failing tests and the report), and one for all the flaky
// tests. A hidden marker in each body makes it idempotent: an open issue with
// the marker is left alone, so a repeated call, and a later baseline that
// fails the same way, file nothing twice.
func (a *actions) baselineIssues(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		ReportID uuid.UUID `json:"report_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	stored, err := a.Store.VerificationReports().Get(ctx, call.Run.AccountID, with.ReportID)
	if err != nil {
		return nil, fmt.Errorf("get verification report %s: %w", with.ReportID, err)
	}
	if stored.ProjectID != call.Run.ProjectID {
		return nil, fmt.Errorf("verification report %s belongs to another project", with.ReportID)
	}
	rep, err := verification.Parse(stored.Report)
	if err != nil {
		return nil, fmt.Errorf("verification report %s: %w", stored.ID, err)
	}
	p, err := a.Store.Projects().Get(ctx, stored.AccountID, stored.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	drafts, err := a.baselineDrafts(ctx, stored, rep)
	if err != nil {
		return nil, err
	}
	out := issuesOutput{Issues: []issueRef{}}
	if len(drafts) == 0 {
		return out, nil
	}
	if err := a.ensureLabel(ctx, p.Repo, labelBaseline, "fbca04", "A failure of the baseline test run"); err != nil {
		return nil, err
	}
	open, err := a.Tracker.ListOpenIssues(ctx, p.Repo, labelBaseline)
	if err != nil {
		return nil, fmt.Errorf("list the baseline issues of %s: %w", p.Repo, err)
	}
	for _, d := range drafts {
		if i := slices.IndexFunc(open, func(i github.Issue) bool { return strings.HasPrefix(i.Body, d.marker) }); i >= 0 {
			out.Issues = append(out.Issues, issueRef{Number: open[i].Number, URL: open[i].URL, Title: open[i].Title})
			continue
		}
		issue, err := a.Tracker.CreateIssue(ctx, p.Repo, github.IssueInput{Title: d.title, Body: d.marker + "\n" + d.body, Labels: []string{labelBaseline}})
		if err != nil {
			return nil, fmt.Errorf("file the baseline issue %q in %s: %w", d.title, p.Repo, err)
		}
		out.Issues = append(out.Issues, issueRef{Number: issue.Number, URL: issue.URL, Title: issue.Title, Created: true})
	}
	return out, nil
}

// issueDraft is an issue to file.
type issueDraft struct {
	marker, title, body string
}

// failureGroup is the failed or errored results of one suite in one
// environment, over its cells.
type failureGroup struct {
	suite, environment string
	results            []verification.Result
}

// failureGroups groups the failed and errored results of a report by suite and
// environment, in the order of the report.
func failureGroups(rep verification.Report) []*failureGroup {
	var groups []*failureGroup
	for _, r := range rep.Results {
		if r.Status != string(domain.TestRunFailed) && r.Status != string(domain.TestRunErrored) {
			continue
		}
		i := slices.IndexFunc(groups, func(g *failureGroup) bool { return g.suite == r.Suite && g.environment == r.Environment })
		if i < 0 {
			groups = append(groups, &failureGroup{suite: r.Suite, environment: r.Environment})
			i = len(groups) - 1
		}
		groups[i].results = append(groups[i].results, r)
	}
	return groups
}

// baselineDrafts turns the failures and flaky tests of a baseline report into
// issue drafts, failures first in the order of the report.
func (a *actions) baselineDrafts(ctx context.Context, stored domain.VerificationReport, rep verification.Report) ([]issueDraft, error) {
	link := a.reportLink(stored, rep)
	var drafts []issueDraft
	for _, g := range failureGroups(rep) {
		d, err := a.failureIssue(ctx, stored.AccountID, rep.SHA, g, link)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
	}
	if len(rep.Flaky) > 0 {
		drafts = append(drafts, flakyIssue(rep.Flaky, link))
	}
	return drafts, nil
}

// failureIssue is the issue of one suite that failed in one environment, with
// the failing tests of its runs.
func (a *actions) failureIssue(ctx context.Context, accountID uuid.UUID, sha string, g *failureGroup, link string) (issueDraft, error) {
	env := cmp.Or(g.environment, "host")
	var b strings.Builder
	fmt.Fprintf(&b, "The baseline run of the existing tests, on commit `%s`, failed for the suite %s in the environment %s.\n", sha, issueCode(g.suite), issueCode(env))
	var cells, tests []string
	for _, r := range g.results {
		if k := verification.CellKey(r.Cell); k != "" {
			cells = append(cells, issueCode(k))
		}
		names, err := a.failingTests(ctx, accountID, r.TestRun)
		if err != nil {
			return issueDraft{}, err
		}
		tests = append(tests, names...)
	}
	if len(cells) > 0 {
		fmt.Fprintf(&b, "\nCells: %s.\n", strings.Join(cells, ", "))
	}
	if len(tests) == 0 {
		b.WriteString("\nThe run left no failing test: the suite errored before it could report one.\n")
	} else {
		b.WriteString("\n### Failing tests\n\n")
		for _, t := range tests[:min(len(tests), maxIssueTests)] {
			b.WriteString("- " + t + "\n")
		}
		if len(tests) > maxIssueTests {
			b.WriteString("- and more, see the report\n")
		}
	}
	b.WriteString("\n" + link + "\n")
	return issueDraft{
		marker: baselineMarker(g.suite + "/" + env),
		title:  fmt.Sprintf("Baseline: %s fails in %s", cutIssueText(g.suite), cutIssueText(env)),
		body:   b.String(),
	}, nil
}

// failingTests returns the failed tests of the stored test run (a UUID as
// text; empty for a result that has none) as inline code, at most one more
// than an issue names.
func (a *actions) failingTests(ctx context.Context, accountID uuid.UUID, run string) ([]string, error) {
	if run == "" {
		return nil, nil
	}
	id, err := uuid.Parse(run)
	if err != nil {
		return nil, fmt.Errorf("test run %q of the report: %w", run, err)
	}
	fails, err := a.Store.TestRuns().Failures(ctx, accountID, id, maxIssueTests+1)
	if err != nil {
		return nil, fmt.Errorf("failures of test run %s: %w", id, err)
	}
	names := make([]string, len(fails))
	for i, f := range fails {
		names[i] = issueCode(f.Name)
		if f.Classname != "" {
			names[i] = issueCode(f.Classname + "::" + f.Name)
		}
	}
	return names, nil
}

// flakyIssue is the one issue for all the flaky tests.
func flakyIssue(flaky []verification.Flaky, link string) issueDraft {
	var b strings.Builder
	b.WriteString("These tests failed and then passed when their cell ran again in the baseline run of the existing tests, so they are flaky.\n\n")
	for _, f := range flaky[:min(len(flaky), maxIssueTests)] {
		envs := make([]string, len(f.Environments))
		for i, e := range f.Environments {
			envs[i] = issueCode(cmp.Or(e, "host"))
		}
		fmt.Fprintf(&b, "- %s in %s\n", issueCode(f.Test), strings.Join(envs, ", "))
	}
	if len(flaky) > maxIssueTests {
		b.WriteString("- and more, see the report\n")
	}
	b.WriteString("\n" + link + "\n")
	return issueDraft{marker: baselineMarker("flaky"), title: "Baseline: flaky tests", body: b.String()}
}

// reportLink says where the report is: its rendering when there is a public
// URL, always the commit and the report's ID.
func (a *actions) reportLink(stored domain.VerificationReport, rep verification.Report) string {
	text := fmt.Sprintf("Baseline report `%s` of commit `%s`", stored.ID, rep.SHA)
	if a.Config != nil && a.Config.Server.PublicBaseURL != "" && stored.ArtifactID != uuid.Nil() {
		return fmt.Sprintf("%s: %s/api/v1/artifacts/%s", text, a.Config.Server.PublicBaseURL, stored.ArtifactID)
	}
	return text + "."
}

// baselineMarker is the hidden text that says an issue is the one for key.
func baselineMarker(key string) string {
	key = strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-/", r)) {
			return r
		}
		return '_'
	}, key)
	return "<!-- forecrew-baseline " + key + " -->"
}

// cutIssueText shortens a name for a title.
func cutIssueText(s string) string {
	s = contextpack.OneLine(s)
	if r := []rune(s); len(r) > maxIssueText {
		return string(r[:maxIssueText-1]) + "…"
	}
	return s
}

// issueCode shows a name that came from the repo as inline code: its
// characters are not Markdown, and an @name in it mentions nobody.
func issueCode(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case unicode.IsControl(r) && !unicode.IsSpace(r):
			return -1
		}
		return r
	}, cutIssueText(s))
	if s == "" {
		return "-"
	}
	return "`" + s + "`"
}
