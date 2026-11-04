package actionsprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"mime"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testreport"
)

// jobRun follows one job.
type jobRun struct {
	p     *Provider
	w     domain.Worker
	j     domain.Job
	spec  domain.JobSpec
	log   *slog.Logger
	start time.Time
	// commit is the full SHA the job asked for, "" when it asked for a
	// branch (the run's own head is the commit then).
	commit string
}

// runJob dispatches the workflow for a leased job and follows it to the
// end. It always ends the lease: it completes the job, releases it, or
// leaves it to expire when forecrewd is shutting down.
func (p *Provider) runJob(ctx context.Context, w domain.Worker, j domain.Job) {
	r := &jobRun{p: p, w: w, j: j, spec: j.Spec, start: p.cfg.Now(),
		log: p.cfg.Logger.With("account_id", j.AccountID, "project_id", j.ProjectID, "job_id", j.ID, "ref", j.Ref(), "repo", j.Spec.Repo)}
	r.log.InfoContext(ctx, "actions job leased", "attempt", j.Attempts+1)
	project, err := p.cfg.Store.Projects().Get(ctx, j.AccountID, j.ProjectID)
	if err != nil {
		r.release(ctx, "get project: %v", err)
		return
	}
	pl, err := plan(r.spec, project.DefaultBranch)
	if err != nil {
		r.fail(ctx, "%v", err)
		return
	}
	r.commit = pl.Commit
	if j.Attempts > 0 && r.resume(ctx) {
		return
	}
	r.event(ctx, domain.EventStatus, "dispatching")
	runID, err := p.cfg.GitHub.DispatchWorkflow(ctx, r.spec.Repo, WorkflowFile, pl.Ref, pl.Inputs)
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, github.ErrNotFound):
		r.fail(ctx, "%s is not installed on %s of %s: onboarding installs it", WorkflowFile, pl.Ref, r.spec.Repo)
		return
	case errors.Is(err, github.ErrInvalid):
		r.fail(ctx, "GitHub refused to dispatch %s: %v", WorkflowFile, err)
		return
	case errors.Is(err, github.ErrForbidden):
		r.fail(ctx, "the GitHub App may not run workflows on %s: it needs the Actions read and write permission: %v", r.spec.Repo, err)
		return
	case errors.Is(err, github.ErrNotInstalled):
		r.fail(ctx, "the GitHub App is not installed on %s", r.spec.Repo)
		return
	case err != nil:
		r.release(ctx, "dispatch: %v", err)
		return
	}
	if runID == 0 && !r.find(ctx, &runID) {
		return
	}
	r.follow(ctx, runID)
}

// lookup returns the newest run in runs whose title names the job and that
// was created after since (zero: any time).
func (r *jobRun) lookup(runs []github.WorkflowRun, since time.Time) (github.WorkflowRun, bool) {
	for _, run := range runs { // newest first
		if run.CreatedAt.After(since) && strings.Contains(run.Title, r.j.ID.String()) {
			return run, true
		}
	}
	return github.WorkflowRun{}, false
}

// find looks for the run a dispatch started, for a GitHub that did not
// return the run's id. It reports whether it found it; otherwise the job has
// been released or ctx has ended.
func (r *jobRun) find(ctx context.Context, id *int64) bool {
	for range findTries {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.p.cfg.FindDelay):
		}
		runs, err := r.p.cfg.GitHub.ListWorkflowRuns(ctx, r.spec.Repo, WorkflowFile)
		if err != nil {
			r.log.WarnContext(ctx, "list workflow runs", "error", err)
			continue
		}
		if run, ok := r.lookup(runs, r.start.Add(-time.Minute)); ok {
			*id = run.ID
			return true
		}
	}
	r.release(ctx, "the dispatched workflow run was not found")
	return false
}

