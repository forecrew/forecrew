package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// Defaults of the per-job timings.
const (
	defaultHeartbeat     = 30 * time.Second
	defaultFlushInterval = 2 * time.Second
	finalCallTimeout     = 2 * time.Minute
	maxListedPaths       = 20
)

// Modes git records for symlinks and submodules; the worker publishes
// neither.
const (
	modeSymlink = "120000"
	modeGitlink = "160000"
)

// errLeaseExpired is the cause of a job stopped because its lease ran out
// without a successful heartbeat: another worker may have the job by now.
var errLeaseExpired = errors.New("lease ended without a heartbeat, or too close to ending to push")

// errJobCanceled is the cause of a job stopped because forecrewd answered a
// heartbeat with cancel.
var errJobCanceled = errors.New("canceled by forecrewd")

// stopsJob reports whether err means the job must stop at once and its
// result be dropped: the lease is gone, the job was canceled or the token was
// revoked.
func stopsJob(err error) bool {
	return errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, errLeaseExpired) || errors.Is(err, errJobCanceled)
}

// leaseClock tracks when the job's lease ends, from the poll and heartbeat
// answers.
type leaseClock struct {
	mu      sync.Mutex
	expires time.Time
	now     func() time.Time
}

func (l *leaseClock) set(t time.Time) {
	l.mu.Lock()
	l.expires = t
	l.mu.Unlock()
}

func (l *leaseClock) get() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expires
}

// valid reports whether the lease has not ended yet.
func (l *leaseClock) valid() bool { return l.now().Before(l.get()) }

// pushMargin is the time that must be left on the lease to push: a push that
// starts just before the lease ends would finish after another worker may
// have taken the job.
const pushMargin = 30 * time.Second

// pushable reports whether the lease has at least pushMargin left.
func (l *leaseClock) pushable() bool { return l.now().Add(pushMargin).Before(l.get()) }

// leaseEnd returns when a lease ends: LeaseDuration after the worker sent
// the request that extended it, or the server's time when that is earlier. A
// server clock that runs ahead cannot make the lease longer than it is.
func leaseEnd(sent, server time.Time) time.Time {
	end := sent.Add(domain.LeaseDuration)
	if !server.IsZero() && server.Before(end) {
		return server
	}
	return end
}

// jobRun is the execution of one leased job.
type jobRun struct {
	w     *Worker
	spec  domain.JobSpec
	write bool // a writing role
	log   *slog.Logger
	red   *runtime.Redactor
	ev    *eventBuffer
	cache string // repo cache
	dir   string // job worktree
	lease *leaseClock
	stop  context.CancelCauseFunc

	attachMu sync.Mutex
	attach   []Attachment // marked by the agent, one per artifact name
}

// handleJob runs a leased job to its end and reports the result. ctx is the
// worker's run context; it ends when the shutdown grace period has passed.
func (w *Worker) handleJob(ctx context.Context, lease *Lease) {
	spec := lease.Job
	log := w.cfg.Logger.With("job_id", spec.JobID, "repo", spec.Repo, "role", spec.Role)
	secrets := map[string]string{"repo_token": spec.RepoToken}
	if spec.RepoToken != "" {
		secrets["git_auth"] = authHeader(spec.RepoToken)
	}
	for k, v := range spec.Env {
		secrets["env_"+k] = v
	}
	if spec.JobMCP != nil {
		secrets["job_mcp_token"] = spec.JobMCP.Token
	}
	red := runtime.NewRedactor(secrets)
	r := &jobRun{
		w: w, spec: spec, write: len(spec.WritePaths) > 0, log: log, red: red,
		ev: &eventBuffer{jobID: spec.JobID, client: w.cfg.Client, red: red, now: w.cfg.Now, log: log},
	}
	sent := lease.SentAt
	if sent.IsZero() {
		sent = w.cfg.Now()
	}
	r.lease = &leaseClock{now: w.cfg.Now, expires: leaseEnd(sent, lease.LeaseExpiresAt)}
	if validRepo(spec.Repo) {
		r.cache = filepath.Join(w.cfg.WorkDir, "repos", strings.ToLower(spec.Repo)+".git")
		r.dir = filepath.Join(w.cfg.WorkDir, "jobs", spec.JobID.String())
	}

	jobCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	r.stop = stop
	bgCtx, stopBg := context.WithCancel(jobCtx)
	var bg sync.WaitGroup
	bg.Go(func() { r.heartbeats(bgCtx, w.heartbeatInterval(lease), stop) })
	bg.Go(func() { r.ev.run(bgCtx, w.cfg.FlushInterval, stop) })
	bg.Go(func() { r.watchLease(bgCtx, stop) })

	log.Info("job started", "runtime", spec.Runtime)
	comp := r.execute(jobCtx)
	stopBg()
	bg.Wait()

	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalCallTimeout)
	defer cancel()
	if cause := context.Cause(jobCtx); cause != nil {
		log.Warn("job stopped, result dropped", "reason", cause)
		if !stopsJob(cause) {
			_ = r.ev.flush(final) // shutdown: keep the log up to date
		}
		return
	}
	if err := r.ev.flush(final); err != nil {
		log.Warn("job stopped while sending events, result dropped", "error", err)
		return
	}
	status, err := w.cfg.Client.Complete(final, spec.JobID, comp)
	switch {
	case errors.Is(err, ErrLeaseLost):
		log.Warn("result dropped, lease lost")
	case err != nil:
		log.Error("report job result", "error", err)
	default:
		log.Info("job reported", "result", comp.Status, "job_status", status)
	}
}

