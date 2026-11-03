package workerapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/store"
)

const summarySchema = `{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`

func TestHappyPath(t *testing.T) {
	e := newEnv(t)
	w, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{needs: []string{"go"}, writePaths: []string{"**"}, schema: summarySchema, branch: "agent/task-1"})

	spec := happyPoll(t, e, w, token, j)
	happyHeartbeatAndEvents(t, e, token, j)
	happyComplete(t, e, token, j)

	// Secrets never reach the logs.
	logged := e.logs.String()
	if !strings.Contains(logged, "job leased") {
		t.Fatalf("logs = %s", logged)
	}
	for _, secret := range []string{spec.RepoToken, token, apiSecret} {
		if strings.Contains(logged, secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logged)
		}
	}
}

// happyPoll leases j to w and checks the spec, the stored job and the
// worker's presence.
func happyPoll(t *testing.T, e *env, w domain.Worker, token string, j domain.Job) domain.JobSpec {
	t.Helper()
	status, lease := e.poll(t, token, "Go", "linux")
	if status != http.StatusOK {
		t.Fatalf("poll status = %d", status)
	}
	spec := lease.Job
	if spec.JobID != j.ID || spec.AccountID != e.account || spec.Repo != repo || spec.Branch != "agent/task-1" ||
		spec.Prompt != "do it" || string(spec.OutputSchema) != summarySchema || lease.HeartbeatInterval != 30 {
		t.Fatalf("lease = %+v", lease)
	}
	if !strings.HasPrefix(spec.RepoToken, "ghs_fake_write_") || spec.Env["ANTHROPIC_API_KEY"] != apiSecret {
		t.Fatalf("secrets missing: token %q env %v", spec.RepoToken, spec.Env)
	}
	if d := lease.LeaseExpiresAt.Sub(e.clock.Now()); lease.LeaseExpiresAt.IsZero() || d > domain.LeaseDuration+time.Minute {
		t.Fatalf("lease expires at %s", lease.LeaseExpiresAt)
	}
	stored := e.job(t, j.ID)
	if stored.Status != domain.JobRunning || stored.WorkerID != w.ID || stored.Spec.RepoToken != "" || stored.Spec.Env != nil {
		t.Fatalf("stored = %+v", stored)
	}
	seen, err := e.st.Workers().Get(t.Context(), e.account, w.ID)
	if err != nil || seen.Version != "v0.1.0" || !slices.Equal(seen.Labels, []string{"go", "linux", "runtime:claude-code"}) || seen.LastSeenAt.IsZero() {
		t.Fatalf("worker = %+v, %v", seen, err)
	}
	return spec
}

func happyHeartbeatAndEvents(t *testing.T, e *env, token string, j domain.Job) {
	t.Helper()
	st, body := e.call(t, token, jobPath(j.ID, "heartbeat"), nil)
	wantStatus(t, st, http.StatusOK, body)
	var hb heartbeatResponse
	decodeBody(t, body, &hb)
	if hb.Action != actionContinue || hb.LeaseExpiresAt.IsZero() {
		t.Fatalf("heartbeat = %s", body)
	}

	st, body = e.call(t, token, jobPath(j.ID, "events"), map[string]any{"events": []map[string]any{
		{"time": "2026-03-01T12:00:01Z", "type": "status", "text": "cloning"},
		{"time": "2026-03-01T12:00:10Z", "type": "tool_call", "data": map[string]any{"name": "Edit"}},
	}})
	wantStatus(t, st, http.StatusNoContent, body)
	st, body = e.call(t, token, jobPath(j.ID, "events"), map[string]any{"events": []any{}})
	wantStatus(t, st, http.StatusNoContent, body)
	logs, err := e.jobs.Logs(t.Context(), e.account, j.ID, 0, 0)
	if err != nil || len(logs.Events) != 2 || logs.Events[0].Text != "cloning" || logs.Events[1].Seq != 2 || logs.NextSeq != 2 {
		t.Fatalf("logs = %+v, %v", logs, err)
	}
}