// resume looks for a run an earlier attempt of the job dispatched (forecrewd
// restarted, or the attempt was released) and follows it instead of starting
// another. It reports whether the job has been handled; false means to
// dispatch. A run that already ended without a usable result is not resumed:
// the job gets a fresh run, so retry-once holds.
func (r *jobRun) resume(ctx context.Context) bool {
	runs, err := r.p.cfg.GitHub.ListWorkflowRuns(ctx, r.spec.Repo, WorkflowFile)
	if err != nil {
		if ctx.Err() == nil {
			r.release(ctx, "look for an earlier workflow run: %v", err)
		}
		return true
	}
	run, ok := r.lookup(runs, time.Time{})
	if !ok {
		return false
	}
	r.log.InfoContext(ctx, "resuming an earlier workflow run", "run_id", run.ID, "status", run.Status)
	if !run.CreatedAt.IsZero() {
		r.start = run.CreatedAt // the timeout counts from the dispatch
	}
	if !run.Completed() {
		r.follow(ctx, run.ID)
		return true
	}
	if r.finish(ctx, run, true) {
		r.start = r.p.cfg.Now()
		return false
	}
	return true
}

// follow reads the run until it ends, keeping the lease alive.
func (r *jobRun) follow(ctx context.Context, runID int64) {
	p := r.p
	wake, done := p.watch(runID)
	defer done()
	limit := time.Duration(r.spec.Timeout)
	if limit <= 0 {
		limit = DefaultMaxRunTime
	}
	deadline := r.start.Add(limit)
	announced, failures := false, 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// The run keeps going: the lease expires, the job is queued again
			// and its next attempt resumes the run.
			return
		case <-wake:
		case <-timer.C:
		}
		timer.Reset(p.cfg.PollInterval)
		if _, err := queue.New(p.cfg.Store.Jobs()).Extend(ctx, r.j.AccountID, r.j.ID, r.w.ID); errors.Is(err, store.ErrLeaseLost) {
			r.log.InfoContext(ctx, "job ended, canceling its workflow run", "run_id", runID)
			r.cancelRun(runID)
			return
		} else if err != nil && ctx.Err() == nil {
			r.log.WarnContext(ctx, "extend lease", "error", err)
		}
		run, err := p.cfg.GitHub.GetWorkflowRun(ctx, r.spec.Repo, runID)
		switch {
		case ctx.Err() != nil:
			continue
		case errors.Is(err, github.ErrNotFound):
			r.release(ctx, "workflow run %d no longer exists", runID)
			return
		case err != nil:
			if failures++; failures >= maxPollErrors {
				r.release(ctx, "reading workflow run %d: %v", runID, err)
				return
			}
			r.log.WarnContext(ctx, "read workflow run", "run_id", runID, "error", err)
			continue
		}
		failures = 0
		if !announced {
			announced = true
			r.event(ctx, domain.EventLog, "workflow run: "+run.URL)
			r.event(ctx, domain.EventStatus, "running")
		}
		if run.Completed() {
			r.event(ctx, domain.EventStatus, "collecting")
			r.finish(ctx, run, false)
			return
		}
		if p.cfg.Now().After(deadline) {
			r.cancelRun(runID)
			r.timedOut(ctx, run, limit)
			return
		}
	}
}

// cancelRun cancels the workflow run, best effort.
func (r *jobRun) cancelRun(runID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.p.cfg.GitHub.CancelWorkflowRun(ctx, r.spec.Repo, runID); err != nil {
		r.log.Warn("cancel workflow run", "run_id", runID, "error", err)
	}
}

// artifactName is the name of the artifact the workflow uploads for the job.
func (r *jobRun) artifactName() string { return ArtifactPrefix + r.j.ID.String() }