func (w *Worker) heartbeatInterval(lease *Lease) time.Duration {
	switch {
	case w.cfg.HeartbeatInterval > 0:
		return w.cfg.HeartbeatInterval
	case lease.HeartbeatInterval > 0:
		return time.Duration(lease.HeartbeatInterval) * time.Second
	}
	return defaultHeartbeat
}

// heartbeats extends the lease every interval until ctx is done. A cancel
// answer or a lost lease calls stop.
func (r *jobRun) heartbeats(ctx context.Context, interval time.Duration, stop context.CancelCauseFunc) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sent := r.w.cfg.Now()
		b, err := r.w.cfg.Client.Heartbeat(ctx, r.spec.JobID, r.attachments())
		if err == nil && b.Action == ActionContinue {
			r.lease.set(leaseEnd(sent, b.LeaseExpiresAt))
			r.marked(b.Attach)
		}
		switch {
		case ctx.Err() != nil:
			return
		case stopsJob(err):
			stop(err)
			return
		case err != nil:
			r.log.Warn("heartbeat failed", "error", err)
		case b.Action == ActionCancel:
			stop(errJobCanceled)
			return
		}
	}
}

// beat sends one heartbeat and returns why the job must stop (it was
// canceled or its lease is gone), or nil to go on. A failed heartbeat is
// tolerated: the lease clock still guards the push.
func (r *jobRun) beat(ctx context.Context) error {
	sent := r.w.cfg.Now()
	b, err := r.w.cfg.Client.Heartbeat(ctx, r.spec.JobID, r.attachments())
	switch {
	case stopsJob(err):
		return err
	case err != nil:
		r.log.Warn("extra heartbeat failed", "error", err)
	case b.Action == ActionCancel:
		return errJobCanceled
	default:
		r.lease.set(leaseEnd(sent, b.LeaseExpiresAt))
		r.marked(b.Attach)
	}
	return nil
}

// marked records the files a heartbeat answer says the agent marked. They
// come again until acknowledged, and a later mark of a name replaces the
// earlier one.
func (r *jobRun) marked(files []Attachment) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	for _, f := range files {
		r.attach = slices.DeleteFunc(r.attach, func(a Attachment) bool { return a.Name == f.Name })
		r.attach = append(r.attach, f)
	}
}

// attachments returns the files marked so far: what the next heartbeat
// acknowledges, and what is uploaded after the run.
func (r *jobRun) attachments() []Attachment {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	return slices.Clone(r.attach)
}