// happyComplete completes j twice: the second complete is refused and
// changes nothing.
func happyComplete(t *testing.T, e *env, token string, j domain.Job) {
	t.Helper()
	complete := map[string]any{
		"status": "succeeded", "output": map[string]any{"summary": "done"}, "head_sha": "4f1c2b9e",
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "cost_usd": 0.5,
	}
	st, body := e.call(t, token, jobPath(j.ID, "complete"), complete)
	wantStatus(t, st, http.StatusOK, body)
	if string(body) != `{"status":"succeeded"}`+"\n" {
		t.Fatalf("complete = %s", body)
	}
	done := e.job(t, j.ID)
	if done.Status != domain.JobSucceeded || done.HeadSHA != "4f1c2b9e" || done.Usage.InputTokens != 10 || done.CostUSD != 0.5 ||
		string(done.Output) == "" {
		t.Fatalf("done = %+v", done)
	}
	if s := e.completedSignals(t, j.ID); !slices.Equal(s, []domain.JobStatus{domain.JobSucceeded}) {
		t.Fatalf("signals = %v", s)
	}

	st, body = e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "failed", "error": "late"})
	wantStatus(t, st, http.StatusConflict, body)
	if code := errorCode(t, body); code != "lease_lost" {
		t.Fatalf("code = %s", code)
	}
	if again := e.job(t, j.ID); again.Status != domain.JobSucceeded || again.Error != "" {
		t.Fatalf("after double complete = %+v", again)
	}
	if s := e.completedSignals(t, j.ID); len(s) != 1 {
		t.Fatalf("signals = %v", s)
	}
	audit, err := e.st.Audit().List(t.Context(), e.account, store.AuditFilter{SubjectID: j.ID})
	if err != nil || len(audit) != 2 || audit[0].Action != "job.completed" || audit[0].Actor != "worker:box" {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	// After the job, heartbeats tell the worker to stop.
	_, body = e.call(t, token, jobPath(j.ID, "heartbeat"), map[string]any{})
	var hb heartbeatResponse
	if decodeBody(t, body, &hb); hb.Action != actionCancel {
		t.Fatalf("heartbeat after complete = %s", body)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	w, token := e.addWorker(t, "box")
	revoked, revokedToken := e.addWorker(t, "old")
	if err := e.st.Workers().Revoke(t.Context(), e.account, revoked.ID); err != nil {
		t.Fatal(err)
	}
	j := e.newJob(t, jobOpts{})
	paths := []string{"/worker/v1/poll", jobPath(j.ID, "heartbeat"), jobPath(j.ID, "events"), jobPath(j.ID, "complete")}
	for _, path := range paths {
		for name, header := range map[string]string{
			"missing": "", "unknown": "Bearer fcw_nope", "revoked": "Bearer " + revokedToken,
			"basic": "Basic " + token, "empty": "Bearer ",
		} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.http.URL+path, strings.NewReader(`{"free_slots":1}`))
			if err != nil {
				t.Fatal(err)
			}
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			resp, err := e.http.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s: status %d", path, name, resp.StatusCode)
			}
		}
	}
	if got := e.job(t, j.ID); got.Status != domain.JobQueued {
		t.Fatalf("job leased by a rejected worker: %+v", got)
	}
	if status, _ := e.poll(t, token); status != http.StatusOK {
		t.Fatalf("valid worker %s: status %d", w.Name, status)
	}
}

func TestWorkerFromContext(t *testing.T) {
	if _, ok := WorkerFromContext(t.Context()); ok {
		t.Fatal("worker in an empty context")
	}
}

