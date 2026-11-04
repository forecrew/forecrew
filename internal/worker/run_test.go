package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

func TestRunHappyPath(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{Edits: []runtime.ScriptEdit{edit("src/a.go", "package a\n")}}))
	s := e.spec()
	e.api.enqueue(s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)

	waitFor(t, "the job to complete", func() bool { return e.api.completionCount(s.JobID) == 1 })
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run = %v", err)
	}
	c := e.api.completion(t, s.JobID)
	if c.Status != StatusSucceeded || c.HeadSHA == "" || remoteSHA(t, e.remote, "agent/task-1") != c.HeadSHA {
		t.Errorf("completion = %+v", c)
	}
	e.api.mu.Lock()
	first, tokens := e.api.polls[0], e.api.tokens
	e.api.mu.Unlock()
	if first != 1 {
		t.Errorf("first poll free_slots = %d, want 1", first)
	}
	for _, h := range tokens {
		if h != "Bearer "+testWorkerTok {
			t.Errorf("Authorization = %q", h)
		}
	}
	e.noSecrets()
}

func TestRunConcurrencyLimit(t *testing.T) {
	g := newGate()
	e := newEnv(t, g.adapter())
	e.cfg.MaxJobs = 2
	e.rebuild()
	specs := []domain.JobSpec{
		e.spec(func(s *domain.JobSpec) { s.Branch = "agent/task-1" }),
		e.spec(func(s *domain.JobSpec) { s.Branch = "agent/task-2" }),
		e.spec(func(s *domain.JobSpec) { s.Branch = "agent/task-3" }),
	}
	e.api.enqueue(specs...)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)

	waitFor(t, "two jobs to run", func() bool { r, _, _ := g.counts(); return r == 2 })
	time.Sleep(150 * time.Millisecond) // the third job must keep waiting
	if running, _, started := g.counts(); running != 2 || started != 2 {
		t.Fatalf("running = %d, started = %d, want 2 and 2", running, started)
	}
	e.api.mu.Lock()
	for _, free := range e.api.polls {
		if free < 1 {
			t.Errorf("polled with free_slots = %d; a worker without a free slot must not poll", free)
		}
	}
	if last := e.api.polls[len(e.api.polls)-1]; last != 1 {
		t.Errorf("poll while one job runs and one slot is reserved: free_slots = %d, want 1", last)
	}
	e.api.mu.Unlock()

	g.release <- struct{}{} // one job ends, the third starts
	waitFor(t, "the third job to start", func() bool { _, _, s := g.counts(); return s == 3 })
	g.release <- struct{}{}
	g.release <- struct{}{}
	waitFor(t, "all jobs to complete", func() bool {
		return e.api.completionCount(specs[0].JobID)+e.api.completionCount(specs[1].JobID)+e.api.completionCount(specs[2].JobID) == 3
	})
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if _, peak, _ := g.counts(); peak != 2 {
		t.Errorf("peak concurrency = %d, want 2", peak)
	}
}

func TestRunBacksOffWhileTheServerFails(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	e.api.pollAnswer = func(n int) int {
		if n <= 3 {
			return 500
		}
		return 0
	}
	s := e.spec()
	e.api.enqueue(s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)
	waitFor(t, "the job to complete after the outage", func() bool { return e.api.completionCount(s.JobID) == 1 })
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(e.logs.String(), "poll failed, retrying"); got != 3 {
		t.Errorf("logged %d poll failures, want 3", got)
	}
}

func TestRunSurvivesAnUnreachableServer(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	e.api.srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := waitRun(t, e.start(ctx)); err != nil {
		t.Errorf("Run = %v, want nil after the context ended", err)
	}
	if got := strings.Count(e.logs.String(), "poll failed, retrying"); got < 2 {
		t.Errorf("logged %d poll failures, want several retries", got)
	}
}

func TestRunStopsWhenTheTokenIsRevoked(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	e.api.pollAnswer = func(int) int { return 401 }
	err := waitRun(t, e.start(t.Context()))
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("Run = %v, want ErrUnauthorized mentioning the revoked token", err)
	}
}

func TestRunCancelsJobsWhenTheTokenIsRevoked(t *testing.T) {
	g := newGate()
	e := newEnv(t, g.adapter())
	e.cfg.MaxJobs = 2
	e.rebuild()
	s := e.spec()
	e.api.enqueue(s)
	e.api.pollAnswer = func(n int) int {
		if _, _, started := g.counts(); n >= 2 && started == 1 { // revoked while the job runs
			return 401
		}
		return 0
	}
	err := waitRun(t, e.start(t.Context()))
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Run = %v", err)
	}
	if running, _, started := g.counts(); started != 1 || running != 0 {
		t.Errorf("started = %d, running = %d; the job should have been canceled", started, running)
	}
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times", n)
	}
}

