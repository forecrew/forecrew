package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/testreport"
)

// Limits of a command job.
const (
	// maxLogLine cuts one line of command output in the job log.
	maxLogLine = 8 << 10
	// maxArtifacts is the most files a command job uploads.
	maxArtifacts   = 200
	versionTimeout = 30 * time.Second
	// walkTimeout and maxWalkEntries bound the search for reports and
	// artifacts, which a repo can make as large as it likes.
	walkTimeout = 30 * time.Second
)

// logEventOverhead is charged to the log limit for every event, besides its
// text: short lines cost memory and a database row as well (eventBuffer's
// flush assumes the same 128 bytes plus the newline).
const logEventOverhead = 129

// Variables for the tests: maxLogBytes is the most command output a job
// logs (the command keeps running, the rest of its output is dropped), and
// maxWalkEntries the most directory entries the search for reports and
// artifacts reads.
var (
	maxLogBytes    int64 = 8 << 20
	maxWalkEntries       = 200_000
)

// Names of the results the worker adds when a suite has no usable report.
const (
	commandResult   = "(command)"
	reportResult    = "(report)"
	reportClassname = "forecrew"
)

// executeCommand runs a command job: clone, overlay, environment setup, the
// command, then reports and artifacts. The job succeeds when the command ran
// and its results were reported; it fails on the environment, timeout and
// spec problems and retries on infrastructure ones.
func (r *jobRun) executeCommand(ctx context.Context) Completion {
	spec := r.spec
	if err := spec.Validate(); err != nil {
		return r.failed("%v", err)
	}
	env := spec.Environment
	switch {
	case env != nil && env.Provider == domain.ProviderActions:
		return r.failed("environment provider %s is not run by workers", env.Provider)
	case env != nil && env.Provider == domain.ProviderMachine && len(env.Services) > 0:
		return r.failed("services need provider docker; environment %s runs on the machine", env.Name)
	}
	start, workdir, done := r.prepareCommand(ctx)
	defer r.w.git.remove(context.WithoutCancel(ctx), r.cache, r.dir)
	if done != nil {
		return *done
	}

	mark := spec.JobID.String()
	if t := time.Duration(spec.Timeout); t > 0 {
		r.deadline = time.Now().Add(t) // build, setup and the command share it
	}
	r.w.reaper.agentStarted()
	sweep := sync.OnceFunc(func() {
		if n := r.w.reaper.agentDone(mark); n > 0 {
			r.log.Warn("killed processes the command left running", "count", n)
		}
		if r.docker != nil {
			r.docker.cleanup(ctx) // the containers and services die with it
		}
	})
	defer sweep()

	if env != nil && env.Provider == domain.ProviderDocker {
		dj, done := r.startDocker(ctx, mark)
		r.docker = dj
		if done != nil {
			return *done
		}
	}
	cmdEnv := r.commandEnv(mark)
	if done := r.setup(ctx, cmdEnv); done != nil {
		return *done
	}
	versions := r.versions(ctx, cmdEnv)

	r.ev.text(domain.EventStatus, "running")
	began := time.Now()
	exit, runErr := r.exec(ctx, r.commandProcess(workdir, cmdEnv))
	elapsed := time.Since(began)
	if ctx.Err() != nil {
		return Completion{}
	}
	// Nothing the command left behind may change files while they are checked
	// and uploaded.
	sweep()
	comp := Completion{Status: StatusSucceeded, HeadSHA: start}
	run := TestRun{Suite: spec.Suite, DurationMS: elapsed.Milliseconds(), ExitCode: exit.Code, Versions: versions}
	switch {
	case errors.Is(runErr, runtime.ErrTimeout):
		comp.Status, comp.Error = StatusFailed, r.red.String(runErr.Error())
		run.ExitCode = -1
		r.fillErrored(&run, "the command timed out: "+runErr.Error())
	case runErr != nil && !errors.Is(runErr, runtime.ErrRunFailed):
		comp.Status, comp.Error = StatusFailed, r.red.String("the command could not run: "+runErr.Error())
		run.ExitCode = -1
		r.fillErrored(&run, comp.Error)
	default:
		r.ev.text(domain.EventStatus, "collecting")
		if stop := r.collect(ctx, &run, &comp); stop {
			return comp
		}
	}
	run.Versions = cleanVersions(run.Versions)
	comp.TestRuns = []TestRun{run}
	return comp
}