func TestLabels(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{needs: []string{"Docker", "go"}})

	for _, labels := range [][]string{nil, {"docker"}, {"GO"}} {
		if status, _ := e.poll(t, token, labels...); status != http.StatusNoContent {
			t.Fatalf("labels %v: status %d, want 204", labels, status)
		}
	}
	// Without the runtime label nothing is leased either.
	st, body := e.call(t, token, "/worker/v1/poll", map[string]any{"labels": []string{"docker", "go"}, "free_slots": 1})
	wantStatus(t, st, http.StatusNoContent, body)

	status, lease := e.poll(t, token, " DOCKER ", "Go")
	if status != http.StatusOK || lease.Job.JobID != j.ID {
		t.Fatalf("matching labels: status %d, lease %+v", status, lease)
	}
}

func TestReadOnlyRoleGetsReadToken(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	e.newJob(t, jobOpts{branch: "agent/task-1"})
	status, lease := e.poll(t, token)
	if status != http.StatusOK || !strings.HasPrefix(lease.Job.RepoToken, "ghs_fake_read_") || lease.Job.Branch != "agent/task-1" {
		t.Fatalf("status %d, lease %+v", status, lease)
	}
}

func TestOtherWorkersJobIsRefused(t *testing.T) {
	e := newEnv(t)
	_, owner := e.addWorker(t, "owner")
	_, other := e.addWorker(t, "other")
	j := e.newJob(t, jobOpts{})
	if status, _ := e.poll(t, owner); status != http.StatusOK {
		t.Fatalf("poll status %d", status)
	}
	for action, body := range map[string]any{
		"heartbeat": nil,
		"events":    map[string]any{"events": []map[string]any{{"time": "2026-03-01T12:00:01Z", "type": "log", "text": "x"}}},
		"complete":  map[string]any{"status": "succeeded"},
	} {
		st, resp := e.call(t, other, jobPath(j.ID, action), body)
		wantStatus(t, st, http.StatusConflict, resp)
		if code := errorCode(t, resp); code != "lease_lost" {
			t.Fatalf("%s: code %s", action, code)
		}
	}
	if got := e.job(t, j.ID); got.Status != domain.JobRunning {
		t.Fatalf("job = %+v", got)
	}
}

func TestCanceledJobAnswersCancel(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{})
	if status, _ := e.poll(t, token); status != http.StatusOK {
		t.Fatalf("poll status %d", status)
	}
	if _, err := e.jobs.Cancel(t.Context(), system, e.account, j.ID); err != nil {
		t.Fatal(err)
	}
	st, body := e.call(t, token, jobPath(j.ID, "heartbeat"), nil)
	wantStatus(t, st, http.StatusOK, body)
	var hb heartbeatResponse
	if decodeBody(t, body, &hb); hb.Action != actionCancel || !hb.LeaseExpiresAt.IsZero() {
		t.Fatalf("heartbeat = %s", body)
	}
	st, body = e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "succeeded"})
	wantStatus(t, st, http.StatusConflict, body)
	if s := e.completedSignals(t, j.ID); !slices.Equal(s, []domain.JobStatus{domain.JobCanceled}) {
		t.Fatalf("signals = %v", s)
	}
}

func TestLeaseExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{})
	for attempt := 1; attempt <= domain.DefaultMaxAttempts; attempt++ {
		if status, _ := e.poll(t, token); status != http.StatusOK {
			t.Fatalf("attempt %d: poll status %d", attempt, status)
		}
		e.expireLeases(t)
		reaped, err := e.jobs.Reap(ctx)
		if err != nil || len(reaped) != 1 || reaped[0].Attempts != attempt {
			t.Fatalf("attempt %d: reaped %+v, %v", attempt, reaped, err)
		}
		// The worker comes back too late and is told to stop.
		st, body := e.call(t, token, jobPath(j.ID, "heartbeat"), nil)
		wantStatus(t, st, http.StatusOK, body)
		if !strings.Contains(string(body), actionCancel) {
			t.Fatalf("heartbeat = %s", body)
		}
	}
	got := e.job(t, j.ID)
	if got.Status != domain.JobFailed || got.Attempts != domain.DefaultMaxAttempts || got.Error != "attempts exhausted: lease expired" {
		t.Fatalf("job = %+v", got)
	}
	v, err := e.jobs.Status(ctx, e.account, j.ID)
	if err != nil || v.Reason != got.Error {
		t.Fatalf("status = %+v, %v", v, err)
	}
	if s := e.completedSignals(t, j.ID); !slices.Equal(s, []domain.JobStatus{domain.JobFailed}) {
		t.Fatalf("signals = %v", s)
	}
}

