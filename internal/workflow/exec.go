package workflow

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
)

// stepKey names a step run: the step's path in the playbook (loop and
// foreach rounds included, e.g. tasks/0/build/2/develop) and the attempt.
type stepKey struct {
	id      string
	attempt int
}

// exec replays one run.
type exec struct {
	e     *Engine
	run   domain.WorkflowRun
	p     *Playbook
	steps map[stepKey]domain.StepRun
	// ending is how the run ends while its finally steps run.
	ending domain.RunStatus
}

func (e *Engine) newExec(ctx context.Context, run domain.WorkflowRun) (*exec, error) {
	recorded, err := e.cfg.Store.StepRuns().ListByRun(ctx, run.AccountID, run.ID)
	if err != nil {
		return nil, err
	}
	x := &exec{e: e, run: run, p: e.cfg.Playbooks[run.Playbook], steps: map[stepKey]domain.StepRun{}}
	for _, sr := range recorded {
		x.steps[stepKey{sr.StepID, sr.Attempt}] = sr
	}
	return x, nil
}

// replay runs the playbook from the start: recorded steps return their
// outcome, the first unfinished one is driven forward. It returns nil when
// the run is done, errWaiting, a *runEnd or an error to retry later.
func (x *exec) replay(ctx context.Context) error {
	if x.p == nil {
		return &runEnd{domain.RunFailed, fmt.Sprintf("playbook %q is not loaded", x.run.Playbook)}
	}
	sc, err := x.scope()
	if err != nil {
		return &runEnd{domain.RunFailed, err.Error()}
	}
	return x.seq(ctx, x.p.Steps, "", sc)
}

// scope returns the scope of a run that has not run any step.
func (x *exec) scope() (*scope, error) {
	sc := &scope{inputs: map[string]any{}, steps: map[string]any{}, vars: map[string]any{}}
	if len(x.run.Inputs) > 0 {
		if err := json.Unmarshal(x.run.Inputs, &sc.inputs); err != nil {
			return nil, fmt.Errorf("inputs: %v", err)
		}
	}
	return sc, nil
}

// finally runs the finally steps of a run that is ending failed or
// canceled. Each is recorded like any step (as finally/<id>), so after a
// restart the recorded ones are not called again and the others continue. A
// failed one is logged. It returns an error only when the engine should try
// again later. The actions see ending as the run's status.
func (x *exec) finally(ctx context.Context, ending domain.RunStatus) error {
	if x.p == nil {
		return nil
	}
	sc, err := x.scope()
	if err != nil {
		return nil // the run ended on its inputs; the finally steps need them
	}
	x.ending = ending
	for _, s := range x.p.Finally {
		_, err := x.leaf(ctx, s, stepKey{"finally/" + s.ID, 1}, sc, "")
		var failed *stepFailed
		switch {
		case errors.As(err, &failed):
			x.e.cfg.Logger.Error("workflow finally step failed", "account_id", x.run.AccountID, "run_id", x.run.ID, "step", s.ID, "reason", failed.reason)
		case err != nil:
			return err
		}
	}
	return nil
}