// cleanVersions removes what Postgres refuses from the version outputs.
func cleanVersions(v map[string]string) map[string]string {
	for k, s := range v {
		v[k] = cleanText(s)
	}
	return v
}

// fillErrored makes run an errored run with one explanatory result.
func (r *jobRun) fillErrored(run *TestRun, msg string) {
	msg = cleanText(r.red.String(msg))
	run.Status = domain.TestRunErrored
	run.Counts = domain.TestCounts{Errored: 1}
	run.Results = []testreport.Case{{Name: reportResult, Classname: reportClassname, Status: domain.TestErrored, Message: msg}}
	if run.ReportArtifacts == nil {
		run.ReportArtifacts = []string{}
	}
}

// exec runs a process and streams its output as log events.
func (r *jobRun) exec(ctx context.Context, p runtime.Process) (runtime.Exit, error) {
	line := func(prefix string) func([]byte) {
		return func(b []byte) {
			s := string(b)
			if len(s) > maxLogLine {
				s = strings.ToValidUTF8(s[:maxLogLine], "") + "... [truncated]"
			}
			r.logLine(prefix + s)
		}
	}
	if left, ok := r.left(); !ok {
		return runtime.Exit{Code: -1}, fmt.Errorf("%w: the timeout of the job is used up", runtime.ErrTimeout)
	} else if left > 0 && (p.Timeout == 0 || left < p.Timeout) {
		p.Timeout = left
	}
	return runtime.Exec(ctx, p, line(""), line("stderr: "))
}

// left returns the time left of the job's timeout, 0 when it has none, and
// false when it is used up.
func (r *jobRun) left() (time.Duration, bool) {
	if r.deadline.IsZero() {
		return 0, true
	}
	d := time.Until(r.deadline)
	return d, d > 0
}

// logLine logs one line of command output, up to maxLogBytes per job. After
// that a single marker says so and the lines are dropped: a command that
// prints without end must not fill the worker's memory or the database.
func (r *jobRun) logLine(s string) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if r.logCut {
		return
	}
	if r.logBytes += int64(len(s)) + logEventOverhead; r.logBytes > maxLogBytes {
		r.logCut = true
		r.ev.text(domain.EventLog, fmt.Sprintf("[output truncated: the job log is limited to %d KiB, the command keeps running]", maxLogBytes>>10))
		return
	}
	r.ev.text(domain.EventLog, s)
}

// commandProcess builds the process of the spec's command.
func (r *jobRun) commandProcess(dir string, env []string) runtime.Process {
	c := r.spec.Command
	if c.Shell != "" {
		return r.shell(c.Shell, dir, env, 0)
	}
	p := runtime.Process{Path: c.Argv[0], Args: c.Argv[1:], Dir: dir, Env: env}
	if r.docker != nil {
		return r.docker.contain(p)
	}
	return p
}

// shell builds the process that runs a shell script on the host, or in the
// job container of a docker environment.
func (r *jobRun) shell(script, dir string, env []string, timeout time.Duration) runtime.Process {
	if r.docker != nil {
		return r.docker.contain(runtime.Process{Path: "sh", Args: []string{"-c", script}, Dir: dir, Env: env, Timeout: timeout})
	}
	return shellProcess(script, dir, env, timeout)
}

func shellProcess(script, dir string, env []string, timeout time.Duration) runtime.Process {
	p := runtime.Process{Path: "sh", Args: []string{"-c", script}, Dir: dir, Env: env, Timeout: timeout}
	if shellIsCmd() {
		p.Path, p.Args = "cmd", []string{"/c", script}
	}
	return p
}

// commandEnvNames are the variables of the worker's own environment a
// command inherits (LC_* too), compared in upper case. Everything else, the
// credentials of the worker's user (ANTHROPIC_API_KEY, GITHUB_TOKEN, ...)
// included, stays out: the command is code of the repo.
var commandEnvNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "TZ": true, "TMPDIR": true,
	"TEMP": true, "TMP": true, "SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true,
	"USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true,
}

// containerEnvNames are the variables of the worker's environment a docker
// environment's container inherits (LC_* too): the locale and time zone.
// Paths and users of the host would only break the image.
var containerEnvNames = map[string]bool{"LANG": true, "TZ": true}

// containerHome is the HOME of the job container: its user may have no entry
// in the image's passwd file, and / is not writable.
const containerHome = "/tmp"