func TestCompleteRetry(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{})
	e.poll(t, token)
	st, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "retry", "error": "disk full"})
	wantStatus(t, st, http.StatusOK, body)
	if string(body) != `{"status":"queued"}`+"\n" {
		t.Fatalf("complete = %s", body)
	}
	got := e.job(t, j.ID)
	if got.Status != domain.JobQueued || got.Attempts != 1 || got.RunAfter.IsZero() {
		t.Fatalf("job = %+v", got)
	}
	// The backoff holds the job back.
	if status, _ := e.poll(t, token); status != http.StatusNoContent {
		t.Fatalf("poll during backoff: %d", status)
	}
}

func TestCompleteBadOutputFails(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{schema: summarySchema})
	e.poll(t, token)
	st, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "succeeded", "output": nil})
	wantStatus(t, st, http.StatusOK, body)
	if got := e.job(t, j.ID); got.Status != domain.JobFailed || got.Error != jobs.ErrSchemaMismatch {
		t.Fatalf("job = %+v", got)
	}
}

func TestNoMatchingWorkerReason(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	_, token := e.addWorker(t, "box")
	j := e.newJob(t, jobOpts{needs: []string{"docker"}})
	if status, _ := e.poll(t, token, "go"); status != http.StatusNoContent {
		t.Fatalf("poll status %d", status)
	}
	v, err := e.jobs.Status(ctx, e.account, j.ID)
	if err != nil || v.Status != domain.JobQueued || v.Reason != "no online worker in pool build with labels docker, runtime:claude-code" {
		t.Fatalf("status = %+v, %v", v, err)
	}
	if status, _ := e.poll(t, token, "Docker"); status != http.StatusOK {
		t.Fatalf("poll status %d", status)
	}
	v, err = e.jobs.Status(ctx, e.account, j.ID)
	if err != nil || v.Reason != "" || v.WorkerName != "box" {
		t.Fatalf("status = %+v, %v", v, err)
	}
}

func TestOnlineFollowsLastSeen(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	_, token := e.addWorker(t, "box")
	views, err := e.jobs.Workers(ctx, e.account)
	if err != nil || len(views) != 1 || views[0].Online {
		t.Fatalf("before poll = %+v, %v", views, err)
	}
	st, body := e.call(t, token, "/worker/v1/poll", map[string]any{"labels": []string{"Go"}, "free_slots": 0, "version": "v2"})
	wantStatus(t, st, http.StatusNoContent, body)
	views, _ = e.jobs.Workers(ctx, e.account)
	if !views[0].Online || views[0].Version != "v2" || !slices.Equal(views[0].Labels, []string{"go"}) {
		t.Fatalf("after poll = %+v", views)
	}
	e.clock.Advance(domain.OnlineWindow + 2*time.Second)
	if views, _ = e.jobs.Workers(ctx, e.account); views[0].Online {
		t.Fatalf("after the online window = %+v", views)
	}
	// A heartbeat counts as seen, too.
	j := e.newJob(t, jobOpts{})
	st, body = e.call(t, token, jobPath(j.ID, "heartbeat"), nil)
	wantStatus(t, st, http.StatusOK, body)
	e.clock.Advance(-(domain.OnlineWindow + 2*time.Second))
	if views, _ = e.jobs.Workers(ctx, e.account); !views[0].Online {
		t.Fatalf("after heartbeat = %+v", views)
	}
}

