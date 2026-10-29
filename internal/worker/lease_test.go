package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// editAndWait is an agent that writes a file, then keeps working for d.
func editAndWait(t *testing.T, d time.Duration) funcAdapter {
	return funcAdapter{fn: func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		writeFile(t, spec.WorkDir, "late.txt", "late\n")
		select {
		case <-time.After(d):
			return runtime.Result{}, nil
		case <-ctx.Done():
			return runtime.Result{}, ctx.Err()
		}
	}}
}

// Without a successful heartbeat the lease ends; another worker may have the
// job, so this one must not push or report.
func TestLostLeaseStopsThePush(t *testing.T) {
	e := newEnv(t, editAndWait(t, 600*time.Millisecond))
	e.api.heartbeat = func(uuid.UUID, int) (int, string) { return 500, "" }
	s := e.spec()
	e.w.handleJob(t.Context(), &Lease{Job: s, LeaseExpiresAt: time.Now().Add(300 * time.Millisecond)})

	if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
		t.Errorf("a job without a lease pushed %s", got)
	}
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times", n)
	}
	if got := e.logs.String(); !strings.Contains(got, "result dropped") || !strings.Contains(got, "lease ended") {
		t.Errorf("logs do not say the lease expired:\n%s", got)
	}
}

// The lease can end between the last check and the push; publish looks again.
func TestPublishChecksTheLeaseBeforePushing(t *testing.T) {
	e := newEnv(t, editAndWait(t, 0))
	s := e.spec()
	r := &jobRun{
		w: e.w, spec: s, write: true, log: e.cfg.Logger, red: runtime.NewRedactor(nil),
		cache: filepath.Join(e.workDir, "repos", "acme", "app.git"), dir: filepath.Join(e.workDir, "jobs", "x"),
		lease: &leaseClock{now: time.Now, expires: time.Now().Add(pushMargin / 3)}, // valid, but inside the margin
	}
	r.ev = &eventBuffer{jobID: s.JobID, client: e.cfg.Client, red: r.red, now: time.Now, log: r.log}
	stopped := make(chan error, 1)
	r.stop = func(cause error) { stopped <- cause }
	start, _, err := e.w.git.prepare(t.Context(), checkout{URL: e.remote, Cache: r.cache, Dir: r.dir, BaseRef: "main", Branch: s.Branch, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, r.dir, "new.txt", "n\n")
	tree, err := e.w.git.snapshot(t.Context(), r.cache, r.dir, start)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := e.w.git.changes(t.Context(), r.cache, start, tree)
	if err != nil {
		t.Fatal(err)
	}
	if head, done := r.publish(t.Context(), start, tree, cs, false); head != "" || done == nil {
		t.Errorf("publish = %q, %v", head, done)
	}
	if cause := <-stopped; cause != errLeaseExpired {
		t.Errorf("stop cause = %v", cause)
	}
	if got := remoteSHA(t, e.remote, "agent/task-1"); got != "" {
		t.Errorf("pushed %s without a lease", got)
	}
}

// Heartbeats keep extending the lease, so a long job outlives its first
// lease.
func TestHeartbeatsExtendTheLease(t *testing.T) {
	e := newEnv(t, editAndWait(t, 400*time.Millisecond))
	s := e.spec()
	e.w.handleJob(t.Context(), &Lease{Job: s, LeaseExpiresAt: time.Now().Add(150 * time.Millisecond)})
	c := e.api.completion(t, s.JobID)
	if c.Status != StatusSucceeded || c.HeadSHA == "" {
		t.Errorf("completion = %+v", c)
	}
}

func TestLeaseClock(t *testing.T) {
	now := time.Now()
	l := &leaseClock{now: func() time.Time { return now }, expires: now.Add(time.Minute)}
	if !l.valid() {
		t.Error("a lease ending in a minute is not valid")
	}
	l.set(now.Add(-time.Second))
	if l.valid() || !l.get().Before(now) {
		t.Error("an ended lease is valid")
	}
}

func TestLeaseEnd(t *testing.T) {
	sent := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		server time.Time
		want   time.Time
	}{
		{"server agrees", sent.Add(domain.LeaseDuration), sent.Add(domain.LeaseDuration)},
		{"server clock runs ahead", sent.Add(time.Hour), sent.Add(domain.LeaseDuration)},
		{"server says earlier", sent.Add(time.Minute), sent.Add(time.Minute)},
		{"server says nothing", time.Time{}, sent.Add(domain.LeaseDuration)},
	}
	for _, tt := range tests {
		if got := leaseEnd(sent, tt.server); !got.Equal(tt.want) {
			t.Errorf("%s: leaseEnd = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestLeasePushMargin(t *testing.T) {
	now := time.Now()
	l := &leaseClock{now: func() time.Time { return now }, expires: now.Add(pushMargin + time.Second)}
	if !l.pushable() {
		t.Error("a lease with more than the margin left is not pushable")
	}
	l.set(now.Add(pushMargin - time.Second))
	if l.pushable() || !l.valid() {
		t.Error("a lease inside the margin must be valid but not pushable")
	}
}