// finish turns the ended run into the job's result. With fresh set, a run
// that left no usable result is not turned into one: finish returns true and
// the caller dispatches again.
func (r *jobRun) finish(ctx context.Context, run github.WorkflowRun, fresh bool) (dispatchAgain bool) {
	p := r.p
	arts, err := p.cfg.GitHub.ListRunArtifacts(ctx, r.spec.Repo, run.ID)
	if err != nil {
		r.release(ctx, "list artifacts of workflow run %d: %v", run.ID, err)
		return false
	}
	var art *github.RunArtifact
	for i := range arts {
		if arts[i].Name == r.artifactName() && !arts[i].Expired {
			art = &arts[i]
		}
	}
	if art == nil {
		if fresh {
			return true
		}
		r.noResult(ctx, run, "the run uploaded no artifact")
		return false
	}
	data, err := p.cfg.GitHub.DownloadArtifact(ctx, r.spec.Repo, art.ID, MaxUnpacked)
	switch {
	case ctx.Err() != nil:
		return false
	case errors.Is(err, github.ErrGone):
		r.errored(ctx, run, "the artifact of the run expired")
		return false
	case errors.Is(err, github.ErrTooLarge):
		r.errored(ctx, run, fmt.Sprintf("the artifact of the run is over %d bytes", MaxUnpacked))
		return false
	case err != nil:
		r.release(ctx, "download the artifact of workflow run %d: %v", run.ID, err)
		return false
	}
	files, err := unpack(data, p.cfg.MaxArtifactSize, MaxUnpacked)
	if err != nil {
		r.errored(ctx, run, err.Error())
		return false
	}
	if len(files.Skipped) > 0 {
		r.event(ctx, domain.EventLog, "skipped in the artifact: "+joinLimited(files.Skipped))
	}
	var res result
	i := slices.IndexFunc(files.Files, func(f file) bool { return f.Name == ResultFile })
	if i < 0 {
		if fresh {
			return true
		}
		r.noResult(ctx, run, "the artifact holds no "+ResultFile)
		return false
	}
	if err := json.Unmarshal(files.Files[i].Data, &res); err != nil {
		r.errored(ctx, run, "the artifact's "+ResultFile+" is not valid: "+err.Error())
		return false
	}
	res.Message = testreport.Clip(testreport.CleanText(res.Message), maxMessage)
	files.Files = slices.DeleteFunc(files.Files, func(f file) bool { return f.Name == ResultFile || f.Name == RootMarker })
	switch res.Phase {
	case phaseSetup:
		r.fail(ctx, "environment setup failed: %s", res.Message)
	case phaseCommand:
		r.report(ctx, run, res, files.Files)
	default:
		r.errored(ctx, run, fmt.Sprintf("the artifact's %s has an unknown phase %q", ResultFile, res.Phase))
	}
	return false
}

// noResult handles a run without a usable result: a run that failed or was
// cancelled never reached the command, which is an infrastructure problem
// retried once; a run that succeeded without result is an errored suite.
func (r *jobRun) noResult(ctx context.Context, run github.WorkflowRun, why string) {
	if run.Conclusion == "success" {
		r.errored(ctx, run, why)
		return
	}
	msg := fmt.Sprintf("workflow run %d ended %s before the command ran (%s)", run.ID, run.Conclusion, why)
	if r.j.Attempts == 0 {
		r.release(ctx, "%s", msg)
		return
	}
	r.fail(ctx, "%s", msg)
}

// expected is the commit the run had to test: the one the job asked for by
// SHA, else the head the run was dispatched on. The workflow's own report of
// the commit is checked against it, never trusted instead of it.
func (r *jobRun) expected(run github.WorkflowRun) string {
	if r.commit != "" {
		return r.commit
	}
	return strings.ToLower(run.HeadSHA)
}

// timedOut ends the job whose run did not finish within limit.
func (r *jobRun) timedOut(ctx context.Context, run github.WorkflowRun, limit time.Duration) {
	msg := fmt.Sprintf("the workflow run did not finish within the timeout of %s and was cancelled", limit)
	res := store.JobResult{Status: domain.JobFailed, Error: msg}
	if sha := r.expected(run); sha != "" {
		res.HeadSHA = sha
		res.TestRuns = []store.TestRunReport{r.erroredReport(msg, -1, 0, nil)}
	}
	r.complete(ctx, res)
}