func TestLongPollWakesOnNewJob(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PollWait = 20 * time.Second })
	_, token := e.addWorker(t, "box")
	type result struct {
		status int
		lease  pollResponse
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		status, lease := e.poll(t, token)
		done <- result{status, lease}
	}()
	time.Sleep(100 * time.Millisecond) // let the poll start waiting
	j := e.newJob(t, jobOpts{})
	select {
	case r := <-done:
		if r.status != http.StatusOK || r.lease.Job.JobID != j.ID {
			t.Fatalf("poll = %d %+v", r.status, r.lease)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("poll took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long poll did not return after the job was created")
	}
}

func TestLongPollRevokedWorkerGetsNoJob(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PollWait = 20 * time.Second })
	w, token := e.addWorker(t, "box")
	type result struct {
		status int
		body   []byte
	}
	done := make(chan result, 1)
	go func() {
		status, body := e.call(t, token, "/worker/v1/poll", map[string]any{"labels": []string{"runtime:claude-code"}, "free_slots": 1})
		done <- result{status, body}
	}()
	time.Sleep(100 * time.Millisecond) // let the poll start waiting
	if err := e.st.Workers().Revoke(t.Context(), e.account, w.ID); err != nil {
		t.Fatal(err)
	}
	j := e.newJob(t, jobOpts{})
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("long poll did not return")
	}
	wantStatus(t, r.status, http.StatusUnauthorized, r.body)
	if strings.Contains(string(r.body), "ghs_") {
		t.Fatalf("revoked worker got a token: %s", r.body)
	}
	if got := e.job(t, j.ID); got.Status != domain.JobQueued || got.WorkerID == w.ID {
		t.Fatalf("job = %+v, want it released", got)
	}
}

func TestPollRechecks(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PollWait = 300 * time.Millisecond; c.PollRecheck = time.Millisecond })
	_, token := e.addWorker(t, "box")
	start := time.Now()
	if status, _ := e.poll(t, token); status != http.StatusNoContent {
		t.Fatalf("status %d", status)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("poll returned after %s, want the full wait", d)
	}
}