func TestRunDrainsOnShutdown(t *testing.T) {
	g := newGate()
	e := newEnv(t, g.adapter())
	e.cfg.GracePeriod = 10 * time.Second
	e.rebuild()
	s := e.spec()
	e.api.enqueue(s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)
	waitFor(t, "the job to run", func() bool { r, _, _ := g.counts(); return r == 1 })

	cancel() // SIGTERM
	select {
	case err := <-done:
		t.Fatalf("Run returned %v while a job was still running", err)
	case <-time.After(150 * time.Millisecond):
	}
	e.api.mu.Lock()
	pollsAtShutdown := len(e.api.polls)
	e.api.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	e.api.mu.Lock()
	after := len(e.api.polls)
	e.api.mu.Unlock()
	if after > pollsAtShutdown+1 {
		t.Errorf("polls went from %d to %d during shutdown; polling must stop", pollsAtShutdown, after)
	}

	g.release <- struct{}{} // the job finishes within the grace period
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if c := e.api.completion(t, s.JobID); c.Status != StatusSucceeded {
		t.Errorf("completion = %+v", c)
	}
}

func TestRunCancelsJobsAfterTheGracePeriod(t *testing.T) {
	g := newGate()
	e := newEnv(t, g.adapter())
	e.cfg.GracePeriod = 100 * time.Millisecond
	e.rebuild()
	s := e.spec()
	e.api.enqueue(s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)
	waitFor(t, "the job to run", func() bool { r, _, _ := g.counts(); return r == 1 })

	begin := time.Now()
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took < 100*time.Millisecond {
		t.Errorf("Run returned after %v, before the grace period ended", took)
	}
	if running, _, _ := g.counts(); running != 0 {
		t.Errorf("%d jobs still running", running)
	}
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("a canceled job was reported %d times; its lease must expire instead", n)
	}
	if !strings.Contains(e.logs.String(), "grace period over") {
		t.Error("the end of the grace period was not logged")
	}
	if _, err := os.Stat(filepath.Join(e.workDir, "jobs", s.JobID.String())); !os.IsNotExist(err) {
		t.Errorf("workspace kept: %v", err)
	}
}

func TestRunCleansLeftoverWorkspaces(t *testing.T) {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	stale := filepath.Join(e.workDir, "jobs", "old-job")
	writeFile(t, stale, "x", "x")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := waitRun(t, e.start(ctx)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale workspace kept: %v", err)
	}

	// A work_dir that cannot be created is an error.
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, filepath.Dir(file), "file", "x")
	e.cfg.WorkDir = filepath.Join(file, "sub")
	e.rebuild()
	if err := waitRun(t, e.start(t.Context())); err == nil {
		t.Error("Run with an unusable work_dir succeeded")
	}
}

func TestNewDefaults(t *testing.T) {
	w := New(Config{})
	if w.cfg.GracePeriod != DefaultGracePeriod || w.cfg.MaxJobs != 1 || w.cfg.FlushInterval != defaultFlushInterval ||
		w.cfg.CloneURL("acme/app") != "https://github.com/acme/app.git" || w.cfg.Now().IsZero() {
		t.Errorf("defaults = %+v", w.cfg)
	}
	tests := []struct {
		override time.Duration
		lease    int
		want     time.Duration
	}{
		{0, 0, defaultHeartbeat},
		{0, 20, 20 * time.Second},
		{time.Second, 20, time.Second},
	}
	for _, tt := range tests {
		w := New(Config{HeartbeatInterval: tt.override})
		if got := w.heartbeatInterval(&Lease{HeartbeatInterval: tt.lease}); got != tt.want {
			t.Errorf("heartbeatInterval(override %v, lease %d) = %v, want %v", tt.override, tt.lease, got, tt.want)
		}
	}
}

func TestRunCleansDockerLeftoversAndPrunes(t *testing.T) {
	for name, cases := range map[string]string{
		"working": `ps) echo c1;; image) if [ "$2" = ls ]; then echo forecrew-env:x; fi;;`,
		"failing": `ps) echo down >&2; exit 1;; image) echo down >&2; exit 1;;`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, scriptAdapter(t, runtime.Script{}))
			bin, log := fakeDocker(t, cases)
			e.cfg.DockerBin = bin
			e.rebuild()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := e.start(ctx)
			waitFor(t, "the image prune", func() bool {
				return slices.ContainsFunc(calls(t, log), func(c string) bool { return strings.HasPrefix(c, "image ls") })
			})
			cancel()
			if err := waitRun(t, done); err != nil {
				t.Fatal(err)
			}
			got := calls(t, log)
			if !strings.HasPrefix(got[0], "ps -aq --filter label=forecrew.worker=") {
				t.Errorf("first docker call = %q, want the leftover cleanup before any job", got[0])
			}
		})
	}
}
