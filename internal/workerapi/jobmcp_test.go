package workerapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/mcp"
)

const gatewayURL = "https://forecrew.example.com/job-mcp"

func withGateway(a *jobmcp.Attachments) option {
	return func(c *Config) { c.JobMCPURL, c.Attachments = gatewayURL, a }
}

// TestJobMCPToken: a leased agent job of any pool gets a job MCP token
// whose hash, and only its hash, is stored with the lease.
func TestJobMCPToken(t *testing.T) {
	for _, pool := range []string{domain.PoolBuild, domain.PoolControl} {
		t.Run(pool, func(t *testing.T) {
			e := newEnv(t, withGateway(nil))
			_, token := e.addPoolWorker(t, "w", pool)
			j := e.queue(t, domain.Job{Pool: pool, Role: "reviewer", Runtime: config.AdapterClaudeCode})
			status, lease := e.poll(t, token)
			if status != http.StatusOK || lease.Job.JobMCP == nil {
				t.Fatalf("poll: %d %+v", status, lease)
			}
			m := lease.Job.JobMCP
			if m.URL != gatewayURL || !strings.HasPrefix(m.Token, jobmcp.TokenPrefix) || len(m.Token) != len(jobmcp.TokenPrefix)+43 {
				t.Fatalf("job_mcp = %+v", m)
			}
			got, err := e.st.Jobs().GetByJobMCPTokenHash(t.Context(), mcp.HashToken(m.Token))
			if err != nil || got.ID != j.ID {
				t.Fatalf("token lookup = %+v, %v", got, err)
			}
			if got.Spec.JobMCP != nil || strings.Contains(e.logs.String(), m.Token) {
				t.Fatal("token stored or logged")
			}
		})
	}
}

func TestJobMCPTokenNotForCommandJobs(t *testing.T) {
	e := newEnv(t, withGateway(nil))
	_, token := e.addWorker(t, "w")
	e.queue(t, domain.Job{Kind: domain.JobKindCommand, Spec: domain.JobSpec{Kind: domain.JobKindCommand, Command: &domain.CommandSpec{Shell: "make test"}}})
	if status, lease := e.poll(t, token); status != http.StatusOK || lease.Job.JobMCP != nil {
		t.Fatalf("poll: %d %+v", status, lease.Job.JobMCP)
	}
}

// TestJobMCPTokenWithoutPublicURL: workers reach the gateway through their
// own server URL, so the token does not depend on the public one.
func TestJobMCPTokenWithoutPublicURL(t *testing.T) {
	e := newEnv(t)
	_, token := e.addWorker(t, "w")
	e.newJob(t, jobOpts{})
	status, lease := e.poll(t, token)
	if status != http.StatusOK || lease.Job.JobMCP == nil || lease.Job.JobMCP.URL != "" || lease.Job.JobMCP.Token == "" {
		t.Fatalf("poll: %d %+v", status, lease.Job.JobMCP != nil)
	}
}

// beat sends a heartbeat acknowledging received and returns the answer.
func (e *env) beat(t *testing.T, token string, j domain.Job, received ...jobmcp.Attachment) heartbeatResponse {
	t.Helper()
	st, body := e.call(t, token, jobPath(j.ID, "heartbeat"), map[string]any{"attach_received": received})
	wantStatus(t, st, http.StatusOK, body)
	var hb heartbeatResponse
	decodeBody(t, body, &hb)
	return hb
}

// TestHeartbeatAttach: heartbeats hand over the marked files until the
// worker acknowledges them, so an answer lost on the way loses nothing.
func TestHeartbeatAttach(t *testing.T) {
	att := jobmcp.NewAttachments(nil)
	e := newEnv(t, withGateway(att))
	_, token := e.addWorker(t, "w")
	j := e.newJob(t, jobOpts{})
	if status, _ := e.poll(t, token); status != http.StatusOK {
		t.Fatalf("poll: %d", status)
	}
	a := jobmcp.Attachment{Path: "shots/a.png", Name: "evidence/shots/a.png"}
	att.Add(e.job(t, j.ID), a)
	for range 2 { // the first answer was lost
		if hb := e.beat(t, token, j); hb.Action != actionContinue || len(hb.Attach) != 1 || hb.Attach[0] != a {
			t.Fatalf("heartbeat = %+v", hb)
		}
	}
	if hb := e.beat(t, token, j, a); hb.Action != actionContinue || len(hb.Attach) != 0 {
		t.Fatalf("heartbeat after the ack = %+v", hb)
	}
	// A canceled job drops what is left.
	att.Add(e.job(t, j.ID), jobmcp.Attachment{Path: "b", Name: "evidence/b"})
	if _, err := e.jobs.Cancel(t.Context(), system, e.account, j.ID); err != nil {
		t.Fatal(err)
	}
	st, body := e.call(t, token, jobPath(j.ID, "heartbeat"), nil)
	wantStatus(t, st, http.StatusOK, body)
	if strings.Contains(string(body), "attach") || att.Pending(e.job(t, j.ID)) != nil {
		t.Fatalf("canceled heartbeat = %s", body)
	}
}

// TestCompleteDropsAttachments: marks nobody picked up end with the job.
func TestCompleteDropsAttachments(t *testing.T) {
	for _, status := range []string{completeSucceeded, completeRetry} {
		t.Run(status, func(t *testing.T) {
			att := jobmcp.NewAttachments(nil)
			e := newEnv(t, withGateway(att))
			_, token := e.addWorker(t, "w")
			j := e.newJob(t, jobOpts{})
			if st, _ := e.poll(t, token); st != http.StatusOK {
				t.Fatalf("poll: %d", st)
			}
			leased := e.job(t, j.ID)
			att.Add(leased, jobmcp.Attachment{Path: "b", Name: "evidence/b"})
			st, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": status, "error": "x"})
			wantStatus(t, st, http.StatusOK, body)
			if att.Pending(leased) != nil {
				t.Fatal("marks kept after complete")
			}
		})
	}
}