// watchLease stops the job when its lease ends without a successful
// heartbeat: from then on another worker may run it, so this one must not
// push.
func (r *jobRun) watchLease(ctx context.Context, stop context.CancelCauseFunc) {
	for {
		wait := r.lease.get().Sub(r.w.cfg.Now())
		if wait <= 0 {
			stop(errLeaseExpired)
			return
		}
		t := time.NewTimer(wait + time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// failed and retry build the completion of a job that did not succeed.
func (r *jobRun) failed(format string, args ...any) Completion {
	return Completion{Status: StatusFailed, Error: r.red.String(fmt.Sprintf(format, args...))}
}

func (r *jobRun) retry(format string, args ...any) Completion {
	return Completion{Status: StatusRetry, Error: r.red.String(fmt.Sprintf(format, args...))}
}

// validate checks the parts of the spec the worker passes to git.
func (r *jobRun) validate() error {
	s := r.spec
	for _, p := range s.WritePaths {
		// An invalid exclude would match nothing, and so allow what it was
		// meant to forbid.
		if !doublestar.ValidatePattern(strings.TrimPrefix(p, "!")) {
			return fmt.Errorf("invalid write_paths pattern %q", p)
		}
	}
	switch {
	case !validRepo(s.Repo):
		return fmt.Errorf("invalid repo %q", s.Repo)
	case !fullSHA.MatchString(s.BaseRef) && !validBranch(s.BaseRef):
		return fmt.Errorf("invalid base_ref %q", s.BaseRef)
	case r.write:
		if _, err := pushRef(s.Branch); err != nil {
			return err
		}
	case s.Branch != "" && !validBranch(s.Branch):
		return fmt.Errorf("invalid branch %q", s.Branch)
	}
	return nil
}

// execute runs the job and returns what to report. When the job was stopped
// from outside (context canceled) the returned completion is not used.
func (r *jobRun) execute(ctx context.Context) Completion {
	if err := r.validate(); err != nil {
		return r.failed("%v", err)
	}
	r.ev.text(domain.EventStatus, "cloning")
	g := r.w.git
	start, branchExists, err := g.prepare(ctx, checkout{
		URL: r.w.cfg.CloneURL(r.spec.Repo), Token: r.spec.RepoToken, Cache: r.cache, Dir: r.dir,
		BaseRef: r.spec.BaseRef, Branch: r.spec.Branch, Write: r.write,
	})
	defer g.remove(context.WithoutCancel(ctx), r.cache, r.dir)
	if err != nil {
		return r.retry("prepare workspace: %v", err)
	}

	prompt := r.spec.Prompt
	if r.spec.IncludeRepoFacts {
		r.ev.text(domain.EventStatus, "analyzing")
		if prompt, err = r.withFacts(ctx, prompt); err != nil {
			return r.retry("analyze repo: %v", err)
		}
	}

	r.ev.text(domain.EventStatus, "running")
	res, err := r.runAgent(ctx, prompt)
	if cause := r.uploadEvidence(ctx); cause != nil {
		r.stop(cause)
		return Completion{}
	}
	comp := Completion{Usage: res.Usage, CostUSD: res.CostUSD}
	if err != nil {
		comp.Status, comp.Error = StatusFailed, r.red.String(err.Error())
		if errors.Is(err, runtime.ErrNotInstalled) || errors.Is(err, runtime.ErrUnknownAdapter) {
			comp.Status = StatusRetry
		}
		return comp
	}
	comp.Output = res.Output

	// Whatever the agent did to its worktree or to git, only the files count:
	// the worker snapshots them itself and publishes its own commit.
	r.ev.text(domain.EventStatus, "checking")
	tree, err := g.snapshot(ctx, r.cache, r.dir, start)
	if errors.Is(err, errStaging) {
		// The agent left files git refuses; retrying would only repeat it.
		comp.Status, comp.Error, comp.Output = StatusFailed, r.red.String(err.Error()), nil
		return comp
	}
	if err != nil {
		return r.retry("%v", err)
	}
	changes, err := g.changes(ctx, r.cache, start, tree)
	if err != nil {
		return r.retry("%v", err)
	}
	if !r.write {
		if len(changes) > 0 {
			comp.Status, comp.Error, comp.Output = StatusFailed, "read-only role changed the checkout: "+listPaths(paths(changes)), nil
			return comp
		}
		comp.Status = StatusSucceeded
		return comp
	}
	head, done := r.publish(ctx, start, tree, changes, branchExists)
	if done != nil {
		done.Usage, done.CostUSD = comp.Usage, comp.CostUSD
		return *done
	}
	comp.Status, comp.HeadSHA = StatusSucceeded, head
	return comp
}

// withFacts appends the analyzer facts of the checkout to the prompt.
func (r *jobRun) withFacts(ctx context.Context, prompt string) (string, error) {
	facts, err := r.w.cfg.Analyzer.Analyze(ctx, r.dir)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode repo facts: %w", err)
	}
	return prompt + "\n\n## Repo facts\n\n```json\n" + string(b) + "\n```\n", nil
}

// runAgent runs the runtime adapter in the worktree and, when it returns,
// kills the processes it left behind.
func (r *jobRun) runAgent(ctx context.Context, prompt string) (runtime.Result, error) {
	adapter, err := r.w.cfg.Runtimes.Get(r.spec.Runtime)
	if err != nil {
		return runtime.Result{}, err
	}
	env := maps.Clone(r.spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	mark := r.spec.JobID.String()
	env[markEnv] = mark
	var gateway *runtime.JobMCP
	if m := r.spec.JobMCP; m != nil {
		// The token goes in the environment only; the runtime's MCP
		// configuration refers to the variable.
		env[runtime.JobMCPTokenEnv] = m.Token
		gateway = &runtime.JobMCP{URL: r.w.cfg.Client.JobMCPURL()}
	}
	r.w.reaper.agentStarted()
	res, err := adapter.Run(ctx, runtime.RunSpec{
		WorkDir: r.dir, Model: r.spec.Model, Prompt: prompt, OutputSchema: r.spec.OutputSchema,
		AllowedTools: r.spec.AllowedTools, MaxTurns: r.spec.MaxTurns,
		Timeout: time.Duration(r.spec.Timeout), Env: env, JobMCP: gateway,
	}, r.ev.add)
	if n := r.w.reaper.agentDone(mark); n > 0 {
		r.log.Warn("killed processes the agent left running", "count", n)
	}
	return res, err
}

// publish checks the changes of the snapshot against write_paths and pushes
// them as one commit of the worker on top of start. Symlinks and submodules
// are refused. It returns the head of the branch on the remote: the pushed
// commit, or, when nothing changed, the commit the existing remote branch
// (branchExists) already had; "" when there is no branch. Or it returns a
// completion when the job ends here.
func (r *jobRun) publish(ctx context.Context, start, tree string, changes []change, branchExists bool) (string, *Completion) {
	g := r.w.git
	if len(changes) == 0 {
		if branchExists {
			return start, nil
		}
		return "", nil
	}
	var special []string
	for _, c := range changes {
		if c.Mode == modeSymlink || c.Mode == modeGitlink {
			special = append(special, c.Path)
		}
	}
	if len(special) > 0 {
		c := r.failed("symlinks and submodules are not allowed, nothing pushed: %s", listPaths(special))
		return "", &c
	}
	if bad := outsideWritePaths(r.spec.WritePaths, paths(changes)); len(bad) > 0 {
		c := r.failed("changed files outside write_paths, nothing pushed: %s", listPaths(bad))
		return "", &c
	}
	head, err := g.commitTree(ctx, r.cache, tree, start)
	if err != nil {
		c := r.retry("%v", err)
		return "", &c
	}
	if stat, err := g.diffStat(ctx, r.cache, start, head); err == nil && stat != "" {
		r.ev.text(domain.EventLog, "diffstat: "+stat)
	}
	r.ev.text(domain.EventStatus, "pushing")
	if !r.lease.pushable() {
		r.stop(errLeaseExpired) // another worker may own the job now: push nothing
		return "", &Completion{}
	}
	// The cancel is otherwise learned at the next heartbeat, up to a
	// heartbeat interval away: ask once more before anything leaves the machine.
	if cause := r.beat(ctx); cause != nil {
		r.stop(cause)
		return "", &Completion{}
	}
	if err := g.push(ctx, r.cache, r.w.cfg.CloneURL(r.spec.Repo), r.spec.RepoToken, head, r.spec.Branch); err != nil {
		c := r.retry("%v", err)
		return "", &c
	}
	return head, nil
}

func listPaths(paths []string) string {
	if len(paths) > maxListedPaths {
		return strings.Join(paths[:maxListedPaths], ", ") + fmt.Sprintf(" and %d more", len(paths)-maxListedPaths)
	}
	return strings.Join(paths, ", ")
}
