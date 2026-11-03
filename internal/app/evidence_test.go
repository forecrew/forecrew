package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

func evidenceConfig(c *config.Config) {
	c.Evidence = &config.Evidence{
		Endpoint: "http://garage:3900", Bucket: "forecrew-evidence", Region: "garage", PathStyle: true,
		MaxArtifactSize: 10, RetentionDays: 1, AccessKeyID: "AKIDSECRET", SecretAccessKey: "SECRETSECRET",
	}
}

// request sends a request to the app and returns the status and body.
func request(t *testing.T, base, method, path, token string, body io.Reader) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, body)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(data)
}

// runningJob adds a worker and leases it a job through the worker API.
func runningJob(t *testing.T, f *fixture, base string) (job domain.Job, workerToken string) {
	t.Helper()
	workerToken = "fcw_test-worker"
	must(t, f.st.Workers().Create(t.Context(), &domain.Worker{AccountID: f.account.ID, Name: "box", TokenHash: mcp.HashToken(workerToken), MaxJobs: 1}))
	job = domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code", Spec: domain.JobSpec{Repo: "acme/app", Model: "m", Prompt: "p"}}
	must(t, f.st.Jobs().Create(t.Context(), &job))
	status, _, body := request(t, base, "POST", "/worker/v1/poll", workerToken, strings.NewReader(`{"labels":["runtime:claude-code"],"free_slots":1,"version":"v"}`))
	if status != http.StatusOK {
		t.Fatalf("poll: %d %s", status, body)
	}
	return job, workerToken
}

func TestEvidenceEndToEnd(t *testing.T) {
	ev := testkit.NewEvidence()
	f := newFixture(t, func(d *Deps) { evidenceConfig(d.Config); d.Evidence = ev })
	srv := httptest.NewServer(f.app.Handler())
	t.Cleanup(srv.Close)
	const userToken = "fcm_user"
	must(t, f.st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: f.account.ID, Name: "me", TokenHash: mcp.HashToken(userToken)}))
	job, workerToken := runningJob(t, f, srv.URL)
	up := "/worker/v1/jobs/" + job.ID.String() + "/artifacts/reports/unit.xml"

	// The size limit of the config applies.
	if status, _, body := request(t, srv.URL, "PUT", up, workerToken, strings.NewReader("12345678901")); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit: %d %s", status, body)
	}
	status, _, body := request(t, srv.URL, "PUT", up, workerToken, strings.NewReader("<ok/>"))
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, body)
	}
	var created struct{ ID string }
	must(t, json.Unmarshal([]byte(body), &created))

	status, _, body = request(t, srv.URL, "GET", "/api/v1/jobs/"+job.Ref()+"/artifacts", userToken, nil)
	if status != http.StatusOK || !strings.Contains(body, created.ID) || !strings.Contains(body, "reports/unit.xml") {
		t.Fatalf("list: %d %s", status, body)
	}
	status, h, body := request(t, srv.URL, "GET", "/api/v1/artifacts/"+created.ID, userToken, nil)
	if status != http.StatusOK || body != "<ok/>" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %q %v", status, body, h)
	}
	// Workers cannot read artifacts, and nobody without a token can.
	if status, _, _ := request(t, srv.URL, "GET", "/api/v1/artifacts/"+created.ID, workerToken, nil); status != http.StatusUnauthorized {
		t.Errorf("worker token downloads: %d", status)
	}
}

func TestEvidenceNotConfigured(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.app.Handler())
	t.Cleanup(srv.Close)
	const userToken = "fcm_user"
	must(t, f.st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: f.account.ID, Name: "me", TokenHash: mcp.HashToken(userToken)}))
	job, workerToken := runningJob(t, f, srv.URL)

	status, _, body := request(t, srv.URL, "PUT", "/worker/v1/jobs/"+job.ID.String()+"/artifacts/a.log", workerToken, strings.NewReader("x"))
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "unavailable") {
		t.Fatalf("upload: %d %s", status, body)
	}
	a := domain.Artifact{AccountID: f.account.ID, ProjectID: f.project.ID, JobID: job.ID, Name: "a.log", StorageKey: "k"}
	must(t, f.st.Artifacts().Put(t.Context(), &a))
	if status, _, body := request(t, srv.URL, "GET", "/api/v1/artifacts/"+a.ID.String(), userToken, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("download: %d %s", status, body)
	}
	if f.app.retention != nil {
		t.Error("retention is on without evidence storage")
	}
}

func TestEvidenceFromConfig(t *testing.T) {
	var logs strings.Builder
	f := newFixture(t, func(d *Deps) {
		evidenceConfig(d.Config)
		d.Logger = slogTo(&logs)
	})
	if f.app.retention == nil || f.app.retention.Days != 1 {
		t.Fatalf("retention = %+v", f.app.retention)
	}
	if !strings.Contains(logs.String(), "evidence storage configured") || strings.Contains(logs.String(), "SECRET") {
		t.Errorf("logs: %s", logs.String())
	}

	cfg := testConfig()
	evidenceConfig(cfg)
	cfg.Evidence.Endpoint = "not a url"
	if _, err := New(Deps{Config: cfg, Store: testkit.NewStore(nil)}); err == nil {
		t.Error("a bad evidence endpoint was accepted")
	}

	cfg = testConfig()
	evidenceConfig(cfg)
	cfg.Evidence.RetentionDays = 0
	a, err := New(Deps{Config: cfg, Store: testkit.NewStore(nil)})
	if err != nil || a.retention != nil {
		t.Errorf("retention with 0 days: %v, %v", a.retention, err)
	}
}

func TestRunSweepsExpiredArtifacts(t *testing.T) {
	ev := testkit.NewEvidence()
	ln := listen(t)
	f := newFixture(t, func(d *Deps) {
		evidenceConfig(d.Config)
		d.Evidence, d.Listener = ev, ln
	})
	job := domain.Job{AccountID: f.account.ID, ProjectID: f.project.ID, Role: "developer"}
	must(t, f.st.Jobs().Create(t.Context(), &job))
	a := domain.Artifact{AccountID: f.account.ID, ProjectID: f.project.ID, JobID: job.ID, Name: "a.log", StorageKey: evidence.JobKey(f.account.ID, job.ID, "a.log")}
	must(t, f.st.Artifacts().Put(t.Context(), &a))
	must(t, ev.Put(t.Context(), a.StorageKey, strings.NewReader("x"), 1, ""))
	f.app.retention.Now = func() time.Time { return t0.AddDate(0, 0, 30) }

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- f.app.Run(ctx) }()
	waitFor(t, "the sweep", func() bool { return len(ev.Keys()) == 0 })
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Artifacts().Get(t.Context(), f.account.ID, a.ID); err == nil {
		t.Error("the row is still there")
	}
}