// errored completes the job as succeeded with one errored test run on the
// expected commit, the way a worker reports a command that left no usable
// report.
func (r *jobRun) errored(ctx context.Context, run github.WorkflowRun, why string) {
	sha := r.expected(run)
	if sha == "" {
		r.fail(ctx, "%s", why)
		return
	}
	r.complete(ctx, store.JobResult{
		Status: domain.JobSucceeded, HeadSHA: sha,
		TestRuns: []store.TestRunReport{r.erroredReport(why, -1, 0, nil)},
	})
}

func (r *jobRun) erroredReport(msg string, exit int, durationMS int64, versions map[string]string) store.TestRunReport {
	o := testreport.Errored(msg, nil)
	return reportOf(r.spec.Suite, o, domain.TestRun{ExitCode: exit, DurationMS: durationMS, Versions: versions, ReportArtifacts: []string{}})
}

// reportOf makes the test run report of an outcome on top of run.
func reportOf(suite string, o testreport.Outcome, run domain.TestRun) store.TestRunReport {
	run.Suite, run.Status, run.Counts = suite, o.Status, o.Counts
	if run.Versions == nil {
		run.Versions = map[string]string{}
	}
	rep := store.TestRunReport{Run: run}
	for _, c := range o.Results {
		rep.Results = append(rep.Results, domain.TestResult{Name: c.Name, Classname: c.Classname, Status: c.Status, DurationMS: c.DurationMS, Message: c.Message})
	}
	return rep
}

// Phases of result.json.
const (
	phaseSetup   = "setup"
	phaseCommand = "command"
)

// result is the forecrew-out/result.json of the artifact, written by the
// workflow. It is data of the repo's workflow: every field is bounded.
type result struct {
	Phase        string            `json:"phase"`
	HeadSHA      string            `json:"head_sha"`
	ExitCode     int               `json:"exit_code"`
	DurationMS   int64             `json:"duration_ms"`
	Versions     map[string]string `json:"versions"`
	Message      string            `json:"message"`
	ImageOS      string            `json:"image_os"`
	ImageVersion string            `json:"image_version"`
	RunnerOS     string            `json:"runner_os"`
	RunnerArch   string            `json:"runner_arch"`
}

// Bounds of the strings the workflow reports.
const (
	maxVersions    = 64
	maxVersionSize = 1024
	maxMessage     = 1024
)

// versions returns the tool versions of the run, plus the runner image the
// run used.
func (res result) versions() map[string]string {
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(res.Versions)) {
		if len(out) < maxVersions && k != "" {
			out[testreport.CleanText(testreport.Clip(k, maxVersionSize))] = testreport.CleanText(testreport.Clip(res.Versions[k], maxVersionSize))
		}
	}
	if res.ImageOS != "" {
		out["runner_image"] = testreport.CleanText(testreport.Clip(res.ImageOS, maxVersionSize))
		out["runner_image_version"] = testreport.CleanText(testreport.Clip(res.ImageVersion, maxVersionSize))
	}
	if res.RunnerOS != "" {
		out["runner_os"] = testreport.CleanText(testreport.Clip(res.RunnerOS+" "+res.RunnerArch, maxVersionSize))
	}
	return out
}