func TestPollTokenFailureReleasesJob(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	j, err := e.jobs.Create(t.Context(), system, domain.Job{
		AccountID: e.account, ProjectID: e.project.ID, Role: "developer", Runtime: "claude-code",
		Spec: domain.JobSpec{Repo: "acme/not-installed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, body := e.call(t, token, "/worker/v1/poll", map[string]any{"labels": []string{"runtime:claude-code"}, "free_slots": 1})
	wantStatus(t, st, http.StatusInternalServerError, body)
	got := e.job(t, j.ID)
	if got.Status != domain.JobQueued || got.Attempts != 1 || got.WorkerID != (domain.Job{}).WorkerID {
		t.Fatalf("job = %+v", got)
	}
	if !strings.Contains(e.logs.String(), "complete job spec") {
		t.Fatalf("logs = %s", e.logs)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	_, otherToken := e.addWorker(t, "other")
	j := e.newJob(t, jobOpts{})
	if status, _ := e.poll(t, token); status != http.StatusOK {
		t.Fatalf("poll status %d", status)
	}
	tooMany := make([]map[string]any, MaxEvents+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"time": "2026-03-01T12:00:01Z", "type": "log"}
	}
	tests := []struct {
		name   string
		token  string
		path   string
		body   any
		status int
		code   string
	}{
		{"malformed poll", token, "/worker/v1/poll", "{", http.StatusBadRequest, "bad_request"},
		{"empty poll", token, "/worker/v1/poll", nil, http.StatusBadRequest, "bad_request"},
		{"negative slots", token, "/worker/v1/poll", map[string]any{"free_slots": -1}, http.StatusBadRequest, "bad_request"},
		{"too large", token, "/worker/v1/poll", `{"version":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "too_large"},
		{"bad job id", token, "/worker/v1/jobs/JOB-1/heartbeat", nil, http.StatusNotFound, "not_found"},
		{"unknown job", token, jobPath(domain.NewID(), "heartbeat"), nil, http.StatusNotFound, "not_found"},
		{"malformed heartbeat", token, jobPath(j.ID, "heartbeat"), "[", http.StatusBadRequest, "bad_request"},
		{"bad events id", token, "/worker/v1/jobs/x/events", map[string]any{}, http.StatusNotFound, "not_found"},
		{"malformed events", token, jobPath(j.ID, "events"), "{", http.StatusBadRequest, "bad_request"},
		{"unknown event type", token, jobPath(j.ID, "events"), map[string]any{"events": []map[string]any{{"time": "2026-03-01T12:00:01Z", "type": "nope"}}}, http.StatusBadRequest, "bad_request"},
		{"event without time", token, jobPath(j.ID, "events"), map[string]any{"events": []map[string]any{{"type": "log"}}}, http.StatusBadRequest, "bad_request"},
		{"too many events", token, jobPath(j.ID, "events"), map[string]any{"events": tooMany}, http.StatusBadRequest, "bad_request"},
		{"events of unknown job", token, jobPath(domain.NewID(), "events"), map[string]any{"events": []any{}}, http.StatusNotFound, "not_found"},
		{"bad complete id", token, "/worker/v1/jobs/x/complete", map[string]any{}, http.StatusNotFound, "not_found"},
		{"malformed complete", token, jobPath(j.ID, "complete"), "{", http.StatusBadRequest, "bad_request"},
		{"unknown status", token, jobPath(j.ID, "complete"), map[string]any{"status": "done"}, http.StatusBadRequest, "bad_request"},
		{"failed without error", token, jobPath(j.ID, "complete"), map[string]any{"status": "failed"}, http.StatusBadRequest, "bad_request"},
		{"retry without error", token, jobPath(j.ID, "complete"), map[string]any{"status": "retry"}, http.StatusBadRequest, "bad_request"},
		{"complete unknown job", token, jobPath(domain.NewID(), "complete"), map[string]any{"status": "succeeded"}, http.StatusNotFound, "not_found"},
		{"retry other's job", otherToken, jobPath(j.ID, "complete"), map[string]any{"status": "retry", "error": "x"}, http.StatusConflict, "lease_lost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, body := e.call(t, tt.token, tt.path, tt.body)
			wantStatus(t, st, tt.status, body)
			if code := errorCode(t, body); code != tt.code {
				t.Fatalf("code = %s, want %s", code, tt.code)
			}
		})
	}
	if got := e.job(t, j.ID); got.Status != domain.JobRunning {
		t.Fatalf("job = %+v", got)
	}
}

func TestOtherAccountsJobIsNotFound(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	other := domain.Account{Name: "other"}
	if err := e.st.Accounts().Create(ctx, &other); err != nil {
		t.Fatal(err)
	}
	theirs := domain.Project{AccountID: other.ID, Name: "theirs", Repo: repo}
	if err := e.st.Projects().Create(ctx, &theirs); err != nil {
		t.Fatal(err)
	}
	j, err := e.jobs.Create(ctx, system, domain.Job{AccountID: other.ID, ProjectID: theirs.ID, Role: "r", Runtime: "claude-code", Spec: domain.JobSpec{Repo: repo}})
	if err != nil {
		t.Fatal(err)
	}
	_, token := e.addWorker(t, "box")
	if status, _ := e.poll(t, token); status != http.StatusNoContent {
		t.Fatalf("leased another account's job: %d", status)
	}
	st, body := e.call(t, token, jobPath(j.ID, "heartbeat"), nil)
	wantStatus(t, st, http.StatusNotFound, body)
}

func TestDecodeKeepsUnknownFields(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "box")
	raw, _ := json.Marshal(map[string]any{"free_slots": 0, "future": true})
	st, body := e.call(t, token, "/worker/v1/poll", string(raw))
	wantStatus(t, st, http.StatusNoContent, body)
}
