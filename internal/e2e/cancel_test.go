//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// analyst waits for the first onboarding analyst job.
func (w *world) analyst() domain.Job {
	w.t.Helper()
	var found domain.Job
	eventually(w.t, "the analyst job", func() bool {
		js := w.jobs("onboarding-analyst")
		if len(js) == 0 {
			return false
		}
		found = js[0]
		return true
	})
	return found
}

func (w *world) cancel(j domain.Job) mcp.JobInfo {
	w.t.Helper()
	var info mcp.JobInfo
	if code := w.api(http.MethodPost, "/jobs/"+j.Ref()+"/cancel", nil, &info); code != http.StatusOK {
		w.t.Fatalf("cancel %s: status %d", j.Ref(), code)
	}
	return info
}

// stepRuns returns the step runs of the latest run by step id.
func (w *world) stepRuns() map[string]domain.StepRun {
	w.t.Helper()
	var runID uuid.UUID
	must(w.t, w.pool.QueryRow(w.t.Context(), `SELECT id FROM workflow_runs ORDER BY created_at DESC LIMIT 1`).Scan(&runID))
	rs, err := w.st.StepRuns().ListByRun(w.t.Context(), w.account.ID, runID)
	must(w.t, err)
	out := map[string]domain.StepRun{}
	for _, r := range rs {
		out[r.StepID] = r
	}
	return out
}

// runStatus waits for the only workflow run to end and returns its status.
func (w *world) runStatus(want domain.RunStatus) {
	w.t.Helper()
	eventually(w.t, "run "+string(want), func() bool {
		var status domain.RunStatus
		must(w.t, w.pool.QueryRow(w.t.Context(), `SELECT status FROM workflow_runs ORDER BY created_at DESC LIMIT 1`).Scan(&status))
		return status == want
	})
}

// A queued job that is canceled ends its playbook step at once: the step
// escalates, and rejecting the escalation ends the run through the playbook's
// finally steps.
func TestCancelQueuedJob(t *testing.T) {
	w := newWorld(t, false) // no worker: the job stays queued
	w.onboard()
	job := w.analyst()
	if job.Status != domain.JobQueued {
		t.Fatalf("job is %s", job.Status)
	}

	if info := w.cancel(job); info.Status != domain.JobCanceled || info.Ref != job.Ref() {
		t.Errorf("canceled job = %+v", info)
	}
	if code := w.api(http.MethodPost, "/jobs/"+job.Ref()+"/cancel", nil, nil); code != http.StatusConflict {
		t.Errorf("cancel again: status %d, want 409", code)
	}
	g := w.gate(domain.GateNeedsYou)
	if !strings.Contains(g.Summary, job.Ref()+" canceled") {
		t.Errorf("escalation summary = %q", g.Summary)
	}
	w.decide(g, "reject", "")
	w.runStatus(domain.RunCanceled)
	steps := w.stepRuns()
	var analyze domain.StepRun
	for _, s := range steps {
		if s.JobID == job.ID {
			analyze = s
		}
	}
	if analyze.Status != domain.StepFailed || !strings.Contains(analyze.Error, job.Ref()+" canceled") {
		t.Errorf("analyze step = %+v", analyze)
	}
	if s := steps["finally/revert"]; s.Status != domain.StepSucceeded {
		t.Errorf("finally step = %+v", s)
	}
	if got := w.project().Status; got != domain.ProjectRegistered {
		t.Errorf("project is %s, want registered (finally ran)", got)
	}
}

// A running job that is canceled stops on its worker: the agent is killed,
// the checkout removed, the worker takes the next job, and the playbook
// escalates, where approving retries the step.
func TestCancelRunningJob(t *testing.T) {
	w := newWorld(t, false)
	started := make(chan string, 1)
	var stopped = make(chan struct{})
	w.ag.Before = func(ctx context.Context, c call) error {
		if c.Round > 1 {
			return nil
		}
		started <- c.WorkDir
		<-ctx.Done() // the agent works until the worker kills it
		close(stopped)
		return ctx.Err()
	}
	worker := w.startWorker("box", claudeLabels...)
	w.onboard()
	job := w.analyst()
	var dir string
	select {
	case dir = <-started:
	case <-time.After(waitFor):
		t.Fatal("the agent never started")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("checkout of the running job: %v", err)
	}

	if info := w.cancel(job); info.Status != domain.JobCanceled {
		t.Errorf("canceled job = %+v", info)
	}
	select { // the agent was killed
	case <-stopped:
	case <-time.After(waitFor):
		t.Fatal("the agent of the canceled job was not stopped")
	}
	eventually(t, "the checkout to be removed", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
	eventually(t, "the worker to say why it stopped", func() bool { return strings.Contains(worker.logs.String(), "canceled by forecrewd") })
	if got, err := w.st.Jobs().Get(t.Context(), w.account.ID, job.ID); err != nil || got.Status != domain.JobCanceled || got.Error != "" {
		t.Errorf("job = %+v, %v; the worker must not overwrite the cancellation", got, err)
	}

	// The step ends in a needs_you gate; approving retries with a new job
	// that the freed worker runs, and the onboarding goes on.
	g := w.gate(domain.GateNeedsYou)
	if !strings.Contains(g.Summary, job.Ref()+" canceled") {
		t.Errorf("escalation summary = %q", g.Summary)
	}
	w.decide(g, "approve", "")
	profile := w.gate(domain.GateProfileApproval)
	if profile.SubjectNumber == 0 {
		t.Errorf("profile gate = %+v", profile)
	}
	js := w.jobs("onboarding-analyst")
	if len(js) != 2 || js[0].Status != domain.JobCanceled || js[1].Status != domain.JobSucceeded {
		t.Errorf("analyst jobs = %+v", js)
	}
}