// commandEnv is the environment of the command: the allowlisted part of the
// worker's own, then the environment's variables and the command's. It
// holds no secret of the job or the worker.
func (r *jobRun) commandEnv(mark string) []string {
	names := commandEnvNames
	if r.docker != nil {
		names = containerEnvNames // the host's PATH or HOME mean nothing in the container
	}
	env := filterEnv(r.w.cfg.Environ, func(name string) bool {
		n := strings.ToUpper(name)
		return names[n] || strings.HasPrefix(n, "LC_")
	})
	if r.docker != nil {
		env = append(env, "HOME="+containerHome)
	}
	add := func(m map[string]string) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			env = append(env, k+"="+m[k])
		}
	}
	if r.docker != nil {
		add(r.docker.env)
	}
	if e := r.spec.Environment; e != nil {
		add(e.Env)
	}
	add(r.spec.Command.Env)
	if r.docker != nil {
		return env // the container's processes are not the worker's children
	}
	return append(env, markEnv+"="+mark)
}

// versions runs the environment's version commands and returns the first
// line of each output; a failing command is recorded as its error. A docker
// environment adds the image digest and the operating system, which a
// version command of the same name replaces.
func (r *jobRun) versions(ctx context.Context, env []string) map[string]string {
	out := map[string]string{}
	e := r.spec.Environment
	if e == nil {
		return out
	}
	if r.docker != nil {
		maps.Copy(out, r.docker.facts(ctx, env))
	}
	for _, tool := range slices.Sorted(maps.Keys(e.Versions)) {
		out[tool] = r.firstLine(ctx, r.shell(e.Versions[tool], r.dir, env, versionTimeout))
	}
	return out
}

// firstLine runs a version command and returns the first line of its output,
// or why it failed.
func (r *jobRun) firstLine(ctx context.Context, p runtime.Process) string {
	var lines []string
	if left, ok := r.left(); ok && left > 0 && left < versionTimeout {
		p.Timeout = left
	}
	_, err := runtime.Exec(ctx, p, func(b []byte) { lines = append(lines, string(b)) }, nil)
	switch {
	case err != nil:
		return r.red.String("unavailable: " + err.Error())
	case len(lines) > 0:
		return r.red.String(lines[0])
	}
	return ""
}

// workdir returns the directory the command runs in: the spec's workdir
// inside the worktree, reached without crossing a symlink.
func (r *jobRun) workdir() (string, error) {
	w := r.spec.Command.Workdir
	if w == "" {
		return r.dir, nil
	}
	if !domain.RelativePath(w) {
		return "", fmt.Errorf("workdir %q must stay inside the repo", w)
	}
	if err := noSymlinks(r.dir, w); err != nil {
		return "", fmt.Errorf("workdir %q: %w", w, err)
	}
	dir := filepath.Join(r.dir, filepath.FromSlash(w))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("workdir %q is not a directory of the repo", w)
	}
	return dir, nil
}

// noSymlinks checks that no component of rel below root is a symlink.
func noSymlinks(root, rel string) error {
	cur := root
	for part := range strings.SplitSeq(rel, "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("crosses a symlink")
		}
	}
	return nil
}

// walkFS is the file system the search for reports reads: it stops after
// maxWalkEntries directory entries or when ctx ends, and remembers why.
type walkFS struct {
	fs.FS
	ctx  context.Context
	seen int
	err  error
}

var errWalkLimit = errors.New("too many files below the repo")

// Stat is os.DirFS's own: without it fs.Stat falls back to Open, which
// blocks on a FIFO.
func (w *walkFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(w.FS, name) }

func (w *walkFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if w.err == nil {
		w.err = w.ctx.Err()
	}
	if w.err != nil {
		return nil, w.err
	}
	entries, err := fs.ReadDir(w.FS, name)
	if w.seen += len(entries); w.seen > maxWalkEntries {
		w.err = errWalkLimit
		return nil, w.err
	}
	return entries, err
}

// files expands globs below root into regular files, as repo-relative
// paths with slashes, sorted. Directories are skipped and symlinks are not
// followed; symlinks, and anything behind one, are refused and returned in
// refused. The search is bounded in entries and time.
func files(ctx context.Context, root string, globs []string) (found, refused []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, walkTimeout)
	defer cancel()
	seen := map[string]bool{}
	fsys := &walkFS{FS: os.DirFS(root), ctx: ctx}
	for _, g := range globs {
		matches, gerr := doublestar.Glob(fsys, path.Clean(g), doublestar.WithNoFollow())
		if fsys.err != nil {
			return nil, nil, fmt.Errorf("glob %q: %w", g, fsys.err)
		}
		if gerr != nil {
			return nil, nil, fmt.Errorf("glob %q: %w", g, gerr)
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			if err := noSymlinks(root, m); err != nil {
				refused = append(refused, m)
				continue
			}
			if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(m))); err == nil && info.Mode().IsRegular() {
				found = append(found, m)
			}
		}
	}
	slices.Sort(found)
	slices.Sort(refused)
	return found, refused, nil
}