// report builds the test run from the files of a command that ran, stores
// them as artifacts and completes the job.
func (r *jobRun) report(ctx context.Context, run github.WorkflowRun, res result, files []file) {
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.Name, b.Name) })
	want := r.expected(run)
	if want == "" {
		r.fail(ctx, "GitHub did not report the commit the run tested")
		return
	}
	if !strings.EqualFold(res.HeadSHA, want) {
		r.errored(ctx, run, fmt.Sprintf("the run reports it tested commit %q, not the requested %s", testreport.Clip(res.HeadSHA, 64), want))
		return
	}
	head := want
	reports := []string{}
	var c testreport.Collector
	var limitErr error
	for _, f := range files {
		if !matchesAny(r.spec.Command.Reports, f.Name) {
			continue
		}
		stored, err := r.store(ctx, f)
		if err != nil {
			return
		}
		if stored {
			reports = append(reports, f.Name)
		}
		if limitErr == nil {
			cases, perr := testreport.Parse(bytes.NewReader(f.Data))
			limitErr = c.Add(f.Name, cases, perr)
		}
	}
	for _, f := range files {
		if !matchesAny(r.spec.Command.Reports, f.Name) {
			if _, err := r.store(ctx, f); err != nil {
				return
			}
		}
	}
	found := countMatches(r.spec.Command.Reports, files)
	var o testreport.Outcome
	if limitErr != nil {
		o = testreport.Errored(limitErr.Error(), nil)
	} else {
		o = c.Evaluate(testreport.Evaluation{Declared: r.spec.Command.Reports, Found: found, ExitCode: res.ExitCode})
	}
	rep := reportOf(r.spec.Suite, o, domain.TestRun{ExitCode: res.ExitCode, DurationMS: max(res.DurationMS, 0), Versions: res.versions(), ReportArtifacts: reports})
	r.complete(ctx, store.JobResult{Status: domain.JobSucceeded, HeadSHA: head, TestRuns: []store.TestRunReport{rep}})
}

func matchesAny(globs []string, name string) bool {
	return slices.ContainsFunc(globs, func(g string) bool {
		ok, err := doublestar.Match(path.Clean(g), name)
		return err == nil && ok
	})
}

func countMatches(globs []string, files []file) int {
	n := 0
	for _, f := range files {
		if matchesAny(globs, f.Name) {
			n++
		}
	}
	return n
}

// store saves f as an artifact of the job. It reports whether it was saved;
// the error is set, and the job already ended, when the job cannot go on.
func (r *jobRun) store(ctx context.Context, f file) (bool, error) {
	ev := r.p.cfg.Evidence
	if ev == nil {
		return false, nil
	}
	key := evidence.NewKey(r.j.AccountID, r.j.ID, f.Name)
	ctype := mime.TypeByExtension(path.Ext(f.Name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if err := ev.Put(ctx, key, bytes.NewReader(f.Data), int64(len(f.Data)), ctype); err != nil {
		r.release(ctx, "store artifact %s: %v", f.Name, err)
		return false, err
	}
	sum := sha256.Sum256(f.Data)
	a := domain.Artifact{
		AccountID: r.j.AccountID, ProjectID: r.j.ProjectID, JobID: r.j.ID, Name: f.Name, ContentType: ctype,
		Size: int64(len(f.Data)), SHA256: hex.EncodeToString(sum[:]), StorageKey: key,
	}
	replaced, err := r.p.cfg.Store.Artifacts().PutLeased(ctx, &a, r.w.ID)
	if err != nil {
		r.removeObject(key)
		if errors.Is(err, store.ErrLeaseLost) {
			r.log.WarnContext(ctx, "job ended while its artifacts were stored")
		} else {
			r.release(ctx, "record artifact %s: %v", f.Name, err)
		}
		return false, err
	}
	if replaced != "" {
		r.removeObject(replaced)
	}
	return true, nil
}

func (r *jobRun) removeObject(key string) {
	if err := r.p.cfg.Evidence.Delete(context.Background(), key); err != nil {
		r.log.Error("delete unused artifact object", "error", err)
	}
}

// joinLimited joins the first few of items for a log line.
func joinLimited(items []string) string {
	const most = 10
	out := ""
	for i, it := range items {
		if i == most {
			return out + fmt.Sprintf(", and %d more", len(items)-most)
		}
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}