// stopJobs cancels the jobs still in flight when a run ends, which only the
// other items of a parallel foreach can have, and fails their steps. A job
// that had finished gives its step its real outcome. A step run's ID is its
// job's, so a job created just before a crash is found too; other unfinished
// steps have no job. Finally steps are not touched: they run after this and
// continue after a restart.
func (x *exec) stopJobs(ctx context.Context) error {
	for k, sr := range x.steps {
		if (sr.Status != domain.StepRunning && sr.Status != domain.StepWaiting) || strings.HasPrefix(k.id, "finally/") {
			continue
		}
		err := x.e.cfg.Jobs.Cancel(ctx, x.run.AccountID, sr.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case errors.Is(err, domain.ErrInvalidTransition):
			err = x.recordFinished(ctx, &sr)
		case err != nil:
			err = fmt.Errorf("cancel job of step %s: %w", k.id, err)
		default:
			err = x.finish(ctx, &sr, nil, "canceled: the run ended")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// closeGates rejects the gates the run waits on as forecrewd, so nobody is
// asked about a run that is being canceled. A gate that was resolved meanwhile
// stays as it is.
func (x *exec) closeGates(ctx context.Context, reason string) error {
	by := domain.SystemActor(x.run.AccountID, domain.ForecrewdName)
	for _, sr := range x.steps {
		if sr.Status != domain.StepWaiting || sr.GateID == uuid.Nil() {
			continue
		}
		_, err := x.e.cfg.Gates.Resolve(ctx, by, sr.GateID, domain.GateRejected, reason)
		if err != nil && !errors.Is(err, domain.ErrInvalidTransition) && !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("reject gate of step %s: %w", sr.StepID, err)
		}
	}
	return nil
}

// recordFinished records the outcome of a job step whose job has finished.
func (x *exec) recordFinished(ctx context.Context, sr *domain.StepRun) error {
	job, err := x.e.cfg.Store.Jobs().Get(ctx, x.run.AccountID, sr.ID)
	if err != nil {
		return err
	}
	return x.jobDone(ctx, sr, job, job.Status)
}

// seq runs steps in order, recording each value in sc.
func (x *exec) seq(ctx context.Context, steps []Step, prefix string, sc *scope) error {
	for _, s := range steps {
		v, err := x.step(ctx, s, prefix+s.ID, sc)
		if err != nil {
			return err
		}
		sc.steps[s.ID] = v
	}
	return nil
}

func (x *exec) step(ctx context.Context, s Step, path string, sc *scope) (any, error) {
	switch s.Type {
	case StepForeach:
		return x.foreach(ctx, s, path, sc)
	case StepLoop:
		return x.loop(ctx, s, path, sc)
	}
	feedback := ""
	for attempt := 1; ; attempt++ {
		v, err := x.leaf(ctx, s, stepKey{path, attempt}, sc, feedback)
		if err == nil && s.Type == StepGate {
			err = gateOutcome(v, path, sc)
		}
		var failed *stepFailed
		if !errors.As(err, &failed) {
			return v, err
		}
		reason := fmt.Sprintf("step %s failed: %s", path, failed.reason)
		if s.OnFailure != Escalate {
			return nil, &runEnd{domain.RunFailed, reason}
		}
		if feedback, err = x.escalate(ctx, path, attempt, reason); err != nil {
			return nil, err
		}
	}
}

// escalate opens (or finds) the n-th needs_you gate of a step and returns
// the feedback of its approval; a rejection ends the run.
func (x *exec) escalate(ctx context.Context, path string, n int, reason string) (string, error) {
	project, err := x.e.cfg.Store.Projects().Get(ctx, x.run.AccountID, x.run.ProjectID)
	if err != nil {
		return "", err
	}
	gate := Step{Type: StepGate, With: map[string]any{
		"kind":         string(domain.GateNeedsYou),
		"title":        fmt.Sprintf("%s needs you at step %s", x.run.Playbook, path),
		"summary":      reason + "\n\nApprove to retry, reject to stop the run.",
		"subject_repo": project.Repo,
	}}
	v, err := x.leaf(ctx, gate, stepKey{path + ":escalate", n}, &scope{}, "")
	if err != nil {
		return "", err
	}
	if decision(v) != domain.GateApproved {
		return "", &runEnd{domain.RunCanceled, fmt.Sprintf("step %s: escalation rejected", path)}
	}
	out, _ := v.(map[string]any)["output"].(map[string]any)
	feedback, _ := out["feedback"].(string)
	return feedback, nil
}

// gateOutcome turns a gate decision into a run outcome: rejected cancels
// the run; changes_requested continues inside a loop (the next round reads
// the feedback) and fails the step elsewhere, so on_failure applies.
func gateOutcome(v any, path string, sc *scope) error {
	switch decision(v) {
	case domain.GateRejected:
		return &runEnd{domain.RunCanceled, fmt.Sprintf("step %s: gate rejected", path)}
	case domain.GateChangesRequested:
		if _, inLoop := sc.vars["iteration"]; !inLoop {
			out, _ := v.(map[string]any)["output"].(map[string]any)
			return &stepFailed{fmt.Sprintf("changes requested outside a loop: %v", out["feedback"])}
		}
	}
	return nil
}

// decision returns the decision of a gate step value.
func decision(v any) domain.GateStatus {
	m, _ := v.(map[string]any)
	out, _ := m["output"].(map[string]any)
	d, _ := out["decision"].(string)
	return domain.GateStatus(d)
}

func (x *exec) foreach(ctx context.Context, s Step, path string, sc *scope) (any, error) {
	ref, _ := singleRef(s.Items)
	items, ok := sc.lookup(ref).([]any)
	if !ok {
		return nil, &runEnd{domain.RunFailed, fmt.Sprintf("step %s: items %s is not a list", path, s.Items)}
	}
	if s.Parallel > 0 {
		return x.parallel(ctx, s, path, sc, items)
	}
	outs := make([]any, 0, len(items))
	for i, item := range items {
		round, err := x.item(ctx, s, path, sc, i, item)
		if err != nil {
			return nil, err
		}
		outs = append(outs, round)
	}
	return map[string]any{"output": outs}, nil
}

// parallel drives every item of a foreach as far as it goes, with at most
// s.Parallel of them unfinished. It starts no goroutines: a job, gate or wait
// only waits for its signal, so one pass over the items keeps them all going,
// and each signal replays the run and starts the next items. Items start in
// order, so the started ones always come first, and what is in flight follows
// from the recorded step runs after a restart too. A failed item ends the
// foreach once every item has finished; a canceled run ends it at once.
func (x *exec) parallel(ctx context.Context, s Step, path string, sc *scope, items []any) (any, error) {
	outs := make([]any, len(items))
	inFlight := 0
	var failed *runEnd
	for i, item := range items {
		if inFlight >= s.Parallel && !x.started(itemPrefix(path, i)) {
			break
		}
		round, err := x.item(ctx, s, path, sc, i, item)
		var end *runEnd
		switch {
		case err == nil:
			outs[i] = round
		case errors.Is(err, errWaiting):
			inFlight++
		case errors.As(err, &end) && end.status == domain.RunFailed:
			failed = cmp.Or(failed, end)
		default:
			return nil, err
		}
	}
	switch {
	case inFlight > 0:
		return nil, errWaiting
	case failed != nil:
		return nil, failed
	}
	return map[string]any{"output": outs}, nil
}

// started reports whether a step run with the path prefix is recorded.
func (x *exec) started(prefix string) bool {
	for k := range x.steps {
		if strings.HasPrefix(k.id, prefix) {
			return true
		}
	}
	return false
}

func itemPrefix(path string, i int) string { return fmt.Sprintf("%s/%d/", path, i) }

// item runs the body of a foreach for one item and returns the values of its
// steps.
func (x *exec) item(ctx context.Context, s Step, path string, sc *scope, i int, item any) (map[string]any, error) {
	c := sc.child()
	c.vars["item"], c.vars["index"] = item, i
	if err := x.seq(ctx, s.Steps, itemPrefix(path, i), c); err != nil {
		return nil, err
	}
	round := map[string]any{}
	for _, b := range s.Steps {
		round[b.ID] = c.steps[b.ID]
	}
	return round, nil
}

func (x *exec) loop(ctx context.Context, s Step, path string, sc *scope) (any, error) {
	until, _ := parseCond(s.Until) // validated on load
	limit, escalations := s.MaxIterations, 0
	cur := sc
	i := 1
	for ; ; i++ {
		if i > limit {
			reason := fmt.Sprintf("loop %s: %q not met after %d iterations", path, s.Until, limit)
			if s.OnFailure != Escalate {
				return nil, &runEnd{domain.RunFailed, reason}
			}
			escalations++
			if _, err := x.escalate(ctx, path, escalations, reason); err != nil {
				return nil, err
			}
			limit++
		}
		cur = cur.child()
		cur.vars["iteration"] = i
		if err := x.seq(ctx, s.Steps, fmt.Sprintf("%s/%d/", path, i), cur); err != nil {
			return nil, err
		}
		if until.holds(cur) {
			break
		}
	}
	// The last round's step values stay visible after the loop.
	for _, b := range s.Steps {
		sc.steps[b.ID] = cur.steps[b.ID]
	}
	return map[string]any{"output": map[string]any{"iterations": i}}, nil
}

// leaf drives one job, action, gate or wait step run: it records the step
// run, performs its side effect once, waits for its signal and returns its
// value. feedback is added to a job retried after an escalation.
func (x *exec) leaf(ctx context.Context, s Step, key stepKey, sc *scope, feedback string) (any, error) {
	sr, ok := x.steps[key]
	if !ok {
		sr = domain.StepRun{
			AccountID: x.run.AccountID, RunID: x.run.ID, StepID: key.id, Attempt: key.attempt,
			Status: domain.StepRunning, StartedAt: x.e.cfg.Now(),
		}
		if err := x.resume(ctx); err != nil {
			return nil, err
		}
		if err := x.e.cfg.Store.StepRuns().Create(ctx, &sr); err != nil {
			return nil, fmt.Errorf("record step %s: %w", key.id, err)
		}
		x.steps[key] = sr
	}
	defer func() { x.steps[key] = sr }()
	if sr.Status == domain.StepRunning {
		if err := x.start(ctx, s, &sr, sc, feedback); err != nil {
			return nil, err
		}
	}
	if sr.Status == domain.StepWaiting {
		if err := x.await(ctx, s, &sr); err != nil {
			return nil, err
		}
	}
	if sr.Status == domain.StepFailed {
		return nil, &stepFailed{sr.Error}
	}
	var v any
	if err := json.Unmarshal(sr.Output, &v); err != nil {
		return nil, fmt.Errorf("step %s output: %w", key.id, err)
	}
	return v, nil
}

// start performs the side effect of a running step run, keyed by its ID,
// and records the outcome: waiting for a signal, succeeded or failed.
func (x *exec) start(ctx context.Context, s Step, sr *domain.StepRun, sc *scope, feedback string) error {
	with, _ := sc.resolve(s.With).(map[string]any)
	var err error
	switch s.Type {
	case StepJob:
		err = x.startJob(ctx, sr, with, feedback)
	case StepGate:
		err = x.startGate(ctx, sr, with)
	case StepWait:
		err = startWait(sr, with)
	case StepAction:
		var out any
		run := x.run
		run.Status = cmp.Or(x.ending, run.Status)
		out, err = x.e.cfg.Actions[s.Action](ctx, ActionCall{Key: sr.ID.String(), Run: run, With: with})
		if err == nil {
			return x.finish(ctx, sr, map[string]any{"output": out}, "")
		}
	}
	if err == nil {
		sr.Status = domain.StepWaiting
		return x.save(ctx, sr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return x.finish(ctx, sr, nil, err.Error())
}

// withFrom is the with key of a command job step that names a value, usually
// ${item}, to take fromKeys from. The with's own keys win.
const withFrom = "from"

// fromKeys are the keys a job step takes from its from value: an element of
// matrix.plan's jobs. Others, such as the element's key, are ignored.
var fromKeys = []string{"cell", "suite", "environment", "needs", "command"}

// expandFrom returns with with the keys of its from value filled in.
func expandFrom(with map[string]any) (map[string]any, error) {
	v, ok := with[withFrom]
	if !ok {
		return with, nil
	}
	from, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("with: from is not an object")
	}
	out := map[string]any{}
	for _, k := range fromKeys {
		if e, ok := from[k]; ok {
			out[k] = e
		}
	}
	maps.Copy(out, with)
	delete(out, withFrom)
	return out, nil
}

func (x *exec) startJob(ctx context.Context, sr *domain.StepRun, with map[string]any, feedback string) error {
	with, err := expandFrom(with)
	if err != nil {
		return err
	}
	var req JobRequest
	if err := DecodeWith(with, &req); err != nil {
		return err
	}
	req.ID, req.AccountID, req.ProjectID = sr.ID, x.run.AccountID, x.run.ProjectID
	req.GoalID = cmp.Or(req.GoalID, x.run.GoalID)
	req.TaskID = cmp.Or(req.TaskID, x.run.TaskID)
	if feedback != "" {
		req.Feedback = strings.TrimSpace(req.Feedback + "\n\n" + feedback)
	}
	_, err = x.e.cfg.Store.Jobs().Get(ctx, x.run.AccountID, sr.ID)
	if errors.Is(err, store.ErrNotFound) {
		var id uuid.UUID
		id, err = x.e.cfg.Jobs.Create(ctx, req)
		if err == nil && id != sr.ID {
			err = fmt.Errorf("job creator returned job %s, want %s: it must use JobRequest.ID", id, sr.ID)
		}
	}
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	sr.JobID, sr.WaitKey = sr.ID, domain.JobKey(sr.ID)
	return nil
}

func (x *exec) startGate(ctx context.Context, sr *domain.StepRun, with map[string]any) error {
	var req gates.OpenRequest
	if err := DecodeWith(with, &req); err != nil {
		return err
	}
	req.ID, req.AccountID, req.ProjectID = sr.ID, x.run.AccountID, x.run.ProjectID
	req.GoalID = cmp.Or(req.GoalID, x.run.GoalID)
	req.TaskID = cmp.Or(req.TaskID, x.run.TaskID)
	g, err := x.e.cfg.Gates.Open(ctx, req)
	if err != nil {
		return err
	}
	sr.GateID, sr.WaitKey = g.ID, domain.GateKey(g.ID)
	return nil
}

// waitWith is the with of a wait step.
type waitWith struct {
	Signal domain.SignalKind `json:"signal"`
	Repo   string            `json:"repo"`
	Number int               `json:"number"`
	SHA    string            `json:"sha"`
	JobID  uuid.UUID         `json:"job_id"`
	GateID uuid.UUID         `json:"gate_id"`
	// CouncilID is the council of a wait for council.decided.
	CouncilID uuid.UUID `json:"council_id"`
}

func startWait(sr *domain.StepRun, with map[string]any) error {
	var w waitWith
	if err := DecodeWith(with, &w); err != nil {
		return err
	}
	var key, missing string
	switch w.Signal {
	case domain.SignalJobCompleted:
		key, missing = domain.JobKey(w.JobID), empty("job_id", w.JobID)
	case domain.SignalGateResolved:
		key, missing = domain.GateKey(w.GateID), empty("gate_id", w.GateID)
		sr.GateID = w.GateID // for the lost-signal fallback of await
	case domain.SignalPRMerged, domain.SignalPRClosed:
		key, missing = domain.PRKey(w.Repo, w.Number), empty("repo", w.Repo)+empty("number", w.Number)
	case domain.SignalCouncilDecided:
		key, missing = domain.CouncilKey(w.CouncilID), empty("council_id", w.CouncilID)
	default: // checks.completed; playbooks are validated
		key, missing = domain.ChecksKey(w.Repo, w.SHA), empty("repo", w.Repo)+empty("sha", w.SHA)
	}
	if missing != "" {
		return fmt.Errorf("wait for %s: empty%s", w.Signal, missing)
	}
	sr.WaitKey = key
	return nil
}

func empty[T comparable](name string, v T) string {
	var zero T
	if v == zero {
		return " " + name
	}
	return ""
}

// await completes a waiting step run with the first signal stored under its
// key, so a signal that arrived before the step started waiting is not
// missed. It returns errWaiting when there is none yet.
func (x *exec) await(ctx context.Context, s Step, sr *domain.StepRun) error {
	signals, err := x.e.cfg.Bus.Lookup(ctx, x.run.AccountID, sr.WaitKey)
	if err != nil {
		return err
	}
	if len(signals) > 0 {
		return x.complete(ctx, sr, signals[0])
	}
	// The job or gate may have finished while its signal was lost (the
	// process died between the state change and the publish).
	switch s.Type {
	case StepJob:
		job, err := x.e.cfg.Store.Jobs().Get(ctx, x.run.AccountID, sr.JobID)
		if err != nil {
			return err
		}
		if job.Status.Terminal() {
			return x.jobDone(ctx, sr, job, job.Status)
		}
	case StepGate:
		g, err := x.e.cfg.Gates.Get(ctx, x.run.AccountID, sr.GateID)
		if err != nil {
			return err
		}
		if g.Status.Terminal() {
			return x.finish(ctx, sr, gateValue(g.ID.String(), g.Status, g.Feedback), "")
		}
	case StepWait:
		if sr.GateID == uuid.Nil() {
			break // not a wait for a gate
		}
		g, err := x.e.cfg.Gates.Get(ctx, x.run.AccountID, sr.GateID)
		if err != nil {
			return err
		}
		if g.Status.Terminal() {
			return x.finish(ctx, sr, gateValue(g.ID.String(), g.Status, g.Feedback), "")
		}
	}
	if s.Timeout > 0 && !x.e.cfg.Now().Before(sr.StartedAt.Add(time.Duration(s.Timeout))) {
		return x.finish(ctx, sr, nil, fmt.Sprintf("timed out after %s waiting for %s", time.Duration(s.Timeout), sr.WaitKey))
	}
	return errWaiting
}

// complete records the outcome of a waiting step run from its signal.
func (x *exec) complete(ctx context.Context, sr *domain.StepRun, sig domain.Signal) error {
	payload, err := sig.Decode()
	if err != nil {
		return x.finish(ctx, sr, nil, err.Error())
	}
	sr.SignalID = sig.ID
	switch p := payload.(type) {
	case *domain.JobCompleted:
		job, err := x.e.cfg.Store.Jobs().Get(ctx, x.run.AccountID, p.JobID)
		if err != nil {
			return err
		}
		return x.jobDone(ctx, sr, job, p.Status)
	case *domain.GateResolved:
		return x.finish(ctx, sr, gateValue(p.GateID.String(), p.Decision, p.Feedback), "")
	case *domain.CouncilDecidedSignal:
		if p.Status != domain.CouncilDecided {
			return x.finish(ctx, sr, nil, fmt.Sprintf("council %s", p.Status))
		}
	}
	var out any
	_ = json.Unmarshal(sig.Payload, &out) // Decode succeeded, so the payload is JSON
	return x.finish(ctx, sr, map[string]any{"output": out, "signal": string(sig.Kind)}, "")
}

// jobDone records the outcome of a job step whose job reached status.
func (x *exec) jobDone(ctx context.Context, sr *domain.StepRun, job domain.Job, status domain.JobStatus) error {
	if status != domain.JobSucceeded {
		reason := fmt.Sprintf("%s %s", job.Ref(), status)
		if job.Error != "" {
			reason += ": " + job.Error
		}
		return x.finish(ctx, sr, nil, reason)
	}
	var out any
	if len(job.Output) > 0 {
		if err := json.Unmarshal(job.Output, &out); err != nil {
			return x.finish(ctx, sr, nil, fmt.Sprintf("job output: %v", err))
		}
	}
	return x.finish(ctx, sr, map[string]any{"output": out, "job_id": job.ID.String(), "head_sha": job.HeadSHA}, "")
}

func gateValue(id string, d domain.GateStatus, feedback string) map[string]any {
	return map[string]any{"output": map[string]any{"decision": string(d), "feedback": feedback}, "gate_id": id}
}

// finish records a step run as succeeded with value, or failed with reason
// when reason is set.
func (x *exec) finish(ctx context.Context, sr *domain.StepRun, value map[string]any, reason string) error {
	sr.FinishedAt = x.e.cfg.Now()
	if reason != "" {
		sr.Status, sr.Error = domain.StepFailed, reason
	} else {
		out, err := json.Marshal(value)
		if err != nil {
			sr.Status, sr.Error = domain.StepFailed, fmt.Sprintf("output: %v", err)
		} else {
			sr.Status, sr.Output = domain.StepSucceeded, out
		}
	}
	return x.save(ctx, sr)
}

func (x *exec) save(ctx context.Context, sr *domain.StepRun) error {
	if err := x.resume(ctx); err != nil {
		return err
	}
	if err := x.e.cfg.Store.StepRuns().Update(ctx, sr); err != nil {
		return fmt.Errorf("record step %s: %w", sr.StepID, err)
	}
	return nil
}

// resume marks a waiting run as running again once it makes progress.
func (x *exec) resume(ctx context.Context) error {
	if x.run.Status != domain.RunWaiting {
		return nil
	}
	x.run.Status = domain.RunRunning
	if err := x.e.cfg.Store.WorkflowRuns().Update(ctx, &x.run); err != nil {
		return fmt.Errorf("resume run: %w", err)
	}
	return nil
}

// DecodeWith decodes a resolved with (a step's or an ActionCall's) into the
// struct v by its json keys, rejecting unknown keys.
func DecodeWith(with map[string]any, v any) error {
	b, err := json.Marshal(with)
	if err != nil {
		return fmt.Errorf("with: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("with: %w", err)
	}
	return nil
}