// collect uploads the reports and artifacts of the command, parses the
// reports into run and sets run's status. It returns true when the job must
// stop with comp (a lease lost, or an infrastructure failure to retry).
func (r *jobRun) collect(ctx context.Context, run *TestRun, comp *Completion) (stop bool) {
	c := r.spec.Command
	reports, refusedReports, err := files(ctx, r.dir, c.Reports)
	if err != nil {
		*comp = r.failed("%v", err)
		return true
	}
	artifacts, refusedArtifacts, err := files(ctx, r.dir, c.Artifacts)
	if err != nil {
		*comp = r.failed("%v", err)
		return true
	}
	if refused := append(refusedReports, refusedArtifacts...); len(refused) > 0 {
		r.ev.text(domain.EventLog, "skipped symlinks: "+listPaths(refused))
	}

	upload := slices.Clone(reports)
	for _, a := range artifacts {
		if !slices.Contains(upload, a) {
			upload = append(upload, a)
		}
	}
	if len(upload) > maxArtifacts {
		r.ev.text(domain.EventLog, fmt.Sprintf("uploading %d of %d files", maxArtifacts, len(upload)))
		upload = upload[:maxArtifacts]
	}
	uploaded := map[string]bool{}
	for _, name := range upload {
		switch err := r.upload(ctx, name); {
		case err == nil:
			uploaded[name] = true
		case stopsJob(err):
			r.stop(err)
			*comp = Completion{}
			return true
		case errors.Is(err, errSkipUpload):
			r.ev.text(domain.EventLog, fmt.Sprintf("not uploaded: %s: %v", name, err))
		default:
			*comp = r.retry("upload %s: %v", name, err)
			return true
		}
	}

	run.ReportArtifacts = []string{}
	for _, name := range reports {
		if uploaded[name] {
			run.ReportArtifacts = append(run.ReportArtifacts, name)
		}
	}
	cases, usable, problems, err := r.parseReports(reports)
	if err != nil {
		r.fillErrored(run, err.Error())
		return false
	}
	r.fillRun(run, c.Reports, len(reports), cases, usable, problems)
	return false
}

// errSkipUpload marks a file that cannot be uploaded but does not fail the
// job.
var errSkipUpload = errors.New("skipped")

// upload sends one file as the artifact name.
func (r *jobRun) upload(ctx context.Context, name string) error {
	if !domain.ValidArtifactName(name) {
		return fmt.Errorf("%w: not a valid artifact name", errSkipUpload)
	}
	_, err := r.w.cfg.Client.UploadArtifact(ctx, r.spec.JobID, name, filepath.Join(r.dir, filepath.FromSlash(name)))
	if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrEvidenceUnavailable) {
		return fmt.Errorf("%w: %w", errSkipUpload, err)
	}
	return err
}

// parseReports parses the report files one by one. It returns their cases,
// how many files parsed and why the others did not. It stops at the first
// report that takes the cases past the limits of a run (testreport.MaxCases
// and MaxTotalSize) and returns that as the error, so the memory a repo can
// make the worker use is bounded by the limits, not by its number of files.
func (r *jobRun) parseReports(reports []string) (cases []testreport.Case, usable int, problems []string, err error) {
	size := 0
	for _, name := range reports {
		parsed, err := parseReport(filepath.Join(r.dir, filepath.FromSlash(name)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("report %s: %v", name, err))
			continue
		}
		size += testreport.Size(parsed)
		if cases = append(cases, parsed...); len(cases) > testreport.MaxCases || size > testreport.MaxTotalSize {
			return nil, 0, nil, fmt.Errorf("the reports hold too many test cases: %w", testreport.Check(cases))
		}
		usable++
	}
	return cases, usable, problems, nil
}

// parseReport parses the JUnit file at p, which must be a regular file: a
// FIFO or a device would block the worker.
func parseReport(p string) ([]testreport.Case, error) {
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(p) // #nosec G304 -- a regular file below the job's workspace
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return testreport.Parse(f)
}

// prepareCommand clones the repo, applies the overlay and resolves the
// workdir. It returns the commit checked out and the directory to run in,
// or the completion that ends the job. The caller removes the workspace.
func (r *jobRun) prepareCommand(ctx context.Context) (start, workdir string, done *Completion) {
	spec := r.spec
	fail := func(c Completion) (string, string, *Completion) { return "", "", &c }
	r.ev.text(domain.EventStatus, "cloning")
	g := r.w.git
	co := checkout{
		URL: r.w.cfg.CloneURL(spec.Repo), Token: spec.RepoToken, Cache: r.cache, Dir: r.dir,
		BaseRef: spec.BaseRef, Branch: spec.Branch,
	}
	start, _, err := g.prepare(ctx, co)
	if err != nil {
		return fail(r.retry("prepare workspace: %v", err))
	}
	if o := spec.Overlay; o != nil {
		r.ev.text(domain.EventStatus, "overlay")
		if err := g.overlay(ctx, co, o.Ref, o.Paths); err != nil {
			if errors.Is(err, errOverlay) || errors.Is(err, errRefused) {
				return fail(r.failed("%v", err))
			}
			return fail(r.retry("%v", err))
		}
	}
	if workdir, err = r.workdir(); err != nil {
		return fail(r.failed("%v", err))
	}
	return start, workdir, nil
}

// setup runs the setup commands of a machine environment on the host. A docker
// environment ran them in the build of its image. It
// returns the completion that ends the job, or nil to go on.
func (r *jobRun) setup(ctx context.Context, env []string) *Completion {
	e := r.spec.Environment
	if e == nil || e.Provider != domain.ProviderMachine { // docker runs it in the image build
		return nil
	}
	for _, line := range e.Setup {
		r.ev.text(domain.EventStatus, "setup")
		exit, err := r.exec(ctx, shellProcess(line, r.dir, env, 0))
		switch {
		case ctx.Err() != nil:
			return &Completion{}
		case errors.Is(err, runtime.ErrTimeout):
			c := r.failed("environment setup %q: %v", line, err)
			return &c
		case err != nil:
			c := r.failed("environment setup %q failed with exit code %d: %v", line, exit.Code, err)
			return &c
		}
	}
	return nil
}

// oversized replaces the results of every test run of comp, which forecrewd
// refused as too large, with an errored run saying so.
func (r *jobRun) oversized(comp Completion) Completion {
	runs := slices.Clone(comp.TestRuns)
	for i := range runs {
		run := runs[i]
		run.Status, run.Counts = domain.TestRunErrored, domain.TestCounts{}
		r.fillErrored(&run, fmt.Sprintf("the %d test results are larger than forecrewd accepts (max_test_results_size)", len(run.Results)))
		run.Results[0].Name = testreport.OversizedResult
		runs[i] = run
	}
	comp.TestRuns = runs
	return comp
}

// fillRun sets the status, counts and results of run from the parsed cases
// of its reports: declared and found are the numbers of report globs and
// files, usable the files that parsed, problems why the others did not.
func (r *jobRun) fillRun(run *TestRun, declared []string, found int, cases []testreport.Case, usable int, problems []string) {
	switch {
	case len(declared) == 0:
		r.fillErrored(run, "the command declares no reports")
	case found == 0:
		r.fillErrored(run, "no report file matched "+strings.Join(declared, ", "))
	case usable == 0:
		r.fillErrored(run, strings.Join(problems, "; "))
	case len(cases) == 0:
		r.fillErrored(run, "the reports hold no test cases")
	default:
		if err := testreport.Check(cases); err != nil {
			r.fillErrored(run, err.Error())
			return
		}
		for i := range cases {
			cases[i].Name, cases[i].Classname, cases[i].Message = cleanText(cases[i].Name), cleanText(cases[i].Classname), cleanText(cases[i].Message)
		}
		for _, p := range problems {
			cases = append(cases, testreport.Case{Name: reportResult, Classname: reportClassname, Status: domain.TestErrored, Message: r.red.String(p)})
		}
		if exit := run.ExitCode; exit != 0 {
			// Tests that all passed under a failing command are not a pass.
			if _, status := testreport.Summarize(cases); status == domain.TestRunPassed {
				cases = append(cases, testreport.Case{Name: commandResult, Classname: reportClassname, Status: domain.TestErrored,
					Message: fmt.Sprintf("the command exited with code %d although no test failed", exit)})
			}
		}
		run.Counts, run.Status = testreport.Summarize(cases)
		run.Results = cases
	}
}
